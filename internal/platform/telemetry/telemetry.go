// Package telemetry bootstraps OpenTelemetry traces and Prometheus metrics.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace/noop"
)

// Version is stamped at build time via
// -ldflags "-X .../platform/telemetry.Version=$(git describe --always)"
// and shows up as service.version on every span.
var Version = "dev"

// Options configures the telemetry pipeline.
type Options struct {
	Enabled     bool
	ServiceName string
	Environment string
	// Empty Exporter preserves stdout for callers that inject a Writer.
	// Binaries pass the validated otlp/stdout selection explicitly.
	Exporter      string
	Endpoint      string
	ExportTimeout time.Duration
	// Nil means sample all roots; explicit zero disables root sampling.
	SampleRatio *float64
	// Writer receives stdout spans only; tests inject an in-memory writer.
	Writer io.Writer
}

// Setup installs the global tracer provider and propagators, and returns the
// shutdown function the caller must defer. When disabled, it installs a
// no-op provider. Export is asynchronous and bounded; collector availability
// is not an API readiness requirement. Callers must shut down on ALL exits,
// including startup errors, with a fresh uncancelled context (ADR 0012).
func Setup(ctx context.Context, opts Options) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if !opts.Enabled {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	var exporter sdktrace.SpanExporter
	var err error
	timeout := opts.ExportTimeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	switch opts.Exporter {
	case "", "stdout":
		w := opts.Writer
		if w == nil {
			w = os.Stdout
		}
		exporter, err = stdouttrace.New(stdouttrace.WithWriter(w))
	case "otlp":
		// The base URL is validated at boot; append the signal path
		// explicitly so /v1/traces cannot be lost by WithEndpointURL.
		exporter, err = otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(strings.TrimRight(opts.Endpoint, "/")+"/v1/traces"),
			otlptracehttp.WithTimeout(timeout),
			otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
		)
	default:
		return nil, fmt.Errorf("telemetry: unknown exporter %q", opts.Exporter)
	}
	if err != nil {
		return nil, fmt.Errorf("telemetry: create exporter: %w", err)
	}

	// Keep the imported semconv version aligned with the one
	// resource.Default() uses (the SDK's newest bundled schema): Merge
	// refuses conflicting schema URLs, and that conflict is a fatal boot
	// error by design — fail fast beats emitting wrong-schema telemetry.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(Version),
		// deployment.environment.name is not in otel-go's stable semconv
		// package, so we emit it as a plain attribute rather than dragging
		// in the experimental module for one key.
		attribute.String("deployment.environment.name", opts.Environment),
	))
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	ratio := 1.0
	if opts.SampleRatio != nil {
		ratio = *opts.SampleRatio
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithExportTimeout(timeout),
		),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Error("telemetry export failed", slog.String("error", err.Error()))
	}))

	return tp.Shutdown, nil
}
