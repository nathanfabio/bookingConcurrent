package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	appauth "github.com/nathanfabio/bookingConcurrent/internal/application/auth"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/middleware"
)

const testRefreshTTL = 720 * time.Hour

// newTestService builds a real Service over the in-memory fakes with CHEAP
// argon2 parameters, so handler tests stay fast while exercising the full
// register→login→refresh→logout chain for real.
func newTestService(t *testing.T) *appauth.Service {
	t.Helper()
	users := memory.NewUserStore()
	tokens := memory.NewRefreshTokenStore(memory.NewManualClock(time.Now()))
	hasher := appauth.NewArgon2Hasher(appauth.Argon2Params{
		MemoryKiB: 64, Time: 1, Parallelism: 1, KeyLen: 32, SaltLen: 16,
	})
	issuer := appauth.NewTokenIssuer("test-secret-test-secret-test-sec", "booking-api", 15*time.Minute)
	return appauth.NewService(users, tokens, hasher, issuer, 15*time.Minute, testRefreshTTL)
}

// setCookieFrom parses the refresh_token Set-Cookie header out of a
// response, failing the test if it is missing.
func setCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == RefreshCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response; headers: %v", RefreshCookieName, rec.Header().Values("Set-Cookie"))
	return nil
}

func TestRegisterHandler(t *testing.T) {
	svc := newTestService(t)
	h := RegisterHandler(svc, testRefreshTTL, false)

	body := `{"email":"alice@example.com","password":"averylongpassword","display_name":"Ada"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"access_token", "token_type", "expires_in", "user"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing snake_case field %q; got %v", key, raw)
		}
	}
	if raw["token_type"] != "Bearer" {
		t.Errorf("token_type = %v, want Bearer", raw["token_type"])
	}
	if _, leaked := raw["refresh_token"]; leaked {
		t.Errorf("refresh token must NOT be in the JSON body (ADR 0007: httpOnly cookie only)")
	}

	cookie := setCookieFrom(t, rec)
	if !cookie.HttpOnly {
		t.Errorf("refresh cookie must be HttpOnly")
	}
	if cookie.Path != "/auth" {
		t.Errorf("cookie path = %q, want /auth", cookie.Path)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Secure {
		t.Errorf("cookie Secure in development; want unset outside production")
	}
	if cookie.MaxAge != int(testRefreshTTL.Seconds()) {
		t.Errorf("cookie MaxAge = %d, want %d", cookie.MaxAge, int(testRefreshTTL.Seconds()))
	}
}

func TestRegisterHandlerValidation(t *testing.T) {
	svc := newTestService(t)
	h := RegisterHandler(svc, testRefreshTTL, false)

	cases := map[string]string{
		"not json":         `{`,
		"unknown field":    `{"email":"a@b.co","password":"averylongpassword","admin":true}`,
		"missing fields":   `{}`,
		"short password":   `{"email":"a@b.co","password":"x"}`,
		"trailing garbage": `{"email":"a@b.co","password":"averylongpassword"}{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			var env errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error body not JSON: %v", err)
			}
			if env.Code != CodeValidation {
				t.Errorf("code = %q, want %s", env.Code, CodeValidation)
			}
		})
	}
}

func TestRegisterHandlerDuplicateEmail(t *testing.T) {
	svc := newTestService(t)
	h := RegisterHandler(svc, testRefreshTTL, false)

	body := `{"email":"dup@example.com","password":"averylongpassword"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first register = %d, want 201", rec.Code)
	}

	// Case variant collides with the same normalized email.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register",
		strings.NewReader(`{"email":"DUP@example.com","password":"averylongpassword"}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate register = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
}

func TestLoginHandler(t *testing.T) {
	svc := newTestService(t)
	reg := RegisterHandler(svc, testRefreshTTL, false)
	login := LoginHandler(svc, testRefreshTTL, false)

	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register",
		strings.NewReader(`{"email":"bob@example.com","password":"averylongpassword"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d, want 201", rec.Code)
	}

	rec = httptest.NewRecorder()
	login.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/login",
		strings.NewReader(`{"email":"bob@example.com","password":"averylongpassword"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	setCookieFrom(t, rec)
}

// TestLoginHandlerFailuresAreIndistinguishable: wrong password and unknown
// email must produce BYTE-IDENTICAL 401 bodies (CLAUDE.md §3).
func TestLoginHandlerFailuresAreIndistinguishable(t *testing.T) {
	svc := newTestService(t)
	reg := RegisterHandler(svc, testRefreshTTL, false)
	login := LoginHandler(svc, testRefreshTTL, false)

	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register",
		strings.NewReader(`{"email":"carol@example.com","password":"averylongpassword"}`)))

	wrong := httptest.NewRecorder()
	login.ServeHTTP(wrong, httptest.NewRequest(http.MethodPost, "/auth/login",
		strings.NewReader(`{"email":"carol@example.com","password":"wrong-password-here"}`)))

	unknown := httptest.NewRecorder()
	login.ServeHTTP(unknown, httptest.NewRequest(http.MethodPost, "/auth/login",
		strings.NewReader(`{"email":"nobody@example.com","password":"averylongpassword"}`)))

	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("statuses = %d/%d, want 401/401", wrong.Code, unknown.Code)
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Errorf("failure bodies differ — would enumerate accounts:\n  wrong-password: %s\n  unknown-email:  %s",
			wrong.Body.String(), unknown.Body.String())
	}
}

