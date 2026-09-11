package steam

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/sync/singleflight"
)

// This file implements Steam WebBrowser refresh-token → web-cookie
// authentication natively in Go, mirroring DoctorMcKay/node-steam-session
// (v1.9.4) LoginSession.getWebCookies(), which is the behavioral
// reference. The inventory/history business logic never touches this
// protocol directly (see SteamAuthenticator); endpoint and message
// constants below keep future Steam changes patchable in one place.
//
// Reference flow (WebBrowser platform):
//  1. sessionid = 12 random bytes, hex.
//  2. POST https://login.steampowered.com/jwt/finalizelogin with multipart
//     {nonce: refreshToken, sessionid, redir} plus Origin/Referer headers.
//  3. The response carries transfer_info [{url, params}]; each transfer is
//     POSTed as multipart {steamID, ...params} (up to 5 attempts) and must
//     yield Set-Cookie headers including steamLoginSecure.
//  4. Returned sessionid cookies are dropped; one sessionid cookie per
//     cookie domain (except login.steampowered.com) is synthesized.
// Refresh-token renewal (GenerateAccessTokenForApp over the CM protobuf
// transport) is intentionally out of scope: it needs the CM WebSocket +
// protobuf stack, and the reference implementation never observes rotation
// inside this flow. If a transfer ever surfaces a replacement refresh
// token, recordRotation exposes it instead of losing it (see TokenUpdate).

const (
	// finalizeLoginURL exchanges a WebBrowser refresh token for transfer
	// URLs. Mirrors the finalizelogin POST in getWebCookies.
	finalizeLoginURL = "https://login.steampowered.com/jwt/finalizelogin"
	// finalizeRedir is sent verbatim in the finalize form.
	finalizeRedir = "https://steamcommunity.com/login/home/?goto="
	// transferAttempts bounds transfer retries (reference: 5).
	transferAttempts = 5
	// transferRetryDelay spaces transfer attempts (reference: 500ms).
	transferRetryDelay = 500 * time.Millisecond
)

// steamCookieHosts is the allowlist for authentication-cookie traffic.
// Cookies and tokens never leave these hosts.
var steamCookieHosts = []string{
	"steamcommunity.com",
	"store.steampowered.com",
	"help.steampowered.com",
	"login.steampowered.com",
}

// ErrSteamReauthenticationRequired is returned when Steam clearly rejects
// the refresh token (expired/revoked). Run `npm run steam-auth` again and
// replace STEAM_REFRESH_TOKEN. Never fall back to stored credentials.
var ErrSteamReauthenticationRequired = errors.New(
	"steam reauthentication required: refresh token rejected; run npm run steam-auth again",
)

// TokenUpdate carries a replacement refresh token Steam issued. The service
// has no safe secret storage, so it never persists the token itself: it
// logs that an update is required (without the value) and exposes it here.
type TokenUpdate struct {
	RefreshToken string
}

// SteamAuthenticator isolates the Steam login protocol behind a small
// interface; business logic depends only on this.
type SteamAuthenticator interface {
	// GetWebCookies returns fresh authenticated web cookies, refreshing
	// through the refresh token when the jar is empty or stale.
	GetWebCookies(ctx context.Context) ([]*http.Cookie, error)
}

// SteamAuthClient is a SteamAuthenticator over a WebBrowser refresh token.
type SteamAuthClient struct {
	steamID      string
	refreshToken string
	httpClient   HTTPDoer
	finalizeURL  string
	log          zerolog.Logger
	// allowHosts overrides the cookie-destination allowlist (tests).
	allowHosts []string
	// cookieTTL bounds how long acquired cookies are trusted without
	// re-checking the jar. It mirrors the reference's 10-minute access
	// token age and stops refresh loops when Steam scopes cookies to a
	// non-community domain.
	cookieTTL time.Duration

	mu       sync.Mutex
	jar      *cookiejar.Jar
	rotated  string
	rotated_ bool
	// lastCookies/lastRefresh short-circuit repeat refreshes.
	lastCookies []*http.Cookie
	lastRefresh time.Time

	group singleflight.Group
}

// NewSteamAuthClient builds the authenticator. httpClient may be nil (a
// 15s-timeout client is used). finalizeURL overrides the endpoint in tests.
func NewSteamAuthClient(steamID, refreshToken string, httpClient HTTPDoer, finalizeURL string) *SteamAuthClient {
	if finalizeURL == "" {
		finalizeURL = finalizeLoginURL
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &SteamAuthClient{
		steamID: steamID, refreshToken: refreshToken,
		httpClient: httpClient, finalizeURL: finalizeURL, jar: jar,
		log: zerolog.Nop(), cookieTTL: 10 * time.Minute,
	}
}

// SetLogger attaches structured logging. Secrets are never logged; a
// rotation only logs that persistent token update is required.
func (c *SteamAuthClient) SetLogger(log zerolog.Logger) { c.log = log }

// RotatedToken reports a replacement refresh token Steam issued, if any.
// The caller owns persistence; the service only logs that rotation
// happened, never the value.
func (c *SteamAuthClient) RotatedToken() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rotated, c.rotated_
}

