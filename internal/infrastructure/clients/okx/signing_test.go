package okx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

type failingDoer struct{ read bool }

func (f failingDoer) Do(*http.Request) (*http.Response, error) {
	if f.read {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(failingReader{})}, nil
	}
	return nil, errors.New("transport failure")
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }

// stubHistory is a minimal ActivityRepository: List serves newest-first
// rows so the oldest-saved cursor lookup resolves to the final row.
type stubHistory struct{ ids []string }

func (s *stubHistory) List(_ context.Context, f repository.ActivityFilter) ([]brokerage.Activity, int, error) {
	rows := make([]brokerage.Activity, 0, len(s.ids))
	for _, id := range s.ids {
		rows = append(rows, brokerage.Activity{SourceRecordID: id})
	}
	if f.Offset > 0 && f.Offset < len(rows) {
		return rows[f.Offset:], len(rows), nil
	}
	if f.Offset >= len(rows) {
		return nil, len(rows), nil
	}
	return rows, len(rows), nil
}

func (s *stubHistory) UpsertBatch(_ context.Context, _ string, _ []brokerage.Activity) error {
	return nil
}

func (s *stubHistory) Delete(_ context.Context, _ string, _ []string) error {
	return nil
}

// stubCursorStore is an in-memory CursorRepository.
type stubCursorStore struct {
	rows map[string]repository.SyncCursor
}

func (s *stubCursorStore) Get(_ context.Context, scope string) (repository.SyncCursor, error) {
	if c, ok := s.rows[scope]; ok {
		return c, nil
	}
	return repository.SyncCursor{}, repository.ErrNotFound
}

func (s *stubCursorStore) Set(_ context.Context, c repository.SyncCursor) error {
	if s.rows == nil {
		s.rows = make(map[string]repository.SyncCursor)
	}
	s.rows[c.Scope] = c
	return nil
}

func (s *stubCursorStore) Delete(_ context.Context, scope string) error {
	delete(s.rows, scope)
	return nil
}

