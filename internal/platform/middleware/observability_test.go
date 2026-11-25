package middleware

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/logger"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func testTraceProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous, propagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
		otel.SetTextMapPropagator(propagator)
	})
	return provider, exporter
}

func TestHTTPSpansMetricsAndCorrelatedLogs(t *testing.T) {
	_, exporter := testTraceProvider(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(logger.WithCorrelation(slog.NewJSONHandler(&logs, nil))))
	t.Cleanup(func() { slog.SetDefault(previous) })
	m := telemetry.NewMetrics(nil, time.Second)
	mux := http.NewServeMux()
	mux.Handle("POST /holds/{sessionID}/confirm", Auth(okValidate, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.InfoContext(r.Context(), "application outcome")
		_, child := otel.Tracer("fake-store").Start(r.Context(), "store call")
		child.End()
		w.WriteHeader(http.StatusCreated)
	})))
	handler := RequestID(Logging(Observability(m)(mux)))
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, id := range []string{"private-session-one", "private-session-two"} {
		r := httptest.NewRequest("POST", "/holds/"+id+"/confirm?access_token=private-query", nil)
		r.Header.Set("traceparent", parent)
		r.Header.Set("X-Request-ID", "request-123")
		r.Header.Set("Authorization", "Bearer good-token")
		r.Header.Set("Cookie", "refresh_token=private-cookie")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 201 {
			t.Fatalf("request status %d", w.Code)
		}
	}
	spans := exporter.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("span count = %d, want 2 HTTP + 2 store", len(spans))
	}
	for i := 0; i < len(spans); i += 2 {
		child, server := spans[i], spans[i+1]
		if server.Name != "POST /holds/{sessionID}/confirm" || server.SpanKind != trace.SpanKindServer ||
			server.Parent.SpanID().String() != "00f067aa0ba902b7" || server.SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" ||
			child.Parent.SpanID() != server.SpanContext.SpanID() {
			t.Errorf("broken distributed span tree: %+v / %+v", child, server)
		}
		if strings.Contains(fmt.Sprint(server.Attributes, server.Name), "private-") {
			t.Error("span leaked a path ID, query or credential")
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.Contains(line, `"request_id":"request-123"`) || !strings.Contains(line, `"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`) || !strings.Contains(line, `"user_id":"user-99"`) {
			t.Errorf("application/access log lost correlation: %s", line)
		}
	}
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `booking_http_requests_total{method="POST",route="/holds/{sessionID}/confirm",status="201"} 2`) || strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), "request-123") {
		t.Fatal("metrics must aggregate IDs into one template and exclude identifiers")
	}
}

func TestHTTPObservabilityPanicAndEarlyRejections(t *testing.T) {
	_, exporter := testTraceProvider(t)
	m := telemetry.NewMetrics(nil, time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) { panic("private-panic-value") })
	mux.HandleFunc("GET /unauthorized", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	mux.HandleFunc("GET /throttled", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) })
	handler := Recovery(func(w http.ResponseWriter, r *http.Request, value any) { w.WriteHeader(500) })(RequestID(Logging(Observability(m)(mux))))
	for _, path := range []string{"/panic", "/unauthorized", "/throttled", "/unknown-private-id"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	spans := exporter.GetSpans()
	if len(spans) != 4 || spans[0].Status.Code != codes.Error || spans[1].Status.Code == codes.Error || spans[2].Status.Code == codes.Error || spans[3].Name != "GET unmatched" {
		t.Fatalf("panic/client-error/unmatched span policy: %+v", spans)
	}
	if strings.Contains(fmt.Sprint(spans[0].Events, spans[0].Attributes, spans[0].Status), "private-panic-value") {
		t.Error("panic contents leaked into traces")
	}
	// An outer rejection never reaches ServeMux: its label is a constant,
	// not a made-up route derived from user input.
	Observability(m)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/private-id", nil))
	spans = exporter.GetSpans()
	if spans[4].Name != "GET unmatched" {
		t.Errorf("pre-routing rejection name = %q", spans[4].Name)
	}
	before := len(spans)
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	if len(exporter.GetSpans()) != before {
		t.Error("probe/scrape traffic must not generate spans")
	}
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `route="/panic",status="500"} 1`) {
		t.Error("panic must count as 500 when no headers were sent")
	}
}

func TestStatusRecorderInformationalAndFlush(t *testing.T) {
	// httptest's recorder does not model interim headers, so use a
	// dedicated writer for the 103→201 sequence below.
	writer := &headerSequenceWriter{header: make(http.Header)}
	r := &statusRecorder{ResponseWriter: writer}
	r.WriteHeader(103)
	r.WriteHeader(201)
	r.WriteHeader(500)
	if r.status() != 201 || fmt.Sprint(writer.codes) != "[103 201]" {
		t.Fatalf("informational/final status sequence: %v, %d", writer.codes, r.status())
	}
	w := httptest.NewRecorder()
	r = &statusRecorder{ResponseWriter: w}
	if err := http.NewResponseController(r).Flush(); err != nil {
		t.Fatal(err)
	}
	r.WriteHeader(500)
	if r.status() != 200 || w.Code != 200 || !w.Flushed {
		t.Fatal("flush commits implicit 200; later headers must not change measured status")
	}
}

func TestPanicAfterHeadersKeepsActualStatusAndCorrelation(t *testing.T) {
	_, exporter := testTraceProvider(t)
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	m := telemetry.NewMetrics(nil, time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /partial", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		panic("after commit")
	})
	handler := Recovery(nil)(RequestID(Logging(Observability(m)(mux))))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/partial", nil))
	spans := exporter.GetSpans()
	if w.Code != 201 || len(spans) != 1 || spans[0].Status.Code != codes.Error {
		t.Fatalf("partial response status=%d spans=%+v", w.Code, spans)
	}
	if !strings.Contains(output.String(), `"status":201`) || strings.Contains(output.String(), `"status":500`) {
		t.Fatal("access log must report already-committed status, not a 500 it could not send")
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if !strings.Contains(line, w.Header().Get(Header)) {
			t.Errorf("panic/access log missing shared request ID: %s", line)
		}
	}
}

type headerSequenceWriter struct {
	header http.Header
	codes  []int
}

func (w *headerSequenceWriter) Header() http.Header         { return w.header }
func (w *headerSequenceWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *headerSequenceWriter) WriteHeader(status int)      { w.codes = append(w.codes, status) }