// recordRotation stores a replacement token and notes the required action.
// The token itself is never logged.
func (c *SteamAuthClient) recordRotation(token string) {
	if token == "" {
		return
	}
	c.mu.Lock()
	c.rotated, c.rotated_ = token, true
	c.mu.Unlock()
}

// newSessionID mints the CSRF session id. Reference: 12 random bytes, hex.
func newSessionID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// allowlisted reports whether a URL targets Steam auth infrastructure.
func (c *SteamAuthClient) allowlisted(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	hosts := c.allowHosts
	if len(hosts) == 0 {
		hosts = steamCookieHosts
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// finalizeResponse mirrors the finalizelogin JSON body.
type finalizeResponse struct {
	Error        any `json:"error"`
	TransferInfo []struct {
		URL    string         `json:"url"`
		Params map[string]any `json:"params"`
	} `json:"transfer_info"`
}

// GetWebCookies returns authenticated web cookies, refreshing once through
// the refresh token when the jar holds nothing usable.
func (c *SteamAuthClient) GetWebCookies(ctx context.Context) ([]*http.Cookie, error) {
	if strings.TrimSpace(c.refreshToken) == "" {
		return nil, ErrSteamReauthenticationRequired
	}
	c.mu.Lock()
	fresh := c.lastRefresh
	held := c.lastCookies
	ttl := c.cookieTTL
	c.mu.Unlock()
	if len(held) > 0 && ttl > 0 && time.Since(fresh) < ttl {
		return held, nil
	}
	if cookies := c.usableCookies(); len(cookies) > 0 {
		return cookies, nil
	}
	// One refresh for any number of concurrent waiters.
	v, err, _ := c.group.Do("refresh", func() (any, error) {
		return c.refresh(ctx)
	})
	if err != nil {
		return nil, err
	}
	cookies, _ := v.([]*http.Cookie)
	return cookies, nil
}

// usableCookies returns jar cookies for the community domain when a
// steamLoginSecure cookie is present.
func (c *SteamAuthClient) usableCookies() []*http.Cookie {
	u := &url.URL{Scheme: "https", Host: "steamcommunity.com", Path: "/"}
	c.mu.Lock()
	defer c.mu.Unlock()
	cookies := c.jar.Cookies(u)
	for _, cookie := range cookies {
		if cookie.Name == "steamLoginSecure" && cookie.Value != "" {
			return cookies
		}
	}
	return nil
}

// refresh runs the finalizelogin + transfer flow and stores the cookies.
func (c *SteamAuthClient) refresh(ctx context.Context) ([]*http.Cookie, error) {
	sessionID, err := newSessionID()
	if err != nil {
		return nil, steamErr("auth sessionid", err)
	}
	if !c.allowlisted(c.finalizeURL) {
		return nil, steamErr("auth", fmt.Errorf("finalize URL outside allowlist: %s", redactURL(c.finalizeURL)))
	}
	form := map[string]string{
		"nonce":     c.refreshToken,
		"sessionid": sessionID,
		"redir":     finalizeRedir,
	}
	var env finalizeResponse
	if err := c.postForm(ctx, c.finalizeURL, form, map[string]string{
		"Origin":  "https://steamcommunity.com",
		"Referer": "https://steamcommunity.com/",
	}, &env); err != nil {
		return nil, err
	}
	if env.Error != nil {
		return nil, classifyRefreshError(env.Error)
	}
	if len(env.TransferInfo) == 0 {
		return nil, steamErr("auth finalizelogin", fmt.Errorf("malformed response: no transfer_info"))
	}
	var cookies []*http.Cookie
	for _, tr := range env.TransferInfo {
		got, err := c.executeTransfer(ctx, tr.URL, tr.Params)
		if err != nil {
			return nil, err
		}
		cookies = append(cookies, got...)
	}
	cookies = synthesizeSessionIDs(cookies, sessionID)
	c.mu.Lock()
	for _, cookie := range cookies {
		u := &url.URL{Scheme: "https", Host: cookieDomain(cookie), Path: "/"}
		c.jar.SetCookies(u, []*http.Cookie{cookie})
		// Forward-compatible rotation detection: surface (never persist)
		// any replacement refresh token Steam embeds in cookie values.
		if strings.EqualFold(cookie.Name, "refresh_token") && cookie.Value != "" {
			c.rotated, c.rotated_ = cookie.Value, true
		}
	}
	c.lastCookies = cookies
	c.lastRefresh = time.Now()
	rotated := c.rotated_
	c.mu.Unlock()
	if rotated {
		// Deliberately value-free: the token is a password-equivalent.
		c.log.Warn().Msg("Steam issued a new refresh token; persistent token update required")
	}
	return cookies, nil
}

// cookieDomain resolves the jar URL host for a cookie: explicit Domain
// wins (reference adds Domain= when Steam omits it), else community.
func cookieDomain(cookie *http.Cookie) string {
	if cookie.Domain != "" {
		return strings.TrimPrefix(cookie.Domain, ".")
	}
	return "steamcommunity.com"
}

// executeTransfer POSTs one transfer with retries and returns its cookies.
func (c *SteamAuthClient) executeTransfer(ctx context.Context, rawURL string, params map[string]any) ([]*http.Cookie, error) {
	if !c.allowlisted(rawURL) {
		return nil, steamErr("auth", fmt.Errorf("transfer URL outside allowlist: %s", redactURL(rawURL)))
	}
	form := map[string]string{"steamID": c.steamID}
	for k, v := range params {
		form[k] = fmt.Sprint(v)
	}
	var lastErr error
	for attempt := 0; attempt < transferAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(transferRetryDelay):
			}
		}
		var result struct {
			Result int `json:"result"`
		}
		setCookies, status, err := c.postFormRaw(ctx, rawURL, form, nil, &result)
		if err != nil {
			lastErr = err
			continue
		}
		if status >= 400 {
			lastErr = steamErr("auth transfer", fmt.Errorf("http %d", status))
			continue
		}
		if result.Result != 0 && result.Result != 1 {
			// EResult.OK is 1; tolerate explicit zero as success.
			lastErr = steamErr("auth transfer", fmt.Errorf("result %d", result.Result))
			continue
		}
		if len(setCookies) == 0 {
			lastErr = steamErr("auth transfer", fmt.Errorf("no Set-Cookie in result"))
			continue
		}
		hasLogin := false
		for _, cookie := range setCookies {
			if cookie.Name == "steamLoginSecure" && cookie.Value != "" {
				hasLogin = true
			}
		}
		if !hasLogin {
			lastErr = steamErr("auth transfer", fmt.Errorf("no steamLoginSecure cookie in result"))
			continue
		}
		return setCookies, nil
	}
	return nil, lastErr
}

