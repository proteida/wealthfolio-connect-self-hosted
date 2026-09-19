package steam

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	domainsteam "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/steam"
)

func owned(asset, class, name string, amount int) InventoryItem {
	return InventoryItem{AssetID: asset, ClassID: class, InstanceID: "0", Amount: amount, MarketHashName: name, Marketable: true, Tradable: true}
}

var _ = Describe("Resolver", func() {
	at := time.Date(2025, 5, 8, 13, 22, 0, 0, time.UTC)

	It("matches exact on absent-before plus event", func() {
		before := map[string]bool{"old": true}
		evs := []historyEvent{{ExternalID: "e1", Timestamp: at, Kind: EventMarketBuy, MarketHashName: "AK", AssetID: "new", ClassID: "1"}}
		resolved, unmatched := resolve([]InventoryItem{owned("new", "1", "AK", 1)}, before, evs, nil, nil, at)
		Expect(resolved).To(HaveLen(1))
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchExact))
		Expect(resolved[0].MatchMethod).To(Equal(domainsteam.MatchSnapshotEvent))
		Expect(resolved[0].AcquiredAt).To(Equal(&at))
		Expect(unmatched).To(BeEmpty())
	})

	It("matches exact on trade new_assetid echo", func() {
		trades := []domainsteam.TradeRecord{{
			TradeID: "t1", Timestamp: at,
			Received: []domainsteam.TradeAsset{{AssetID: "99", NewAssetID: "cur", Quantity: 1}},
		}}
		resolved, _ := resolve([]InventoryItem{owned("cur", "2", "AWP", 1)}, nil, nil, nil, trades, at)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchExact))
		Expect(resolved[0].Type).To(Equal(domainsteam.AcquisitionTrade))
		Expect(resolved[0].Reference).To(Equal("t1"))
	})

	It("matches high on history plus market agreement", func() {
		evs := []historyEvent{{ExternalID: "e1", Timestamp: at, Kind: EventMarketBuy, MarketHashName: "AK", Quantity: 1}}
		market := map[string]domainsteam.MarketTransaction{
			"market:9": {ExternalID: "market:9", Type: "buy", Timestamp: at.Add(time.Hour), MarketHashName: "AK", Quantity: 1, Gross: 23.41, Currency: "USD"},
		}
		resolved, unmatched := resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, evs, market, nil, at)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchHigh))
		Expect(resolved[0].MatchMethod).To(Equal(domainsteam.MatchHistoryMarket))
		Expect(*resolved[0].CostBasis).To(BeNumerically("~", 23.41, 1e-9))
		Expect(resolved[0].CostCurrency).To(Equal("USD"))
		Expect(unmatched).To(BeEmpty())
	})

	It("matches medium on attribute proximity", func() {
		evs := []historyEvent{{ExternalID: "e1", Timestamp: at, Kind: EventReceived, ClassID: "1", InstanceID: "0", MarketHashName: "AK", Quantity: 1}}
		resolved, _ := resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, evs, nil, nil, at)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchMedium))
		Expect(resolved[0].Type).To(Equal(domainsteam.AcquisitionTrade))
	})

	It("leaves unmatched events for later runs", func() {
		evs := []historyEvent{
			{ExternalID: "e1", Timestamp: at, Kind: EventReceived, ClassID: "9", MarketHashName: "OTHER", Quantity: 1},
			{ExternalID: "e2", Timestamp: at, Kind: EventMarketBuy, MarketHashName: "AK", Quantity: 1},
		}
		market := map[string]domainsteam.MarketTransaction{
			"market:9": {ExternalID: "market:9", Type: "buy", Timestamp: at, MarketHashName: "AK", Quantity: 1, Gross: 5, Currency: "USD"},
		}
		resolved, unmatched := resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, evs, market, nil, at)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchHigh))
		Expect(unmatched).To(HaveLen(1))
		Expect(unmatched[0].ExternalID).To(Equal("e1"))
	})

	It("never pretends an inference is exact", func() {
		resolved, unmatched := resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, nil, nil, nil, at)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchUnresolved))
		Expect(unmatched).To(BeEmpty())
	})

	It("groups indistinguishable duplicates into lots", func() {
		items := []InventoryItem{
			owned("a1", "7", "Dreams & Nightmares Case", 1),
			owned("a2", "7", "Dreams & Nightmares Case", 1),
			owned("solo", "1", "AK", 1),
		}
		resolved, _ := resolve(items, nil, nil, nil, nil, at)
		lots := buildLots(resolved)
		Expect(lots).To(HaveLen(1))
		Expect(lots[0].MarketHashName).To(Equal("Dreams & Nightmares Case"))
		Expect(lots[0].Quantity).To(Equal(2))
		Expect(lots[0].Source).To(Equal(domainsteam.AcquisitionUnresolved))
	})
})

