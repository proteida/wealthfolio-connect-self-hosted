package steam

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// authFixture serves a finalizelogin + transfer pair against itself.
type authFixture struct {
	server       *httptest.Server
	finalizeHits *int64
	transferHits *int64
	finalizeBody map[string]any
	transferCode int
	transferBody string
	transferSet  []string
}

func newAuthFixture(finalizeBody map[string]any) *authFixture {
	f := &authFixture{
		finalizeHits: new(int64),
		transferHits: new(int64),
		finalizeBody: finalizeBody,
		transferCode: 200,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/jwt/finalizelogin", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(f.finalizeHits, 1)
		Expect(r.Method).To(Equal(http.MethodPost))
		Expect(r.Header.Get("Origin")).To(Equal("https://steamcommunity.com"))
		w.Header().Add("Set-Cookie", "steamRefresh_steam=web||p|0; Path=/; Secure; HttpOnly")
		writeJSON(w, f.finalizeBody)
	})
	mux.HandleFunc("/transfer", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(f.transferHits, 1)
		Expect(r.Method).To(Equal(http.MethodPost))
		if f.transferCode != 200 {
			w.WriteHeader(f.transferCode)
			return
		}
		for _, c := range f.transferSet {
			w.Header().Add("Set-Cookie", c)
		}
		if f.transferBody != "" {
			_, _ = fmt.Fprint(w, f.transferBody)
			return
		}
		writeJSON(w, map[string]any{"result": 1})
	})
	f.server = httptest.NewServer(mux)
	return f
}

func (f *authFixture) transferURL() string { return f.server.URL + "/transfer" }

func (f *authFixture) client() *SteamAuthClient {
	c := NewSteamAuthClient("76561198000000000", "refresh-token-value", f.server.Client(), f.server.URL+"/jwt/finalizelogin")
	c.allowHosts = []string{"127.0.0.1"}
	return c
}

func transferBody(f *authFixture) map[string]any {
	return map[string]any{
		"transfer_info": []any{
			map[string]any{"url": f.transferURL(), "params": map[string]any{"nonce": "n"}},
		},
	}
}

