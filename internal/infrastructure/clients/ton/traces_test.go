package ton

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

const (
	tWallet = "0:WALLET"
	tFriend = "0:FRIEND"
	tJW1    = "0:JW1"
	tJW2    = "0:JW2"
	tVault  = "0:VAULT"
	tUSDT   = "0:USDT"
	tAFF    = "0:AFF"
)

var traceMasters = map[string]tokenMeta{
	tUSDT: {Address: tUSDT, Symbol: "USDT", Decimals: 6, DecimalsKnown: true},
	tAFF:  {Address: tAFF, Symbol: "affUSDe", Decimals: 6, DecimalsKnown: true},
}

func txMsg(hash, source, dest string) traceMsg {
	return traceMsg{Hash: hash, Source: source, Destination: dest, Value: "1"}
}

func tx(hash, account string, in traceMsg, out ...traceMsg) traceTx {
	return traceTx{Account: account, Hash: hash, In: in, Out: out}
}

// affluentTree mirrors a real async vault deposit: wallet → own jetton
// wallet → vault jetton wallet → vault contract (notify) + excess change
// back. No receipt exists in-trace.
func affluentTree() *traceEnvelope {
	txs := map[string]traceTx{
		"h1": tx("h1", tWallet, txMsg("ext", "", tWallet), txMsg("m1", tWallet, tJW1)),
		"h2": tx("h2", tJW1, txMsg("m1", tWallet, tJW1), txMsg("m2", tJW1, tJW2)),
		"h3": tx("h3", tJW2, txMsg("m2", tJW1, tJW2), txMsg("m3", tJW2, tVault), txMsg("m4", tJW2, tWallet)),
		"h4": tx("h4", tVault, txMsg("m3", tJW2, tVault)),
		"h5": tx("h5", tWallet, txMsg("m4", tJW2, tWallet)),
	}
	out := jettonTransfer(tUSDT, tWallet, tVault, "250207042")
	out.ActionID, out.TraceID = "dep1", "trace-dep"
	out.Details = mustDetails(map[string]any{
		"asset": tUSDT, "sender": tWallet, "receiver": tVault, "amount": "250207042",
		"sender_jetton_wallet": tJW1, "receiver_jetton_wallet": tJW2,
	})
	return &traceEnvelope{
		TraceID: "trace-dep", Info: traceInfo{State: "complete"},
		Order: []string{"h1", "h2", "h3", "h4", "h5"},
		Txs:   txs, Actions: []tonAction{out},
	}
}

// syncedMintTree mirrors a synchronous conversion: the outflow subtree pays
// a receipt of a different asset back to the wallet in-trace.
func syncedMintTree() *traceEnvelope {
	txs := map[string]traceTx{
		"h1": tx("h1", tWallet, txMsg("ext", "", tWallet), txMsg("m1", tWallet, tJW1)),
		"h2": tx("h2", tJW1, txMsg("m1", tWallet, tJW1), txMsg("m2", tJW1, tVault)),
		"h3": tx("h3", tVault, txMsg("m2", tJW1, tVault), txMsg("m3", tVault, tWallet)),
		"h4": tx("h4", tWallet, txMsg("m3", tVault, tWallet)),
	}
	out := jettonTransfer(tUSDT, tWallet, tVault, "250000000")
	out.ActionID, out.TraceID = "dep2", "trace-sync"
	receipt := jettonTransfer(tAFF, tVault, tWallet, "249117000")
	receipt.ActionID, receipt.TraceID = "mint2", "trace-sync"
	return &traceEnvelope{
		TraceID: "trace-sync", Info: traceInfo{State: "complete"},
		Order: []string{"h1", "h2", "h3", "h4"},
		Txs:   txs, Actions: []tonAction{out, receipt},
	}
}

