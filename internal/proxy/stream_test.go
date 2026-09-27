package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// pipeReadCloser adapts an io.Pipe's read end to io.ReadCloser. Close
// closes the pipe read end, unblocking any pending read.
type pipeReadCloser struct {
	r *io.PipeReader
}

func (p pipeReadCloser) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p pipeReadCloser) Close() error              { return p.r.Close() }

// feedPipe writes raw to the pipe writer in a goroutine, then closes it.
func feedPipe(t *testing.T, raw string) io.ReadCloser {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(raw))
		_ = pw.Close()
	}()
	return pipeReadCloser{pr}
}

// makePipe wraps an io.Pipe's read end in an io.ReadCloser.
func makePipe() (io.ReadCloser, *io.PipeWriter) {
	pr, pw := io.Pipe()
	return pipeReadCloser{pr}, pw
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPassthroughByteIdentity verifies that a non-looping transcript is
// forwarded byte-for-byte and does not trigger.
func TestPassthroughByteIdentity(t *testing.T) {
	transcript := strings.Join([]string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"Hello "}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"world, "}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"how are you?"}}]}`,
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"","finish_reason":"stop"}}]}`,
		`data:`,
		``,
	}, "\n")

	var buf bytes.Buffer
	f := NewStreamFilter(&buf, feedPipe(t, transcript), testParams(), testLog())
	triggered, res, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if triggered {
		t.Fatalf("unexpected trigger: %+v", res)
	}
	if buf.String() != transcript {
		t.Fatalf("output not byte-identical:\n got: %q\nwant: %q", buf.String(), transcript)
	}
}

func testParams() Params {
	return Params{MinCount: 4, MinLen: 12, MaxLen: 200, MaxGap: 0}
}

// chunk builds an SSE data line carrying delta content for the given choice
// index.
func chunk(index int, content string) string {
	b, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}{ID: "1", Choices: []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	}{{Index: index, Delta: struct {
		Content string `json:"content"`
	}{Content: content}}}})
	return "data: " + string(b) + "\n"
}

