package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	domainsteam "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/steam"
)

var _ = Describe("Review hardening", func() {
	It("rejects unsuccessful inventory envelopes", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": false, "assets": []any{}, "descriptions": []any{}})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		_, complete, err := c.fetchInventory(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(complete).To(BeFalse())
	})

	It("marks total-count mismatches partial", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"success": 1, "total_inventory_count": 5,
				"assets":       []any{map[string]any{"assetid": "1", "classid": "1", "instanceid": "0", "amount": "1"}},
				"descriptions": []any{},
			})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		items, complete, err := c.fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(items).To(HaveLen(1))
	})

	It("parses European decimal commas", func() {
		v, ok := parseSteamMoney("31,28€")
		Expect(ok).To(BeTrue())
		Expect(v).To(BeNumerically("~", 31.28, 1e-9))
		v, ok = parseSteamMoney("$1,234.56")
		Expect(ok).To(BeTrue())
		Expect(v).To(BeNumerically("~", 1234.56, 1e-9))
		v, ok = parseSteamMoney("1.234,56€")
		Expect(ok).To(BeTrue())
		Expect(v).To(BeNumerically("~", 1234.56, 1e-9))
	})

	It("redacts API keys from network errors", func() {
		ue := &url.Error{Op: "Get", URL: "https://api.steampowered.com/x?key=SECRET123&foo=bar", Err: fmt.Errorf("boom")}
		err := sanitizeHTTPError(ue)
		Expect(err.Error()).NotTo(ContainSubstring("SECRET123"))
		Expect(err.Error()).To(ContainSubstring("redacted"))
	})

	It("requires HTTPS outside test overrides", func() {
		c := NewSteamAuthClient("1", "tok", nil, "")
		Expect(c.allowlisted("http://steamcommunity.com/x")).To(BeFalse())
		Expect(c.allowlisted("https://steamcommunity.com/x")).To(BeTrue())
		Expect(c.allowlisted("https://evil.example/x")).To(BeFalse())
	})

	It("consumes one purchase for identical items deterministically", func() {
		now := time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)
		ev := historyEvent{ExternalID: "e1", Timestamp: now, Kind: EventMarketBuy, MarketHashName: "AK", Quantity: 1}
		market := map[string]domainsteam.MarketTransaction{
			"m1": {ExternalID: "m1", Type: "buy", Timestamp: now, MarketHashName: "AK", Quantity: 1, Gross: 10},
		}
		owned := []InventoryItem{
			{AssetID: "a1", ClassID: "1", InstanceID: "0", Amount: 1, MarketHashName: "AK"},
			{AssetID: "a2", ClassID: "1", InstanceID: "0", Amount: 1, MarketHashName: "AK"},
		}
		resolved, _ := Resolve(owned, nil, []historyEvent{ev}, market, nil, now)
		Expect(resolved).To(HaveLen(2))
		high := 0
		for _, r := range resolved {
			if r.MatchConfidence == domainsteam.MatchHigh {
				high++
			}
		}
		Expect(high).To(Equal(1))
	})

	It("retries with the refreshed cookie, not the stale one", func() {
		var seen []string
		var calls int
		mux := http.NewServeMux()
		mux.HandleFunc("/jwt/finalizelogin", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Set-Cookie", "steamRefresh_steam=x; Path=/")
			writeJSON(w, map[string]any{"transfer_info": []any{
				map[string]any{"url": "http://" + r.Host + "/transfer", "params": map[string]any{"nonce": "n"}},
			}})
		})
		mux.HandleFunc("/transfer", func(w http.ResponseWriter, r *http.Request) {
			calls++
			// First refresh issues old cookie, second issues new.
			v := "old-cookie-value"
			if calls > 1 {
				v = "new-cookie-value"
			}
			w.Header().Add("Set-Cookie", "steamLoginSecure="+v+"; Path=/")
			writeJSON(w, map[string]any{"result": 1})
		})
		mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Header.Get("Cookie"))
			if len(seen) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = fmt.Fprint(w, "ok")
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := NewSteamAuthClient("1", "tok", srv.Client(), srv.URL+"/jwt/finalizelogin")
		c.allowHosts = []string{"127.0.0.1"}
		authed, err := c.AuthenticatedClient(context.Background())
		Expect(err).NotTo(HaveOccurred())
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/data", nil)
		resp, err := authed.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(seen).To(HaveLen(2))
		Expect(seen[1]).To(ContainSubstring("new-cookie-value"))
		Expect(seen[1]).NotTo(ContainSubstring("old-cookie-value"))
	})
})

// stubCursors is an in-memory CursorRepository for cursor tests.
type stubCursors struct {
	m    map[string]repository.SyncCursor
	sets []repository.SyncCursor
}