// stubSteamStore is an in-memory SteamAssetRepository for Fetch tests.
type stubSteamStore struct {
	snapshots  []repository.SteamInventorySnapshot
	events     []repository.SteamEventRow
	market     []repository.SteamMarketRow
	trades     []repository.SteamTradeRow
	lots       []repository.SteamLotRow
	acqs       []repository.SteamAcquisitionRow
	acqRows    []repository.SteamAcquisitionRow
	matched    []string
	failSnap   bool
	failLatest bool
	failEvents bool
	failMarket bool
	failAcq    bool
	lotsCalls  int
}

func (s *stubSteamStore) LatestSnapshot(_ context.Context, _ string) (repository.SteamInventorySnapshot, error) {
	if s.failLatest {
		return repository.SteamInventorySnapshot{}, errStubStore
	}
	if len(s.snapshots) == 0 {
		return repository.SteamInventorySnapshot{}, repository.ErrNotFound
	}
	return s.snapshots[len(s.snapshots)-1], nil
}
func (s *stubSteamStore) SaveSnapshot(_ context.Context, snap repository.SteamInventorySnapshot) error {
	if s.failSnap {
		return errStubStore
	}
	s.snapshots = append(s.snapshots, snap)
	return nil
}
func (s *stubSteamStore) RecordEvents(_ context.Context, _ string, evs []repository.SteamEventRow) error {
	if s.failEvents {
		return errStubStore
	}
	s.events = append(s.events, evs...)
	return nil
}
func (s *stubSteamStore) UnmatchedEvents(_ context.Context, _ string) ([]repository.SteamEventRow, error) {
	return nil, nil
}
func (s *stubSteamStore) MarkEventsMatched(_ context.Context, _ string, ids []string) error {
	s.matched = append(s.matched, ids...)
	return nil
}
func (s *stubSteamStore) SaveMarketTransactions(_ context.Context, _ string, txs []repository.SteamMarketRow) error {
	if s.failMarket {
		return errStubStore
	}
	s.market = append(s.market, txs...)
	return nil
}
func (s *stubSteamStore) SaveTrades(_ context.Context, _ string, trs []repository.SteamTradeRow) error {
	s.trades = append(s.trades, trs...)
	return nil
}
func (s *stubSteamStore) SaveLots(_ context.Context, _ string, lots []repository.SteamLotRow) error {
	s.lotsCalls++
	s.lots = append(s.lots, lots...)
	return nil
}
func (s *stubSteamStore) SaveAcquisitions(_ context.Context, _ string, acqs []repository.SteamAcquisitionRow) error {
	if s.failAcq {
		return errStubStore
	}
	s.acqs = append(s.acqs, acqs...)
	return nil
}
func (s *stubSteamStore) CurrentAssets(_ context.Context, _ string) ([]repository.SteamAssetState, error) {
	return nil, nil
}
func (s *stubSteamStore) AcquisitionsForAssets(_ context.Context, _ string, _ []string) ([]repository.SteamAcquisitionRow, error) {
	return s.acqRows, nil
}

// writeEmptyProvenance serves valid but empty success envelopes per
// provenance path (history and market validate success before parsing).
func writeEmptyProvenance(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/my/inventoryhistory/":
		writeJSON(w, map[string]any{
			"success": true, "html": "", "descriptions": map[string]any{}, "apps": []any{},
		})
	case strings.HasPrefix(r.URL.Path, "/market/myhistory"):
		writeJSON(w, map[string]any{
			"success": true, "total_count": 0, "start": 0,
			"purchases": map[string]any{}, "listings": map[string]any{},
		})
	default:
		writeJSON(w, map[string]any{
			"response": map[string]any{"more": false, "trades": []any{}},
		})
	}
}

var errStubStore = errTest()

func errTest() error { return errorString("store down") }

type errorString string

func (e errorString) Error() string { return string(e) }