// TestTriggerMidStream verifies the cut-off block and the returned Result.
func TestTriggerMidStream(t *testing.T) {
	span := "ABCDEFGHIJKL" // 12 runes
	lines := []string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n",
		chunk(0, span),
		chunk(0, span),
		chunk(0, span),
		chunk(0, span), // triggers here (4th occurrence)
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"never forwarded"}}]}`,
	}

	var buf bytes.Buffer
	f := NewStreamFilter(&buf, feedPipe(t, strings.Join(lines, "")), testParams(), testLog())
	triggered, res, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !triggered {
		t.Fatal("expected trigger, got none")
	}
	if res.Span != span || res.SpanLen != 12 || res.Count < 4 {
		t.Fatalf("unexpected result: %+v", res)
	}

	out := buf.String()
	// All pre-trigger lines unchanged.
	for i, l := range lines[:5] {
		if !strings.Contains(out, l) {
			t.Fatalf("line %d missing from output:\n%s", i, out)
		}
	}
	if strings.Contains(out, "never forwarded") {
		t.Fatalf("post-trigger line was forwarded:\n%s", out)
	}
	if n := strings.Count(out, "event: anti_loop"); n != 1 {
		t.Fatalf("expected exactly one anti_loop event, got %d:\n%s", n, out)
	}

	// Verify the exact trailing block.
	wantBlock := "event: anti_loop\ndata: "
	idx := strings.Index(out, wantBlock)
	if idx < 0 {
		t.Fatalf("anti_loop event not found:\n%s", out)
	}
	rest := out[idx+len(wantBlock):]
	dataLine := rest[:strings.Index(rest, "\n")]
	var payload struct {
		Reason  string `json:"reason"`
		Count   int    `json:"count"`
		SpanLen int    `json:"span_len"`
		Span    string `json:"span"`
	}
	if err := json.Unmarshal([]byte(dataLine), &payload); err != nil {
		t.Fatalf("invalid JSON data line %q: %v", dataLine, err)
	}
	if payload.Reason != "duplicate_content" || payload.Count < 4 || payload.SpanLen != 12 || payload.Span != span {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !strings.HasSuffix(out, "event: anti_loop\ndata: "+dataLine+"\n\ndata: [DONE]\n\n") {
		t.Fatalf("output does not end with the exact trigger block:\n%s", out)
	}
}

// TestMalformedJSON verifies malformed data lines are forwarded, logged, and
// do not break the stream.
func TestMalformedJSON(t *testing.T) {
	raw := "data: {not json\n" + chunk(0, "unique content here") + "\ndata: [DONE]\n"
	var buf bytes.Buffer
	f := NewStreamFilter(&buf, feedPipe(t, raw), testParams(), testLog())
	triggered, _, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if triggered {
		t.Fatal("unexpected trigger")
	}
	if buf.String() != raw {
		t.Fatalf("malformed line not forwarded byte-for-byte:\n got: %q\nwant: %q", buf.String(), raw)
	}
}

// TestLongLine verifies a line longer than the default bufio buffer
// (4096 bytes) is forwarded whole and parsed correctly.
func TestLongLine(t *testing.T) {
	// 5000 digits: unique enough to avoid a loop trigger.
	content := fmt.Sprintf("%05000d", 123456789)
	raw := chunk(0, content)
	if len(raw) < 4096 {
		t.Fatalf("test line too short to span buffer reads: %d", len(raw))
	}
	var buf bytes.Buffer
	f := NewStreamFilter(&buf, feedPipe(t, raw), testParams(), testLog())
	triggered, _, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if triggered {
		t.Fatal("unexpected trigger")
	}
	if buf.String() != raw {
		t.Fatalf("long line not forwarded byte-for-byte: got %d bytes, want %d", len(buf.String()), len(raw))
	}
}

// failWriter succeeds on the first write, then fails.
type failWriter struct {
	buf bytes.Buffer
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.buf.Len() == 0 {
		return f.buf.Write(p)
	}
	return 0, errors.New("client connection closed")
}

// TestClientWriteFailure verifies Run returns the write error and closes the
// upstream pipe.
func TestClientWriteFailure(t *testing.T) {
	pr, pw := io.Pipe()
	writeErr := make(chan error, 1)
	go func() {
		// Keep feeding lines until the reader closes the pipe.
		for i := 0; ; i++ {
			_, err := pw.Write([]byte(fmt.Sprintf("data: line %d\n", i)))
			if err != nil {
				writeErr <- err
				return
			}
		}
	}()

	w := &failWriter{}
	f := NewStreamFilter(w, pipeReadCloser{pr}, testParams(), testLog())
	_, _, err := f.Run(context.Background())
	if err == nil {
		t.Fatal("expected write error, got nil")
	}
	if !strings.Contains(err.Error(), "client connection closed") {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case werr := <-writeErr:
		if !errors.Is(werr, io.ErrClosedPipe) {
			t.Fatalf("upstream writer did not get a closed-pipe error: %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream writer to see the closed pipe")
	}
}

// TestContextCancellation verifies Run returns context.Canceled when ctx is
// cancelled while reading.
func TestContextCancellation(t *testing.T) {
	up, pw := makePipe()
	// Write one line, then keep the pipe open so Run blocks in ReadSlice.
	go func() {
		_, _ = pw.Write([]byte("data: first\n"))
	}()

	ctx, cancel := context.WithCancel(context.Background())
	f := NewStreamFilter(io.Discard, up, testParams(), testLog())
	go func() {
		// Cancel once Run is blocked reading.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, _, err := f.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

// TestPerChoiceIsolation verifies that only the looping choice triggers,
// while the other choice's unique content is ignored.
func TestPerChoiceIsolation(t *testing.T) {
	span := "ABCDEFGHIJKL"
	lines := []string{
		chunk(0, span), chunk(1, "totally unique text"),
		chunk(0, span), chunk(1, "more unique text"),
		chunk(0, span), chunk(1, "even more unique"),
		chunk(0, span), chunk(1, "still unique"),
	}
	var buf bytes.Buffer
	f := NewStreamFilter(&buf, feedPipe(t, strings.Join(lines, "")), testParams(), testLog())
	triggered, res, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !triggered {
		t.Fatal("expected trigger from choice 0")
	}
	if res.Span != span {
		t.Fatalf("unexpected span: %+v", res)
	}
	// Choice-1 lines up to the trigger must have been forwarded.
	for _, s := range []string{
		"totally unique text", "more unique text", "even more unique",
	} {
		if !strings.Contains(buf.String(), s) {
			t.Fatalf("choice-1 content %q not forwarded:\n%s", s, buf.String())
		}
	}
	// The choice-1 line after the trigger must NOT have been forwarded.
	if strings.Contains(buf.String(), "still unique") {
		t.Fatalf("post-trigger choice-1 line was forwarded:\n%s", buf.String())
	}
}
