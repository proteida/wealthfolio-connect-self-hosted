package ton

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rs/zerolog"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

func TestTON(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TON Client Suite")
}

const (
	testWallet = "UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testRaw    = "0:1111111111111111111111111111111111111111111111111111111111111111"
	testMaster = "0:2222222222222222222222222222222222222222222222222222222222222222"
	usdMaster  = "0:3333333333333333333333333333333333333333333333333333333333333333"
)

// stubValue is a fixed USD pricer for tests.
type stubValue struct {
	v      float64
	ok     bool
	unit   float64
	unitOK bool
}

func (s stubValue) swapValue(context.Context, string, string, float64, string, string, float64, int64) (float64, bool) {
	return s.v, s.ok
}

func (s stubValue) unitPrice(context.Context, string, string, int64) (float64, bool) {
	return s.unit, s.unitOK
}

func testClient(srv *httptest.Server) *Client {
	c := New("test-key", []string{testWallet}, srv.URL, srv.Client())
	c.retryBase = time.Millisecond
	c.sleep = func(time.Duration) {}
	c.value = stubValue{v: 100, ok: true, unit: 2, unitOK: true}
	return c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	Expect(json.NewEncoder(w).Encode(v)).To(Succeed())
}

// stubHistoryStore is an in-memory ActivityRepository recording deletes.
type stubHistoryStore struct {
	rows    []brokerage.Activity
	deleted []struct {
		account string
		ids     []string
	}
}

func (s *stubHistoryStore) List(_ context.Context, f repository.ActivityFilter) ([]brokerage.Activity, int, error) {
	rows := s.rows
	if f.Offset < len(rows) {
		rows = rows[f.Offset:]
	} else {
		rows = nil
	}
	if f.Limit > 0 && len(rows) > f.Limit {
		rows = rows[:f.Limit]
	}
	return rows, len(s.rows), nil
}

func (s *stubHistoryStore) UpsertBatch(_ context.Context, _ string, _ []brokerage.Activity) error {
	return nil
}