var _ = Describe("Steam Fetch", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	BeforeEach(func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	newClient := func(store *stubSteamStore) *Client {
		cfg := defaultClientConfig()
		cfg.SteamID = "76561199495663064"
		cfg.MinInterval = time.Millisecond // New() treats <=0 as unset
		cfg.CommunityBase = server.URL
		cfg.StoreBase = server.URL
		cfg.Session = "sessionid=test-session"
		cfg.APIKey = "test-key"
		c := New(cfg, server.Client())
		if store != nil {
			c.SetSteamStore(store)
		}
		return c
	}

	inventoryHandler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/inventory/"):
			writeJSON(w, map[string]any{
				"assets": []any{map[string]any{
					"assetid": "100", "classid": "1", "instanceid": "0", "amount": "1",
				}},
				"descriptions": []any{map[string]any{
					"classid": "1", "instanceid": "0",
					"market_hash_name": "AK-47 | Redline (Field-Tested)",
					"marketable":       1, "tradable": 1,
					"name": "AK-47", "type": "Rifle",
				}},
				"success": 1,
			})
		default:
			writeEmptyProvenance(w, r)
		}
	}

	It("builds positions and persists the snapshot", func() {
		handler = inventoryHandler
		store := &stubSteamStore{}
		snap, err := newClient(store).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Holdings[0].Partial).To(BeFalse())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("AK-47 | Redline (Field-Tested)"))
		// No provenance: no activities, unresolved acquisition recorded.
		Expect(snap.Activities["steam-76561199495663064"]).To(BeEmpty())
		Expect(store.snapshots).To(HaveLen(1))
		Expect(store.snapshots[0].Complete).To(BeTrue())
		Expect(store.acqs).To(HaveLen(1))
		Expect(store.acqs[0].MatchConfidence).To(Equal(string(domainsteam.MatchUnresolved)))
	})

	It("marks partial snapshots without erasing", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				writeJSON(w, map[string]any{
					"assets": []any{map[string]any{
						"assetid": "100", "classid": "1", "instanceid": "0", "amount": "1",
					}},
					"descriptions": []any{},
					"more_items":   true,
					"last_assetid": "",
					"success":      1,
				})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}
		store := &stubSteamStore{}
		c := newClient(store)
		c.cfg.MaxRetries = 0
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Partial).To(BeTrue())
		// Zero-price positions stay visible even when partial.
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(store.snapshots).To(HaveLen(1))
		Expect(store.snapshots[0].Complete).To(BeFalse())
	})

	It("fails when the snapshot cannot be stored", func() {
		handler = inventoryHandler
		store := &stubSteamStore{failSnap: true}
		_, err := newClient(store).Fetch(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("fails closed on a first complete empty inventory but records a probe", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{}, "descriptions": []any{}, "success": 1,
				})
				return
			}
			writeEmptyProvenance(w, r)
		}
		store := &stubSteamStore{
			snapshots: []repository.SteamInventorySnapshot{{
				ID: "prev", SteamID: "76561199495663064", TakenAt: time.Now().Add(-time.Hour),
				Complete: true,
				Assets:   []repository.SteamAssetRow{{AssetID: "100", MarketHashName: "AK"}},
			}},
			lots: []repository.SteamLotRow{{ID: "lot:AK", MarketHashName: "AK", Quantity: 1}},
		}
		cur := &stubCursors{}
		c := newClient(store)
		c.ConfigureCursors(cur)
		_, err := c.Fetch(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unconfirmed"))
		// The probe is stored as an incomplete snapshot (never the
		// baseline) plus a separately marked probe cursor; lots stay
		// untouched and no holdings are returned.
		Expect(store.snapshots).To(HaveLen(2))
		Expect(store.snapshots[1].Complete).To(BeFalse())
		Expect(store.snapshots[1].Assets).To(BeEmpty())
		Expect(store.lotsCalls).To(Equal(0))
		Expect(store.lots).To(HaveLen(1))
		probe, err := cur.Get(context.Background(), steamCursorScope(cursorScopeEmptyProbe, "76561199495663064"))
		Expect(err).NotTo(HaveOccurred())
		Expect(probe.Position).NotTo(BeEmpty())
	})

	It("confirms a genuinely emptied inventory on the second sighting", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{}, "descriptions": []any{}, "success": 1,
				})
				return
			}
			writeEmptyProvenance(w, r)
		}
		store := &stubSteamStore{
			snapshots: []repository.SteamInventorySnapshot{{
				ID: "prev", SteamID: "76561199495663064", TakenAt: time.Now().Add(-2 * time.Hour),
				Complete: true,
				Assets:   []repository.SteamAssetRow{{AssetID: "100", MarketHashName: "AK"}},
			}},
			lots: []repository.SteamLotRow{{ID: "lot:AK", MarketHashName: "AK", Quantity: 1}},
		}
		cur := &stubCursors{m: map[string]repository.SyncCursor{
			steamCursorScope(cursorScopeEmptyProbe, "76561199495663064"): {Scope: steamCursorScope(cursorScopeEmptyProbe, "76561199495663064"),
				Position: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		}}
		c := newClient(store)
		c.ConfigureCursors(cur)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(BeEmpty())
		// Confirmed empty: lots are rebuilt (cleared).
		Expect(store.lotsCalls).To(Equal(1))
	})

	It("ignores stale probes after a successful sync intervened", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{}, "descriptions": []any{}, "success": 1,
				})
				return
			}
			writeEmptyProvenance(w, r)
		}
		store := &stubSteamStore{
			snapshots: []repository.SteamInventorySnapshot{{
				ID: "fresh", SteamID: "76561199495663064", TakenAt: time.Now().UTC(),
				Complete: true,
				Assets:   []repository.SteamAssetRow{{AssetID: "100", MarketHashName: "AK"}},
			}},
		}
		cur := &stubCursors{m: map[string]repository.SyncCursor{
			steamCursorScope(cursorScopeEmptyProbe, "76561199495663064"): {Scope: steamCursorScope(cursorScopeEmptyProbe, "76561199495663064"),
				Position: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		}}
		c := newClient(store)
		c.ConfigureCursors(cur)
		_, err := c.Fetch(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unconfirmed"))
	})

	It("fails closed when the baseline cannot be read", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{}, "descriptions": []any{}, "success": 1,
				})
				return
			}
			writeEmptyProvenance(w, r)
		}
		store := &stubSteamStore{failLatest: true}
		_, err := newClient(store).Fetch(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot confirm"))
		// Nothing was recorded: not even the probe.
		Expect(store.snapshots).To(BeEmpty())
	})

	It("proceeds when the first sync finds nothing to erase", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{}, "descriptions": []any{}, "success": 1,
				})
				return
			}
			writeEmptyProvenance(w, r)
		}
		snap, err := newClient(&stubSteamStore{}).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(BeEmpty())
	})

	It("restores partial-snapshot acquisitions from ID-keyed lookup", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/inventory/"):
				writeJSON(w, map[string]any{
					"assets": []any{map[string]any{
						"assetid": "100", "classid": "1", "instanceid": "0", "amount": "1",
					}},
					"descriptions": []any{map[string]any{
						"classid": "1", "instanceid": "0",
						"market_hash_name": "AK-47 | Redline (Field-Tested)",
						"marketable":       1, "tradable": 1,
					}},
					"success": 1,
				})
			default:
				writeEmptyProvenance(w, r)
			}
		}
		basis := 23.41
		at := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
		store := &stubSteamStore{
			// The complete baseline predates this asset entirely; only
			// the ID-keyed lookup can restore its evidence.
			snapshots: []repository.SteamInventorySnapshot{{
				ID: "prev", SteamID: "76561199495663064", TakenAt: time.Now().Add(-time.Hour),
				Complete: true,
			}},
			acqRows: []repository.SteamAcquisitionRow{{
				AssetID: "100", MarketHashName: "AK-47 | Redline (Field-Tested)",
				AcquiredAt: &at, Type: string(domainsteam.AcquisitionMarketBuy),
				Reference: "market:buy:7", CostBasis: &basis, CostCurrency: "USD",
				MatchMethod: string(domainsteam.MatchHistoryMarket), MatchConfidence: string(domainsteam.MatchHigh),
			}},
		}
		c := newClient(store)
		c.SetPriceService(nil)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		acts := snap.Activities["steam-76561199495663064"]
		Expect(acts).To(HaveLen(1))
		Expect(acts[0].Type).To(Equal(brokerage.ActivityBuy))
		Expect(acts[0].Price).To(BeNumerically("~", 23.41, 1e-9))
		Expect(snap.Holdings[0].Positions[0].AveragePurchasePrice).To(BeNumerically("~", 23.41, 1e-9))
	})

	It("rewinds resumed market backfills by one page", func() {
		var starts []string
		purchase := map[string]any{
			"listingid": "7", "purchaseid": "7",
			"time_sold": 1746720120, "steamid_purchaser": "76561199495663064",
			"failed": 0, "needs_rollback": 0,
			"asset":       map[string]any{"classid": "1", "instanceid": "0", "amount": "1"},
			"paid_amount": 2341, "currencyid": "2001",
		}
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/inventory/"):
				writeJSON(w, map[string]any{
					"assets": []any{map[string]any{
						"assetid": "100", "classid": "1", "instanceid": "0", "amount": "1",
					}},
					"descriptions": []any{map[string]any{
						"classid": "1", "instanceid": "0",
						"market_hash_name": "AK-47 | Redline (Field-Tested)",
						"marketable":       1, "tradable": 1,
					}},
					"success": 1,
				})
			case strings.HasPrefix(r.URL.Path, "/market/myhistory"):
				starts = append(starts, r.URL.Query().Get("start"))
				writeJSON(w, map[string]any{
					"success": true, "total_count": 1200, "start": 0,
					"purchases": map[string]any{"7": purchase}, "listings": map[string]any{},
				})
			default:
				writeEmptyProvenance(w, r)
			}
		}
		store := &stubSteamStore{}
		cur := &stubCursors{m: map[string]repository.SyncCursor{
			steamCursorScope(cursorScopeMarket, "76561199495663064"): {Scope: steamCursorScope(cursorScopeMarket, "76561199495663064"),
				Position: `{"watermark":"2025-05-08T12:00:00Z","resume":{"offset":1000}}`,
				Complete: false},
		}}
		c := newClient(store)
		c.ConfigureCursors(cur)
		_, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Stored offset 1000 rewinds one page to 500; the walk then
		// covers 500 and 1000 of 1200 and completes.
		Expect(starts).To(Equal([]string{"500", "1000"}))
		var marketCur *repository.SyncCursor
		for i, pc := range c.pendingCursors {
			if pc.Scope == steamCursorScope(cursorScopeMarket, "76561199495663064") {
				marketCur = &c.pendingCursors[i]
			}
		}
		Expect(marketCur).NotTo(BeNil())
		Expect(marketCur.Complete).To(BeTrue())
		Expect(marketCur.Position).NotTo(ContainSubstring("resume"))
	})

	It("requires STEAM_ID", func() {
		c := New(ClientConfig{}, nil)
		_, err := c.Fetch(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("values positions and emits BUY activities on resolved evidence", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/inventory/"):
				writeJSON(w, map[string]any{
					"assets": []any{map[string]any{
						"assetid": "100", "classid": "1", "instanceid": "0", "amount": "1",
					}},
					"descriptions": []any{map[string]any{
						"classid": "1", "instanceid": "0",
						"market_hash_name": "AK-47 | Redline (Field-Tested)",
						"marketable":       1, "tradable": 1,
					}},
					"success": 1,
				})
			case r.URL.Path == "/my/inventoryhistory/":
				writeJSON(w, map[string]any{
					"success": true,
					"html": `<div class="tradehistoryrow" data-appid="730" data-contextid="2" data-classid="1" data-instanceid="0" data-amount="1" data-assetid="100">` +
						`<div class="event_description">8 May, 2025 1:00pm Purchased on Community Market - AK-47 | Redline (Field-Tested)</div></div>`,
					"descriptions": map[string]any{
						"730": map[string]any{
							"1_0": map[string]any{"market_hash_name": "AK-47 | Redline (Field-Tested)"},
						},
					},
					"apps": []any{map[string]any{"appid": 730}},
				})
			case r.URL.Path == "/market/myhistory/render/":
				writeJSON(w, map[string]any{
					"success":     true,
					"total_count": 1,
					"start":       0,
					"purchases": map[string]any{
						"7_7": map[string]any{
							"listingid": "7", "purchaseid": "7",
							"time_sold": 1746720120, "steamid_purchaser": "76561199495663064",
							"failed": 0, "needs_rollback": 0,
							"asset":       map[string]any{"classid": "1", "instanceid": "0", "amount": "1"},
							"paid_amount": 2341, "currencyid": "2001",
						},
					},
					"listings": map[string]any{},
				})
			default:
				writeJSON(w, map[string]any{"response": map[string]any{"more": false, "trades": []any{}}})
			}
		}
		store := &stubSteamStore{}
		c := newClient(store)
		c.SetPriceService(nil)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		acts := snap.Activities["steam-76561199495663064"]
		Expect(acts).To(HaveLen(1))
		Expect(acts[0].Type).To(Equal(brokerage.ActivityBuy))
		Expect(acts[0].Price).To(BeNumerically("~", 23.41, 1e-9))
		Expect(store.matched).To(ContainElement("hist:Purchased on Community Market:1746709200:100:1:0:AK-47 | Redline (Field-Tested):1"))
	})
})

