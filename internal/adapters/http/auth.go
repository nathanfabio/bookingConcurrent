package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	appauth "github.com/nathanfabio/bookingConcurrent/internal/application/auth"
	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/middleware"
)

// maxBodyBytes caps request bodies before they reach argon2id: hashing is
// deliberately expensive, so unbounded input would be a DoS vector.
const maxBodyBytes = 1 << 20 // 1 MiB

// Auth DTOs. snake_case JSON is a hard rule (CLAUDE.md §4) — the tag test
// in auth_test.go fails the build on drift.
type (
	registerRequest struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}

	loginRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	// tokenResponse answers register/login/refresh. The refresh token is
	// NOT here on purpose: it travels exclusively in the httpOnly cookie
	// (ADR 0007), so no JavaScript can ever read it out of a response body
	// either.
	tokenResponse struct {
		AccessToken string       `json:"access_token"`
		TokenType   string       `json:"token_type"` // always "Bearer"
		ExpiresIn   int64        `json:"expires_in"` // seconds
		User        userResponse `json:"user"`
	}

	userResponse struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
	}
)

func sessionResponse(s appauth.Session) tokenResponse {
	return tokenResponse{
		AccessToken: s.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(s.ExpiresIn.Seconds()),
		User: userResponse{
			ID:          s.User.ID,
			Email:       s.User.Email,
			DisplayName: s.User.DisplayName,
			Role:        string(s.User.Role),
		},
	}
}

// RegisterHandler implements POST /auth/register (CLAUDE.md §3): create
// the user, hash the password, open a session. 201 + access token in the
// body, refresh token in the httpOnly cookie.
func RegisterHandler(svc *appauth.Service, refreshTTL time.Duration, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return // decodeJSON wrote the 400
		}
		sess, err := svc.Register(r.Context(), req.Email, req.Password, req.DisplayName)
		if err != nil {
			writeAuthError(w, r, err)
			return
		}
		SetRefreshCookie(w, sess.RefreshToken, refreshTTL, secureCookies)
		WriteJSON(w, http.StatusCreated, sessionResponse(sess))
	}
}

// LoginHandler implements POST /auth/login. Unknown email and wrong
// password are byte-identical 401s — writeAuthError maps both to the same
// sentinel-driven response (CLAUDE.md §3).
func LoginHandler(svc *appauth.Service, refreshTTL time.Duration, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		sess, err := svc.Login(r.Context(), req.Email, req.Password)
		if err != nil {
			writeAuthError(w, r, err)
			return
		}
		SetRefreshCookie(w, sess.RefreshToken, refreshTTL, secureCookies)
		WriteJSON(w, http.StatusOK, sessionResponse(sess))
	}
}

// RefreshHandler implements POST /auth/refresh: consume the cookie's
// refresh token, rotate it (ADR 0004), return a fresh access token +
// cookie. EVERY failure path clears the cookie so clients converge on
// "no session" instead of retrying a dead credential.
func RefreshHandler(svc *appauth.Service, refreshTTL time.Duration, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := RefreshCookieValue(r)
		if !ok {
			ClearRefreshCookie(w, secureCookies)
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or expired session")
			return
		}
		sess, err := svc.Refresh(r.Context(), token)
		if err != nil {
			// Unknown, expired, or stolen — the client sees the same
			// message either way (theft is logged server-side by the
			// store/use case, never echoed to the caller).
			ClearRefreshCookie(w, secureCookies)
			writeAuthError(w, r, err)
			return
		}
		SetRefreshCookie(w, sess.RefreshToken, refreshTTL, secureCookies)
		WriteJSON(w, http.StatusOK, sessionResponse(sess))
	}
}

// LogoutHandler implements POST /auth/logout: revoke the whole token
// family and clear the cookie. Always 204, always idempotent — even an
// unknown or dead cookie logs out cleanly (CLAUDE.md §3: the client's goal
// state is "no session").
func LogoutHandler(svc *appauth.Service, secureCookies bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := RefreshCookieValue(r) // may legitimately be absent
		if err := svc.Logout(r.Context(), token); err != nil {
			writeAuthError(w, r, err)
			return
		}
		ClearRefreshCookie(w, secureCookies)
		w.WriteHeader(http.StatusNoContent)
	}
}

// MeHandler implements GET /auth/me: return the authenticated user. The
// user ID comes from the context the Auth middleware populated — NEVER
// from a request parameter (CLAUDE.md §3). Must be wired behind the auth
// middleware (cmd/api does this per route).
func MeHandler(svc *appauth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.UserIDFromContext(r.Context())
		if !ok {
			// Defense in depth: this route is wrapped in middleware.Auth,
			// so reaching here means a wiring bug, not a client error.
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
			return
		}
		u, err := svc.Me(r.Context(), userID)
		if err != nil {
			writeAuthError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, userResponse{
			ID:          u.ID,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			Role:        string(u.Role),
		})
	}
}

// writeAuthError is the central sentinel→status mapping for auth
// (CLAUDE.md §5). Booking and catalog sentinels have their own switch in
// writeBookingError (booking.go). Infra errors log server-side and surface
// a generic 500 — never details.
func writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, user.ErrValidation):
		WriteError(w, http.StatusBadRequest, CodeValidation, validationMessage(err))
	case errors.Is(err, user.ErrEmailTaken):
		WriteError(w, http.StatusConflict, CodeConflict, "email is already registered")
	case errors.Is(err, user.ErrInvalidCredentials):
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid email or password")
	case errors.Is(err, user.ErrRefreshTokenUnknown),
		errors.Is(err, user.ErrRefreshTokenExpired),
		errors.Is(err, user.ErrRefreshTokenReused):
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or expired session")
	case errors.Is(err, user.ErrUserNotFound):
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "authentication required")
	default:
		attrs := []any{slog.String("error", err.Error())}
		if id := middleware.RequestIDFromContext(r.Context()); id != "" {
			attrs = append(attrs, slog.String("request_id", id))
		}
		slog.ErrorContext(r.Context(), "auth request failed", attrs...)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}

// validationMessage strips the domain package scaffolding from a
// validation error so clients get the human-readable half only:
// "user: email is required: validation failed" -> "email is required".
func validationMessage(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "user: ")
	msg = strings.TrimSuffix(msg, ": "+user.ErrValidation.Error())
	return msg
}

// decodeJSON reads a bounded request body into dst, rejecting unknown
// fields and trailing garbage. All failures write a 400 validation_error
// and return a non-nil error so the caller just returns.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "request body must be valid JSON")
		return errors.New("empty body")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "request body must be valid JSON")
		return err
	}
	// A second decode must hit EOF: extra JSON values after the first
	// object are a smuggling trick worth rejecting outright.
	if dec.More() {
		WriteError(w, http.StatusBadRequest, CodeValidation, "request body must be valid JSON")
		return fmt.Errorf("trailing data after JSON body")
	}
	return nil
}
