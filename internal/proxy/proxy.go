// Package proxy: reverse-proxy wiring for the anti-loop proxy.
//
// New returns an http.Handler that serves /healthz locally and reverse-proxies
// everything else to the configured upstream. For SSE responses to requests
// with "stream": true, the response body is replaced by a filter body that
// runs the StreamFilter exactly once. The filter writes into a pipe (not the
// client ResponseWriter directly, which would race with the proxy's own
// copy loop and bypass transfer framing); the proxy's copy loop delivers the
// bytes to the client.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"anti-loop-proxy/internal/config"
)

// maxSniffBytes caps the request body size that is read for sniffing the
// "stream" flag. Bodies with a known ContentLength above this cap are left
// completely untouched (no read at all).
const maxSniffBytes = 10 << 20 // 10 MB

type stateKey struct{}

// requestState is a per-request side channel shared between the wrapper
// handler, the Director/ModifyResponse hooks (via request context), and the
// filtered response body.
type requestState struct {
	stream    bool // request body had "stream": true
	streamCut bool // StreamFilter triggered and cut the stream
}

// statusWriter captures the response status code for post-copy logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// handler is the outer http.Handler: it serves /healthz, sniffs the request
// body, and measures duration for the post-copy access log.
type handler struct {
	proxy *httputil.ReverseProxy
	log   *slog.Logger
}

// New builds the proxy handler for cfg.
func New(cfg *config.Config, log *slog.Logger) *http.Handler {
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil || upstream.Host == "" {
		var bad http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "invalid upstream", http.StatusBadGateway)
		})
		return &bad
	}
	h := &handler{log: log}
	h.proxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			// Preserve the upstream's base path prefix (e.g. /v1).
			req.URL.Path = path.Join(upstream.Path, req.URL.Path)
			req.Host = upstream.Host
			if cfg.UpstreamAPIKey != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.UpstreamAPIKey)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			if !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
				return nil // byte-for-byte passthrough
			}
			req := res.Request
			st, _ := req.Context().Value(stateKey{}).(*requestState)
			if st == nil || !st.stream {
				return nil // non-stream request: passthrough
			}
			// Replace the body: the StreamFilter writes into a pipe, and
			// the proxy's own copy loop reads from it, so the bytes go
			// through the proper transfer framing (writing to the client
			// ResponseWriter directly would bypass the chunked encoder).
			pr, pw := io.Pipe()
			res.Body = &filteredBody{
				ctx:    req.Context(),
				filter: NewStreamFilter(pw, res.Body, paramsFrom(cfg), log),
				st:     st,
				pr:     pr,
				pw:     pw,
			}
			return nil
		},
	}
	hnd := http.Handler(h)
	return &hnd
}

func paramsFrom(cfg *config.Config) Params {
	return Params{
		MinCount: cfg.MinCount,
		MinLen:   cfg.MinLen,
		MaxLen:   cfg.MaxLen,
		MaxGap:   cfg.MaxGap,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
		return
	}

	// Read the body once, sniff "stream", and reattach it for the proxy.
	st := &requestState{stream: sniffStream(r)}
	r = r.WithContext(context.WithValue(r.Context(), stateKey{}, st))

	sw := &statusWriter{ResponseWriter: w}
	start := time.Now()
	h.proxy.ServeHTTP(sw, r)
	h.log.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", sw.status,
		"stream_cut", st.streamCut,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// sniffStream reads the request body once (capped at maxSniffBytes), decodes
// the top-level "stream" bool, and replaces req.Body with a bytes.Reader so
// the proxy can send it. Bodies with a known ContentLength above the cap are
// passed through unmodified (never read).
func sniffStream(r *http.Request) bool {
	if r.Body == nil {
		return false
	}
	if r.ContentLength > maxSniffBytes {
		return false // too large: do not enable stream filtering
	}
	data, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Stream
}

// filteredBody is the replacement response body for stream-filtered SSE
// responses. Its first Read starts the StreamFilter exactly once; the filter
// writes into the pipe and this body reads from it, so the proxy's own
// copy-to-client loop delivers every byte (including the anti_loop trigger
// event) through the proper transfer framing. The filter records streamCut
// in the shared requestState so the wrapper handler can include it in the
// post-copy access log.
type filteredBody struct {
	ctx    context.Context
	filter *StreamFilter
	st     *requestState
	once   sync.Once
	pr     *io.PipeReader
	pw     *io.PipeWriter
}

func (b *filteredBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		go func() {
			cut, _, err := b.filter.Run(b.ctx)
			b.st.streamCut = cut
			if err != nil {
				b.pw.CloseWithError(err)
				return
			}
			_ = b.pw.Close()
		}()
	})
	return b.pr.Read(p)
}

// Close unblocks the filter if the proxy stops copying (e.g. client
// disconnect) by closing the pipe reader.
func (b *filteredBody) Close() error {
	if b.pr != nil {
		_ = b.pr.Close()
	}
	return nil
}