var _ = Describe("Item titles", func() {
	It("prefers the market name, then inventory name, never bare ids", func() {
		mk := func(name, market string) Resolution {
			return Resolution{Asset: InventoryItem{AssetID: "1", ClassID: "2", InstanceID: "3", Name: name, MarketHashName: market}}
		}
		Expect(titleOf(mk("Inv", "Market"))).To(Equal("Market"))
		Expect(titleOf(mk("Inv", ""))).To(Equal("Inv"))
		Expect(titleOf(mk("", ""))).To(Equal("Steam item 1"))
	})
})

var _ = Describe("Value filter and history backfill", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	BeforeEach(func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	twoItems := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/inventory/"):
			writeJSON(w, map[string]any{
				"assets": []any{
					map[string]any{"assetid": "100", "classid": "1", "instanceid": "0", "amount": "1"},
					map[string]any{"assetid": "200", "classid": "2", "instanceid": "0", "amount": "1"},
				},
				"descriptions": []any{
					map[string]any{"classid": "1", "instanceid": "0", "market_hash_name": "AK-47 | Redline", "marketable": 1, "tradable": 1},
					map[string]any{"classid": "2", "instanceid": "0", "market_hash_name": "Cheap Case", "marketable": 1, "tradable": 1},
				},
				"success": 1,
			})
		case r.URL.Path == "/market/priceoverview/":
			name := r.URL.Query().Get("market_hash_name")
			median := "$0.50"
			if strings.Contains(name, "Redline") {
				median = "$100.00"
			}
			writeJSON(w, map[string]any{"success": true, "median_price": median})
		default:
			writeEmptyProvenance(w, r)
		}
	}

	newFilteredClient := func(store *stubSteamStore, minValue float64) *Client {
		cfg := defaultClientConfig()
		cfg.SteamID = "76561199495663064"
		cfg.MinInterval = time.Millisecond
		cfg.CommunityBase = server.URL
		cfg.StoreBase = server.URL
		cfg.Session = "sessionid=test-session"
		cfg.APIKey = "test-key"
		cfg.MinItemValueUSD = minValue
		c := New(cfg, server.Client())
		if store != nil {
			c.SetSteamStore(store)
		}
		return c
	}

	It("drops new dust from positions but keeps it in the snapshot", func() {
		handler = twoItems
		store := &stubSteamStore{}
		snap, err := newFilteredClient(store, 10).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("AK-47 | Redline"))
		Expect(store.snapshots).To(HaveLen(1))
		Expect(store.snapshots[0].Assets).To(HaveLen(2))
	})

	It("keeps dust out on every sync until its price recovers", func() {
		handler = twoItems
		store := &stubSteamStore{}
		Expect(store.SaveSnapshot(context.Background(), repository.SteamInventorySnapshot{
			ID: "prev", SteamID: "76561199495663064", TakenAt: time.Now().Add(-time.Hour),
			Complete: true,
			Assets: []repository.SteamAssetRow{
				{AssetID: "100", ClassID: "1", InstanceID: "0", MarketHashName: "AK-47 | Redline", Amount: 1},
				{AssetID: "200", ClassID: "2", InstanceID: "0", MarketHashName: "Cheap Case", Amount: 1},
			},
		})).To(Succeed())
		snap, err := newFilteredClient(store, 10).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("AK-47 | Redline"))
	})

	It("excludes items exactly at the threshold (strictly above only)", func() {
		handler = twoItems
		store := &stubSteamStore{}
		// AK-47 | Redline is worth exactly $100 × 1: at a $100 threshold
		// it must stay out, since only totals strictly above sync.
		snap, err := newFilteredClient(store, 100).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(0))
		Expect(store.snapshots).To(HaveLen(1))
		Expect(store.snapshots[0].Assets).To(HaveLen(2))
	})

	It("admits stacks whose combined total clears the threshold", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				assets := make([]any, 0, 9)
				for i := 0; i < 9; i++ {
					assets = append(assets, map[string]any{
						"assetid": fmt.Sprintf("30%d", i), "classid": "7", "instanceid": "0", "amount": "1",
					})
				}
				writeJSON(w, map[string]any{
					"assets": assets,
					"descriptions": []any{
						map[string]any{"classid": "7", "instanceid": "0", "market_hash_name": "Gamma 2 Case", "marketable": 1, "tradable": 1},
					},
					"success": 1,
				})
				return
			}
			if r.URL.Path == "/market/priceoverview/" {
				writeJSON(w, map[string]any{"success": true, "median_price": "$4.43"})
				return
			}
			writeEmptyProvenance(w, r)
		}
		store := &stubSteamStore{}
		// Each row alone is $4.43 < $10, but 9 × $4.43 = $39.87 clears it.
		snap, err := newFilteredClient(store, 10).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(9))
		Expect(snap.RetractedActivities).To(BeEmpty())
		Expect(store.snapshots).To(HaveLen(1))
		Expect(store.snapshots[0].Assets).To(HaveLen(9))
	})

	It("retracts stale activities for filtered items", func() {
		handler = twoItems
		store := &stubSteamStore{}
		snap, err := newFilteredClient(store, 10).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		retracted := snap.RetractedActivities["steam-76561199495663064"]
		Expect(retracted).To(ContainElement("steam:200"))
		Expect(retracted).NotTo(ContainElement("steam:100"))
	})

	It("never retracts activities from a partial snapshot", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				if r.URL.Query().Get("start_assetid") == "" {
					writeJSON(w, map[string]any{
						"assets": []any{
							map[string]any{"assetid": "200", "classid": "2", "instanceid": "0", "amount": "1"},
						},
						"descriptions": []any{
							map[string]any{"classid": "2", "instanceid": "0", "market_hash_name": "Cheap Case", "marketable": 1, "tradable": 1},
						},
						"success": 1, "more_items": true, "last_assetid": "200",
					})
					return
				}
				// Second page fails mid-walk: the snapshot is partial.
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, "boom")
				return
			}
			if r.URL.Path == "/market/priceoverview/" {
				writeJSON(w, map[string]any{"success": true, "median_price": "$0.50"})
				return
			}
			writeEmptyProvenance(w, r)
		}
		store := &stubSteamStore{}
		snap, err := newFilteredClient(store, 10).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Partial).To(BeTrue())
		// The cheap item is filtered from activities, but its stale
		// rows must survive: a truncated page cannot tell dust from
		// items lost to the failed page.
		Expect(snap.RetractedActivities).To(BeEmpty())
	})

	It("excludes unpriced items while the threshold filter is on", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				twoItems(w, r)
				return
			}
			if r.URL.Path == "/market/priceoverview/" {
				writeJSON(w, map[string]any{"success": false})
				return
			}
			writeEmptyProvenance(w, r)
		}
		snap, err := newFilteredClient(&stubSteamStore{}, 1000).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Unknown value proves nothing: with the filter on, unpriced
		// items stay out of positions (but remain in the snapshot and
		// their stale activities are retracted).
		Expect(snap.Holdings[0].Positions).To(HaveLen(0))
		Expect(snap.RetractedActivities).NotTo(BeEmpty())
	})

	It("keeps unpriced items as zero-price positions when the filter is off", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				twoItems(w, r)
				return
			}
			if r.URL.Path == "/market/priceoverview/" {
				writeJSON(w, map[string]any{"success": false})
				return
			}
			writeEmptyProvenance(w, r)
		}
		snap, err := newFilteredClient(&stubSteamStore{}, 0).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(2))
	})

	It("backfills missing histories within budget", func() {
		handler = twoItems
		store := &stubSteamStore{}
		priceStore := newStubPriceStore()
		c := newFilteredClient(store, 0)
		c.SetPriceHistoryStore(priceStore)
		c.cfg.Session = "sessionid=x"
		// Price history endpoint is stubbed through the same server.
		oldHandler := handler
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/market/pricehistory/" {
				writeJSON(w, map[string]any{"success": true, "prices": []any{
					[]any{"8 May, 2025", 100.0, "5"},
				}})
				return
			}
			oldHandler(w, r)
		}
		c.cfg.HistoryBudget = 5
		_, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(priceStore.puts).To(BeNumerically(">", 0))
	})

	It("does not mistake current-quote rows for history coverage", func() {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if r.URL.Path == "/market/pricehistory/" {
				hits++
				writeJSON(w, map[string]any{"success": true, "prices": []any{
					[]any{"8 May, 2025", 100.0, "5"},
				}})
				return
			}
			writeJSON(w, map[string]any{"success": false})
		}))
		defer srv.Close()
		mkClient := func(store *stubPriceStore) *Client {
			c := New(ClientConfig{
				SteamID: "1", HistoryBudget: 5, Currency: 1,
				CommunityBase: srv.URL, StoreBase: srv.URL,
				MinInterval: time.Millisecond,
			}, srv.Client())
			c.SetPriceHistoryStore(store)
			c.cfg.Session = "sessionid=x"
			return c
		}
		asset, curr := PriceAssetKey("AK-47 | Redline", 1)
		resolved := []Resolution{{Asset: InventoryItem{MarketHashName: "AK-47 | Redline", Marketable: true}}}
		// Two recency rows: backfill must still fetch.
		few := newStubPriceStore()
		Expect(few.Upsert(context.Background(), []repository.HistoricalPrice{
			{Asset: asset, Timestamp: time.Now().UTC().Add(-time.Hour), Currency: curr, Price: 10, Source: "steam_market"},
			{Asset: asset, Timestamp: time.Now().UTC(), Currency: curr, Price: 10, Source: "steam_market"},
		})).To(Succeed())
		mkClient(few).backfillPriceHistory(context.Background(), resolved, map[string]pricedQuote{})
		Expect(hits).To(Equal(0))
		// With a fresh validation quote the fetch proceeds.
		quoted := map[string]pricedQuote{priceDedupeKey("AK-47 | Redline"): {price: 100, observed: time.Now().UTC(), ok: true}}
		mkClient(few).backfillPriceHistory(context.Background(), resolved, quoted)
		Expect(hits).To(Equal(1))
		// A real series: no fetch.
		many := newStubPriceStore()
		for i := 0; i < minHistoryPoints; i++ {
			Expect(many.Upsert(context.Background(), []repository.HistoricalPrice{
				{Asset: asset, Timestamp: time.Now().UTC().Add(-time.Duration(i) * 24 * time.Hour), Currency: curr, Price: 10, Source: "steam_market"},
			})).To(Succeed())
		}
		mkClient(many).backfillPriceHistory(context.Background(), resolved, quoted)
		Expect(hits).To(Equal(1))
	})

	It("dedupes backfill attempts across name variants", func() {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if r.URL.Path == "/market/pricehistory/" {
				hits++
				writeJSON(w, map[string]any{"success": true, "prices": []any{
					[]any{"8 May, 2025", 100.0, "5"},
				}})
				return
			}
			writeJSON(w, map[string]any{"success": false})
		}))
		defer srv.Close()
		c := New(ClientConfig{
			SteamID: "1", HistoryBudget: 5, Currency: 1,
			CommunityBase: srv.URL, StoreBase: srv.URL,
			MinInterval: time.Millisecond,
		}, srv.Client())
		c.SetPriceHistoryStore(newStubPriceStore())
		c.cfg.Session = "sessionid=x"
		resolved := []Resolution{
			{Asset: InventoryItem{MarketHashName: "Gamma 2 Case", Marketable: true}},
			{Asset: InventoryItem{MarketHashName: "gamma  2  CASE", Marketable: true}},
		}
		quoted := map[string]pricedQuote{priceDedupeKey("Gamma 2 Case"): {price: 100, observed: time.Now().UTC(), ok: true}}
		c.backfillPriceHistory(context.Background(), resolved, quoted)
		Expect(hits).To(Equal(1))
	})
})

