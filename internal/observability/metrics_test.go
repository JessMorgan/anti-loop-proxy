package observability

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// newTestMetrics builds a Metrics on a fresh registry so tests are isolated
// from the default registry and from each other.
func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	return New(prometheus.NewRegistry())
}

// familyNames lists the names of the gathered metric families, for
// diagnostics.
func familyNames(families []*dto.MetricFamily) []string {
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	return names
}

// histogramText renders the Prometheus exposition text for a single
// histogram series (model) with one observation of value, used as the
// expected side of testutil.CollectAndCompare.
func histogramText(name, model string, value float64, buckets []float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP %s help\n", name)
	fmt.Fprintf(&b, "# TYPE %s histogram\n", name)
	for _, bucket := range buckets {
		cum := 0.0
		if value <= bucket {
			cum = 1
		}
		fmt.Fprintf(&b, "%s_bucket{model=%q,le=%q} %v\n", name, model, strconv.FormatFloat(bucket, 'g', -1, 64), cum)
	}
	fmt.Fprintf(&b, "%s_bucket{model=%q,le=\"+Inf\"} 1\n", name, model)
	fmt.Fprintf(&b, "%s_sum{model=%q} %v\n", name, model, value)
	fmt.Fprintf(&b, "%s_count{model=%q} 1\n", name, model)
	return b.String()
}

func TestNewRegistersExactlySevenMetrics(t *testing.T) {
	m := newTestMetrics(t)
	// Vec collectors emit no family until a child series exists, so record
	// one observation per metric first.
	m.RecordRequest("gpt-4", 500, time.Second)
	m.RecordStreamRequest("gpt-4")
	m.StreamStarted("gpt-4")
	m.StreamFinished("gpt-4")
	m.RecordStreamCut("gpt-4", 24)

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) != 7 {
		t.Fatalf("expected 7 registered metric families, got %d: %v", len(families), familyNames(families))
	}
}

func TestRecordRequest(t *testing.T) {
	m := newTestMetrics(t)
	m.RecordRequest("gpt-4", 200, 1500*time.Millisecond)

	if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues("gpt-4", "200")); got != 1 {
		t.Errorf("requests_total{gpt-4,200} = %v, want 1", got)
	}
	if err := testutil.CollectAndCompare(m.requestDuration,
		strings.NewReader(histogramText("anti_loop_request_duration_seconds", "gpt-4", 1.5, requestDurationBuckets)),
		"sum"); err != nil {
		t.Errorf("request_duration_seconds{gpt-4} sum: %v", err)
	}
	// A 200 must not count as an upstream error.
	if got := testutil.CollectAndCount(m.upstreamErrorsTotal); got != 0 {
		t.Errorf("upstream_errors_total series = %d, want 0 after a 200", got)
	}
}

func TestRecordRequestUpstreamErrors(t *testing.T) {
	tests := []struct {
		status int
		class  string
	}{
		{400, "4xx"},
		{404, "4xx"},
		{429, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
	}
	for _, tc := range tests {
		t.Run("status "+strconv.Itoa(tc.status), func(t *testing.T) {
			m := newTestMetrics(t)
			m.RecordRequest("gpt-4", tc.status, time.Second)
			if got := testutil.ToFloat64(m.upstreamErrorsTotal.WithLabelValues("gpt-4", tc.class)); got != 1 {
				t.Errorf("upstream_errors_total{gpt-4,%s} = %v, want 1", tc.class, got)
			}
		})
	}
}

func TestRecordStreamRequest(t *testing.T) {
	m := newTestMetrics(t)
	m.RecordStreamRequest("gpt-4")
	m.RecordStreamRequest("gpt-4")

	if got := testutil.ToFloat64(m.streamRequestsTotal.WithLabelValues("gpt-4")); got != 2 {
		t.Errorf("stream_requests_total{gpt-4} = %v, want 2", got)
	}
}

func TestStreamStartedFinished(t *testing.T) {
	m := newTestMetrics(t)

	if got := testutil.ToFloat64(m.activeStreams.WithLabelValues("gpt-4")); got != 0 {
		t.Fatalf("active_streams{gpt-4} before start = %v, want 0", got)
	}
	m.StreamStarted("gpt-4")
	if got := testutil.ToFloat64(m.activeStreams.WithLabelValues("gpt-4")); got != 1 {
		t.Fatalf("active_streams{gpt-4} after start = %v, want 1", got)
	}
	m.StreamFinished("gpt-4")
	if got := testutil.ToFloat64(m.activeStreams.WithLabelValues("gpt-4")); got != 0 {
		t.Fatalf("active_streams{gpt-4} after finish = %v, want 0", got)
	}
}

func TestRecordStreamCut(t *testing.T) {
	m := newTestMetrics(t)
	m.RecordStreamCut("gpt-4", 24)

	if got := testutil.ToFloat64(m.streamCutsTotal.WithLabelValues("gpt-4")); got != 1 {
		t.Errorf("stream_cuts_total{gpt-4} = %v, want 1", got)
	}
	if err := testutil.CollectAndCompare(m.cutSpanLen,
		strings.NewReader(histogramText("anti_loop_cut_span_len", "gpt-4", 24, cutSpanLenBuckets)),
		"sum"); err != nil {
		t.Errorf("cut_span_len{gpt-4} sum: %v", err)
	}
}

func TestHandler(t *testing.T) {
	m := newTestMetrics(t)
	m.RecordRequest("gpt-4", 200, time.Second)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "anti_loop_requests_total") {
		t.Errorf("body does not contain anti_loop_requests_total:\n%s", body)
	}
}
