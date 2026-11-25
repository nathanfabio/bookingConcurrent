package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Logging emits one structured log line per request: method, path, status,
// duration, request ID, and — once the auth middleware has run (M3) — the
// authenticated user ID.
//
// It wraps the ResponseWriter to capture the status code. If a downstream
// handler panics, the deferred log records status 500 and re-panics so the
// (outer) Recovery middleware still handles the panic: logging must never
// swallow it.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r) {
			next.ServeHTTP(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()

		defer func() {
			if rec.traceContext != nil {
				r = r.WithContext(rec.traceContext)
			}
			if p := recover(); p != nil {
				status := http.StatusInternalServerError
				if rec.wroteHeader {
					status = rec.status() // already on the wire, even if recovery fails
				}
				logRequest(r, status, time.Since(start), rec.userID)
				panic(p) // not ours to handle — Recovery is outside us
			}
			logRequest(r, rec.status(), time.Since(start), rec.userID)
		}()

		next.ServeHTTP(rec, r)
	})
}

func logRequest(r *http.Request, status int, dur time.Duration, userID string) {
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(dur.Microseconds())/1000.0),
	}
	if id := RequestIDFromContext(r.Context()); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	// The authenticated user ID normally arrives via the recorder's side
	// channel (set by the auth middleware, which runs INSIDE Logging and so
	// cannot propagate a context value back out). The context is the
	// fallback for setups where auth wraps Logging instead.
	if userID == "" {
		userID, _ = UserIDFromContext(r.Context())
	}
	if userID != "" {
		attrs = append(attrs, slog.String("user_id", userID))
	}

	args := make([]any, len(attrs))
	for i, a := range attrs {
		args[i] = a
	}
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "request finished", args...)
	} else {
		slog.InfoContext(r.Context(), "request finished", args...)
	}
}

// statusRecorder captures the response status without disturbing the
// wrapped writer's behavior (including http.Flusher when supported).
//
// It also carries the authenticated user ID as a side channel. CLAUDE.md §5
// puts Logging OUTSIDE Auth, but Go contexts only flow inward: the auth
// middleware's r.WithContext(...) is invisible to this outer deferred log.
// So Auth type-asserts the writer to the setUserID interface below and
// hands the ID over directly. Same package, so the interface stays private.
type statusRecorder struct {
	http.ResponseWriter
	code         int
	wroteHeader  bool
	userID       string
	traceContext context.Context
}

// setUserID is the side channel the Auth middleware writes through.
func (r *statusRecorder) setUserID(id string) {
	r.userID = id
	if outer, ok := r.ResponseWriter.(interface{ setUserID(string) }); ok {
		outer.setUserID(id)
	}
}

func (r *statusRecorder) setTraceContext(ctx context.Context) { r.traceContext = ctx }

// Unwrap lets http.ResponseController discover capabilities of the original
// writer (deadlines, flushing, hijacking) through nested instrumentation.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if !r.wroteHeader {
		r.code = code
		r.wroteHeader = true
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		// net/http semantics: an implicit 200 is sent with the first Write.
		r.code = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// status returns the recorded status, defaulting to 200 per net/http
// implicit-write semantics (a handler that writes a body without calling
// WriteHeader has sent a 200).
func (r *statusRecorder) status() int {
	if !r.wroteHeader {
		return http.StatusOK
	}
	return r.code
}

// Flush implements http.Flusher when the underlying writer supports it.
func (r *statusRecorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
