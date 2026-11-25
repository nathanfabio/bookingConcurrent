package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Observability sits inside Logging and outside CORS/RateLimit/Auth so their
// rejections are measured too. Only registered ServeMux patterns become
// span names/metric routes; requests rejected before routing use "unmatched".
func Observability(metrics *telemetry.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbe(r) {
				next.ServeHTTP(w, r)
				return
			}
			method := telemetry.HTTPMethod(r.Method)
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := otel.Tracer("internal/platform/middleware").Start(ctx, method,
				trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
					attribute.String("http.request.method", method),
					attribute.String("request_id", RequestIDFromContext(ctx)),
				))
			// Outer access logging cannot see r.WithContext from this inner
			// layer. Carry the trace context back through its recorder.
			if outer, ok := w.(interface{ setTraceContext(context.Context) }); ok {
				outer.setTraceContext(ctx)
			}
			r = r.WithContext(ctx)
			rec := &statusRecorder{ResponseWriter: w}
			started := time.Now()
			defer func() {
				p := recover()
				status := rec.status()
				if p != nil && !rec.wroteHeader {
					status = http.StatusInternalServerError
				}
				route := "unmatched"
				if r.Pattern != "" {
					route = r.Pattern
					// ServeMux patterns include the method. Keep http.route as
					// the path template; method is already its own attribute.
					if _, path, ok := strings.Cut(route, " "); ok {
						route = path
					}
					span.SetAttributes(attribute.String("http.route", route))
				}
				span.SetName(method + " " + route)
				span.SetAttributes(attribute.Int("http.response.status_code", status))
				if status >= 500 || p != nil {
					// Never attach panic values, request bodies, query strings,
					// authorization/cookies or payment signatures to spans.
					span.SetStatus(codes.Error, "HTTP handler failed")
				}
				metrics.RecordHTTP(method, route, status, time.Since(started))
				span.End()
				if p != nil {
					panic(p) // Recovery remains the outermost error writer.
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

func isProbe(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/healthz", "/readyz", "/metrics":
		return true
	default:
		return false
	}
}