var _ = Describe("Steam auth cookies", func() {
	It("parses steamLoginSecure with full attributes", func() {
		for _, h := range parseSetCookie("steamLoginSecure=abc%7C%7Cdef; Path=/; Secure; HttpOnly; SameSite=None; Domain=steamcommunity.com", "x.example") {
			Expect(h.Name).To(Equal("steamLoginSecure"))
			Expect(h.Domain).To(Equal("steamcommunity.com"))
			Expect(h.Secure).To(BeTrue())
			Expect(h.HttpOnly).To(BeTrue())
		}
		got := parseSetCookie("steamLoginSecure=v", "")
		Expect(got).To(HaveLen(1))
	})

	It("acquires cookies through finalize plus transfer", func() {
		f := newAuthFixture(nil)
		defer f.server.Close()
		f.finalizeBody = transferBody(f)
		f.transferSet = []string{"steamLoginSecure=user%7C%7Ctoken; Path=/; Secure; HttpOnly"}
		cookies, err := f.client().GetWebCookies(context.Background())
		Expect(err).NotTo(HaveOccurred())
		names := map[string]bool{}
		for _, c := range cookies {
			names[c.Name] = true
		}
		Expect(names).To(HaveKey("steamLoginSecure"))
		Expect(names).To(HaveKey("sessionid"))
		Expect(atomic.LoadInt64(f.finalizeHits)).To(Equal(int64(1)))
	})

	It("reuses cached cookies without refetching", func() {
		f := newAuthFixture(nil)
		defer f.server.Close()
		f.finalizeBody = transferBody(f)
		f.transferSet = []string{"steamLoginSecure=v; Path=/"}
		c := f.client()
		_, err := c.GetWebCookies(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = c.GetWebCookies(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(atomic.LoadInt64(f.finalizeHits)).To(Equal(int64(1)))
	})

	It("rejects empty refresh tokens without HTTP", func() {
		c := NewSteamAuthClient("1", "", nil, "")
		_, err := c.GetWebCookies(context.Background())
		Expect(err).To(MatchError(ErrSteamReauthenticationRequired))
	})

	It("reports reauthentication when Steam rejects the token", func() {
		f := newAuthFixture(map[string]any{"error": "InvalidGrant"})
		defer f.server.Close()
		_, err := f.client().GetWebCookies(context.Background())
		Expect(err).To(MatchError(ErrSteamReauthenticationRequired))
	})

	It("fails closed outside the allowlist", func() {
		c := NewSteamAuthClient("1", "tok", nil, "https://evil.example/jwt/finalizelogin")
		_, err := c.GetWebCookies(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("allowlist"))
	})
})

var _ = Describe("Authenticated retry", func() {
	It("refreshes once and retries after a 403", func() {
		var finalizeHits, transferHits, dataCalls int64
		var transferBody map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("/jwt/finalizelogin", func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&finalizeHits, 1)
			w.Header().Add("Set-Cookie", "steamRefresh_steam=x; Path=/")
			writeJSON(w, transferBody)
		})
		mux.HandleFunc("/transfer", func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&transferHits, 1)
			w.Header().Add("Set-Cookie", "steamLoginSecure=v; Path=/")
			writeJSON(w, map[string]any{"result": 1})
		})
		mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
			// First call always rejects (stale session); the retry must
			// carry a refreshed cookie.
			if atomic.AddInt64(&dataCalls, 1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if !strings.Contains(r.Header.Get("Cookie"), "steamLoginSecure=") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, "ok")
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		transferBody = map[string]any{
			"transfer_info": []any{
				map[string]any{"url": srv.URL + "/transfer", "params": map[string]any{"nonce": "n"}},
			},
		}
		c := NewSteamAuthClient("76561198000000000", "tok", srv.Client(), srv.URL+"/jwt/finalizelogin")
		c.allowHosts = []string{"127.0.0.1"}
		authed, err := c.AuthenticatedClient(context.Background())
		Expect(err).NotTo(HaveOccurred())

		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/data", nil)
		resp, err := authed.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(atomic.LoadInt64(&dataCalls)).To(Equal(int64(2)))
		Expect(atomic.LoadInt64(&finalizeHits)).To(Equal(int64(2))) // initial + retry refresh
		Expect(atomic.LoadInt64(&transferHits)).To(Equal(int64(2)))
	})

	It("detects auth failures conservatively", func() {
		Expect(isAuthFailure(&http.Response{StatusCode: 401})).To(BeTrue())
		Expect(isAuthFailure(&http.Response{StatusCode: 403})).To(BeTrue())
		Expect(isAuthFailure(&http.Response{StatusCode: 200})).To(BeFalse())
		Expect(isAuthFailure(&http.Response{StatusCode: 500})).To(BeFalse())
		Expect(isAuthFailure(&http.Response{
			StatusCode: 302, Header: http.Header{"Location": {"https://steamcommunity.com/login/home/"}},
		})).To(BeTrue())
		Expect(isAuthFailure(&http.Response{
			StatusCode: 302, Header: http.Header{"Location": {"https://example.com/other"}},
		})).To(BeFalse())
		Expect(isLoginPage([]byte(`<html><a href="https://steamcommunity.com/login">sign in</a></html>`))).To(BeTrue())
		Expect(isLoginPage([]byte(`{"success":true}`))).To(BeFalse())
	})
})

var _ = Describe("Auth concurrency", func() {
	It("refreshes once for many simultaneous waiters", func() {
		f := newAuthFixture(nil)
		defer f.server.Close()
		f.finalizeBody = transferBody(f)
		f.transferSet = []string{"steamLoginSecure=v; Path=/"}
		c := f.client()
		var wg sync.WaitGroup
		errs := make([]error, 20)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = c.GetWebCookies(context.Background())
			}(i)
		}
		wg.Wait()
		for _, err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(atomic.LoadInt64(f.finalizeHits)).To(Equal(int64(1)))
	})
})

var _ = Describe("Secret redaction", func() {
	It("redacts secret headers and query values", func() {
		h := redactHeaders(http.Header{
			"Authorization": {"Bearer tok"},
			"Cookie":        {"steamLoginSecure=abc"},
			"Set-Cookie":    {"a=b"},
			"Content-Type":  {"application/json"},
		})
		Expect(h.Get("Authorization")).To(Equal("[redacted]"))
		Expect(h.Get("Cookie")).To(Equal("[redacted]"))
		Expect(h.Get("Content-Type")).To(Equal("application/json"))
		Expect(redactURL("https://x.example/?key=SECRET&nonce=TOK&other=1")).NotTo(ContainSubstring("SECRET"))
		Expect(redactURL("https://x.example/?key=SECRET&nonce=TOK&other=1")).To(ContainSubstring("other=1"))
		Expect(redactURL("::bad-url::")).To(Equal("[unparseable url]"))
	})

	It("never leaks the refresh token through refresh errors", func() {
		f := newAuthFixture(map[string]any{"error": "InvalidGrant"})
		defer f.server.Close()
		_, err := f.client().GetWebCookies(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("refresh-token-value"))
	})
})
