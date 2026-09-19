package steam

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Authenticated request execution: cookie attach, single auth refresh and
// one retry, then a typed error. At most 1 refresh + 1 retry per request;
// normal errors never trigger auth loops.

// Refresh forces a cookie refresh through the refresh token, coalesced
// across concurrent callers.
func (c *SteamAuthClient) Refresh(ctx context.Context) ([]*http.Cookie, error) {
	v, err, _ := c.group.Do("refresh", func() (any, error) {
		return c.refresh(ctx)
	})
	if err != nil {
		return nil, err
	}
	cookies, ok := v.([]*http.Cookie)
	if !ok {
		return nil, steamErr("auth", fmt.Errorf("unexpected refresh result type %T", v))
	}
	return cookies, nil
}

// AuthenticatedClient returns an HTTP client whose transport attaches jar
// cookies (allowlisted Steam hosts only) and retries once after a refresh
// on authentication failure. The jar is shared with GetWebCookies.
// Redirects are constrained to the allowlist so refresh-token transfers
// cannot replay credentials to an off-Steam destination.
func (c *SteamAuthClient) AuthenticatedClient(ctx context.Context) (*http.Client, error) {
	if _, err := c.GetWebCookies(ctx); err != nil {
		return nil, err
	}
	auth := c
	return &http.Client{
		Transport: &authRetryTransport{base: doerTransport{doer: c.httpClient, check: c.allowlisted}, auth: c},
		Jar:       c.jarForClient(),
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if !auth.allowlisted(req.URL.String()) {
				return steamErr("auth", fmt.Errorf("redirect outside allowlist: %s", redactURL(req.URL.String())))
			}
			return nil
		},
	}, nil
}

// doerTransport adapts the testable HTTPDoer to http.RoundTripper. When
// the doer is a plain *http.Client, redirects it would follow internally
// are constrained by check (the auth allowlist): without this, a 307/308
// would replay request headers to an off-allowlist host before the outer
// client's CheckRedirect ever sees the hop.
type doerTransport struct {
	doer  HTTPDoer
	check func(string) bool
}

// RoundTrip adapts the HTTPDoer to http.RoundTripper, constraining
// redirects through the auth allowlist check described on doerTransport.
func (t doerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.doer == nil {
		return http.DefaultTransport.RoundTrip(req)
	}
	if hc, ok := t.doer.(*http.Client); ok && t.check != nil {
		guarded := *hc
		prev := guarded.CheckRedirect
		check := t.check
		guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if !check(req.URL.String()) {
				return steamErr("auth", fmt.Errorf("redirect outside allowlist: %s", redactURL(req.URL.String())))
			}
			if prev != nil {
				return prev(req, via)
			}
			return nil
		}
		return guarded.Do(req) //nolint:gosec // guarded client inherits the allowlisted CheckRedirect above, so redirects cannot leave Steam.
	}
	return t.doer.Do(req)
}

func (c *SteamAuthClient) jarForClient() http.CookieJar {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jar
}

// authRetryTransport attaches cookies and retries once after refresh.
type authRetryTransport struct {
	base http.RoundTripper
	auth *SteamAuthClient
}

// RoundTrip attaches jar cookies and retries once after a refresh,
// as documented on authRetryTransport.
func (t *authRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.auth.allowlisted(req.URL.String()) {
		return nil, steamErr("auth", fmt.Errorf("refusing credentials outside allowlist: %s", redactURL(req.URL.String())))
	}
	// Cookies come from the client's jar (shared with GetWebCookies); the
	// transport only adds retry behavior, never headers, so cookies cannot
	// duplicate or leak across domains.
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if !isAuthFailure(resp) {
		return resp, nil
	}
	_ = resp.Body.Close()
	// One refresh, one retry. Bodies are rewindable for retries below.
	if _, refreshErr := t.auth.Refresh(req.Context()); refreshErr != nil {
		if isReauth(refreshErr) {
			return nil, refreshErr
		}
		return nil, steamErr("auth", fmt.Errorf("refresh before retry failed: %w", refreshErr))
	}
	retry := req.Clone(req.Context())
	// Drop the stale Cookie header: the refreshed jar owns cookies now.
	// Re-attach fresh cookies for the retry target so the retry never
	// replays the rejected cookie.
	retry.Header.Del("Cookie")
	if jar := t.auth.jarForClient(); jar != nil {
		retry.Header.Set("Cookie", strings.Join(cookieHeader(jar.Cookies(retry.URL)), "; "))
		if retry.Header.Get("Cookie") == "" {
			retry.Header.Del("Cookie")
		}
	}
	if req.Body != nil {
		if req.GetBody == nil {
			return nil, steamErr("auth", fmt.Errorf("cannot retry request with consumed body"))
		}
		body, err := req.GetBody()
		if err != nil {
			return nil, steamErr("auth", fmt.Errorf("cannot rewind body: %w", err))
		}
		retry.Body = body
	}
	retryResp, retryErr := t.base.RoundTrip(retry)
	if retryErr != nil {
		return retryResp, retryErr
	}
	if isAuthFailure(retryResp) {
		_ = retryResp.Body.Close()
		return nil, steamErr("auth", fmt.Errorf("still unauthenticated after refresh"))
	}
	return retryResp, nil
}

// cookieHeader renders jar cookies for a retry request.
func cookieHeader(cookies []*http.Cookie) []string {
	out := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		if ck == nil || ck.Name == "" {
			continue
		}
		out = append(out, ck.Name+"="+ck.Value)
	}
	return out
}

// isAuthFailure conservatively detects unauthenticated sessions: 401/403,
// redirects into a login flow, or an HTML login page body. Anything else
// (5xx, JSON API errors) is an ordinary failure, never an auth loop.
func isAuthFailure(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return true
	}
	if resp.StatusCode/100 == 3 {
		if loc := resp.Header.Get("Location"); strings.Contains(strings.ToLower(loc), "login") {
			return true
		}
	}
	return false
}

// isLoginPage sniffs a body for the Steam login form. Used when callers
// read a 200 that contains a login page instead of API data.
func isLoginPage(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "steamcommunity.com/login") &&
		(strings.Contains(lower, "signin") || strings.Contains(lower, "sign in"))
}

func isReauth(err error) bool {
	return err != nil && (errors.Is(err, ErrSteamReauthenticationRequired) || strings.Contains(err.Error(), "reauthentication required"))
}