var _ = Describe("Trace trees", func() {
	forms := []string{tWallet}

	It("walks downstream message edges in order", func() {
		tree := affluentTree()
		reached, accounts := downstream(tree, "h1")
		Expect(reached).To(Equal([]string{"h1", "h2", "h3", "h4", "h5"}))
		Expect(accounts).To(HaveKeyWithValue(tVault, true))
		Expect(accounts).To(HaveKeyWithValue(tWallet, true))
	})

	It("finds the wallet's originating transaction", func() {
		Expect(walletOutflowTx(affluentTree(), forms)).To(Equal("h1"))
		Expect(walletOutflowTx(&traceEnvelope{}, forms)).To(Equal(""))
	})

	It("round-trips trees with and without decoded payloads", func() {
		tree := affluentTree()
		raw, err := json.Marshal(map[string]any{"traces": []any{tree}})
		Expect(err).NotTo(HaveOccurred())
		var env tracesResponse
		Expect(json.Unmarshal(raw, &env)).To(Succeed())
		Expect(env.Traces).To(HaveLen(1))
		Expect(env.Traces[0].Order).To(HaveLen(5))
		// Bare-string and wrapper shapes both decode.
		var body decodedBody
		Expect(json.Unmarshal([]byte(`{"@type":"jetton_internal_transfer","query_id":"7","amount":"42","from":"0:X","response_address":{"@type":"addr_std","workchain_id":"0","address":"BB"}}`), &body)).To(Succeed())
		Expect(body.Amount).To(Equal("42"))
		Expect(body.From).To(Equal("0:X"))
		Expect(body.ResponseAddress).To(Equal("0:BB"))
	})
	It("detects protocol intake past jetton reservoirs", func() {
		tree := affluentTree()
		m := detectVaultPattern(tree, "h1", forms, jettonReservoirs(tree.Actions))
		Expect(m.vault).To(Equal(tVault))
		// Without reservoir knowledge the first hop wins; with it the vault does.
		raw := detectVaultPattern(tree, "h1", forms, nil)
		Expect(raw.vault).To(Equal(tJW1))
	})

	It("ignores pure wallet-to-wallet chains", func() {
		tree := &traceEnvelope{
			Order: []string{"h1", "h2"},
			Txs: map[string]traceTx{
				"h1": tx("h1", tWallet, txMsg("ext", "", tWallet), txMsg("m1", tWallet, tFriend)),
				"h2": tx("h2", tFriend, txMsg("m1", tWallet, tFriend)),
			},
		}
		Expect(detectVaultPattern(tree, "h1", forms, nil).vault).To(Equal(tFriend))
	})

	It("keeps async deposits separate with vault labeling", func() {
		tree := affluentTree()
		group := tree.Actions
		legs, _, err := correlateDeposit(context.Background(), tWallet, forms, traceMasters,
			stubValue{v: 250.21, ok: true}, group, tree, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(BeEmpty())
		// The outflow still classifies as a labeled transfer, not a guess.
		outflows := make(map[string]VaultSpend)
		vaults := map[string]vaultMarker{"dep1": {vault: tVault}}
		act, err := transferActivity(context.Background(), tWallet, forms, traceMasters,
			stubValue{unit: 1, unitOK: true}, outflows, vaults, nil, group[0])
		Expect(err).NotTo(HaveOccurred())
		Expect(act.Type).To(Equal(brokerage.ActivityTransferOut))
		Expect(act.Description).To(ContainSubstring("protocol deposit"))
		Expect(act.Description).To(ContainSubstring("asynchronously"))
		Expect(act.IsExternal).To(BeTrue()) // vault is not a tracked wallet
	})

	It("pairs synchronous receipts through USD without comparing amounts", func() {
		tree := syncedMintTree()
		group := []tonAction{tree.Actions[0]}
		legs, _, err := correlateDeposit(context.Background(), tWallet, forms, traceMasters,
			stubValue{v: 500, ok: true}, group, tree, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(HaveLen(2))
		Expect(legs[0].Type).To(Equal(brokerage.ActivitySell))
		Expect(legs[0].RawType).To(Equal("STAKE_SELL"))
		Expect(legs[0].Symbol.Symbol).To(Equal("USDT"))
		Expect(legs[0].Units).To(Equal(250.0))
		Expect(legs[1].Type).To(Equal(brokerage.ActivityBuy))
		Expect(legs[1].RawType).To(Equal("STAKE_BUY"))
		Expect(legs[1].Symbol.Symbol).To(Equal("AFFSENTORAENT"))
		Expect(legs[1].Units).To(Equal(249.117))
		Expect(legs[1].Amount).To(Equal(legs[0].Amount))
		Expect(legs[1].TradeDate.Unix()).To(Equal(legs[0].TradeDate.Unix() + 1))
		Expect(legs[0].Description).To(ContainSubstring("trace-correlated"))
	})

	It("never pairs same-asset returns or cross-trace lookalikes", func() {
		tree := affluentTree()
		// Same-asset excess back to the wallet is change, not a conversion.
		excess := tonTransfer(tJW2, tWallet, "22037597")
		excess.ActionID, excess.TraceID = "ex1", "trace-dep"
		tree.Actions = append(tree.Actions, excess)
		legs, _, err := correlateDeposit(context.Background(), tWallet, forms, traceMasters,
			stubValue{v: 250.21, ok: true}, tree.Actions[:1], tree, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(BeEmpty())
		// A receipt-shaped action in another trace never pairs: the outflow
		// belongs to trace-dep, not to the other tree.
		other := syncedMintTree()
		legs, _, err = correlateDeposit(context.Background(), tWallet, forms, traceMasters,
			stubValue{v: 250.21, ok: true}, tree.Actions[:1], other, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(BeEmpty())
	})

	It("verifies interpreted legs against complete trees", func() {
		tree := affluentTree()
		stake := tonAction{ActionID: "st", TraceID: "trace-dep", Success: true, Type: "stake_deposit",
			Details: mustDetails(map[string]any{
				"provider": "p", "stake_holder": tWallet, "pool": "0:NOWHERE",
				"amount": "1000000000", "tokens_minted": "1000000000", "asset": tUSDT,
			})}
		Expect(verifyDerived(tree, []tonAction{stake})).To(BeFalse())
		stake.Details = mustDetails(map[string]any{
			"provider": "p", "stake_holder": tWallet, "pool": tVault,
			"amount": "1000000000", "tokens_minted": "1000000000", "asset": tUSDT,
		})
		Expect(verifyDerived(tree, []tonAction{stake})).To(BeTrue())
		Expect(verifyDerived(&traceEnvelope{}, []tonAction{stake})).To(BeTrue())
		// Malformed derived details cannot contradict: keep interpreted.
		stake.Details = json.RawMessage(`broken`)
		Expect(verifyDerived(tree, []tonAction{stake})).To(BeTrue())
	})

	It("classifies groups end to end with trace context", func() {
		tree := syncedMintTree()
		tractx := &traceContext{tree: tree, vaults: map[string]vaultMarker{}}
		legs, err := classifyActionGroup(context.Background(), tWallet, forms, traceMasters,
			stubValue{v: 500, ok: true}, make(map[string]VaultSpend), []tonAction{tree.Actions[0]}, tractx, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(legs).To(HaveLen(2))
		Expect(legs[0].RawType).To(Equal("STAKE_SELL"))
		// Nil context keeps the old transfer behavior.
		solo, err := classifyActionGroup(context.Background(), tWallet, forms, traceMasters,
			stubValue{unit: 1, unitOK: true}, make(map[string]VaultSpend), []tonAction{tree.Actions[0]}, nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(solo).To(HaveLen(1))
		Expect(solo[0].Type).To(Equal(brokerage.ActivityTransferOut))
	})
})

var _ = Describe("Real Affluent traces", func() {
	const (
		// Fresh stranger traces (refetched 2026-09-11); all wallets below
		// are synthetic placeholders, only the vault/master constants stay.
		stagAWallet = "0:0001000000000000000000000000000000000000000000000000000000000000"
		stagAJWU    = "0:0003000000000000000000000000000000000000000000000000000000000000"
		stagAJWS    = "0:0004000000000000000000000000000000000000000000000000000000000000"
		stagBWallet = "0:0017000000000000000000000000000000000000000000000000000000000000"
		stagBJWU    = "0:0018000000000000000000000000000000000000000000000000000000000000"
		stagBJWS    = "0:0015000000000000000000000000000000000000000000000000000000000000"
		unstWallet  = "0:0011000000000000000000000000000000000000000000000000000000000000"
		unstJWU     = "0:0014000000000000000000000000000000000000000000000000000000000000"
		unstJWS     = "0:0013000000000000000000000000000000000000000000000000000000000000"
		affV1       = "0:4444444444444444444444444444444444444444444444444444444444444444"
		affV2       = "0:5555555555555555555555555555555555555555555555555555555555555555"
	)
	masters := map[string]tokenMeta{
		usdMaster: {Address: usdMaster, Symbol: "USDT", Decimals: 6, DecimalsKnown: true},
		affV1:     {Address: affV1, Symbol: "affUSDe", Decimals: 8, DecimalsKnown: true},
		affV2:     {Address: affV2, Symbol: "affUSDe", Decimals: 8, DecimalsKnown: true},
	}
	load := func(name string) *traceEnvelope {
		raw, err := os.ReadFile("testdata/" + name + ".json")
		Expect(err).NotTo(HaveOccurred())
		var tree traceEnvelope
		Expect(json.Unmarshal(raw, &tree)).To(Succeed())
		Expect(tree.complete()).To(BeTrue())
		return &tree
	}
	classify := func(wallet string, tree *traceEnvelope, jw map[string]bool) []*brokerageActivity {
		tractx := &traceContext{tree: tree, vaults: map[string]vaultMarker{}}
		legs, err := classifyActionGroup(context.Background(), wallet, []string{testWallet, wallet}, masters,
			stubValue{v: 1000, ok: true}, make(map[string]VaultSpend), tree.Actions, tractx, jw, nil)
		Expect(err).NotTo(HaveOccurred())
		return legs
	}
	pair := func(legs []*brokerageActivity) (sell, buy brokerage.Activity) {
		Expect(legs).To(HaveLen(2))
		Expect(legs[0].Type).To(Equal(brokerage.ActivitySell))
		Expect(legs[0].RawType).To(Equal("STAKE_SELL"))
		Expect(legs[1].Type).To(Equal(brokerage.ActivityBuy))
		Expect(legs[1].RawType).To(Equal("STAKE_BUY"))
		Expect(legs[1].Amount).To(Equal(legs[0].Amount))
		Expect(legs[1].TradeDate.Unix()).To(Equal(legs[0].TradeDate.Unix() + 1))
		Expect(legs[0].Description).To(ContainSubstring("trace-correlated"))
		return *legs[0], *legs[1]
	}

	It("stakes 9987.24 USDT into 9773.55 AFFSENTORAENT via query_id echo", func() {
		sell, buy := pair(classify(stagAWallet, load("staking1"), map[string]bool{stagAJWS: true}))
		Expect(sell.Symbol.Symbol).To(Equal("USDT"))
		Expect(sell.Units).To(Equal(9987.235406))
		Expect(buy.Symbol.Symbol).To(Equal("AFFSENTORAENT"))
		Expect(buy.Units).To(Equal(9773.54689423))
	})

	It("pairs structurally when the query_id echo is missing", func() {
		tree := load("staking1")
		for _, a := range tree.Actions {
			if a.Type != "jetton_transfer" {
				continue
			}
			var d map[string]any
			Expect(json.Unmarshal(a.Details, &d)).To(Succeed())
			d["query_id"] = "0"
			raw, err := json.Marshal(d)
			Expect(err).NotTo(HaveOccurred())
			a.Details = raw
		}
		// No jetton-wallet set either: response_address alone attributes.
		sell, buy := pair(classify(stagAWallet, tree, nil))
		Expect(sell.Units).To(Equal(9987.235406))
		Expect(buy.Units).To(Equal(9773.54689423))
	})

	It("keeps separate when neither query_id nor attribution matches", func() {
		tree := load("staking1")
		for h, tx := range tree.Txs {
			changed := false
			for i, m := range tx.Out {
				if m.Content.Decoded.Type != "jetton_internal_transfer" {
					continue
				}
				m.Content.Decoded.ResponseAddress = "0:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
				tx.Out[i] = m
				changed = true
			}
			if changed {
				tree.Txs[h] = tx
			}
		}
		for _, a := range tree.Actions {
			if a.Type != "jetton_transfer" {
				continue
			}
			var d map[string]any
			Expect(json.Unmarshal(a.Details, &d)).To(Succeed())
			d["query_id"] = "0"
			raw, err := json.Marshal(d)
			Expect(err).NotTo(HaveOccurred())
			a.Details = raw
		}
		legs := classify(stagAWallet, tree, nil)
		for _, l := range legs {
			Expect(l.RawType).NotTo(Equal("STAKE_SELL"))
			Expect(l.RawType).NotTo(Equal("STAKE_BUY"))
		}
	})

	It("stakes 21 USDT into 20.55 AFFSENTORAENT", func() {
		sell, buy := pair(classify(stagBWallet, load("staking2"), map[string]bool{stagBJWS: true}))
		Expect(sell.Symbol.Symbol).To(Equal("USDT"))
		Expect(sell.Units).To(Equal(21.0))
		Expect(buy.Symbol.Symbol).To(Equal("AFFSENTORAENT"))
		Expect(buy.Units).To(Equal(20.55117042))
	})

	It("unstakes 19.57 AFFSENTORAENT into 19.97 USDT", func() {
		sell, buy := pair(classify(unstWallet, load("unstaking3"), nil))
		Expect(sell.Symbol.Symbol).To(Equal("AFFSENTORAENT"))
		Expect(sell.Units).To(Equal(19.5729299))
		Expect(buy.Symbol.Symbol).To(Equal("USDT"))
		Expect(buy.Units).To(Equal(19.970967))
		Expect(sell.Description).To(ContainSubstring("unstake"))
	})
})

var _ = Describe("Trace verification fallback", func() {
	It("allows the valued fallback when trace calls fail", func() {
		var server *httptest.Server
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/accountStates":
				writeJSON(w, map[string]any{"accounts": []any{map[string]any{
					"address": testRaw, "balance": "1000000000", "status": "active",
				}}, "address_book": map[string]any{testRaw: map[string]any{"user_friendly": testWallet}}})
			case "/jetton/wallets":
				writeJSON(w, map[string]any{"jetton_wallets": []any{map[string]any{
					"address": "0:JW", "balance": "1000000", "owner": testRaw, "jetton": usdMaster,
				}}, "metadata": map[string]any{}})
			case "/jetton/masters":
				writeJSON(w, map[string]any{"jetton_masters": []any{map[string]any{
					"address":        usdMaster,
					"jetton_content": map[string]any{"symbol": "USDT", "name": "Tether", "decimals": "6"},
				}}, "metadata": map[string]any{}})
			case "/actions":
				if r.URL.Query().Get("tx_hash") != "" {
					writeJSON(w, map[string]any{"actions": []any{}})
					return
				}
				writeJSON(w, map[string]any{"actions": []any{
					action("dep1", "trace-dep", "jetton_transfer", true, map[string]any{
						"asset": usdMaster, "sender": testRaw,
						"receiver": usdMaster, "amount": "1000000",
					}),
				}})
			case "/traces":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				Fail(r.URL.Path)
			}
		}
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			handler(w, r)
		}))
		defer server.Close()
		c := testClient(server)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		acts := snap.Activities["ton-"+testRaw]
		// Transfer out plus the fallback synthetic buy for the held shares.
		Expect(acts).To(HaveLen(2))
	})

	It("labels vault deposits end to end without pairing receipts", func() {
		tree := affluentTree()
		// Remap the fixture onto the test wallet's address forms.
		remap := map[string]string{tWallet: testRaw}
		for h, t := range tree.Txs {
			if v, ok := remap[t.Account]; ok {
				t.Account = v
			}
			if v, ok := remap[t.In.Source]; ok {
				t.In.Source = v
			}
			if v, ok := remap[t.In.Destination]; ok {
				t.In.Destination = v
			}
			for i, m := range t.Out {
				if v, ok := remap[m.Source]; ok {
					t.Out[i].Source = v
				}
				if v, ok := remap[m.Destination]; ok {
					t.Out[i].Destination = v
				}
			}
			tree.Txs[h] = t
		}
		for i, a := range tree.Actions {
			var d map[string]any
			Expect(json.Unmarshal(a.Details, &d)).To(Succeed())
			if s, ok := d["sender"].(string); ok {
				if v, ok := remap[s]; ok {
					d["sender"] = v
				}
			}
			raw, err := json.Marshal(d)
			Expect(err).NotTo(HaveOccurred())
			tree.Actions[i].Details = raw
		}
		var server *httptest.Server
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/accountStates":
				writeJSON(w, map[string]any{"accounts": []any{map[string]any{
					"address": testRaw, "balance": "1000000000", "status": "active",
				}}, "address_book": map[string]any{testRaw: map[string]any{"user_friendly": testWallet}}})
			case "/jetton/wallets":
				writeJSON(w, map[string]any{"jetton_wallets": []any{}, "metadata": map[string]any{}})
			case "/jetton/masters":
				writeJSON(w, map[string]any{"jetton_masters": []any{map[string]any{
					"address":        tUSDT,
					"jetton_content": map[string]any{"symbol": "USDT", "name": "Tether", "decimals": "6"},
				}}, "metadata": map[string]any{}})
			case "/actions":
				if r.URL.Query().Get("tx_hash") != "" {
					writeJSON(w, map[string]any{"actions": tree.Actions})
					return
				}
				writeJSON(w, map[string]any{"actions": tree.Actions})
			case "/traces":
				writeJSON(w, map[string]any{"traces": []any{tree}})
			default:
				Fail(r.URL.Path)
			}
		}
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			handler(w, r)
		}))
		defer server.Close()
		c := testClient(server)
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		acts := snap.Activities["ton-"+testRaw]
		Expect(acts).To(HaveLen(1))
		Expect(acts[0].Type).To(Equal(brokerage.ActivityTransferOut))
		Expect(acts[0].Description).To(ContainSubstring("protocol deposit"))
		for _, a := range acts {
			Expect(a.RawType).NotTo(Equal("STAKE_SELL"))
		}
	})
})
