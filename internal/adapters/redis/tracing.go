package redis

import (
	"context"
	"errors"
	"strings"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracingHook records operation names ONLY. Redis command arguments contain
// hold tokens/user IDs, and EVAL contains entire Lua scripts; never record
// cmd.String(), keys, arguments or raw Redis errors (ADR 0012).
type tracingHook struct{}

func (tracingHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (tracingHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if !trace.SpanFromContext(ctx).IsRecording() {
			return next(ctx, cmd)
		}
		ctx, span := redisSpan(ctx, strings.ToUpper(cmd.Name()))
		defer span.End()
		err := next(ctx, cmd)
		redisResult(span, err)
		return err
	}
}

func (tracingHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if !trace.SpanFromContext(ctx).IsRecording() {
			return next(ctx, cmds)
		}
		ctx, span := redisSpan(ctx, "PIPELINE")
		defer span.End()
		span.SetAttributes(attribute.Int("db.operation.batch.size", len(cmds)))
		err := next(ctx, cmds)
		redisResult(span, err)
		return err
	}
}

func redisSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	return otel.Tracer("internal/adapters/redis").Start(ctx, "redis "+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system.name", "redis"), attribute.String("db.operation.name", operation)))
}

func redisResult(span trace.Span, err error) {
	// Missing keys are expected for expired holds, not infrastructure errors.
	if err != nil && !errors.Is(err, goredis.Nil) {
		span.SetStatus(codes.Error, "Redis command failed")
	}
}