func (s *stubCursors) Get(_ context.Context, scope string) (repository.SyncCursor, error) {
	if s.m == nil {
		return repository.SyncCursor{}, repository.ErrNotFound
	}
	c, ok := s.m[scope]
	if !ok {
		return repository.SyncCursor{}, repository.ErrNotFound
	}
	return c, nil
}
func (s *stubCursors) Set(_ context.Context, c repository.SyncCursor) error {
	if s.m == nil {
		s.m = map[string]repository.SyncCursor{}
	}
	s.m[c.Scope] = c
	s.sets = append(s.sets, c)
	return nil
}
func (s *stubCursors) Delete(_ context.Context, scope string) error {
	delete(s.m, scope)
	return nil
}

// stubCurrentCache is an in-memory CurrentPriceCache for cache tests.
type stubCurrentCache struct {
	m map[string]float64
}

func (s *stubCurrentCache) key(asset, currency string) string {
	return appprices.CurrentKey(asset, currency)
}
func (s *stubCurrentCache) Get(_ context.Context, key string) (float64, bool, error) {
	if s.m == nil {
		return 0, false, nil
	}
	v, ok := s.m[key]
	return v, ok, nil
}
func (s *stubCurrentCache) Set(_ context.Context, key string, price float64, _ time.Duration) error {
	if s.m == nil {
		s.m = map[string]float64{}
	}
	s.m[key] = price
	return nil
}
func (s *stubCurrentCache) GetRaw(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *stubCurrentCache) SetRaw(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (s *stubCurrentCache) Delete(_ context.Context, _ string) error { return nil }

var _ = Describe("Review follow-ups", func() {
	It("refuses auth redirects outside the allowlist without following", func() {
		var externalHits int
		external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			externalHits++
		}))
		defer external.Close()
		// Same machine, different hostname: the allowlist matches by
		// hostname, so this hop must be refused before any dial.
		externalHost := strings.Replace(external.URL, "127.0.0.1", "localhost", 1)
		allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 307 replays the refresh-token form body: the dangerous hop.
			http.Redirect(w, r, externalHost+"/steal", http.StatusTemporaryRedirect)
		}))
		defer allowed.Close()
		au, _ := url.Parse(allowed.URL)
		// Production supplies a non-nil *http.Client with no redirect
		// policy: the guard must live on the inner call, not the
		// constructor branch.
		c := NewSteamAuthClient("1", "tok", &http.Client{Timeout: 5 * time.Second}, "")
		c.allowHosts = []string{au.Hostname()}
		_, _, err := c.postFormRaw(context.Background(), allowed.URL+"/jwt/finalizelogin", map[string]string{"nonce": "tok"}, nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("redirect outside allowlist"))
		Expect(externalHits).To(Equal(0))
	})

	It("still follows redirects inside the allowlist", func() {
		allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/final" {
				writeJSON(w, map[string]any{"ok": true})
				return
			}
			http.Redirect(w, r, "/final", http.StatusFound)
		}))
		defer allowed.Close()
		au, _ := url.Parse(allowed.URL)
		c := NewSteamAuthClient("1", "tok", &http.Client{}, "")
		c.allowHosts = []string{au.Hostname()}
		var out struct {
			OK bool `json:"ok"`
		}
		_, _, err := c.postFormRaw(context.Background(), allowed.URL+"/start", map[string]string{"a": "b"}, nil, &out)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.OK).To(BeTrue())
	})

	It("keeps existing lots on partial snapshots", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		store := &stubSteamStore{lots: []repository.SteamLotRow{{ID: "lot:Case", MarketHashName: "Case", Quantity: 2}}}
		c.SetSteamStore(store)
		items := []InventoryItem{
			{AssetID: "a1", ClassID: "7", InstanceID: "0", Amount: 1, MarketHashName: "Case"},
		}
		resolved := []Resolution{{
			Asset: items[0], Type: domainsteam.AcquisitionUnresolved,
			MatchMethod: domainsteam.MatchMethodUnresolved, MatchConfidence: domainsteam.MatchUnresolved,
		}}
		now := time.Now().UTC()
		Expect(c.persistProvenance(context.Background(), items, false, provenanceOutcomes{}, now, nil, nil, nil, resolved, nil)).To(Succeed())
		Expect(store.lots).To(HaveLen(1))
		Expect(store.lots[0].ID).To(Equal("lot:Case"))
	})

	It("rebuilds lots on complete snapshots", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		store := &stubSteamStore{}
		c.SetSteamStore(store)
		items := []InventoryItem{
			{AssetID: "a1", ClassID: "7", InstanceID: "0", Amount: 1, MarketHashName: "Case"},
			{AssetID: "a2", ClassID: "7", InstanceID: "0", Amount: 1, MarketHashName: "Case"},
		}
		resolved := []Resolution{
			{Asset: items[0], Type: domainsteam.AcquisitionUnresolved, MatchConfidence: domainsteam.MatchUnresolved},
			{Asset: items[1], Type: domainsteam.AcquisitionUnresolved, MatchConfidence: domainsteam.MatchUnresolved},
		}
		Expect(c.persistProvenance(context.Background(), items, true, provenanceOutcomes{
			History: provenanceOutcome{Complete: true},
			Market:  provenanceOutcome{Complete: true},
			Trades:  provenanceOutcome{Complete: true},
		}, time.Now().UTC(), nil, nil, nil, resolved, nil)).To(Succeed())
		Expect(store.lots).To(HaveLen(1))
		Expect(store.lots[0].MarketHashName).To(Equal("Case"))
		Expect(c.pendingCursors).To(HaveLen(4))
	})

	It("reuses stored acquisitions for unresolved resolutions", func() {
		at := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
		basis := 23.41
		resolved := []Resolution{{
			Asset:       InventoryItem{AssetID: "a1", ClassID: "1", MarketHashName: "AK"},
			Type:        domainsteam.AcquisitionUnresolved,
			MatchMethod: domainsteam.MatchMethodUnresolved, MatchConfidence: domainsteam.MatchUnresolved,
		}}
		applyStoredAcquisitions(resolved, []repository.SteamAcquisitionRow{{
			AssetID: "a1", MarketHashName: "AK",
			AcquiredAt: &at, Type: string(domainsteam.AcquisitionMarketBuy), Reference: "market:buy:7",
			CostBasis: &basis, CostCurrency: "USD",
			MatchMethod: string(domainsteam.MatchHistoryMarket), MatchConfidence: string(domainsteam.MatchHigh),
		}})
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchHigh))
		Expect(resolved[0].Type).To(Equal(domainsteam.AcquisitionMarketBuy))
		Expect(resolved[0].AcquiredAt).To(Equal(&at))
		Expect(*resolved[0].CostBasis).To(BeNumerically("~", 23.41, 1e-9))
	})

	It("never overwrites fresh matches with stored ones", func() {
		resolved := []Resolution{{
			Asset:       InventoryItem{AssetID: "a1"},
			Type:        domainsteam.AcquisitionMarketBuy,
			MatchMethod: domainsteam.MatchHistoryMarket, MatchConfidence: domainsteam.MatchHigh,
		}}
		applyStoredAcquisitions(resolved, []repository.SteamAcquisitionRow{{
			AssetID: "a1",
			Type:    string(domainsteam.AcquisitionDrop), MatchConfidence: string(domainsteam.MatchLow),
		}})
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchHigh))
		Expect(resolved[0].Type).To(Equal(domainsteam.AcquisitionMarketBuy))
	})

	It("keeps stored exact evidence over weak fresh matches", func() {
		at := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
		basis := 23.41
		resolved := []Resolution{{
			Asset: InventoryItem{AssetID: "a1", ClassID: "1", MarketHashName: "AK"},
			Type:  domainsteam.AcquisitionTrade, Reference: "weak",
			MatchMethod: domainsteam.MatchAttributes, MatchConfidence: domainsteam.MatchMedium,
		}}
		applyStoredAcquisitions(resolved, []repository.SteamAcquisitionRow{{
			AssetID: "a1", MarketHashName: "AK",
			AcquiredAt: &at, Type: string(domainsteam.AcquisitionMarketBuy), Reference: "market:buy:7",
			CostBasis: &basis, CostCurrency: "USD",
			MatchMethod: string(domainsteam.MatchHistoryMarket), MatchConfidence: string(domainsteam.MatchHigh),
		}})
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchHigh))
		Expect(resolved[0].Type).To(Equal(domainsteam.AcquisitionMarketBuy))
		Expect(resolved[0].Reference).To(Equal("market:buy:7"))
		Expect(*resolved[0].CostBasis).To(BeNumerically("~", 23.41, 1e-9))
	})

	It("prefers richer stored evidence at equal confidence", func() {
		at := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
		basis := 23.41
		fresh := func() []Resolution {
			return []Resolution{{
				Asset:       InventoryItem{AssetID: "a1", ClassID: "1", MarketHashName: "AK"},
				Type:        domainsteam.AcquisitionMarketBuy,
				MatchMethod: domainsteam.MatchHistoryMarket, MatchConfidence: domainsteam.MatchHigh,
			}}
		}
		stored := func() []repository.SteamAcquisitionRow {
			return []repository.SteamAcquisitionRow{{
				AssetID: "a1", MarketHashName: "AK",
				AcquiredAt: &at, Type: string(domainsteam.AcquisitionMarketBuy), Reference: "market:buy:7",
				CostBasis: &basis, CostCurrency: "USD",
				MatchMethod: string(domainsteam.MatchHistoryMarket), MatchConfidence: string(domainsteam.MatchHigh),
			}}
		}
		// A bare fresh match must not displace richer stored evidence.
		r := fresh()
		applyStoredAcquisitions(r, stored())
		Expect(r[0].Reference).To(Equal("market:buy:7"))
		Expect(*r[0].CostBasis).To(BeNumerically("~", 23.41, 1e-9))
		// A richer fresh match keeps winning.
		r = fresh()
		r[0].AcquiredAt = &at
		r[0].CostBasis = &basis
		r[0].Reference = "market:buy:9"
		applyStoredAcquisitions(r, stored())
		Expect(r[0].Reference).To(Equal("market:buy:9"))
	})

	It("refuses exact assignment from non-acquisition events", func() {
		at := time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)
		evs := []historyEvent{{
			ExternalID: "e1", Timestamp: at, Kind: EventMarketList,
			MarketHashName: "AK", AssetID: "new", ClassID: "1",
		}}
		resolved, unmatched := Resolve(
			[]InventoryItem{{AssetID: "new", ClassID: "1", InstanceID: "0", Amount: 1, MarketHashName: "AK"}},
			map[string]bool{}, evs, nil, nil, at)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchUnresolved))
		Expect(unmatched).To(HaveLen(1))
	})

	It("expires stale attribute evidence after three days", func() {
		now := time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)
		mk := func(id string, at time.Time) historyEvent {
			return historyEvent{ExternalID: id, Timestamp: at, Kind: EventReceived,
				ClassID: "1", InstanceID: "0", MarketHashName: "AK", Quantity: 1}
		}
		it := InventoryItem{AssetID: "x", ClassID: "1", InstanceID: "0", Amount: 1, MarketHashName: "AK"}
		stale, _ := Resolve([]InventoryItem{it}, nil, []historyEvent{mk("old", now.Add(-100*time.Hour))}, nil, nil, now)
		Expect(stale[0].MatchConfidence).To(Equal(domainsteam.MatchUnresolved))
		fresh, _ := Resolve([]InventoryItem{it}, nil, []historyEvent{mk("new", now.Add(-time.Hour))}, nil, nil, now)
		Expect(fresh[0].MatchConfidence).To(Equal(domainsteam.MatchMedium))
	})

	It("loads only complete cursors for incremental provenance", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		Expect(c.loadProvenanceCursor(context.Background(), steamCursorScope(cursorScopeHistory, "1"))).To(Equal(provenanceCursor{}))
		mark, err := json.Marshal(provenanceCursor{Watermark: time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)})
		Expect(err).NotTo(HaveOccurred())
		cur := &stubCursors{m: map[string]repository.SyncCursor{
			steamCursorScope(cursorScopeHistory, "1"): {Scope: steamCursorScope(cursorScopeHistory, "1"), Position: string(mark), Complete: true},
			steamCursorScope(cursorScopeTrades, "1"):  {Scope: steamCursorScope(cursorScopeTrades, "1"), Position: "not-json-or-time", Complete: false},
		}}
		c.ConfigureCursors(cur)
		got := c.loadProvenanceCursor(context.Background(), steamCursorScope(cursorScopeHistory, "1"))
		Expect(got.Watermark).To(Equal(time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)))
		Expect(got.Resume).To(BeEmpty())
		Expect(c.loadProvenanceCursor(context.Background(), steamCursorScope(cursorScopeTrades, "1"))).To(Equal(provenanceCursor{}))
		Expect(c.loadProvenanceCursor(context.Background(), steamCursorScope(cursorScopeMarket, "1"))).To(Equal(provenanceCursor{}))
	})

	It("isolates cursor state between Steam accounts", func() {
		t0 := time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)
		mark, err := json.Marshal(provenanceCursor{Watermark: t0})
		Expect(err).NotTo(HaveOccurred())
		cur := &stubCursors{m: map[string]repository.SyncCursor{
			steamCursorScope(cursorScopeHistory, "A"): {
				Scope: steamCursorScope(cursorScopeHistory, "A"), Position: string(mark), Complete: true,
			},
			// Legacy unscoped state from previous releases: a newer
			// watermark that must never be adopted by any account.
			cursorScopeHistory: {
				Scope: cursorScopeHistory, Position: "2099-01-01T00:00:00Z", Complete: true,
			},
		}}
		a := New(ClientConfig{SteamID: "A"}, nil)
		a.ConfigureCursors(cur)
		b := New(ClientConfig{SteamID: "B"}, nil)
		b.ConfigureCursors(cur)
		Expect(a.loadProvenanceCursor(context.Background(), steamCursorScope(cursorScopeHistory, "A")).Watermark).To(Equal(t0))
		Expect(b.loadProvenanceCursor(context.Background(), steamCursorScope(cursorScopeHistory, "B"))).To(Equal(provenanceCursor{}))
	})

	It("issues unique snapshot IDs within one second", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		store := &stubSteamStore{}
		c.SetSteamStore(store)
		base := time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)
		Expect(c.persistProvenance(context.Background(), nil, true, provenanceOutcomes{}, base, nil, nil, nil, nil, nil)).To(Succeed())
		Expect(c.persistProvenance(context.Background(), nil, true, provenanceOutcomes{}, base.Add(100*time.Nanosecond), nil, nil, nil, nil, nil)).To(Succeed())
		Expect(store.snapshots).To(HaveLen(2))
		Expect(store.snapshots[0].ID).NotTo(Equal(store.snapshots[1].ID))
	})

	It("keeps events unmatched when acquisitions are not durable", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		store := &stubSteamStore{failAcq: true}
		c.SetSteamStore(store)
		now := time.Now().UTC()
		ev := historyEvent{ExternalID: "e1", Timestamp: now.Add(-time.Hour), Kind: EventMarketBuy,
			MarketHashName: "AK", Quantity: 1}
		resolved := []Resolution{{
			Asset: InventoryItem{AssetID: "a1", MarketHashName: "AK"},
			Type:  domainsteam.AcquisitionMarketBuy, Reference: "market:buy:7",
			MatchMethod: domainsteam.MatchSnapshotEvent, MatchConfidence: domainsteam.MatchExact,
		}}
		Expect(c.persistProvenance(context.Background(), nil, true, provenanceOutcomes{},
			now, []historyEvent{ev}, nil, nil, resolved, nil)).To(Succeed())
		// No durable acquisition: the consumed event stays available
		// for the next sync instead of vanishing from UnmatchedEvents.
		Expect(store.matched).To(BeEmpty())
		Expect(store.acqs).To(BeEmpty())
	})

	It("decodes legacy RFC3339 cursors as watermarks", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		cur := &stubCursors{m: map[string]repository.SyncCursor{
			steamCursorScope(cursorScopeHistory, "1"): {Scope: steamCursorScope(cursorScopeHistory, "1"), Position: "2025-05-08T12:00:00Z", Complete: true},
		}}
		c.ConfigureCursors(cur)
		got := c.loadProvenanceCursor(context.Background(), steamCursorScope(cursorScopeHistory, "1"))
		Expect(got.Watermark).To(Equal(time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)))
	})

	It("merges outcomes without advancing on empty runs", func() {
		prev := provenanceCursor{Watermark: time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)}
		// Empty complete run: watermark holds, resume clears.
		next := mergeProvenanceCursor(prev, provenanceOutcome{Complete: true})
		Expect(next.Watermark).To(Equal(prev.Watermark))
		Expect(next.Resume).To(BeEmpty())
		// Capped run: watermark advances to observed data and the
		// provider offset persists for continuation.
		next = mergeProvenanceCursor(prev, provenanceOutcome{
			Newest: time.Date(2025, 5, 9, 12, 0, 0, 0, time.UTC),
			Resume: json.RawMessage(`{"s":"x"}`),
		})
		Expect(next.Watermark).To(Equal(time.Date(2025, 5, 9, 12, 0, 0, 0, time.UTC)))
		Expect(string(next.Resume)).To(Equal(`{"s":"x"}`))
		// Older observations never move the watermark backwards.
		next = mergeProvenanceCursor(next, provenanceOutcome{
			Complete: true, Newest: time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC),
		})
		Expect(next.Watermark).To(Equal(time.Date(2025, 5, 9, 12, 0, 0, 0, time.UTC)))
		Expect(next.Resume).To(BeEmpty())
	})

	It("round-trips provider resume offsets", func() {
		Expect(decodeHistoryResume(nil)).To(BeNil())
		Expect(decodeHistoryResume(json.RawMessage(`oops`))).To(BeNil())
		Expect(decodeHistoryResume(json.RawMessage(`{}`))).To(BeNil())
		hc := decodeHistoryResume(mustMarshalResume(&historyCursor{S: "abc"}))
		Expect(hc).NotTo(BeNil())
		Expect(hc.S).To(Equal("abc"))
		Expect(mustMarshalResume(nil)).To(BeEmpty())
		Expect(mustMarshalResume((*historyCursor)(nil))).To(BeEmpty())
		Expect(mustMarshalResume((*tradeResume)(nil))).To(BeEmpty())
		tr := decodeTradeResume(mustMarshalResume(&tradeResume{AfterID: "t1"}))
		Expect(tr).NotTo(BeNil())
		Expect(tr.AfterID).To(Equal("t1"))
		Expect(decodeTradeResume(nil)).To(BeNil())
		Expect(decodeTradeResume(json.RawMessage(`oops`))).To(BeNil())
		off, ok := decodeMarketResume(mustMarshalResume(marketResume{Offset: 500}))
		Expect(ok).To(BeTrue())
		Expect(off).To(Equal(500))
		_, ok = decodeMarketResume(nil)
		Expect(ok).To(BeFalse())
		_, ok = decodeMarketResume(json.RawMessage(`{"offset":-1}`))
		Expect(ok).To(BeFalse())
		Expect(newestTradeTime(nil).IsZero()).To(BeTrue())
		Expect(newestEventTime(nil).IsZero()).To(BeTrue())
	})

	It("preserves prior state on incomplete walks without resume", func() {
		prev := provenanceCursor{
			Watermark: time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC),
			Resume:    json.RawMessage(`{"s":"x"}`),
		}
		// An error/stall outcome observes newer partial rows but offers
		// no continuation: advancing would strand the unfetched pages
		// between the watermarks, so the prior state stands.
		next := mergeProvenanceCursor(prev, provenanceOutcome{
			Newest: time.Date(2025, 5, 9, 12, 0, 0, 0, time.UTC),
		})
		Expect(next).To(Equal(prev))
	})

	It("holds cursors when source writes fail", func() {
		t0 := time.Date(2025, 5, 8, 12, 0, 0, 0, time.UTC)
		mark, err := json.Marshal(provenanceCursor{Watermark: t0})
		Expect(err).NotTo(HaveOccurred())
		cur := &stubCursors{m: map[string]repository.SyncCursor{
			steamCursorScope(cursorScopeHistory, "1"): {Scope: steamCursorScope(cursorScopeHistory, "1"), Position: string(mark), Complete: true},
		}}
		c := New(ClientConfig{SteamID: "1"}, nil)
		c.SetSteamStore(&stubSteamStore{failEvents: true})
		c.ConfigureCursors(cur)
		now := time.Now().UTC()
		ev := historyEvent{ExternalID: "e1", Timestamp: t0.Add(time.Hour), Kind: EventMarketBuy,
			MarketHashName: "AK", Quantity: 1}
		Expect(c.persistProvenance(context.Background(), nil, true, provenanceOutcomes{
			History: provenanceOutcome{Complete: true, Newest: t0.Add(time.Hour)},
		}, now, []historyEvent{ev}, nil, nil, nil, nil)).To(Succeed())
		var staged *repository.SyncCursor
		for i, pc := range c.pendingCursors {
			if pc.Scope == steamCursorScope(cursorScopeHistory, "1") {
				staged = &c.pendingCursors[i]
			}
		}
		Expect(staged).NotTo(BeNil())
		// The rows never reached the database: prior state preserved,
		// marked incomplete so the next sync re-imports them.
		Expect(staged.Complete).To(BeFalse())
		Expect(staged.Position).To(Equal(string(mark)))
	})

	It("persists per-source cursors through SnapshotCommitted", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		cur := &stubCursors{}
		c.ConfigureCursors(cur)
		c.pendingCursors = []repository.SyncCursor{
			{Scope: cursorScopeInventory, Position: "2025-05-08T12:00:00Z", Complete: true},
			{Scope: cursorScopeHistory, Position: "2025-05-08T12:00:00Z", Complete: false},
		}
		c.pendingDirty = true
		c.SnapshotCommitted()
		Expect(cur.sets).To(HaveLen(2))
		Expect(cur.m[cursorScopeHistory].Complete).To(BeFalse())
		c.SnapshotCommitted()
		Expect(cur.sets).To(HaveLen(2))
	})

	It("resolves market names from the response assets map", func() {
		toRaw := func(m map[string]any) map[string]json.RawMessage {
			raw, err := json.Marshal(m)
			Expect(err).NotTo(HaveOccurred())
			var out map[string]json.RawMessage
			Expect(json.Unmarshal(raw, &out)).To(Succeed())
			return out
		}
		names := mergeMarketNames(
			map[string]string{"1_0": "From History"},
			parseMarketAssets(toRaw(map[string]any{
				"730": map[string]any{
					"2": map[string]any{
						"4242": map[string]any{"classid": "9", "instanceid": "0", "market_hash_name": "From Assets"},
					},
				},
				"1_0": map[string]any{"classid": "1", "instanceid": "0", "market_hash_name": "Shadowed"},
			})),
		)
		// History descriptions win; the assets map fills the rest.
		Expect(names["1_0"]).To(Equal("From History"))
		Expect(names["9_0"]).To(Equal("From Assets"))
	})

	It("measures market completeness against total_count, not parsed rows", func() {
		var total int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := r.URL.Query().Get("start")
			purchases := map[string]any{}
			if start == "0" {
				purchases["7"] = map[string]any{
					"listingid": "7", "purchaseid": "7",
					"time_sold": 1746720120, "steamid_purchaser": "me",
					"failed": 0, "needs_rollback": 0,
					"asset":       map[string]any{"classid": "1", "instanceid": "0", "amount": "1"},
					"paid_amount": 2341, "currencyid": "2001",
				}
			}
			writeJSON(w, map[string]any{
				"success": true, "total_count": total, "start": 0,
				"purchases": purchases, "listings": map[string]any{},
			})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL

		total = 501
		txs, complete, _, err := c.fetchMarketHistory(context.Background(), "me", map[string]string{"1_0": "AK"}, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(txs).To(HaveLen(1))
		// One parsed row is not one covered page: 501 total needs the
		// second page, which comes back empty.
		Expect(complete).To(BeFalse())

		total = 1
		txs, complete, _, err = c.fetchMarketHistory(context.Background(), "me", map[string]string{"1_0": "AK"}, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(txs).To(HaveLen(1))
		Expect(complete).To(BeTrue())
	})

	It("rejects unsuccessful market envelopes as incomplete", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": false, "total_count": 3})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		_, complete, _, err := c.fetchMarketHistory(context.Background(), "me", map[string]string{}, 0)
		Expect(err).To(HaveOccurred())
		Expect(complete).To(BeFalse())
	})

	It("pages market history from a start offset with resume", func() {
		var starts []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			starts = append(starts, r.URL.Query().Get("start"))
			writeJSON(w, map[string]any{
				"success": true, "total_count": 1200, "start": 0,
				"purchases": map[string]any{
					"7": map[string]any{
						"listingid": "7", "purchaseid": "7",
						"time_sold": 1746720120, "steamid_purchaser": "me",
						"failed": 0, "needs_rollback": 0,
						"asset":       map[string]any{"classid": "1", "instanceid": "0", "amount": "1"},
						"paid_amount": 2341, "currencyid": "2001",
					},
				},
				"listings": map[string]any{},
			})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		c.cfg.MaxPages = 1
		txs, complete, next, err := c.fetchMarketHistory(context.Background(), "me", map[string]string{"1_0": "AK"}, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(next).To(Equal(500))
		Expect(txs).To(HaveLen(1))
		txs, complete, next, err = c.fetchMarketHistory(context.Background(), "me", map[string]string{"1_0": "AK"}, next)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(next).To(Equal(1000))
		Expect(txs).To(HaveLen(1))
		Expect(starts).To(Equal([]string{"0", "500"}))
	})

	It("retries truncated market pages at the same offset", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"success": true, "total_count": 1200, "start": 0,
				"purchases": map[string]any{}, "listings": map[string]any{},
			})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		_, complete, next, err := c.fetchMarketHistory(context.Background(), "me", map[string]string{}, 500)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(next).To(Equal(500))
	})

	It("names purchases through the assets map without history descs", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"success": true, "total_count": 1, "start": 0,
				"purchases": map[string]any{
					"7": map[string]any{
						"listingid": "7", "purchaseid": "7",
						"time_sold": 1746720120, "steamid_purchaser": "me",
						"failed": 0, "needs_rollback": 0,
						"asset":       map[string]any{"classid": "9", "instanceid": "0", "amount": "1"},
						"paid_amount": 100, "currencyid": "2001",
					},
				},
				"listings": map[string]any{},
				"assets": map[string]any{
					"730": map[string]any{
						"2": map[string]any{
							"4242": map[string]any{"classid": "9", "instanceid": "0", "market_hash_name": "Mystery Skin"},
						},
					},
				},
			})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		txs, complete, _, err := c.fetchMarketHistory(context.Background(), "me", map[string]string{}, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(txs).To(HaveLen(1))
		Expect(txs[0].MarketHashName).To(Equal("Mystery Skin"))
	})

	It("does not spend breaker budget on permanent no-price answers", func() {
		var mode atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode.Load() == 0 {
				writeJSON(w, map[string]any{"success": false})
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		c.cfg.MaxRetries = 0
		_, _, _, ok, err := c.CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(c.priceFails.Load()).To(Equal(int64(0)))
		mode.Store(1)
		_, _, _, ok, err = c.CurrentPrice(context.Background(), "AK")
		Expect(err).To(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(c.priceFails.Load()).To(Equal(int64(1)))
	})

	It("falls back to recent stored quotes when live pricing fails", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{
						map[string]any{"assetid": "1", "classid": "1", "instanceid": "0", "amount": "1"},
						map[string]any{"assetid": "2", "classid": "2", "instanceid": "0", "amount": "1"},
						map[string]any{"assetid": "3", "classid": "3", "instanceid": "0", "amount": "1"},
					},
					"descriptions": []any{
						map[string]any{"classid": "1", "instanceid": "0", "market_hash_name": "Cheap Skin", "marketable": 1, "tradable": 1},
						map[string]any{"classid": "2", "instanceid": "0", "market_hash_name": "Rich Skin", "marketable": 1, "tradable": 1},
						map[string]any{"classid": "3", "instanceid": "0", "market_hash_name": "Ancient Skin", "marketable": 1, "tradable": 1},
					},
					"success": 1,
				})
				return
			}
			if strings.HasPrefix(r.URL.Path, "/market/priceoverview/") {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			writeEmptyProvenance(w, r)
		}))
		defer srv.Close()
		now := time.Now().UTC()
		mkClient := func() *Client {
			c := testClient(srv)
			c.cfg.CommunityBase = srv.URL
			c.cfg.StoreBase = srv.URL
			c.cfg.Session = "sessionid=x"
			c.cfg.MaxRetries = 0
			c.cfg.MinItemValueUSD = 10
			store := newStubPriceStore()
			Expect(store.Upsert(context.Background(), []repository.HistoricalPrice{
				{Asset: "steam:730:CHEAP SKIN", Timestamp: now.Add(-time.Hour), Currency: "STEAM_1", Price: 5, Source: "steam_quote"},
				{Asset: "steam:730:RICH SKIN", Timestamp: now.Add(-time.Hour), Currency: "STEAM_1", Price: 88, Source: "steam_quote"},
				{Asset: "steam:730:ANCIENT SKIN", Timestamp: now.Add(-48 * time.Hour), Currency: "STEAM_1", Price: 88, Source: "steam_quote"},
			})).To(Succeed())
			c.SetPriceHistoryStore(store)
			c.SetSteamStore(&stubSteamStore{})
			return c
		}
		snap, err := mkClient().Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		got := map[string]float64{}
		for _, p := range snap.Holdings[0].Positions {
			got[p.Symbol.Symbol] = p.Price
		}
		// Stale-but-recent quotes stabilize the filter: cheap stays
		// out, rich shows its stored price instead of zero.
		Expect(got).NotTo(HaveKey("Cheap Skin"))
		Expect(got["Rich Skin"]).To(BeNumerically("~", 88, 1e-9))
		// Ancient quotes stay unvalued rather than misleading.
		Expect(got["Ancient Skin"]).To(Equal(0.0))
	})

	It("still filters from stale quotes when the breaker trips", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{
						map[string]any{"assetid": "1", "classid": "1", "instanceid": "0", "amount": "1"},
					},
					"descriptions": []any{
						map[string]any{"classid": "1", "instanceid": "0", "market_hash_name": "Cheap Skin", "marketable": 1, "tradable": 1},
					},
					"success": 1,
				})
				return
			}
			writeEmptyProvenance(w, r)
		}))
		defer srv.Close()
		now := time.Now().UTC()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		c.cfg.StoreBase = srv.URL
		c.cfg.Session = "sessionid=x"
		c.cfg.MinItemValueUSD = 10
		store := newStubPriceStore()
		Expect(store.Upsert(context.Background(), []repository.HistoricalPrice{
			{Asset: "steam:730:CHEAP SKIN", Timestamp: now.Add(-time.Hour), Currency: "STEAM_1", Price: 5, Source: "steam_quote"},
		})).To(Succeed())
		c.SetPriceHistoryStore(store)
		c.SetSteamStore(&stubSteamStore{})
		c.priceFails.Store(priceFailBreaker)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(BeEmpty())
	})

	It("reports the live observation time with current quotes", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": true, "median_price": "$31.28"})
		}))
		defer srv.Close()
		before := time.Now().UTC()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		store := newStubPriceStore()
		c.SetPriceHistoryStore(store)
		v, kind, observed, ok, err := c.CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(kind).To(Equal("steam_median"))
		Expect(v).To(BeNumerically("~", 31.28, 1e-9))
		Expect(observed.After(before.Add(-time.Minute))).To(BeTrue())
		Expect(store.rows).To(HaveLen(1))
		Expect(store.rows[0].Source).To(Equal(priceSourceQuote))
		Expect(store.rows[0].Timestamp).To(Equal(observed))
	})

	It("maps empty price responses to not-found, not upstream errors", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": false})
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		c.cfg.MaxRetries = 0
		_, _, _, ok, err := c.CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("surfaces throttled price responses as errors", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		c.cfg.MaxRetries = 0
		_, _, _, ok, err := c.CurrentPrice(context.Background(), "AK")
		Expect(err).To(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("isolates shared-cache entries between TTL roles", func() {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			writeJSON(w, map[string]any{"success": true, "median_price": "$31.28"})
		}))
		defer srv.Close()
		shared := &stubCurrentCache{}
		mk := func(ns string) *Client {
			c := testClient(srv)
			c.cfg.CommunityBase = srv.URL
			c.cfg.CacheNamespace = ns
			c.SetPriceService(appprices.NewService(nil, shared))
			return c
		}
		syncClient, publicClient := mk(""), mk("public")
		_, _, _, ok, err := publicClient.CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(calls).To(Equal(1))
		// The sync role must not reuse the public role's 24h entry.
		_, _, _, ok, err = syncClient.CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(calls).To(Equal(2))
	})

	It("never re-stamps cache-served quotes with fresh timestamps", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		c := testClient(srv)
		c.cfg.CommunityBase = srv.URL
		store := newStubPriceStore()
		c.SetPriceHistoryStore(store)
		shared := &stubCurrentCache{}
		c.SetPriceService(appprices.NewService(nil, shared))
		asset, curr := priceKey("AK", c.cfg.Currency)
		shared.m = map[string]float64{appprices.CurrentKey(asset, curr): 31.28}
		v, _, observed, ok, err := c.CurrentPrice(context.Background(), "AK")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(v).To(Equal(31.28))
		Expect(store.puts).To(Equal(0))
		// No observation exists for cache hits: zero lets the caller
		// fall back to the real stored timestamp instead of serve time.
		Expect(observed.IsZero()).To(BeTrue())
	})
})
