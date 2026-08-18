package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// skipLoggingPaths are high-frequency, zero-signal probes (liveness/
// readiness) that would drown request logs without teaching anything.
// The CLAUDE.md §5 logging requirement targets real traffic.
var skipLoggingPaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
}

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
		if skipLoggingPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()

		defer func() {
			if p := recover(); p != nil {
				logRequest(r, http.StatusInternalServerError, time.Since(start))
				panic(p) // not ours to handle — Recovery is outside us
			}
			logRequest(r, rec.status(), time.Since(start))
		}()

		next.ServeHTTP(rec, r)
	})
}

func logRequest(r *http.Request, status int, dur time.Duration) {
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Float64("duration_ms", float64(dur.Microseconds())/1000.0),
	}
	if id := RequestIDFromContext(r.Context()); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	if uid, ok := UserIDFromContext(r.Context()); ok {
		attrs = append(attrs, slog.String("user_id", uid))
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
type statusRecorder struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.code = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
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
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
