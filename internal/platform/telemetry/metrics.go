package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a private registry: no global registration or cross-test
// leakage. Instrumentation failures never affect booking/payment outcomes.
type Metrics struct {
	registry        *prometheus.Registry
	requests        *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	confirmations   prometheus.Counter
	captured        prometheus.Counter
	captureAttempts *prometheus.CounterVec
}

// ActiveHoldCount is a scrape-time read, not increment/decrement accounting:
// Redis TTL can expire without executing application code. The callback must
// count only future expiry scores, not stale ZSET members (ADR 0012).
type ActiveHoldCount func(context.Context, time.Time) (int64, error)

func NewMetrics(active ActiveHoldCount, redisTimeout time.Duration) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "booking_http_requests_total", Help: "Completed HTTP requests, excluding GET/HEAD probes and metrics scrapes.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "booking_http_request_duration_seconds", Help: "HTTP handler duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "route", "status"}),
		confirmations: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "booking_confirmations_total", Help: "Newly committed bookings observed by this process; excludes replays.",
		}),
		captured: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "booking_payments_captured_total", Help: "New durable captured-payment transitions observed by this process, including webhooks.",
		}),
		captureAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "booking_payment_capture_attempts_total", Help: "Client-initiated gateway capture calls by outcome; excludes webhook delivery and short-circuited replays.",
		}, []string{"outcome"}),
	}
	m.registry.MustRegister(m.requests, m.duration, m.confirmations, m.captured, m.captureAttempts,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	for _, outcome := range []string{"success", "declined", "error"} {
		m.captureAttempts.WithLabelValues(outcome)
	}
	if active != nil {
		m.registry.MustRegister(&activeHoldsCollector{
			count: active, timeout: redisTimeout,
			holds: prometheus.NewDesc("booking_active_holds", "Holds whose Redis expiry score is strictly in the future; omitted when Redis cannot be read.", nil, nil),
			up:    prometheus.NewDesc("booking_active_holds_collection_success", "1 if the active-holds read succeeded, 0 otherwise.", nil, nil),
		})
	}
	return m
}

// Handler caps simultaneous scrapes. Redis reads have their own deadline;
// a Redis outage omits that gauge and exports success=0, never a false zero.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{MaxRequestsInFlight: 2})
}

// RecordHTTP accepts only a ServeMux template (or "unmatched"), never a raw
// URL, query, ID or header value. Methods are a finite set, even for probes
// using attacker-selected HTTP method tokens.
func (m *Metrics) RecordHTTP(method, route string, status int, elapsed time.Duration) {
	method = HTTPMethod(method)
	code := strconv.Itoa(status)
	m.requests.WithLabelValues(method, route, code).Inc()
	m.duration.WithLabelValues(method, route, code).Observe(elapsed.Seconds())
}

func HTTPMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH":
		return method
	default:
		return "OTHER"
	}
}

func (m *Metrics) BookingConfirmed() { m.confirmations.Inc() }
func (m *Metrics) PaymentCaptured()  { m.captured.Inc() }

func (m *Metrics) GatewayCapture(outcome string) {
	if outcome != "success" && outcome != "declined" {
		outcome = "error"
	}
	m.captureAttempts.WithLabelValues(outcome).Inc()
}

type activeHoldsCollector struct {
	count   ActiveHoldCount
	timeout time.Duration
	holds   *prometheus.Desc
	up      *prometheus.Desc
}

func (c *activeHoldsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.holds
	ch <- c.up
}

func (c *activeHoldsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	count, err := c.count(ctx, time.Now())
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.holds, prometheus.GaugeValue, float64(count))
}
