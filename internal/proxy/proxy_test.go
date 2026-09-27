package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"anti-loop-proxy/internal/config"
	"anti-loop-proxy/internal/observability"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// testCfg builds a valid Config pointing at upstream.
func testCfg(upstream, apiKey string) *config.Config {
	return &config.Config{
		Listen:         ":0",
		Upstream:       upstream,
		UpstreamAPIKey: apiKey,
		MinCount:       4,
		MinLen:         12,
		MaxLen:         200,
		MaxGap:         0,
		LogLevel:       "info",
	}
}

func proxyTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startProxy starts an httptest upstream and the proxy handler (with fresh
// metrics on a dedicated registry), returning both servers and the metrics.
func startProxy(t *testing.T, upstreamHandler http.HandlerFunc, cfg *config.Config) (*httptest.Server, *httptest.Server, *prometheus.Registry) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)
	if cfg == nil {
		cfg = testCfg(up.URL, "")
	} else {
		cfg.Upstream = up.URL
	}
	reg := prometheus.NewRegistry()
	proxy := httptest.NewServer(*New(cfg, proxyTestLog(), observability.New(reg)))
	t.Cleanup(proxy.Close)
	return up, proxy, reg
}

// sseData builds an SSE data line for a chat completion delta.
func sseData(content string) string {
	payload, _ := json.Marshal(map[string]any{
		"id":     "chatcmpl-test",
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]string{"content": content},
		}},
	})
	return "data: " + string(payload) + "\n\n"
}

func TestNonStreamPassthrough(t *testing.T) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	_, proxy, _ := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("upstream Content-Type = %q, want application/json", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}, nil)

	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","stream":false,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(got) != body {
		t.Errorf("body = %q, want byte-identical %q", got, body)
	}
}

func TestStreamNoDuplicationPassthrough(t *testing.T) {
	chunks := []string{"alpha ", "beta ", "gamma ", "delta "}
	want := ""
	for _, c := range chunks {
		want += sseData(c)
	}
	want += "data: [DONE]\n\n"

	_, proxy, _ := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprint(w, sseData(c))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}, nil)

	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(got) != want {
		t.Errorf("stream body mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestStreamDuplicationCut(t *testing.T) {
	// 12-rune span repeated 4 times triggers the default Params.
	span := "abcdefghijkl"
	chunk := sseData(span) // "data: {...}\n\n"
	// The filter cuts immediately after the 4th data line, before that
	// line's trailing blank line, so the last chunk contributes only its
	// "data: {...}\n" (one trailing newline), not the full "\n\n".
	want := chunk + chunk + chunk + chunk[:len(chunk)-1] +
		"event: anti_loop\ndata: " +
		`{"reason":"duplicate_content","count":4,"span_len":12,"span":"abcdefghijkl"}` +
		"\n\ndata: [DONE]\n\n"

	upClosed := make(chan struct{})
	_, proxy, _ := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			fmt.Fprint(w, chunk)
			fl.Flush()
		}
		// Keep writing keepalive comments until the proxy closes the
		// upstream connection. The filter closes the upstream response
		// body when it cuts the stream, which makes the transport close
		// the connection; the server then cancels the request context.
		for {
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
			select {
			case <-r.Context().Done():
				close(upClosed)
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}, nil)

	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(got) != want {
		t.Errorf("stream body mismatch:\ngot:  %q\nwant: %q", got, want)
	}

	deadline := time.After(2 * time.Second)
	select {
	case <-upClosed:
	case <-deadline:
		t.Error("upstream body was not closed after stream cut")
	}
}

func TestUpstream401Passthrough(t *testing.T) {
	body := `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`
	_, proxy, _ := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, body)
	}, nil)

	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","stream":false,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if string(got) != body {
		t.Errorf("body = %q, want byte-identical %q", got, body)
	}
}

func TestHealthz(t *testing.T) {
	_, proxy, _ := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be reached for /healthz")
	}, nil)

	resp, err := http.Get(proxy.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(got) != "ok\n" {
		t.Errorf("body = %q, want %q", got, "ok\n")
	}
}

func TestAuthorizationOverride(t *testing.T) {
	var seen string
	_, proxy, _ := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}, testCfg("", "test-key"))

	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","stream":false,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen != "Bearer test-key" {
		t.Errorf("upstream Authorization = %q, want %q", seen, "Bearer test-key")
	}
}

func TestMetrics(t *testing.T) {
	// 12-rune span repeated 4 times triggers the default Params.
	span := "abcdefghijkl"
	chunk := sseData(span)

	_, proxy, reg := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/metrics") {
			t.Error("upstream should not be reached for /metrics")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for i := 0; i < 4; i++ {
				fmt.Fprint(w, chunk)
				fl.Flush()
			}
			// Keep writing until the proxy closes the upstream body.
			for {
				fmt.Fprint(w, ": keepalive\n\n")
				fl.Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","choices":[]}`)
	}, nil)

	// Non-stream request: only the request counter should increment.
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","stream":false,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-stream status = %d, want 200", resp.StatusCode)
	}

	// Looping stream request: request, stream-request, and cut counters.
	resp, err = http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	// testutil.CollectAndCompare asserts the exact exposition of each
	// counter family (the metrics are registered on the dedicated reg).
	if cmpErr := testutil.CollectAndCompare(reg, strings.NewReader(`
# HELP anti_loop_requests_total Total proxied requests, by model and response status.
# TYPE anti_loop_requests_total counter
anti_loop_requests_total{model="gpt-4",status="200"} 2
`), "anti_loop_requests_total"); cmpErr != nil {
		t.Errorf("anti_loop_requests_total: %v", cmpErr)
	}
	if cmpErr := testutil.CollectAndCompare(reg, strings.NewReader(`
# HELP anti_loop_stream_requests_total Total streaming (stream:true) requests, by model.
# TYPE anti_loop_stream_requests_total counter
anti_loop_stream_requests_total{model="gpt-4"} 1
`), "anti_loop_stream_requests_total"); cmpErr != nil {
		t.Errorf("anti_loop_stream_requests_total: %v", cmpErr)
	}
	if cmpErr := testutil.CollectAndCompare(reg, strings.NewReader(`
# HELP anti_loop_stream_cuts_total Total streams cut for looping, by model.
# TYPE anti_loop_stream_cuts_total counter
anti_loop_stream_cuts_total{model="gpt-4"} 1
`), "anti_loop_stream_cuts_total"); cmpErr != nil {
		t.Errorf("anti_loop_stream_cuts_total: %v", cmpErr)
	}

	// GET /metrics is served locally and returns 200.
	resp, err = http.Get(proxy.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "anti_loop_requests_total") {
		t.Errorf("/metrics body missing anti_loop_requests_total:\n%s", body)
	}
}
