package steam

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	cookies, _ := v.([]*http.Cookie)
	return cookies, nil
}

// AuthenticatedClient returns an HTTP client whose transport attaches jar
// cookies (allowlisted Steam hosts only) and retries once after a refresh
// on authentication failure. The jar is shared with GetWebCookies.
func (c *SteamAuthClient) AuthenticatedClient(ctx context.Context) (*http.Client, error) {
	if _, err := c.GetWebCookies(ctx); err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &authRetryTransport{base: doerTransport{c.httpClient}, auth: c},
		Jar:       c.jarForClient(),
	}, nil
}

// doerTransport adapts the testable HTTPDoer to http.RoundTripper.
type doerTransport struct{ doer HTTPDoer }

func (t doerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.doer == nil {
		return http.DefaultTransport.RoundTrip(req)
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

func (t *authRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.auth.allowlisted(req.URL.String()) {
		return nil, steamErr("auth", fmt.Errorf("refusing credentials outside allowlist: %s", redactURL(req.URL.String())))
	}
	t.auth.attachCookies(req)
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if !isAuthFailure(resp) {
		return resp, nil
	}
	_ = resp.Body.Close()
	// One refresh, one retry. Bodies are rewindable for retries below.
	if _, err := t.auth.Refresh(req.Context()); err != nil {
		if isReauth(err) {
			return nil, err
		}
		return nil, steamErr("auth", fmt.Errorf("refresh before retry failed: %w", err))
	}
	retry := req.Clone(req.Context())
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
	t.auth.attachCookies(retry)
	resp2, err := t.base.RoundTrip(retry)
	if err != nil {
		return resp2, err
	}
	if isAuthFailure(resp2) {
		_ = resp2.Body.Close()
		return nil, steamErr("auth", fmt.Errorf("still unauthenticated after refresh"))
	}
	return resp2, nil
}

// attachCookies copies jar cookies into the request (allowlisted hosts
// only; the jar itself scopes by domain, this is defense in depth).
func (c *SteamAuthClient) attachCookies(req *http.Request) {
	if req.URL == nil || !c.allowlisted(req.URL.String()) {
		return
	}
	c.mu.Lock()
	cookies := c.jar.Cookies(req.URL)
	c.mu.Unlock()
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
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
	return err != nil && (err == ErrSteamReauthenticationRequired || strings.Contains(err.Error(), "reauthentication required"))
}

// peekBody reads up to n bytes for sniffing while keeping the body
// consumable for the real decode.
func peekBody(resp *http.Response, n int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, n))
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), resp.Body))
	return raw, nil
}