var _ = Describe("Shared OKX signing and CEX compatibility", func() {
	It("preserves CEX balances and paginated fills while Web3 uses v6", func() {
		var cursors []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if r.URL.Path == "/api/v5/account/balance" {
				fmt.Fprint(w, `{"code":"0","data":[{"details":[{"ccy":"ETH","cashBal":"0","eqUsd":"0"},{"ccy":"USDT","cashBal":"10","eqUsd":"10"}]}]}`)
				return
			}
			Expect(r.URL.Path).To(Equal("/api/v5/trade/fills-history"))
			after := r.URL.Query().Get("after")
			cursors = append(cursors, after)
			if after != "" {
				fmt.Fprint(w, `{"code":"0","data":[]}`)
				return
			}
			fmt.Fprint(w, `{"code":"0","data":[{"billId":"fill-1","instId":"ETH-USDT","side":"sell","fillPx":"3000","fillSz":"1","fee":"-0.1","feeCcy":"USDT","ts":"1700000000000"},{"billId":"fill-2","instId":"ETH-USDT","side":"buy","fillPx":"3001","fillSz":"1","fee":"","feeCcy":"USDT","ts":"1700000001000"}]}`)
		}))
		defer server.Close()
		c := NewCEX(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, server.URL, server.Client())
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// A short first page exhausts the range: no follow-up request,
		// history complete.
		Expect(cursors).To(Equal([]string{""}))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(10.0))
		Expect(snap.Activities["okx-spot"]).To(HaveLen(4))
		Expect(snap.Activities["okx-spot"][0].Type).To(Equal(brokerage.ActivitySell))
		Expect(snap.Activities["okx-spot"][0].Symbol.Symbol).To(Equal("ETH"))
		Expect(snap.Activities["okx-spot"][0].Fee).To(Equal(0.1))
		Expect(snap.Activities["okx-spot"][0].FeeAsset).To(Equal("USD"))
	})
	It("reports pending on full pages and resumes the backfill", func() {
		var cursors []string
		fills := func(prefix string, n int) string {
			page := make([]string, 0, n)
			for i := 0; i < n; i++ {
				page = append(page, fmt.Sprintf(`{"billId":"%s-%03d","instId":"ETH-USDT","side":"sell","fillPx":"3000","fillSz":"1","fee":"0","feeCcy":"USDT","ts":"1700000000000"}`, prefix, i))
			}
			return strings.Join(page, ",")
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if r.URL.Path == "/api/v5/account/balance" {
				fmt.Fprint(w, `{"code":"0","data":[{"details":[{"ccy":"USDT","cashBal":"10","eqUsd":"10"}]}]}`)
				return
			}
			after := r.URL.Query().Get("after")
			cursors = append(cursors, after)
			switch after {
			case "":
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("new", 100))
			case "new-099":
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("mid", 100))
			case "mid-099":
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("old", 100))
			default: // "old-099": tail exhausted
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("tail", 1))
			}
		}))
		defer server.Close()
		c := NewCEX(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, server.URL, server.Client())
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Three full pages within budget → pending, cursor kept, all
		// fetched pages retained (300 fills → 600 legs).
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Activities["okx-spot"]).To(HaveLen(600))
		Expect(c.fillsAfter).To(Equal("old-099"))
		// Without a commit (failed persistence), the tentative cursor is
		// dropped and the next run replays from the newest fills instead
		// of skipping unsaved rows.
		cursors = nil
		snap, err = c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cursors[0]).To(Equal(""))
		Expect(snap.Activities["okx-spot"]).To(HaveLen(600))
		// Next run after commit resumes from the cursor and exhausts the range.
		c.SnapshotCommitted()
		snap, err = c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cursors[len(cursors)-1]).To(Equal("old-099"))
		Expect(c.fillsAfter).To(BeEmpty())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
	})
	It("resumes past saved history after a restart via the durable cursor", func() {
		var cursors []string
		fills := func(prefix string, n int) string {
			page := make([]string, 0, n)
			for i := 0; i < n; i++ {
				page = append(page, fmt.Sprintf(`{"billId":"%s-%03d","instId":"ETH-USDT","side":"sell","fillPx":"3000","fillSz":"1","fee":"0","feeCcy":"USDT","ts":"1700000000000"}`, prefix, i))
			}
			return strings.Join(page, ",")
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if r.URL.Path == "/api/v5/account/balance" {
				fmt.Fprint(w, `{"code":"0","data":[{"details":[{"ccy":"USDT","cashBal":"10","eqUsd":"10"}]}]}`)
				return
			}
			after := r.URL.Query().Get("after")
			cursors = append(cursors, after)
			switch after {
			case "":
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("new", 100))
			case "new-099":
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("mid", 100))
			case "mid-099":
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("old", 100))
			default: // "old-099": tail exhausted
				fmt.Fprintf(w, `{"code":"0","data":[%s]}`, fills("tail", 1))
			}
		}))
		defer server.Close()
		// 400 fills: the first client imports 300 and commits the cursor.
		first := NewCEX(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, server.URL, server.Client())
		store := &stubCursorStore{}
		first.ConfigureCursors(store)
		snap, err := first.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Activities["okx-spot"]).To(HaveLen(600))
		first.SnapshotCommitted()
		Expect(store.rows["okx-fills"].Position).To(Equal("old-099"))
		// Restarted client resumes past the saved prefix and reaches the tail.
		restarted := NewCEX(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, server.URL, server.Client())
		restarted.ConfigureCursors(store)
		snap, err = restarted.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cursors).To(Equal([]string{"", "new-099", "mid-099", "old-099"}))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Activities["okx-spot"]).To(HaveLen(2))
	})

	It("stops at already-persisted history via the durable cursor", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if r.URL.Path == "/api/v5/account/balance" {
				fmt.Fprint(w, `{"code":"0","data":[{"details":[{"ccy":"USDT","cashBal":"10","eqUsd":"10"}]}]}`)
				return
			}
			after := r.URL.Query().Get("after")
			if after == "" || after == "saved-100" {
				fmt.Fprint(w, `{"code":"0","data":[{"billId":"saved-100","instId":"ETH-USDT","side":"sell","fillPx":"3000","fillSz":"1","fee":"0","feeCcy":"USDT","ts":"1700000000000"}]}`)
				return
			}
			fmt.Fprint(w, `{"code":"0","data":[]}`)
		}))
		defer server.Close()
		c := NewCEX(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, server.URL, server.Client())
		c.ConfigureHistory(&stubHistory{ids: []string{"saved-100-quote", "saved-100-base"}})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Reached the persisted billID on the first page: newest-to-saved
		// range fully covered, history complete.
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(c.fillsAfter).To(BeEmpty())
	})
	It("preserves balances without completion on CEX history errors", func() {
		for _, status := range []int{200, 503} {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "account/") {
					fmt.Fprint(w, `{"code":"0","data":[]}`)
					return
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"code":"500","msg":"failed"}`)
			}))
			snap, err := NewCEX(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, server.URL, server.Client()).Fetch(context.Background())
			server.Close()
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		}
	})
	It("propagates transport and body failures without completing either product", func() {
		for _, read := range []bool{false, true} {
			c := NewCEX(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, "https://example.test", failingDoer{read: read})
			_, err := c.Fetch(context.Background())
			Expect(err).To(HaveOccurred())
			web3 := NewWeb3(c.creds, []Wallet{{Address: "0xabc", Chains: []string{"1"}}}, c.baseURL, failingDoer{read: read})
			snap, err := web3.Fetch(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Accounts).To(BeEmpty())
		}
	})
	It("signs the encoded query, v6 path and body with the actual timestamp", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			body, err := io.ReadAll(r.Body)
			Expect(err).NotTo(HaveOccurred())
			mac := hmac.New(sha256.New, []byte("s"))
			mac.Write([]byte(r.Header.Get("OK-ACCESS-TIMESTAMP") + r.Method + r.URL.RequestURI() + string(body)))
			Expect(r.Header.Get("OK-ACCESS-SIGN")).To(Equal(base64.StdEncoding.EncodeToString(mac.Sum(nil))))
			json.NewEncoder(w).Encode(map[string]string{"code": "0"})
		}))
		defer server.Close()
		Expect(signedRequest(context.Background(), server.Client(), server.URL, http.MethodPost, "/api/v6/test", url.Values{"chains": {"1,56"}}, []byte(`{"address":"0xabc"}`), Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, nil)).To(Succeed())
	})
})
