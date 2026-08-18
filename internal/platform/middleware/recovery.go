package middleware

import (
	"log/slog"
	"net/http"
	"runtime"
)

// Recovery converts panics in any downstream handler or middleware into a
// logged stack trace plus an error response, instead of a dropped
// connection. It must be the OUTERMOST middleware (CLAUDE.md §5) — only
// then does it catch panics from every other layer.
//
// writeError is injected by the composition root (which wires in the
// central structured-error writer from adapters/http). Keeping this
// package adapter-free is a hexagonal-architecture rule: platform depends
// on nothing but the standard library, adapters are wired in at the edge.
//
// Caveat worth knowing: if the handler panicked after already writing
// response bytes, the error response cannot be sent cleanly — the client
// will see a truncated body. Recovery still logs and returns; there is
// nothing correct to do at that point.
func Recovery(writeError func(w http.ResponseWriter, r *http.Request, recovered any)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				stack := make([]byte, 8192)
				n := runtime.Stack(stack, false)

				attrs := []any{
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(stack[:n])),
				}
				if id := RequestIDFromContext(r.Context()); id != "" {
					attrs = append(attrs, slog.String("request_id", id))
				}
				slog.ErrorContext(r.Context(), "panic recovered", attrs...)

				if writeError != nil {
					writeError(w, r, rec)
				} else {
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}