func (s *stubHistoryStore) Delete(_ context.Context, accountID string, ids []string) error {
	s.deleted = append(s.deleted, struct {
		account string
		ids     []string
	}{accountID, ids})
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

func action(id, trace, typ string, success bool, details any) map[string]any {
	raw, err := json.Marshal(details)
	Expect(err).NotTo(HaveOccurred())
	return map[string]any{
		"action_id": id, "trace_id": trace,
		"start_utime": 1700000000, "end_utime": 1700000000,
		"success": success, "type": typ, "details": json.RawMessage(raw),
	}
}

var _ = Describe("Client fetching", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	var calls []string
	var keys []string
	BeforeEach(func() {
		calls = nil
		keys = nil
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			calls = append(calls, r.URL.Path+"?"+r.URL.RawQuery)
			keys = append(keys, r.Header.Get("X-API-Key"))
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	accountStates := func(balance string) map[string]any {
		return map[string]any{"accounts": []any{map[string]any{
			"address": testRaw, "balance": balance, "status": "active",
		}}, "address_book": map[string]any{testRaw: map[string]any{"user_friendly": testWallet}}}
	}
	wallets := func(rows ...any) map[string]any {
		return map[string]any{"jetton_wallets": rows, "metadata": map[string]any{}}
	}
	masters := func() map[string]any {
		return map[string]any{"jetton_masters": []any{
			map[string]any{
				"address":        testMaster,
				"jetton_content": map[string]any{"symbol": "tsTON", "name": "Tonstakers TON", "decimals": "9"},
			},
			map[string]any{
				"address":        usdMaster,
				"jetton_content": map[string]any{"symbol": "USDT", "name": "Tether", "decimals": "6"},
			},
		}, "metadata": map[string]any{}}
	}

	It("returns slug ton and an empty snapshot without wallets", func() {
		c := New("", nil, "", nil)
		Expect(c.ID()).To(Equal("ton"))
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(BeEmpty())
		Expect(calls).To(BeEmpty())
	})

	It("imports swaps, stakes and transfers from actions with USD legs", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/accountStates":
				Expect(r.URL.Query().Get("address")).To(Equal(testWallet))
				writeJSON(w, accountStates("400000000"))
			case "/jetton/wallets":
				writeJSON(w, wallets(map[string]any{
					"address": "0:JW", "balance": "214365209487", "owner": testRaw, "jetton": testMaster,
				}))
			case "/jetton/masters":
				writeJSON(w, masters())
			case "/traces":
				writeJSON(w, map[string]any{"traces": []any{}})
			case "/actions":
				if r.URL.Query().Get("tx_hash") != "" {
					writeJSON(w, map[string]any{"actions": []any{}})
					return
				}
				writeJSON(w, map[string]any{"actions": []any{
					action("sw1", "trace-swap", "jetton_swap", true, map[string]any{
						"dex": "stonfi", "sender": testRaw,
						"asset_in":  usdMaster,
						"asset_out": testMaster,
						"dex_incoming_transfer": map[string]any{
							"asset": usdMaster, "source": testRaw,
							"destination": "0:POOL", "amount": "100000000",
						},
						"dex_outgoing_transfer": map[string]any{
							"asset": testMaster, "source": "0:POOL",
							"destination": testRaw, "amount": "5000000000",
						},
					}),
					// Primitive legs of the same swap must not double count.
					action("sw1-leg", "trace-swap", opJettonTransfer, true, map[string]any{
						"asset": usdMaster, "sender": testRaw,
						"receiver": "0:POOL", "amount": "100000000",
					}),
					action("st1", "trace-stake", "stake_deposit", true, map[string]any{
						"provider": "liquid_staking", "stake_holder": testRaw,
						"pool":          "0:POOL",
						"amount":        "10000000000",
						"tokens_minted": "9000000000",
						"asset":         testMaster,
					}),
					action("d1", "trace-dep", opTonTransfer, true, map[string]any{
						"source": "0:STRANGER", "destination": testRaw, "value": "2000000000",
					}),
					action("w1", "trace-wd", opJettonTransfer, true, map[string]any{
						"asset": testMaster, "sender": testRaw,
						"receiver": "0:STRANGER", "amount": "1000000000",
					}),
					action("t1", "trace-internal", opTonTransfer, true, map[string]any{
						"source": testRaw, "destination": "0:FRIEND", "value": "1000000000",
					}),
					action("f1", "trace-fail", opTonTransfer, false, map[string]any{
						"source": "0:X", "destination": testRaw, "value": "1000000000",
					}),
					action("c1", "trace-call", "call_contract", true, map[string]any{
						"source": testRaw, "destination": "0:DAPP", "value": "1000000",
					}),
				}})
			default:
				Fail(r.URL.Path)
			}
		}
		c := testClient(server)
		c.SetLogger(zerolog.Nop())
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		for _, k := range keys {
			Expect(k).To(Equal("test-key"))
		}
		Expect(snap.Connection.BrokerageSlug).To(Equal("ton"))
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Accounts[0].ID).To(Equal("ton-" + testRaw))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Holdings[0].Positions).To(HaveLen(2))
		acts := snap.Activities["ton-"+testRaw]
		// swap pair + stake pair + top-up + token send + internal transfer.
		Expect(acts).To(HaveLen(7))
		leg := func(typ brokerage.ActivityType, raw, symbol string) brokerage.Activity {
			for _, a := range acts {
				if a.Type == typ && a.RawType == raw && a.Symbol.Symbol == symbol {
					return a
				}
			}
			Fail(fmt.Sprintf("missing %s %s %s", typ, raw, symbol))
			return brokerage.Activity{}
		}
		sell := leg(brokerage.ActivitySell, "SWAP_SELL", "USDT")
		Expect(sell.Units).To(Equal(100.0))
		Expect(sell.Amount).To(Equal(100.0))
		Expect(sell.Price).To(Equal(1.0))
		Expect(sell.RawType).To(Equal("SWAP_SELL"))
		buy := leg(brokerage.ActivityBuy, "SWAP_BUY", "TSTON")
		Expect(buy.Units).To(Equal(5.0))
		Expect(buy.Amount).To(Equal(100.0))
		Expect(buy.Price).To(BeNumerically("<", 20.0))
		Expect(buy.Price).To(BeNumerically("~", 20.0, 1e-8))
		// SELL before BUY: the buy leg carries +1s so date-sorted views
		// keep causal order where equal timestamps would tie.
		Expect(buy.TradeDate.Unix()).To(Equal(sell.TradeDate.Unix() + 1))
		stakeSell := leg(brokerage.ActivitySell, "STAKE_SELL", nativeTONSymbol)
		Expect(stakeSell.RawType).To(Equal("STAKE_SELL"))
		Expect(stakeSell.Units).To(Equal(10.0))
		Expect(stakeSell.Amount).To(Equal(100.0))
		stakeBuy := leg(brokerage.ActivityBuy, "STAKE_BUY", "TSTON")
		Expect(stakeBuy.RawType).To(Equal("STAKE_BUY"))
		Expect(stakeBuy.TradeDate.Unix()).To(Equal(stakeSell.TradeDate.Unix() + 1))
		dep := leg(brokerage.ActivityTransferIn, "TON_TRANSFER_IN", nativeTONSymbol)
		Expect(dep.Units).To(Equal(2.0))
		wd := leg(brokerage.ActivityTransferOut, "JETTON_TRANSFER_OUT", "TSTON")
		Expect(wd.Units).To(Equal(1.0))
		// Failed and technical actions contribute nothing.
		for _, a := range acts {
			Expect(a.ExternalReferenceID).NotTo(BeElementOf("f1", "c1", "sw1-leg"))
		}
	})

	It("syncs two wallets with transfers between them", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/accountStates":
				if r.URL.Query().Get("address") == "0:FRIEND" {
					writeJSON(w, map[string]any{"accounts": []any{map[string]any{
						"address": "0:FRIEND", "balance": "0", "status": "active",
					}}, "address_book": map[string]any{}})
					return
				}
				writeJSON(w, accountStates("1000000000"))
			case "/jetton/wallets":
				writeJSON(w, wallets())
			case "/jetton/masters":
				writeJSON(w, map[string]any{"jetton_masters": []any{}, "metadata": map[string]any{}})
			case "/traces":
				writeJSON(w, map[string]any{"traces": []any{}})
			case "/actions":
				if r.URL.Query().Get("tx_hash") != "" {
					writeJSON(w, map[string]any{"actions": []any{}})
					return
				}
				if strings.Contains(r.URL.RawQuery, "0%3AFRIEND") {
					writeJSON(w, map[string]any{"actions": []any{}})
					return
				}
				writeJSON(w, map[string]any{"actions": []any{
					action("t1", "tr1", opTonTransfer, true, map[string]any{
						"source": testRaw, "destination": "0:FRIEND", "value": "1000000000",
					}),
				}})
			default:
				Fail(r.URL.Path)
			}
		}
		c := testClient(server)
		c.wallets = []string{testWallet, "0:FRIEND"}
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(2))
		acts := snap.Activities["ton-"+testRaw]
		Expect(acts).To(HaveLen(1))
		Expect(acts[0].Type).To(Equal(brokerage.ActivityTransferOut))
	})

	It("paginates actions across full pages", func() {
		var offsets []string
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/accountStates":
				writeJSON(w, accountStates("1000000000"))
			case "/jetton/wallets":
				writeJSON(w, wallets())
			case "/jetton/masters":
				writeJSON(w, map[string]any{"jetton_masters": []any{}, "metadata": map[string]any{}})
			case "/actions":
				offsets = append(offsets, r.URL.Query().Get("offset"))
				Expect(r.URL.Query().Get("limit")).To(Equal("1000"))
				rows := make([]any, 0)
				if r.URL.Query().Get("offset") == "0" {
					for i := 0; i < actionPageLimit; i++ {
						rows = append(rows, action(fmt.Sprintf("a%d", i), fmt.Sprintf("tr%d", i), opTonTransfer, true, map[string]any{
							"source": "0:STRANGER", "destination": testRaw, "value": "1000000000",
						}))
					}
				} else {
					rows = append(rows, action("last", "tr-last", opTonTransfer, true, map[string]any{
						"source": "0:STRANGER", "destination": testRaw, "value": "1000000000",
					}))
				}
				writeJSON(w, map[string]any{"actions": rows})
			default:
				Fail(r.URL.Path)
			}
		}
		snap, err := testClient(server).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(offsets).To(Equal([]string{"0", "1000"}))
		Expect(snap.Activities["ton-"+testRaw]).To(HaveLen(actionPageLimit + 1))
	})

	It("retries 429s honoring Retry-After and then succeeds", func() {
		var sleeps []time.Duration
		attempts := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/accountStates" && attempts < 2 {
				attempts++
				if attempts == 1 {
					w.Header().Set("Retry-After", "5")
				}
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			switch r.URL.Path {
			case "/accountStates":
				writeJSON(w, accountStates("1000000000"))
			case "/jetton/wallets":
				writeJSON(w, wallets())
			case "/jetton/masters":
				writeJSON(w, map[string]any{"jetton_masters": []any{}, "metadata": map[string]any{}})
			case "/actions":
				writeJSON(w, map[string]any{"actions": []any{}})
			default:
				Fail(r.URL.Path)
			}
		}
		c := testClient(server)
		c.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(attempts).To(Equal(2))
		Expect(sleeps).To(HaveLen(2))
		Expect(sleeps[0]).To(Equal(5 * time.Second))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
	})

	It("retries indexer timeout envelopes", func() {
		first := true
		handler = func(w http.ResponseWriter, r *http.Request) {
			if first && r.URL.Path == "/actions" {
				first = false
				writeJSON(w, map[string]any{"error": "timeout: context deadline exceeded"})
				return
			}
			switch r.URL.Path {
			case "/accountStates":
				writeJSON(w, accountStates("1000000000"))
			case "/jetton/wallets":
				writeJSON(w, wallets())
			case "/jetton/masters":
				writeJSON(w, map[string]any{"jetton_masters": []any{}, "metadata": map[string]any{}})
			case "/actions":
				writeJSON(w, map[string]any{"actions": []any{}})
			default:
				Fail(r.URL.Path)
			}
		}
		snap, err := testClient(server).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
	})

	It("keeps balances without completion when history fails", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/accountStates":
				writeJSON(w, accountStates("1000000000"))
			case "/jetton/wallets":
				writeJSON(w, wallets())
			case "/jetton/masters":
				writeJSON(w, map[string]any{"jetton_masters": []any{}, "metadata": map[string]any{}})
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}
		var logs strings.Builder
		c := testClient(server)
		c.SetLogger(zerolog.New(&logs))
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(logs.String()).To(ContainSubstring("transaction history incomplete"))
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Accounts[0].LastTxSync).To(BeNil())
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
	})

	It("skips unknown wallets without failing the snapshot", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"accounts": []any{}, "address_book": map[string]any{}})
		}
		snap, err := testClient(server).Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(BeEmpty())
	})

	It("fails fast on client errors and honors cancellation", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"bad address"}`)
		}
		c := testClient(server)
		before := len(calls)
		_, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred()) // wallet skipped, snapshot empty
		Expect(len(calls)).To(Equal(before + 1))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err = c.accountState(ctx, testWallet)
		Expect(err).To(MatchError(context.Canceled))
	})

	It("advances large histories across runs instead of discarding them", func() {
		page := func(offset, n int) []any {
			rows := make([]any, 0, n)
			for i := 0; i < n; i++ {
				rows = append(rows, action(
					fmt.Sprintf("a-%d-%d", offset, i),
					fmt.Sprintf("t-%d-%d", offset, i),
					opTonTransfer, true,
					map[string]any{"source": "0:STRANGER", "destination": testRaw, "value": "1000000000"},
				))
			}
			return rows
		}
		var offsets []int
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/actions" {
				Fail(r.URL.Path)
			}
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			offsets = append(offsets, offset)
			switch {
			case offset < 20000:
				writeJSON(w, map[string]any{"actions": page(offset, 1000), "metadata": map[string]any{}})
			case offset == 20000:
				writeJSON(w, map[string]any{"actions": page(offset, 1000), "metadata": map[string]any{}})
			case offset == 21000:
				writeJSON(w, map[string]any{"actions": page(offset, 10), "metadata": map[string]any{}})
			default:
				writeJSON(w, map[string]any{"actions": []any{}, "metadata": map[string]any{}})
			}
		}
		c := testClient(server)
		// Run 1: newest window (0, 1000) full → tail (2000..20000) full →
		// truncated, newest 20k kept, tentative cursor parked at 19000.
		acts, _, truncated, err := c.walletActionHistory(context.Background(), testRaw)
		Expect(err).NotTo(HaveOccurred())
		Expect(truncated).To(BeTrue())
		Expect(acts).To(HaveLen(20000))
		Expect(offsets).To(Equal([]int{0, 1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000, 11000, 12000, 13000, 14000, 15000, 16000, 17000, 18000, 19000}))
		// Without a commit (failed persistence), the tentative cursor is
		// dropped and the next run replays from the start instead of
		// skipping unsaved rows.
		offsets = nil
		_, _, truncated, err = c.walletActionHistory(context.Background(), testRaw)
		Expect(err).NotTo(HaveOccurred())
		Expect(truncated).To(BeTrue())
		Expect(offsets[0]).To(Equal(0))
		// Run 2 (committed): newest refresh + tail from 19000; short page
		// at 21000 completes the backfill and clears the cursor (run-to-run
		// overlap dedupes downstream by stable activity identity).
		c.SnapshotCommitted()
		offsets = nil
		acts, _, truncated, err = c.walletActionHistory(context.Background(), testRaw)
		Expect(err).NotTo(HaveOccurred())
		Expect(truncated).To(BeFalse())
		Expect(acts).To(HaveLen(2000 + 2010))
		Expect(c.tailCursor(context.Background(), testRaw)).To(Equal(0))
		Expect(offsets).To(Equal([]int{0, 1000, 19000, 20000, 21000}))
	})

	It("retires superseded fallbacks and rebuilds basis from the stored ledger", func() {
		mkLeg := func(id string, typ brokerage.ActivityType, units, amount float64, ts int64) brokerage.Activity {
			return brokerage.Activity{ID: id, SourceRecordID: id, Type: typ,
				Units: units, Amount: amount,
				TradeDate: time.Unix(ts, 0).UTC(),
				Symbol:    &brokerage.Symbol{Symbol: "VAULT"}}
		}
		vaultID := "vault:" + testMaster
		// Stored middle pages: a superseded synthetic fallback plus a real
		// buy 10 @ $1 that the final window never saw.
		store := &stubHistoryStore{rows: []brokerage.Activity{
			mkLeg(vaultID, brokerage.ActivityBuy, 10, 100, 500),
			mkLeg("mid-buy", brokerage.ActivityBuy, 10, 10, 1000),
		}}
		data := TonWalletData{
			Address: testWallet, Canonical: testRaw,
			Tokens:         []TonToken{{Symbol: "VAULT", TokenAddr: testMaster, Quantity: 16, Decimals: 9}},
			MasterOutflows: map[string]VaultSpend{testMaster: {Amount: 100, Time: 1000}},
			Activities: []brokerage.Activity{
				// Final window: sell 4, rebuy 10 @ $3, plus a verified
				// inbound that supersedes the stored fallback.
				mkLeg("head-sell", brokerage.ActivitySell, 4, 10, 2000),
				mkLeg("head-buy", brokerage.ActivityBuy, 10, 30, 3000),
			},
			ActivitiesFetched: true,
		}
		c := &Client{log: zerolog.Nop(), history: store}
		c.reconcileWallet(context.Background(), &data, 19000)
		// Physical deletion is staged for post-commit, not executed here:
		// the replacement must persist first.
		Expect(store.deleted).To(BeEmpty())
		// Basis covers the full ledger excluding the superseded fallback:
		// +10@$1, -4 relief (cost 10→6, qty 6), +10@$3 → 36/16 = $2.25.
		// Counting the stale $100 fallback would give $4.54; the window
		// slice alone would give $3.
		Expect(data.Basis).To(HaveKeyWithValue("VAULT", BeNumerically("~", 2.25, 1e-9)))
		// Post-commit the staged retirement executes.
		c.SnapshotCommitted()
		Expect(store.deleted).To(HaveLen(1))
		Expect(store.deleted[0].account).To(Equal("ton-" + testRaw))
		Expect(store.deleted[0].ids).To(Equal([]string{vaultID}))
		// A second commit does not repeat the deletion.
		c.SnapshotCommitted()
		Expect(store.deleted).To(HaveLen(1))
	})

	It("skips reconciliation without stored history", func() {
		data := TonWalletData{Address: testWallet, Canonical: testRaw, ActivitiesFetched: true}
		c := &Client{log: zerolog.Nop()}
		c.reconcileWallet(context.Background(), &data, 19000)
		Expect(data.Basis).To(BeNil())
	})

	It("surfaces truncation as incomplete history while keeping newest legs", func() {
		page := func(offset, n int) []any {
			rows := make([]any, 0, n)
			for i := 0; i < n; i++ {
				rows = append(rows, action(
					fmt.Sprintf("c-%d-%d", offset, i), fmt.Sprintf("ct-%d-%d", offset, i),
					opTonTransfer, true,
					map[string]any{"source": "0:STRANGER", "destination": testRaw, "value": "1000000000"},
				))
			}
			return rows
		}
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/actions" {
				Fail(r.URL.Path)
			}
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			if offset < 20000 {
				writeJSON(w, map[string]any{"actions": page(offset, 1000), "metadata": map[string]any{}})
				return
			}
			writeJSON(w, map[string]any{"actions": []any{}, "metadata": map[string]any{}})
		}
		c := testClient(server)
		acts, _, _, err := c.walletHistory(context.Background(),
			[]string{testRaw}, map[string]tokenMeta{}, c.value, nil, nil)
		// Truncation is an error for completion purposes: the newest
		// groups import, but history must not read as complete.
		Expect(err).To(MatchError(ContainSubstring("page cap")))
		Expect(acts).To(HaveLen(20000 - 1))
	})

	It("resumes the tail from durable state after a restart", func() {
		var offsets []int
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/actions" {
				Fail(r.URL.Path)
			}
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			offsets = append(offsets, offset)
			n := 1000
			if offset >= 21000 {
				n = 10
			}
			rows := make([]any, 0, n)
			for i := 0; i < n; i++ {
				rows = append(rows, action(
					fmt.Sprintf("d-%d-%d", offset, i), fmt.Sprintf("dt-%d-%d", offset, i),
					opTonTransfer, true,
					map[string]any{"source": "0:STRANGER", "destination": testRaw, "value": "1000000000"},
				))
			}
			writeJSON(w, map[string]any{"actions": rows, "metadata": map[string]any{}})
		}
		store := &stubCursorStore{rows: map[string]repository.SyncCursor{
			"ton-tail-" + testRaw: {Scope: "ton-tail-" + testRaw, Position: "19000"},
		}}
		c := testClient(server)
		c.ConfigureCursors(store)
		acts, _, truncated, err := c.walletActionHistory(context.Background(), testRaw)
		Expect(err).NotTo(HaveOccurred())
		Expect(truncated).To(BeFalse())
		// Newest refresh plus the durable tail window, then the short page.
		Expect(offsets).To(Equal([]int{0, 1000, 19000, 20000, 21000}))
		Expect(acts).To(HaveLen(2000 + 2010))
	})

	It("keeps complete groups and stays incomplete when paging fails", func() {
		page := func(n int) []any {
			rows := make([]any, 0, n)
			for i := 0; i < n; i++ {
				rows = append(rows, action(
					fmt.Sprintf("b-%d", i), fmt.Sprintf("bt-%d", i),
					opTonTransfer, true,
					map[string]any{"source": "0:STRANGER", "destination": testRaw, "value": "1000000000"},
				))
			}
			return rows
		}
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/actions" {
				Fail(r.URL.Path)
			}
			if r.URL.Query().Get("offset") != "0" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, map[string]any{"actions": page(1000), "metadata": map[string]any{}})
		}
		c := testClient(server)
		c.value = stubValue{unit: 2, unitOK: true}
		acts, _, _, err := c.walletHistory(context.Background(),
			[]string{testRaw}, map[string]tokenMeta{}, c.value, nil, nil)
		Expect(err).To(HaveOccurred())
		// The failed page discards nothing collected: all complete groups
		// but the boundary-oldest still classify, while the error keeps
		// history incomplete upstream.
		Expect(acts).To(HaveLen(999))
	})
})
