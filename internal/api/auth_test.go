package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// findCookie returns the cookie with the given name from the recorder's
// response, or nil if absent.
func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestBearerAuth_CookieFallback verifies that a request carrying the
// session cookie (and no Authorization header) is admitted, matching the
// browser flow where the token lives only in the HttpOnly cookie.
func TestBearerAuth_CookieFallback(t *testing.T) {
	const token = "expected-token"
	mw := BearerAuth(token, nil, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("cookie auth: got %d, want 200", rec.Code)
	}
}

// TestBearerAuth_HeaderWins verifies the header is consulted before the
// cookie: a bad header is rejected even when a valid cookie is present,
// so a caller can't smuggle a stale header past a good session.
func TestBearerAuth_HeaderWins(t *testing.T) {
	const token = "expected-token"
	mw := BearerAuth(token, nil, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad header + good cookie: got %d, want 401", rec.Code)
	}
}

// TestBearerAuth_BadCookie verifies a wrong cookie value is rejected.
func TestBearerAuth_BadCookie(t *testing.T) {
	mw := BearerAuth("expected-token", nil, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/foo", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "nope"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad cookie: got %d, want 401", rec.Code)
	}
}

// TestLogin_SetsCookie verifies a correct token yields 204 and an
// HttpOnly session cookie carrying the token.
func TestLogin_SetsCookie(t *testing.T) {
	h := &Handler{AuthToken: "secret", SecureCookies: true}
	body, _ := json.Marshal(loginRequest{Token: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	h.login(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("login: got %d, want 204", rec.Code)
	}
	c := findCookie(rec, SessionCookieName)
	if c == nil {
		t.Fatal("login did not set session cookie")
	}
	if c.Value != "secret" {
		t.Fatalf("cookie value: got %q, want %q", c.Value, "secret")
	}
	if !c.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("cookie must be Secure when SecureCookies is set")
	}
	if c.MaxAge <= 0 {
		t.Errorf("cookie MaxAge: got %d, want > 0", c.MaxAge)
	}
}

// TestLogin_InsecureCookieWhenNotConfigured pins the other half of the
// SecureCookies contract: without it the flag is absent, so a deployment that
// forgets to set it is visibly wrong rather than silently downgraded.
func TestLogin_InsecureCookieWhenNotConfigured(t *testing.T) {
	h := &Handler{AuthToken: "secret"}
	body, _ := json.Marshal(loginRequest{Token: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	h.login(rec, req)

	c := findCookie(rec, SessionCookieName)
	if c == nil {
		t.Fatal("login did not set session cookie")
	}
	if c.Secure {
		t.Error("cookie must not be Secure when SecureCookies is unset")
	}
}

// TestLogin_IgnoresForwardedProto verifies the Secure flag comes from
// configuration only. X-Forwarded-Proto is caller-controlled on any hop that
// is not a trusted proxy, so honouring it would let a client decide whether
// the token cookie is protected.
func TestLogin_IgnoresForwardedProto(t *testing.T) {
	h := &Handler{AuthToken: "secret"}
	body, _ := json.Marshal(loginRequest{Token: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	h.login(rec, req)

	c := findCookie(rec, SessionCookieName)
	if c == nil {
		t.Fatal("login did not set session cookie")
	}
	if c.Secure {
		t.Error("X-Forwarded-Proto must not turn on the Secure flag")
	}
}

// TestLogout_MatchesLoginCookieAttributes verifies the deletion cookie carries
// the same Secure/Path attributes as the one login set. A browser ignores a
// clearing cookie whose attributes do not match, which would leave the session
// alive after logout.
func TestLogout_MatchesLoginCookieAttributes(t *testing.T) {
	h := &Handler{AuthToken: "secret", SecureCookies: true}
	rec := httptest.NewRecorder()

	h.logout(rec, httptest.NewRequest(http.MethodPost, "/logout", nil))

	c := findCookie(rec, SessionCookieName)
	if c == nil {
		t.Fatal("logout did not set a clearing cookie")
	}
	if !c.Secure {
		t.Error("clearing cookie must be Secure when SecureCookies is set")
	}
	if c.Path != "/" {
		t.Errorf("clearing cookie Path: got %q, want %q", c.Path, "/")
	}
	if c.MaxAge >= 0 {
		t.Errorf("clearing cookie MaxAge: got %d, want < 0", c.MaxAge)
	}
}

// TestLogin_WrongToken verifies a bad token is rejected with 401 and no
// cookie, and that the audit hook fires.
func TestLogin_WrongToken(t *testing.T) {
	var failed bool
	h := &Handler{
		AuthToken:     "secret",
		OnAuthFailure: func(_, _, _, _ string) { failed = true },
	}
	body, _ := json.Marshal(loginRequest{Token: "wrong"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	h.login(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login wrong token: got %d, want 401", rec.Code)
	}
	if findCookie(rec, SessionCookieName) != nil {
		t.Fatal("login set a cookie despite wrong token")
	}
	if !failed {
		t.Fatal("OnAuthFailure was not called")
	}
}

// TestLogin_InsecureMode verifies that with no AuthToken configured,
// login is a no-op success so the SPA flow still works in dev.
func TestLogin_InsecureMode(t *testing.T) {
	h := &Handler{AuthToken: ""}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("{}"))
	rec := httptest.NewRecorder()

	h.login(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("insecure login: got %d, want 204", rec.Code)
	}
}

// TestRouter_LoginIsUnauthenticated verifies that through the real
// Router() chain, /login is reachable without a token while a protected
// route is rejected — i.e. the auth group split is wired correctly and
// chi doesn't apply BearerAuth to the public group.
func TestRouter_LoginIsUnauthenticated(t *testing.T) {
	h := &Handler{AuthToken: "secret"}
	router := mustRouter(t, h)

	// /login with the right token -> 204 + cookie, no Authorization header.
	body, _ := json.Marshal(loginRequest{Token: "secret"})
	loginReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusNoContent {
		t.Fatalf("POST /login (no auth header): got %d, want 204", loginRec.Code)
	}
	if findCookie(loginRec, SessionCookieName) == nil {
		t.Fatal("POST /login did not set session cookie through Router")
	}

	// A protected route with no credentials -> 401.
	protReq := httptest.NewRequest(http.MethodGet, "/v1/environments", nil)
	protRec := httptest.NewRecorder()
	router.ServeHTTP(protRec, protReq)
	if protRec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/environments (no creds): got %d, want 401", protRec.Code)
	}
}

// TestLogout_ClearsCookie verifies logout emits a deletion cookie
// (MaxAge < 0) for the session.
func TestLogout_ClearsCookie(t *testing.T) {
	h := &Handler{AuthToken: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	rec := httptest.NewRecorder()

	h.logout(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: got %d, want 204", rec.Code)
	}
	c := findCookie(rec, SessionCookieName)
	if c == nil {
		t.Fatal("logout did not emit a cookie")
	}
	if c.MaxAge >= 0 {
		t.Errorf("logout cookie MaxAge: got %d, want < 0 (deletion)", c.MaxAge)
	}
}