var _ = Describe("Price circuit breaker", func() {
	It("stops pricing after repeated provider failures", func() {
		c := New(ClientConfig{SteamID: "1"}, nil)
		for i := 0; i < 9; i++ {
			c.priceFails.Add(1)
		}
		Expect(c.priceFails.Load() >= priceFailBreaker).To(BeFalse())
		c.priceFails.Add(1)
		Expect(c.priceFails.Load() >= priceFailBreaker).To(BeTrue())
	})
})

var _ = Describe("Unmarketable pricing", func() {
	It("classifies pricable items by name and marketable flag", func() {
		Expect(pricable(InventoryItem{MarketHashName: "AK", Marketable: true})).To(BeTrue())
		Expect(pricable(InventoryItem{MarketHashName: "5 Year Veteran Coin", Marketable: false})).To(BeFalse())
		Expect(pricable(InventoryItem{MarketHashName: "", Marketable: true})).To(BeFalse())
		Expect(pricable(InventoryItem{})).To(BeFalse())
	})

	It("leaves unmarketable items zero-price without provider calls", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			if strings.HasPrefix(r.URL.Path, "/inventory/") {
				writeJSON(w, map[string]any{
					"assets": []any{
						map[string]any{"assetid": "1", "classid": "1", "instanceid": "0", "amount": "1"},
						map[string]any{"assetid": "2", "classid": "2", "instanceid": "0", "amount": "1"},
					},
					"descriptions": []any{
						map[string]any{"classid": "1", "instanceid": "0", "market_hash_name": "AK-47 | Redline", "marketable": 1, "tradable": 1},
						map[string]any{"classid": "2", "instanceid": "0", "market_hash_name": "5 Year Veteran Coin", "marketable": 0, "tradable": 1},
					},
					"success": 1,
				})
				return
			}
			if r.URL.Path == "/market/priceoverview/" {
				writeJSON(w, map[string]any{"success": true, "median_price": "$10.00"})
				return
			}
			writeEmptyProvenance(w, r)
		}))
		defer server.Close()
		cfg := defaultClientConfig()
		cfg.SteamID = "76561199495663064"
		cfg.MinInterval = time.Millisecond
		cfg.CommunityBase = server.URL
		cfg.StoreBase = server.URL
		c := New(cfg, server.Client())
		c.SetSteamStore(&stubSteamStore{})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Holdings[0].Positions).To(HaveLen(2))
		// Marketable item priced, veteran coin stays zero-price but visible.
		bySymbol := map[string]float64{}
		for _, p := range snap.Holdings[0].Positions {
			bySymbol[p.Symbol.Symbol] = p.Price
		}
		Expect(bySymbol["AK-47 | Redline"]).To(BeNumerically("~", 10.0, 1e-9))
		Expect(bySymbol["5 Year Veteran Coin"]).To(Equal(0.0))
		Expect(c.priceFails.Load()).To(Equal(int64(0)))
	})
})
