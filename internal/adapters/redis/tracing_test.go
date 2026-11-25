package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestRedisSpansParentageAndRedaction(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	ctx, parent := provider.Tracer("test").Start(context.Background(), "HTTP request")
	hook := tracingHook{}
	const secret = "private-hold-token"
	cmd := goredis.NewStatusCmd(ctx, "SET", "seat:private-user", secret)
	err := hook.ProcessHook(func(ctx context.Context, cmd goredis.Cmder) error {
		if trace.SpanContextFromContext(ctx).SpanID() == parent.SpanContext().SpanID() {
			t.Error("Redis command must run in its own child span")
		}
		return errors.New(secret)
	})(ctx, cmd)
	if err == nil {
		t.Fatal("hook must preserve Redis failure")
	}
	_ = hook.ProcessHook(func(context.Context, goredis.Cmder) error { return goredis.Nil })(ctx, goredis.NewStringCmd(ctx, "GET", "missing"))
	_ = hook.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { return nil })(ctx, []goredis.Cmder{cmd})
	parent.End()
	spans := exporter.GetSpans()
	if len(spans) != 4 || spans[0].Name != "redis SET" || spans[0].Status.Code != codes.Error || spans[1].Status.Code == codes.Error {
		t.Fatalf("unexpected Redis spans: %+v", spans)
	}
	for _, span := range spans[:3] {
		if span.Parent.SpanID() != parent.SpanContext().SpanID() || span.SpanKind != trace.SpanKindClient {
			t.Error("Redis spans must be HTTP children with client kind")
		}
		encoded := fmt.Sprint(span.Name, span.Attributes, span.Events, span.Status)
		if strings.Contains(encoded, secret) || strings.Contains(encoded, "private-user") {
			t.Error("Redis argument/error contents leaked to a span")
		}
	}
	_ = hook.ProcessHook(func(context.Context, goredis.Cmder) error { return nil })(context.Background(), goredis.NewStatusCmd(context.Background(), "PING"))
	if len(exporter.GetSpans()) != 4 {
		t.Error("probe/scrape Redis reads without a recording parent must not create root traces")
	}
}