// synthesizeSessionIDs drops any sessionid cookies Steam returned and adds
// one per cookie domain except login.steampowered.com (reference behavior).
func synthesizeSessionIDs(cookies []*http.Cookie, sessionID string) []*http.Cookie {
	var out []*http.Cookie
	domains := map[string]bool{}
	for _, cookie := range cookies {
		if strings.EqualFold(cookie.Name, "sessionid") {
			continue
		}
		out = append(out, cookie)
		d := strings.ToLower(strings.TrimPrefix(cookieDomain(cookie), "."))
		if d != "" && d != "login.steampowered.com" {
			domains[d] = true
		}
	}
	for d := range domains {
		out = append(out, &http.Cookie{
			Name: "sessionid", Value: sessionID, Path: "/",
			Domain: d, Secure: true, SameSite: http.SameSiteNoneMode,
		})
	}
	return out
}

// refreshErrorSignatures that unambiguously mean the refresh token is dead.
var refreshErrorSignatures = []string{
	"invalidgrant", "invalid_grant", "expired", "revoked",
}

// classifyRefreshError maps a finalizelogin error to reauthentication (when
// Steam clearly rejects the token) or a plain refresh failure otherwise.
func classifyRefreshError(v any) error {
	s := strings.ToLower(fmt.Sprint(v))
	for _, sig := range refreshErrorSignatures {
		if strings.Contains(s, sig) {
			return ErrSteamReauthenticationRequired
		}
	}
	return steamErr("auth finalizelogin", fmt.Errorf("%v", v))
}

// postForm POSTs multipart form fields and decodes a JSON body into out.
func (c *SteamAuthClient) postForm(ctx context.Context, rawURL string, form map[string]string, headers map[string]string, out any) error {
	_, _, err := c.postFormRaw(ctx, rawURL, form, headers, out)
	return err
}

// postFormRaw is postForm plus raw status and Set-Cookie headers.
func (c *SteamAuthClient) postFormRaw(ctx context.Context, rawURL string, form map[string]string, headers map[string]string, out any) ([]*http.Cookie, int, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range form {
		if err := w.WriteField(k, v); err != nil {
			return nil, 0, steamErr("auth form", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, 0, steamErr("auth form", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, &body)
	if err != nil {
		return nil, 0, steamErr("auth request", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, steamErr("auth http", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, steamErr("auth body", err)
	}
	var setCookies []*http.Cookie
	host := ""
	if resp.Request != nil && resp.Request.URL != nil {
		host = resp.Request.URL.Host
	}
	for _, h := range resp.Header["Set-Cookie"] {
		parsed := parseSetCookie(h, host)
		setCookies = append(setCookies, parsed...)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return setCookies, resp.StatusCode, steamErr("auth decode", err)
		}
	}
	return setCookies, resp.StatusCode, nil
}

// parseSetCookie parses Set-Cookie headers, adding Domain= from the
// response host when Steam omits it (reference behavior). Ports never
// belong in a cookie domain and are stripped.
func parseSetCookie(header, host string) []*http.Cookie {
	if host == "" {
		host = "steamcommunity.com"
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	resp := &http.Response{Header: http.Header{"Set-Cookie": []string{header}}}
	cookies := resp.Cookies()
	for _, cookie := range cookies {
		if cookie.Domain == "" && host != "" {
			cookie.Domain = host
		}
	}
	return cookies
}
