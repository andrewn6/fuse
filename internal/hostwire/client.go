package hostwire

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Timeouts for orchestrator-to-host-agent HTTP calls. The overall Timeout is
// the backstop; the per-phase ones exist so a host that accepts a connection
// and then goes quiet fails at the phase where it stalled instead of holding
// the full budget.
//
// Create is the slowest real call (boot a VM, wait for ssh) and can legitimately
// run for minutes, so the overall ceiling is generous. Callers that want
// something tighter pass a context deadline, which always wins.
const (
	agentDialTimeout           = 10 * time.Second
	agentTLSHandshakeTimeout   = 10 * time.Second
	agentResponseHeaderTimeout = 5 * time.Minute
	agentIdleConnTimeout       = 90 * time.Second
	agentRequestTimeout        = 10 * time.Minute
)

// MaxErrorBodyBytes bounds how much of a non-2xx response body is read into an
// error message. A host agent that answers an error with an unbounded stream
// would otherwise be read to completion into memory, and the interesting part
// of any real agent error is in the first line anyway.
const MaxErrorBodyBytes = 64 << 10

// ErrRedirectNotAllowed is returned when a host agent answers with a redirect.
var ErrRedirectNotAllowed = errors.New("host agent redirects are not followed")

// NewClient returns the HTTP client the providers use to reach a host agent.
//
// It exists because http.DefaultClient has no timeout at all: a host that
// accepts the connection and never answers would pin a provider call, and
// through it a fleet operation, forever.
//
// Redirects are refused rather than followed. Requests to a host agent carry
// that host's bearer token in a header, and Go's default policy forwards
// headers on same-host redirects while a redirect to a new origin would still
// take the orchestrator somewhere the operator never registered. Neither is
// something a host agent has any reason to ask for.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: agentRequestTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("%w: %s", ErrRedirectNotAllowed, req.URL.Redacted())
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   agentDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   agentTLSHandshakeTimeout,
			ResponseHeaderTimeout: agentResponseHeaderTimeout,
			IdleConnTimeout:       agentIdleConnTimeout,
			MaxIdleConnsPerHost:   4,
		},
	}
}

// ReadErrorBody reads at most MaxErrorBodyBytes of an error response body and
// returns it trimmed, for use in an error message.
func ReadErrorBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, MaxErrorBodyBytes))
	return strings.TrimSpace(string(b))
}

// ValidateAgentURL reports whether raw is an acceptable host-agent base URL.
//
// The URL is caller-supplied at host registration and the orchestrator then
// makes authenticated requests to it, so it is a destination policy decision,
// not a formatting one. The policy:
//
//   - http or https only. Any other scheme (file, gopher, unix, ...) is not a
//     host agent, and some of them read local state.
//   - a host must be present, and must not be an empty or malformed authority.
//   - no embedded credentials. A userinfo section would be sent to the
//     destination on every request and would end up in stored config.
//   - no query string and no fragment. Paths are concatenated onto this base
//     URL, so anything after the path is either ignored or silently changes
//     the request that gets built.
//
// A path prefix is allowed: a host agent may legitimately sit behind a reverse
// proxy at /agent.
func ValidateAgentURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("host url is required")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("host url is not a valid URL: %w", err)
	}

	switch u.Scheme {
	case "http", "https":
	case "":
		return errors.New("host url must include a scheme (http:// or https://)")
	default:
		return fmt.Errorf("host url scheme %q is not supported; use http or https", u.Scheme)
	}

	if u.User != nil {
		return errors.New("host url must not embed credentials; pass the agent token in the token field")
	}
	if u.Host == "" {
		return errors.New("host url must include a host")
	}
	if u.Hostname() == "" {
		return errors.New("host url must include a hostname")
	}
	if port := u.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return fmt.Errorf("host url has an invalid port %q", port)
		}
	}
	if u.RawQuery != "" || u.ForceQuery {
		return errors.New("host url must not include a query string")
	}
	if u.Fragment != "" {
		return errors.New("host url must not include a fragment")
	}

	return nil
}
