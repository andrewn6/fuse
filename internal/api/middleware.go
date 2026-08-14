package api

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// APIKeyAuthenticator validates a presented bearer token against the set
// of issued API keys. It is implemented by the orchestrator's API key
// store. A nil APIKeyAuthenticator disables key auth (only the master
// token is accepted).
type APIKeyAuthenticator interface {
	// Authenticate returns (keyID, true) for a live, non-revoked key, or
	// ("", false) if the token matches no key or the key is revoked.
	Authenticate(ctx context.Context, rawToken string) (string, bool)
}

// Principal identifies how a request authenticated. It is stored in the
// request context by BearerAuth so handlers (and audit) can distinguish
// the master operator from an individual API key — e.g. to restrict key
// management to the master token.
type Principal struct {
	// Master is true when the request authenticated with the static
	// master token (ORCH_AUTH_TOKEN). When auth is disabled (empty
	// master token, insecure mode), requests are also treated as Master.
	Master bool
	// KeyID is the public id of the authenticating API key, empty for a
	// master-token request.
	KeyID string
}

type principalKey struct{}

// withPrincipal returns a derived context carrying p.
func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the authenticated principal for the request.
// When auth is disabled (insecure mode) no principal is set; callers should
// treat a missing principal as the master operator, mirroring BearerAuth's
// open pass-through. The bool reports whether a principal was present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// callerTenantID derives the tenant scope of the authenticated caller.
//
// It is deliberately not readable from the request: a tenant a caller can
// name is a tenant a caller can probe, and the build cache is exactly the
// kind of thing worth probing (an artifact carries whatever the build baked
// into it, and its mere existence tells you someone built that recipe). So
// the scope comes from how the request authenticated and nowhere else.
//
// An API key is its own scope. The master token, and insecure/dev mode where
// no principal is set at all, share the default scope: there is one operator
// behind the master token, so there is nothing to separate them from.
func callerTenantID(ctx context.Context) string {
	p, ok := PrincipalFromContext(ctx)
	if !ok || p.Master {
		return ""
	}
	return p.KeyID
}

// AuthFailureFunc is invoked once for every rejected authentication
// attempt. It receives the per-request correlation ID (see
// [RequestIDMiddleware]) so audit events and log lines can be tied
// back to the originating request, even when the rejection happens
// before the handler runs.
//
// The requestID is always non-empty when [RequestIDMiddleware] is
// mounted upstream of this middleware (the standard router order).
type AuthFailureFunc func(remoteAddr, method, path, requestID string)

// IPRejectFunc is invoked once for every request rejected by the
// CIDR allowlist. Same shape and contract as [AuthFailureFunc].
type IPRejectFunc func(remoteAddr, method, path, requestID string)

// BearerAuth returns a chi-compatible middleware that authenticates the
// caller as either the static master token or a live API key, and records
// the resulting Principal in the request context. Returns 401 with the
// standard Error envelope on mismatch.
//
// The token is read from the Authorization header ("Bearer <token>")
// for CLI/server callers, or — when no usable header is present — from
// the SessionCookieName cookie for browser callers (a separate SPA
// stores the token in an HttpOnly cookie via POST /login rather than
// in JavaScript). The same token is checked against the master secret
// first (constant-time), then against the API key store.
//
// keys may be nil to disable API-key auth (only the master token is
// accepted). If expectedToken is empty AND keys is nil, the middleware
// is a no-op pass-through (insecure/dev mode, matching fused's Insecure
// flag pattern); requests carry no Principal.
func BearerAuth(expectedToken string, keys APIKeyAuthenticator, onFailure AuthFailureFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if expectedToken == "" && keys == nil {
			return next // insecure mode
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reject := func(msg string) {
				if onFailure != nil {
					onFailure(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusUnauthorized, CodeUnauthorized, msg, nil)
			}

			token, ok := tokenFromRequest(r)
			if !ok {
				reject("missing or malformed credentials")
				return
			}

			// Master token path (constant-time). Skipped when no master
			// token is configured.
			if expectedToken != "" &&
				subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) == 1 {
				ctx := withPrincipal(r.Context(), Principal{Master: true})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// API key path.
			if keys != nil {
				if keyID, ok := keys.Authenticate(r.Context(), token); ok {
					ctx := withPrincipal(r.Context(), Principal{KeyID: keyID})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			reject("invalid token")
		})
	}
}

// tokenFromRequest extracts the caller's bearer token from either the
// Authorization header or the session cookie. The header takes
// precedence; the cookie is only consulted when the header is absent.
// Returns ("", false) when neither yields a usable token.
func tokenFromRequest(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	if header := r.Header.Get("Authorization"); header != "" {
		if !strings.HasPrefix(header, prefix) {
			return "", false
		}
		return header[len(prefix):], true
	}

	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value, true
	}

	return "", false
}

// CIDRAllowlist returns a middleware that rejects requests whose
// remote address is not within any of the given CIDR blocks. Returns
// 403 with the standard Error envelope on rejection.
//
// If cidrs is empty, the middleware is a no-op pass-through (open
// access). CIDRs are parsed at construction time; an invalid CIDR
// returns an error so the server fails fast at startup rather than
// silently admitting traffic.
func CIDRAllowlist(cidrs []string, onReject IPRejectFunc) (func(http.Handler) http.Handler, error) {
	if len(cidrs) == 0 {
		return func(next http.Handler) http.Handler { return next }, nil
	}

	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		nets = append(nets, ipNet)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			ip := net.ParseIP(host)
			if ip == nil {
				if onReject != nil {
					onReject(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
				}
				writeError(w, http.StatusForbidden, "forbidden",
					"could not parse remote address", nil)
				return
			}

			for _, n := range nets {
				if n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}

			if onReject != nil {
				onReject(r.RemoteAddr, r.Method, r.URL.Path, RequestID(r.Context()))
			}
			writeError(w, http.StatusForbidden, "forbidden",
				"source address not in allowlist", nil)
		})
	}, nil
}
