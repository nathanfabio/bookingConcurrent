package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// bearerPrefix is the Authorization scheme this middleware understands.
// Matching is case-insensitive per RFC 7235; everything after the first
// space is the credential.
const bearerPrefix = "bearer "

// Auth guards a route: a valid Bearer access token puts the authenticated
// user ID into the request context; anything else is handed to the
// injected 401 writer and never reaches the handler.
//
// Like Recovery, both dependencies are INJECTED by the composition root so
// this package stays adapter-free (CLAUDE.md §1): validate is the JWT check
// from application/auth, writeUnauthorized is the central error writer from
// adapters/http. Wiring them here — rather than importing them — keeps the
// dependency direction explicit and the middleware reusable.
//
// The middleware is applied PER ROUTE (CLAUDE.md §5 lists auth last, but
// only protected routes wear it; /auth/register|login|refresh stay public).
func Auth(
	validate func(ctx context.Context, accessToken string) (userID string, err error),
	writeUnauthorized func(w http.ResponseWriter, r *http.Request),
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				reject(w, r, writeUnauthorized, "missing or malformed Authorization header")
				return
			}

			userID, err := validate(r.Context(), token)
			if err != nil {
				reject(w, r, writeUnauthorized, "access token rejected")
				return
			}

			// Side channel for the Logging middleware: it sits OUTSIDE auth
			// (CLAUDE.md §5) so it cannot see the context value we are about
			// to set. Hand the user ID to the recorder directly if this is
			// the Logging middleware's writer — see logging.go.
			if rec, ok := w.(interface{ setUserID(string) }); ok {
				rec.setUserID(userID)
			}

			next.ServeHTTP(w, r.WithContext(ContextWithUserID(r.Context(), userID)))
		})
	}
}

// bearerToken extracts the credential from an Authorization header value.
// The scheme is matched case-insensitively; a missing header, a different
// scheme, or an empty credential all yield ok=false.
func bearerToken(header string) (string, bool) {
	if len(header) < len(bearerPrefix) {
		return "", false
	}
	if !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// reject logs (at debug — the request ID correlates it with the access log)
// and writes the 401. The log never names the reason in a way that leaks
// the credential or distinguishes failure modes to an attacker; attribute
// names avoid the words banned by scripts/lint-guards.sh.
func reject(w http.ResponseWriter, r *http.Request, writeUnauthorized func(http.ResponseWriter, *http.Request), reason string) {
	attrs := []any{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("reason", reason),
	}
	if id := RequestIDFromContext(r.Context()); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	slog.DebugContext(r.Context(), "request rejected by auth middleware", attrs...)

	if writeUnauthorized != nil {
		writeUnauthorized(w, r)
	} else {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}
