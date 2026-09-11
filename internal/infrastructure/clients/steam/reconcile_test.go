package steam

import (
	"context"
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
		resolved, unmatched := Resolve([]InventoryItem{owned("new", "1", "AK", 1)}, before, evs, nil, nil)
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
		resolved, _ := Resolve([]InventoryItem{owned("cur", "2", "AWP", 1)}, nil, nil, nil, trades)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchExact))
		Expect(resolved[0].Type).To(Equal(domainsteam.AcquisitionTrade))
		Expect(resolved[0].Reference).To(Equal("t1"))
	})

	It("matches high on history plus market agreement", func() {
		evs := []historyEvent{{ExternalID: "e1", Timestamp: at, Kind: EventMarketBuy, MarketHashName: "AK", Quantity: 1}}
		market := map[string]domainsteam.MarketTransaction{
			"market:9": {ExternalID: "market:9", Type: "buy", Timestamp: at.Add(time.Hour), MarketHashName: "AK", Quantity: 1, Gross: 23.41, Currency: "USD"},
		}
		resolved, unmatched := Resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, evs, market, nil)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchHigh))
		Expect(resolved[0].MatchMethod).To(Equal(domainsteam.MatchHistoryMarket))
		Expect(*resolved[0].CostBasis).To(BeNumerically("~", 23.41, 1e-9))
		Expect(resolved[0].CostCurrency).To(Equal("USD"))
		Expect(unmatched).To(BeEmpty())
	})

	It("matches medium on attribute proximity", func() {
		evs := []historyEvent{{ExternalID: "e1", Timestamp: at, Kind: EventReceived, ClassID: "1", InstanceID: "0", MarketHashName: "AK", Quantity: 1}}
		resolved, _ := Resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, evs, nil, nil)
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
		resolved, unmatched := Resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, evs, market, nil)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchHigh))
		Expect(unmatched).To(HaveLen(1))
		Expect(unmatched[0].ExternalID).To(Equal("e1"))
	})

	It("never pretends an inference is exact", func() {
		resolved, unmatched := Resolve([]InventoryItem{owned("x", "1", "AK", 1)}, nil, nil, nil, nil)
		Expect(resolved[0].MatchConfidence).To(Equal(domainsteam.MatchUnresolved))
		Expect(unmatched).To(BeEmpty())
	})

	It("groups indistinguishable duplicates into lots", func() {
		items := []InventoryItem{
			owned("a1", "7", "Dreams & Nightmares Case", 1),
			owned("a2", "7", "Dreams & Nightmares Case", 1),
			owned("solo", "1", "AK", 1),
		}
		resolved, _ := Resolve(items, nil, nil, nil, nil)
		lots := buildLots(resolved)
		Expect(lots).To(HaveLen(1))
		Expect(lots[0].MarketHashName).To(Equal("Dreams & Nightmares Case"))
		Expect(lots[0].Quantity).To(Equal(2))
		Expect(lots[0].Source).To(Equal(domainsteam.AcquisitionUnresolved))
	})
})

// stubSteamStore is an in-memory SteamAssetRepository for Fetch tests.
type stubSteamStore struct {
	snapshots []repository.SteamInventorySnapshot
	events    []repository.SteamEventRow
	market    []repository.SteamMarketRow
	trades    []repository.SteamTradeRow
	lots      []repository.SteamLotRow
	acqs      []repository.SteamAcquisitionRow
	matched   []string
	failSnap  bool
}

func (s *stubSteamStore) LatestSnapshot(_ context.Context, _ string) (repository.SteamInventorySnapshot, error) {
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
	s.market = append(s.market, txs...)
	return nil
}
func (s *stubSteamStore) SaveTrades(_ context.Context, _ string, trs []repository.SteamTradeRow) error {
	s.trades = append(s.trades, trs...)
	return nil
}
func (s *stubSteamStore) SaveLots(_ context.Context, _ string, lots []repository.SteamLotRow) error {
	s.lots = append(s.lots, lots...)
	return nil
}
func (s *stubSteamStore) SaveAcquisitions(_ context.Context, _ string, acqs []repository.SteamAcquisitionRow) error {
	s.acqs = append(s.acqs, acqs...)
	return nil
}
func (s *stubSteamStore) CurrentAssets(_ context.Context, _ string) ([]repository.SteamAssetState, error) {
	return nil, nil
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
		cfg.SteamID = "76561198000000000"
		cfg.MinInterval = time.Millisecond // New() treats <=0 as unset
		cfg.CommunityBase = server.URL
		cfg.StoreBase = server.URL
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
			writeJSON(w, map[string]any{"events": []any{}, "response": map[string]any{"more": false, "trades": []any{}}})
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
		Expect(snap.Activities["steam-76561198000000000"]).To(BeEmpty())
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
				writeJSON(w, map[string]any{"events": []any{
					map[string]any{
						"event_name": "Purchased on Community Market", "assetid": "100",
						"classid": "1", "instanceid": "0",
						"market_hash_name": "AK-47 | Redline (Field-Tested)",
						"quantity":         1, "time": "1746720120",
					},
				}, "more": false})
			case r.URL.Path == "/market/myhistory/render/":
				writeJSON(w, map[string]any{
					"success":     true,
					"total_count": 1,
					"results_html": `<div class="market_listing_row" data-listingid="7" data-timestamp="1746720120">` +
						`<span class="market_listing_item_name">AK-47 | Redline (Field-Tested)</span>` +
						`<span>Purchased for -$23.41</span> <span>8 May, 2025</span></div>`,
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
		acts := snap.Activities["steam-76561198000000000"]
		Expect(acts).To(HaveLen(1))
		Expect(acts[0].Type).To(Equal(brokerage.ActivityBuy))
		Expect(acts[0].Price).To(BeNumerically("~", 23.41, 1e-9))
		Expect(store.matched).To(ContainElement("hist:Purchased on Community Market:1746720120:100:1:0:AK-47 | Redline (Field-Tested):1"))
	})
})
