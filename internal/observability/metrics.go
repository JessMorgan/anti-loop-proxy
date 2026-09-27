// Package observability exposes the proxy's Prometheus metrics.
//
// All metric vectors are owned by a single Metrics value so the rest of the
// codebase (notably internal/proxy) records observations through methods
// without ever touching the prometheus client directly.
package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// requestDurationBuckets are sensible latency buckets (seconds) for an LLM
// proxy: sub-second pings up to multi-minute streaming completions.
var requestDurationBuckets = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120}

// cutSpanLenBuckets are the span-length (runes) buckets observed when a
// stream is cut for looping.
var cutSpanLenBuckets = []float64{12, 16, 24, 32, 64, 128, 200, 4096}

// Metrics owns all Prometheus vectors for the proxy.
type Metrics struct {
	registry prometheus.Gatherer

	requestsTotal       *prometheus.CounterVec
	streamRequestsTotal *prometheus.CounterVec
	streamCutsTotal     *prometheus.CounterVec
	requestDuration     *prometheus.HistogramVec
	activeStreams       *prometheus.GaugeVec
	upstreamErrorsTotal *prometheus.CounterVec
	cutSpanLen          *prometheus.HistogramVec
}

// New registers all metrics on r and returns them.
func New(r prometheus.Registerer) *Metrics {
	m := &Metrics{
		registry: gathererFor(r),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "anti_loop_requests_total",
			Help: "Total proxied requests, by model and response status.",
		}, []string{"model", "status"}),
		streamRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "anti_loop_stream_requests_total",
			Help: "Total streaming (stream:true) requests, by model.",
		}, []string{"model"}),
		streamCutsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "anti_loop_stream_cuts_total",
			Help: "Total streams cut for looping, by model.",
		}, []string{"model"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "anti_loop_request_duration_seconds",
			Help:    "Request latency in seconds, by model.",
			Buckets: requestDurationBuckets,
		}, []string{"model"}),
		activeStreams: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "anti_loop_active_streams",
			Help: "In-flight streaming requests, by model.",
		}, []string{"model"}),
		upstreamErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "anti_loop_upstream_errors_total",
			Help: "Total upstream error responses, by model and status class.",
		}, []string{"model", "status_class"}),
		cutSpanLen: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "anti_loop_cut_span_len",
			Help:    "Detected span length in runes at trigger, by model.",
			Buckets: cutSpanLenBuckets,
		}, []string{"model"}),
	}

	r.MustRegister(
		m.requestsTotal,
		m.streamRequestsTotal,
		m.streamCutsTotal,
		m.requestDuration,
		m.activeStreams,
		m.upstreamErrorsTotal,
		m.cutSpanLen,
	)
	return m
}

// NewDefault registers all metrics on the prometheus default registry.
func NewDefault() *Metrics {
	return New(prometheus.DefaultRegisterer)
}

// gathererFor returns the Gatherer that exposes metrics registered on r.
// For the default registerer that is the default gatherer; for any other
// registerer (e.g. prometheus.NewRegistry) it is the registerer itself.
func gathererFor(r prometheus.Registerer) prometheus.Gatherer {
	if g, ok := r.(prometheus.Gatherer); ok {
		return g
	}
	return prometheus.DefaultGatherer
}

// Handler returns an http.Handler that serves the Prometheus exposition
// format from the registry the metrics were registered on (typically
// mounted at /metrics).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// RecordRequest records one proxied request: the request counter, the
// duration histogram, and — for 4xx/5xx statuses — the upstream error
// counter.
func (m *Metrics) RecordRequest(model string, status int, d time.Duration) {
	m.requestsTotal.WithLabelValues(model, strconv.Itoa(status)).Inc()
	m.requestDuration.WithLabelValues(model).Observe(d.Seconds())
	if status >= 400 && status < 600 {
		class := "4xx"
		if status >= 500 {
			class = "5xx"
		}
		m.upstreamErrorsTotal.WithLabelValues(model, class).Inc()
	}
}

// RecordStreamRequest records one streaming (stream:true) request.
func (m *Metrics) RecordStreamRequest(model string) {
	m.streamRequestsTotal.WithLabelValues(model).Inc()
}

// StreamStarted increments the active-streams gauge for model.
func (m *Metrics) StreamStarted(model string) {
	m.activeStreams.WithLabelValues(model).Inc()
}

// StreamFinished decrements the active-streams gauge for model.
func (m *Metrics) StreamFinished(model string) {
	m.activeStreams.WithLabelValues(model).Dec()
}

// RecordStreamCut records one stream cut for looping: the cut counter and
// the span-length histogram (spanLen is in runes).
func (m *Metrics) RecordStreamCut(model string, spanLen int) {
	m.streamCutsTotal.WithLabelValues(model).Inc()
	m.cutSpanLen.WithLabelValues(model).Observe(float64(spanLen))
}
