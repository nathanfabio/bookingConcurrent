package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func preserveGlobals(t *testing.T) {
	t.Helper()
	provider, propagator, handler := otel.GetTracerProvider(), otel.GetTextMapPropagator(), otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetTracerProvider(provider)
		otel.SetTextMapPropagator(propagator)
		otel.SetErrorHandler(handler)
	})
}

func TestOTLPHTTPExportAndShutdown(t *testing.T) {
	preserveGlobals(t)
	received := make(chan *collectortracepb.ExportTraceServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("invalid OTLP request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request collectortracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		received <- &request
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	shutdown, err := Setup(context.Background(), Options{
		Enabled: true, Exporter: "otlp", Endpoint: server.URL + "/", ServiceName: "test-api", Environment: "test", ExportTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "committed request")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-received:
		if len(request.ResourceSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
			t.Fatalf("unexpected export shape: %v", request)
		}
		exported := request.ResourceSpans[0].ScopeSpans[0].Spans[0]
		if exported.Name != "committed request" {
			t.Fatalf("exported name %q", exported.Name)
		}
		found := false
		for _, attr := range request.ResourceSpans[0].Resource.Attributes {
			if attr.Key == "service.name" && attr.Value.GetStringValue() == "test-api" {
				found = true
			}
		}
		if !found {
			t.Error("service identity missing")
		}
	default:
		t.Fatal("shutdown must flush a pending span")
	}
}

func TestSamplingAndDisabledTracing(t *testing.T) {
	preserveGlobals(t)
	for _, ratio := range []float64{0, 1} {
		var output bytes.Buffer
		shutdown, err := Setup(context.Background(), Options{Enabled: true, Writer: &output, SampleRatio: &ratio})
		if err != nil {
			t.Fatal(err)
		}
		_, span := otel.Tracer("test").Start(context.Background(), "root")
		if span.IsRecording() != (ratio == 1) {
			t.Errorf("root recording at ratio %v", ratio)
		}
		span.End()
		// Parent-based sampling honors upstream sampled decisions even when
		// local root sampling is zero.
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
		})
		_, child := otel.Tracer("test").Start(trace.ContextWithRemoteSpanContext(context.Background(), sc), "child")
		if !child.IsRecording() {
			t.Error("sampled upstream parent must be honored")
		}
		child.End()
		if err := shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "child") {
			t.Error("stdout exporter must flush")
		}
	}
	shutdown, err := Setup(context.Background(), Options{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("disabled").Start(context.Background(), "disabled")
	if span.IsRecording() {
		t.Error("disabled setup must replace the old global provider")
	}
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorOutageDoesNotPreventSetupOrMetrics(t *testing.T) {
	preserveGlobals(t)
	attempted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempted <- struct{}{}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	shutdown, err := Setup(context.Background(), Options{
		Enabled: true, Exporter: "otlp", Endpoint: server.URL, ExportTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("collector availability must not prevent boot: %v", err)
	}
	// Suppress the expected SDK export diagnostic in this isolated test.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	_, span := otel.Tracer("test").Start(context.Background(), "business success")
	span.End()
	m := NewMetrics(nil, time.Second)
	m.BookingConfirmed()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx) // export failure is telemetry failure, not sale failure
	select {
	case <-attempted:
	default:
		t.Fatal("test did not exercise an unavailable exporter")
	}
	if body := scrape(t, m); !strings.Contains(body, "booking_confirmations_total 1") {
		t.Fatal("export outage must not interfere with operational counters")
	}
}
