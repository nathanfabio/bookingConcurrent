package logger

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type correlationKey uint8

const (
	requestKey correlationKey = iota
	userKey
)

func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestKey, id)
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestKey).(string)
	return id
}

func ContextWithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userKey, id)
}

func UserID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userKey).(string)
	return id, ok
}

// WithCorrelation enriches ALL slog.*Context calls using their context,
// including application and adapter logs. Storing a logger in context alone
// does not enrich slog's global logger. It adds no credentials or baggage.
func WithCorrelation(handler slog.Handler) slog.Handler {
	return correlationHandler{Handler: handler}
}

type correlationHandler struct{ slog.Handler }

func (h correlationHandler) Handle(ctx context.Context, record slog.Record) error {
	attrs := []slog.Attr{}
	if id := RequestID(ctx); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	if id, ok := UserID(ctx); ok && id != "" {
		attrs = append(attrs, slog.String("user_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	record = record.Clone()
	for _, attr := range attrs {
		present := false
		record.Attrs(func(existing slog.Attr) bool {
			present = existing.Key == attr.Key
			return !present
		})
		if !present {
			record.AddAttrs(attr)
		}
	}
	return h.Handler.Handle(ctx, record)
}

func (h correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return correlationHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h correlationHandler) WithGroup(name string) slog.Handler {
	return correlationHandler{Handler: h.Handler.WithGroup(name)}
}