func TestRefreshHandlerRotates(t *testing.T) {
	svc := newTestService(t)
	reg := RegisterHandler(svc, testRefreshTTL, false)
	refresh := RefreshHandler(svc, testRefreshTTL, false)

	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register",
		strings.NewReader(`{"email":"dave@example.com","password":"averylongpassword"}`)))
	first := setCookieFrom(t, rec)

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(first)
	rec = httptest.NewRecorder()
	refresh.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	second := setCookieFrom(t, rec)
	if second.Value == first.Value {
		t.Errorf("refresh did not rotate the cookie value")
	}
}

func TestRefreshHandlerRejectsAndClears(t *testing.T) {
	svc := newTestService(t)
	refresh := RefreshHandler(svc, testRefreshTTL, false)

	t.Run("no cookie", func(t *testing.T) {
		rec := httptest.NewRecorder()
		refresh.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/refresh", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("dead token clears cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		//nolint:gosec // G124: this is a client REQUEST cookie under test, not a Set-Cookie the server issues.
		req.AddCookie(&http.Cookie{Name: RefreshCookieName, Value: "never-was-valid"})
		rec := httptest.NewRecorder()
		refresh.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		cookie := setCookieFrom(t, rec) // the CLEAR cookie
		if cookie.MaxAge != -1 {
			t.Errorf("failure must clear the cookie (MaxAge=-1), got MaxAge=%d", cookie.MaxAge)
		}
	})
}

func TestLogoutHandler(t *testing.T) {
	svc := newTestService(t)
	reg := RegisterHandler(svc, testRefreshTTL, false)
	logout := LogoutHandler(svc, false)
	refresh := RefreshHandler(svc, testRefreshTTL, false)

	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register",
		strings.NewReader(`{"email":"frank@example.com","password":"averylongpassword"}`)))
	cookie := setCookieFrom(t, rec)

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	logout.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("logout = %d, want 204", rec.Code)
	}
	cleared := setCookieFrom(t, rec)
	if cleared.MaxAge != -1 {
		t.Errorf("logout must clear the cookie; MaxAge = %d", cleared.MaxAge)
	}

	// The revoked token can no longer refresh.
	req = httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	refresh.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("refresh after logout = %d, want 401", rec.Code)
	}

	// Logout without any cookie is still a clean 204 (idempotent).
	rec = httptest.NewRecorder()
	logout.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/logout", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("cookieless logout = %d, want 204", rec.Code)
	}
}

func TestMeHandler(t *testing.T) {
	svc := newTestService(t)
	reg := RegisterHandler(svc, testRefreshTTL, false)
	me := MeHandler(svc)

	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register",
		strings.NewReader(`{"email":"grace@example.com","password":"averylongpassword","display_name":"Grace"}`)))
	var regResp tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("register response: %v", err)
	}

	t.Run("authenticated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
		req = req.WithContext(middleware.ContextWithUserID(req.Context(), regResp.User.ID))
		rec := httptest.NewRecorder()
		me.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		var got userResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if got.Email != "grace@example.com" || got.DisplayName != "Grace" {
			t.Errorf("me = %+v, want grace@example.com / Grace", got)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		rec := httptest.NewRecorder()
		me.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/me", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// TestMeHandlerThroughMiddleware exercises the production wiring shape:
// the Auth middleware validates a real JWT and the handler reads the user
// ID from the context — the exact chain cmd/api builds for GET /auth/me.
func TestMeHandlerThroughMiddleware(t *testing.T) {
	svc := newTestService(t)
	reg := RegisterHandler(svc, testRefreshTTL, false)
	issuer := appauth.NewTokenIssuer("test-secret-test-secret-test-sec", "booking-api", 15*time.Minute)

	validate := func(ctx context.Context, token string) (string, error) {
		return issuer.ValidateAccessToken(token)
	}
	chain := middleware.Auth(validate, WriteUnauthorized)(MeHandler(svc))

	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/register",
		strings.NewReader(`{"email":"heidi@example.com","password":"averylongpassword"}`)))
	var regResp tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("register response: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+regResp.AccessToken)
	rec = httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	chain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without token: status = %d, want 401", rec.Code)
	}
}

func TestCookieSecureInProduction(t *testing.T) {
	rec := httptest.NewRecorder()
	SetRefreshCookie(rec, "token-value", time.Hour, true)
	cookie := setCookieFrom(t, rec)
	if !cookie.Secure {
		t.Errorf("production cookie must set the Secure flag")
	}
}

// TestAuthDTOsHaveSnakeCaseTags enforces CLAUDE.md §4 mechanically: every
// field of every auth request/response struct must carry a snake_case json
// tag, so a rename cannot silently break frontend integrations.
func TestAuthDTOsHaveSnakeCaseTags(t *testing.T) {
	dtos := []any{registerRequest{}, loginRequest{}, tokenResponse{}, userResponse{}}
	for _, dto := range dtos {
		typ := reflect.TypeOf(dto)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag, ok := field.Tag.Lookup("json")
			if !ok {
				t.Errorf("%s.%s has no json tag", typ.Name(), field.Name)
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" || strings.ToLower(name) != name || strings.Contains(name, " ") {
				t.Errorf("%s.%s json tag %q is not snake_case", typ.Name(), field.Name, tag)
			}
		}
	}
}
