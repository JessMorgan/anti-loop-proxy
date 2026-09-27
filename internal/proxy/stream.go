package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"
)

// maxSpanRunes is the maximum number of runes of the detected span
// included in the anti_loop event payload.
const maxSpanRunes = 80

// StreamFilter reads an upstream SSE stream line-by-line, forwards every
// line byte-for-byte to w, and feeds delta content into per-choice
// Detectors. When any detector triggers it cuts the stream off with an
// anti_loop event.
type StreamFilter struct {
	w    io.Writer
	up   io.ReadCloser
	p    Params
	log  *slog.Logger
	dets map[int]*Detector

	closeOnce sync.Once
}

// closeUp closes the upstream exactly once, no matter which path
// (trigger, EOF, write error, cancellation) runs it.
func (f *StreamFilter) closeUp() {
	f.closeOnce.Do(func() { _ = f.up.Close() })
}

// NewStreamFilter creates a StreamFilter that reads from up, writes to w,
// uses p as the detection thresholds, and logs via log.
func NewStreamFilter(w io.Writer, up io.ReadCloser, p Params, log *slog.Logger) *StreamFilter {
	return &StreamFilter{
		w:    w,
		up:   up,
		p:    p,
		log:  log,
		dets: make(map[int]*Detector),
	}
}

// Run pumps up into w until a detector triggers, up reaches EOF, w fails,
// or ctx is cancelled.
func (f *StreamFilter) Run(ctx context.Context) (bool, Result, error) {
	// Unblock a pending read if ctx is cancelled while we are blocked
	// reading from up.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			f.closeUp()
		case <-done:
		}
	}()

	br := bufio.NewReader(f.up)
	for {
		// ReadString grows its buffer as needed, so lines longer than the
		// initial bufio buffer (which may span multiple Read calls) are
		// returned whole.
		line, err := br.ReadString('\n')
		if line != "" {
			if _, werr := f.w.Write([]byte(line)); werr != nil {
				f.closeUp()
				return false, Result{}, werr
			}
			if res, ok := f.process(line); ok {
				f.writeTrigger(res)
				f.closeUp()
				f.log.Warn("stream cut off: duplicate content",
					"count", res.Count,
					"span_len", res.SpanLen,
					"span", res.Span)
				return true, res, nil
			}
		}
		if err != nil {
			f.closeUp()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, Result{}, ctxErr
			}
			// Upstream EOF (or a read error that ends the stream):
			// nothing more to do.
			return false, Result{}, nil
		}
	}
}

// process inspects one forwarded line (with its terminator). It returns a
// Result and true if any per-choice detector triggered on this line.
func (f *StreamFilter) process(line string) (Result, bool) {
	line = strings.TrimSuffix(line, "\r")
	if !strings.HasPrefix(line, "data: ") {
		return Result{}, false
	}
	payload := strings.TrimSpace(line[len("data: "):])
	if payload == "" {
		return Result{}, false
	}
	var msg struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		f.log.Debug("malformed JSON in data line", "err", err, "payload", payload)
		return Result{}, false
	}
	for _, c := range msg.Choices {
		if c.Delta.Content == "" {
			continue
		}
		d := f.dets[c.Index]
		if d == nil {
			d = NewDetector(f.p)
			f.dets[c.Index] = d
		}
		if res, ok := d.Feed(c.Delta.Content); ok {
			return res, true
		}
	}
	return Result{}, false
}

// writeTrigger writes the anti_loop event block that terminates the stream.
func (f *StreamFilter) writeTrigger(res Result) {
	span := res.Span
	if r := utf8.RuneCountInString(span); r > maxSpanRunes {
		span = string([]rune(span)[:maxSpanRunes])
	}
	payload, _ := json.Marshal(struct {
		Reason  string `json:"reason"`
		Count   int    `json:"count"`
		SpanLen int    `json:"span_len"`
		Span    string `json:"span"`
	}{
		Reason:  "duplicate_content",
		Count:   res.Count,
		SpanLen: res.SpanLen,
		Span:    span,
	})
	_, _ = fmt.Fprintf(f.w, "event: anti_loop\ndata: %s\n\ndata: [DONE]\n\n", payload)
}
