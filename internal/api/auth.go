package api

import (
	"crypto/subtle"
	"net/http"
	"time"
)

// SessionCookieName is the name of the HttpOnly cookie that carries the
// bearer token for browser callers. A separate SPA POSTs the operator's
// token to /login, which sets this cookie; subsequent API requests are
// authenticated from it by [BearerAuth] via [tokenFromRequest]. The
// token never lives in JavaScript-readable storage.
const SessionCookieName = "fuse_session"

// sessionTTL is how long a login cookie remains valid. There is no
// server-side session store — this Max-Age is the entire "remember me".
const sessionTTL = 7 * 24 * time.Hour

// loginRequest is the body of POST /login: the single shared secret the
// operator pastes into the login form. There are no users, so there is
// no username field.
type loginRequest struct {
	Token string `json:"token"`
}

// login validates the posted token against the configured AuthToken and,
// on success, sets the HttpOnly session cookie. It is mounted outside the
// BearerAuth middleware so an unauthenticated browser can reach it.
//
//	@Summary		Log in
//	@Description	Exchanges the shared access token for an HttpOnly session cookie.
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		loginRequest	true	"Access token"
//	@Success		204
//	@Failure		401	{object}	Error
//	@Router			/login [post]
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	// When auth is disabled (insecure/dev mode) there is no secret to
	// check; treat login as a no-op success so the same SPA flow works.
	if h.AuthToken == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Tightly bounded: this is the only body-carrying route reachable
	// without credentials, so it is the one an anonymous caller could use to
	// make the server allocate.
	var req loginRequest
	if !decodeJSON(w, r, MaxLoginBodyBytes, &req) {
		return
	}

	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.AuthToken)) != 1 {
		if h.OnAuthFailure != nil {
			h.OnAuthFailure(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
		}
		writeError(w, http.StatusUnauthorized, CodeUnauthorized,
			"invalid token", nil)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    h.AuthToken,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// logout clears the session cookie. It is safe to call unauthenticated —
// clearing a cookie the caller may not hold is harmless — so it is
// mounted alongside login, outside BearerAuth.
//
//	@Summary		Log out
//	@Description	Clears the session cookie.
//	@Tags			auth
//	@Success		204
//	@Router			/logout [post]
func (h *Handler) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1, // delete now
		HttpOnly: true,
		Secure:   h.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
