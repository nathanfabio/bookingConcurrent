// Package telemetry bootstraps OpenTelemetry tracing (CLAUDE.md §8).
//
// M0 scope: a console (stdout) span exporter so traces are visible from day
// one and the middleware chain has a real tracer provider to talk to. M8
// swaps in the OTLP exporter aimed at Jaeger; the rest of the codebase will
// not need to change because everything acquires tracers from the global
// provider set up here.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
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
	// Writer receives spans when Enabled. Defaults to os.Stdout. Tests
	// inject a buffer; production leaves it nil.
	Writer io.Writer
}

// Setup installs the global tracer provider and propagators, and returns the
// shutdown function the caller must defer. When disabled, it installs a
// no-op shutdown and leaves the SDK's default (no-op) provider in place, so
// instrumentation code stays call-safe either way.
func Setup(ctx context.Context, opts Options) (func(context.Context) error, error) {
	if !opts.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}
	// PrettyPrint keeps spans readable in dev; M8 replaces the exporter
	// entirely rather than toggling this.
	exporter, err := stdouttrace.New(stdouttrace.WithWriter(w), stdouttrace.WithPrettyPrint())
	if err != nil {
		return nil, fmt.Errorf("telemetry: create stdout exporter: %w", err)
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
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	// TraceContext propagation makes inbound `traceparent` headers join the
	// caller's trace — the requestID middleware derives its ID from the same
	// header, so logs and traces stay correlated.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}
