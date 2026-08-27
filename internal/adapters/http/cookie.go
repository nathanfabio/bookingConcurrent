package http

import (
	"net/http"
	"time"
)

// RefreshCookieName is the cookie that carries the opaque refresh token.
// The access token deliberately does NOT travel in a cookie — it lives in
// the JSON body and the client's memory, so an XSS payload can at most
// steal 15 minutes of access, never the long-lived session (ADR 0007).
const RefreshCookieName = "refresh_token"

// refreshCookiePath scopes the cookie to /auth/*: it is only ever sent to
// the endpoints that need it (login, refresh, logout), shrinking the
// exposure surface on every other request. Path=/auth covers all four
// /auth endpoints via prefix matching.
const refreshCookiePath = "/auth"

// SetRefreshCookie writes the refresh credential into an httpOnly cookie.
//
// Attribute choices (ADR 0007):
//   - HttpOnly always: JavaScript cannot read the long-lived credential.
//   - Secure only in production: dev runs plain-http localhost, where a
//     Secure cookie would simply never be sent back.
//   - SameSite=Lax: the browser omits the cookie on cross-site POST, which
//     CSRF-protects POST /auth/refresh and /auth/logout without a token
//     scheme. The documented limitation (cross-origin SPA) is in the ADR.
//   - No Domain: host-only, never leaks to subdomains.
func SetRefreshCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	//nolint:gosec // G124: Secure is deliberately dynamic — true in production, false on plain-http dev (ADR 0007). HttpOnly+SameSite are always set.
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    token,
		Path:     refreshCookiePath,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearRefreshCookie expires the cookie. Same attributes as SetRefreshCookie
// except MaxAge=-1 — the browser matches cookies for deletion by
// name+path+domain, so those must be identical. Called on logout and on
// every refresh failure, so clients converge on "no session".
func ClearRefreshCookie(w http.ResponseWriter, secure bool) {
	//nolint:gosec // G124: Secure is deliberately dynamic (see SetRefreshCookie; ADR 0007).
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// RefreshCookieValue reads the presented refresh credential, if any.
func RefreshCookieValue(r *http.Request) (string, bool) {
	c, err := r.Cookie(RefreshCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}
