package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
)

var _ = Describe("Web3 v6 syncing", func() {
	const address = "0xabcdef0123456789abcdef0123456789abcdef01"
	var client *Web3Client
	var server *httptest.Server
	var handler http.HandlerFunc
	var now time.Time
	BeforeEach(func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			Expect(r.Header.Get("OK-ACCESS-SIGN")).NotTo(BeEmpty())
			handler(w, r)
		}))
		now = time.Unix(1800000000, 0)
		client = NewWeb3(Credentials{APIKey: "k", Secret: "s", Passphrase: "p"}, []Wallet{{Address: address}}, server.URL, server.Client())
		client.now = func() time.Time { return now }
	})
	AfterEach(func() { server.Close() })
	respond := func(w http.ResponseWriter, data any) {
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": data})
	}
	balance := func(chain, quantity string) map[string]any {
		return map[string]any{"chainIndex": chain, "symbol": "ETH", "tokenContractAddress": "0xtoken", "balance": quantity, "tokenPrice": "0"}
	}
	tokenResponse := func(w http.ResponseWriter, rows ...map[string]any) {
		respond(w, []any{map[string]any{"tokenAssets": rows}})
	}
	tx := func() web3Transaction {
		return web3Transaction{ChainIndex: "1", Hash: "0xhash", Tier: "2", Time: "1700000000123", From: []transactionParty{{Address: "0xother"}}, To: []transactionParty{{Address: address}}, Token: "0xtoken", Amount: "2", Symbol: "ETH", Status: "success", Fee: "0.01"}
	}
	It("uses exactly configured chains for balances and history without discovery", func() {
		client.wallets[0].Chains = []string{"1", "56"}
		var paths []string
		handler = func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			Expect(r.URL.Query().Get("chains")).To(Equal("1,56"))
			Expect(r.URL.Query().Get("address")).To(Equal(address))
			switch r.URL.Path {
			case "/api/v6/dex/balance/all-token-balances-by-address":
				Expect(r.URL.Query().Get("excludeRiskToken")).To(Equal("0"))
				tokenResponse(w, balance("1", "0"))
			case "/api/v6/dex/post-transaction/transactions-by-address":
				Expect(r.URL.Query().Get("limit")).To(Equal("20"))
				respond(w, []transactionPage{{TransactionList: []web3Transaction{tx()}}})
			default:
				Fail(r.URL.Path)
			}
		}
		snap, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(paths).To(HaveLen(2))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Accounts[0].LastTxSync).NotTo(BeNil())
		Expect(snap.Activities[walletAccountID(address)]).To(HaveLen(1))
	})
	It("discovers supported funded chains, preserves unpriced dust, caches and refreshes", func() {
		var discoveries int
		var balanceChains, historyChains []string
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v6/explorer/address/supported-chains":
				respond(w, []chainRow{{ChainIndex: "1", APIName: "address-active-chain"}, {ChainIndex: "56", APIName: "address-active-chain"}, {ChainIndex: "137", APIName: "address-active-chain"}, {ChainIndex: "10", APIName: "information-evm"}})
			case "/api/v6/explorer/address/address-active-chain":
				discoveries++
				respond(w, []chainRow{{ChainIndex: "1"}, {ChainIndex: "56"}, {ChainIndex: "137"}, {ChainIndex: "10"}, {ChainIndex: "1"}})
			case "/api/v6/dex/balance/supported/chain":
				respond(w, []chainRow{{ChainIndex: "1"}, {ChainIndex: "56"}, {ChainIndex: "10"}})
			case "/api/v6/dex/balance/all-token-balances-by-address":
				balanceChains = append(balanceChains, r.URL.Query().Get("chains"))
				tokenResponse(w, balance("1", "0.00000001"), balance("56", "0"), balance("137", "100"))
			case "/api/v6/dex/post-transaction/transactions-by-address":
				historyChains = append(historyChains, r.URL.Query().Get("chains"))
				respond(w, []transactionPage{{}})
			default:
				Fail(r.URL.Path)
			}
		}
		for range 2 {
			snap, err := client.Fetch(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Accounts).To(HaveLen(1))
			Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		}
		Expect(discoveries).To(Equal(1))
		Expect(balanceChains).To(Equal([]string{"1,56", "1", "1"}))
		// History runs over the active scope (1, 56), not just funded (1).
		Expect(historyChains).To(Equal([]string{"1,56", "1,56"}))
		now = now.Add(chainDiscoveryTTL)
		_, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(discoveries).To(Equal(2))
	})
	It("caches empty discovery and still scans history scope", func() {
		count := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			count++
			switch r.URL.Path {
			case "/api/v6/explorer/address/supported-chains":
				respond(w, []chainRow{{ChainIndex: "1", APIName: "address-active-chain"}})
			case "/api/v6/explorer/address/address-active-chain", "/api/v6/dex/balance/supported/chain":
				respond(w, []chainRow{{ChainIndex: "1"}})
			case "/api/v6/dex/balance/all-token-balances-by-address":
				tokenResponse(w, balance("1", "0"))
			case "/api/v6/dex/post-transaction/transactions-by-address":
				// Chain 1 is active but unfunded (e.g. fully withdrawn):
				// history still scans it, completing empty.
				respond(w, []transactionPage{{}})
			default:
				Fail(r.URL.Path)
			}
		}
		for range 2 {
			snap, err := client.Fetch(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		}
		// Run 1: 3 discovery + 1 probe + 1 history. Run 2 (cached): history only.
		Expect(count).To(Equal(6))
	})
	It("keeps the account but marks history incomplete with no scope", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v6/explorer/address/supported-chains":
				respond(w, []chainRow{{ChainIndex: "1", APIName: "address-active-chain"}})
			case "/api/v6/explorer/address/address-active-chain", "/api/v6/dex/balance/supported/chain":
				respond(w, []chainRow{})
			case "/api/v6/dex/balance/all-token-balances-by-address":
				tokenResponse(w, balance("1", "0"))
			default:
				Fail(r.URL.Path)
			}
		}
		snap, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		// Unknown scope must never read as completed empty history.
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Accounts[0].LastTxSync).To(BeNil())
	})

	It("batches configured and discovered chains at 50", func() {
		chains := make([]string, 51)
		rows := make([]chainRow, 51)
		tokens := make([]map[string]any, 51)
		for i := range chains {
			chains[i] = strconv.Itoa(i + 1)
			rows[i] = chainRow{ChainIndex: chains[i], APIName: "address-active-chain"}
			tokens[i] = balance(chains[i], "1")
		}
		var batches []int
		handler = func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v6/explorer/address/supported-chains", "/api/v6/explorer/address/address-active-chain", "/api/v6/dex/balance/supported/chain":
				respond(w, rows)
			case "/api/v6/dex/balance/all-token-balances-by-address":
				n := len(strings.Split(r.URL.Query().Get("chains"), ","))
				batches = append(batches, n)
				Expect(n).To(BeNumerically("<=", 50))
				tokenResponse(w, tokens...)
			case "/api/v6/dex/post-transaction/transactions-by-address":
				n := len(strings.Split(r.URL.Query().Get("chains"), ","))
				batches = append(batches, n)
				Expect(r.URL.Query().Get("limit")).To(Equal("20"))
				respond(w, []transactionPage{{}})
			default:
				Fail(r.URL.Path)
			}
		}
		_, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(batches).To(Equal([]int{50, 1, 50, 1, 50, 1}))
		batches = nil
		client.wallets[0].Chains = chains
		_, err = client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(batches).To(Equal([]int{50, 1, 50, 1}))
	})
	It("paginates overlapping history with stable IDs and preserves swap legs", func() {
		client.wallets[0].Chains = []string{"1"}
		outgoing := tx()
		outgoing.From, outgoing.To = outgoing.To, outgoing.From
		outgoing.Token = "0xusdc"
		outgoing.Symbol = "USDC"
		outgoing.Amount = "6000"
		var cursors []string
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "balance/") {
				tokenResponse(w)
				return
			}
			cursor := r.URL.Query().Get("cursor")
			cursors = append(cursors, cursor)
			Expect(r.URL.Query().Has("tokenContractAddress")).To(BeFalse())
			Expect(r.URL.Query().Get("end")).To(Equal(strconv.FormatInt(now.UnixMilli(), 10)))
			if cursor == "" {
				respond(w, []transactionPage{{Cursor: "next", Transactions: []web3Transaction{tx(), outgoing}}})
				return
			}
			Expect(cursor).To(Equal("next"))
			respond(w, []transactionPage{{TransactionList: []web3Transaction{tx(), outgoing}}})
		}
		first, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		second, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Activities).To(Equal(second.Activities))
		acts := first.Activities[walletAccountID(address)]
		Expect(acts).To(HaveLen(2))
		Expect(acts[0].Type).To(Equal(brokerage.ActivityTransferIn))
		Expect(acts[1].Type).To(Equal(brokerage.ActivityTransferOut))
		Expect(acts[0].SourceGroupID).To(Equal(acts[1].SourceGroupID))
		Expect(acts[0].ID).NotTo(Equal(acts[1].ID))
		Expect(acts[0].Units).To(Equal(2.0))
		Expect(acts[0].Price).To(BeZero())
		Expect(acts[0].NeedsReview).To(BeTrue())
		Expect(acts[0].TradeDate).To(Equal(time.UnixMilli(1700000000123).UTC()))
		Expect(cursors).To(Equal([]string{"", "next", "", "next"}))
	})
	It("does not complete history on repeated cursors, malformed payloads or API errors", func() {
		client.wallets[0].Chains = []string{"1"}
		for _, mode := range []string{"cursor", "api", "json", "pages", "invalid"} {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "balance/") {
					tokenResponse(w, balance("1", "2"))
					return
				}
				switch mode {
				case "cursor":
					respond(w, []transactionPage{{Cursor: "stuck"}})
				case "api":
					fmt.Fprint(w, `{"code":"500","msg":"oops"}`)
				case "json":
					fmt.Fprint(w, `broken`)
				case "pages":
					respond(w, []transactionPage{{}, {}})
				case "invalid":
					bad := tx()
					bad.Amount = "bad"
					respond(w, []transactionPage{{Transactions: []web3Transaction{bad}}})
				}
			}
			snap, err := client.Fetch(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(snap.Accounts).To(HaveLen(1))
			Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
			Expect(snap.Accounts[0].LastTxSync).To(BeNil())
		}
	})
	It("does not cache failed discovery and skips an unreachable wallet", func() {
		for _, path := range []string{"/api/v6/explorer/address/supported-chains", "/api/v6/explorer/address/address-active-chain", "/api/v6/dex/balance/supported/chain", "/api/v6/dex/balance/all-token-balances-by-address"} {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					w.WriteHeader(503)
					return
				}
				respond(w, []chainRow{{ChainIndex: "1", APIName: "address-active-chain"}})
			}
			_, _, err := client.resolveChains(context.Background(), client.wallets[0])
			Expect(err).To(HaveOccurred())
			Expect(client.chainCache).To(BeEmpty())
		}
		snap, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(BeEmpty())
		client.wallets[0].Chains = []string{"1"}
		snap, err = client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(BeEmpty())
	})
	It("requires explicit chains for non-EVM addresses and keeps their case", func() {
		for _, addr := range []string{"SolAnaAddress", "0x" + strings.Repeat("z", 40)} {
			_, _, err := client.resolveChains(context.Background(), Wallet{Address: addr})
			Expect(err).To(HaveOccurred())
		}
		Expect(walletAccountID("SolAnaAddress")).NotTo(Equal(walletAccountID("solanaaddress")))
		Expect(NewWeb3(Credentials{}, nil, "", nil).baseURL).To(Equal("https://web3.okx.com"))
	})
	It("rejects malformed discovery balances without caching a false empty result", func() {
		for _, field := range []string{"balance", "tokenPrice"} {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "all-token-balances") {
					row := balance("1", "1")
					row[field] = "NaN"
					tokenResponse(w, row)
					return
				}
				respond(w, []chainRow{{ChainIndex: "1", APIName: "address-active-chain"}})
			}
			_, _, err := client.resolveChains(context.Background(), client.wallets[0])
			Expect(err).To(HaveOccurred())
			Expect(client.chainCache).To(BeEmpty())
		}
	})
	It("accepts a terminal empty page and ignores transactions from unrequested chains", func() {
		client.wallets[0].Chains = []string{"1"}
		handler = func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "balance/") {
				tokenResponse(w)
				return
			}
			if r.URL.Query().Get("cursor") == "" {
				row := tx()
				row.ChainIndex = "56"
				respond(w, []transactionPage{{Cursor: "next", Transactions: []web3Transaction{row}}})
				return
			}
			respond(w, []transactionPage{})
		}
		snap, err := client.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Activities[walletAccountID(address)]).To(BeEmpty())
	})

	It("validates balance API errors and transport errors", func() {
		handler = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"code":"400","msg":"bad"}`) }
		_, err := client.fetchWallet(context.Background(), Wallet{Address: address, Chains: []string{"1"}})
		Expect(err).To(HaveOccurred())
		client.baseURL = ":bad"
		_, err = web3Get[chainRow](context.Background(), client, "/test", url.Values{})
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Onchain activity normalization", func() {
	base := func() web3Transaction {
		return web3Transaction{ChainIndex: "1", Hash: "0xabc", Tier: "2", Time: "1700000000000", From: []transactionParty{{Address: "0xother"}}, To: []transactionParty{{Address: "0xwallet"}}, Amount: "1", Symbol: "ETH", Status: "success"}
	}
	It("ignores unsuccessful, blacklisted, unrelated, zero and self transfers", func() {
		for _, mode := range []string{"pending", "fail", "blacklist", "unrelated", "zero", "self"} {
			row := base()
			switch mode {
			case "pending", "fail":
				row.Status = mode
			case "blacklist":
				row.Blacklisted = true
			case "unrelated":
				row.To = nil
			case "zero":
				row.Amount = "0"
			case "self":
				row.From = row.To
			}
			act, err := transactionActivity("0xwallet", row, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(act).To(BeNil())
		}
	})
	It("keeps exact units on transfers of every asset", func() {
		row := base()
		row.Amount = "321.654987"
		act, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(act.Units).To(Equal(321.654987))
		row.Amount = "321.6549870000"
		same, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(same.Units).To(Equal(321.654987))
		Expect(same.ID).To(Equal(act.ID))
		// Sub-cent transfers survive for every asset, stablecoins included.
		row.Amount = "0.001"
		dust, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(dust.Units).To(Equal(0.001))
		row.Symbol = "USDT"
		row.Amount = "0.001"
		stable, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(stable.Units).To(Equal(0.001))
	})
	It("flags only untracked counterparties as external", func() {
		// base(): From 0xother → To 0xwallet, wallet is 0xwallet.
		tracked := map[string]bool{"0xwallet": true, "0xother": true}
		row := base()
		act, err := transactionActivity("0xwallet", row, tracked)
		Expect(err).NotTo(HaveOccurred())
		Expect(act.IsExternal).To(BeFalse())
		stranger := base()
		stranger.From = []transactionParty{{Address: "0xstranger"}}
		act, err = transactionActivity("0xwallet", stranger, tracked)
		Expect(err).NotTo(HaveOccurred())
		Expect(act.IsExternal).To(BeTrue())
		// Nil tracked set: everything outside the wallet is external.
		act, err = transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(act.IsExternal).To(BeTrue())
	})
	It("uses wallet-specific amounts and nets change outputs", func() {
		row := base()
		row.From = []transactionParty{{Address: "0xWALLET", Amount: "5"}}
		row.To = []transactionParty{{Address: "0xother", Amount: "3"}, {Address: "0xwallet", Amount: "2"}}
		row.Amount = "5"
		act, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(act.Type).To(Equal(brokerage.ActivityTransferOut))
		Expect(act.Units).To(Equal(3.0))
		row.From = []transactionParty{{Address: "0xother", Amount: "5"}}
		row.To = []transactionParty{{Address: "0xwallet", Amount: "2"}, {Address: "0xanother", Amount: "3"}}
		act, err = transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(act.Units).To(Equal(2.0))
	})
	It("ignores party ordering and formatting when identifying the same transfer", func() {
		row := base()
		row.From = []transactionParty{{Address: "0xB,0xA"}, {Address: "0xC"}}
		first, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		row.From = []transactionParty{{Address: "0xc"}, {Address: "0xa,0xb"}}
		row.Amount = "1.0000"
		row.Fee = "0.001"
		row.Time = "1700000001000"
		second, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(second.ID).To(Equal(first.ID))
		row.Token = "different"
		other, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(other.ID).NotTo(Equal(first.ID))
		row = base()
		row.ChainIndex = "56"
		other, err = transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(other.ID).NotTo(Equal(first.ID))
	})
	It("keeps IDs unique for opposite wallets in the same transaction", func() {
		row := base()
		first, err := transactionActivity("0xwallet", row, nil)
		Expect(err).NotTo(HaveOccurred())
		second, err := transactionActivity("0xother", row, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(first.ID).NotTo(Equal(second.ID))
	})
	It("rejects invalid numbers, identity, timestamps and ambiguous party amounts", func() {
		for _, mode := range []string{"amount", "from", "to", "mixed", "missing", "time", "hash", "symbol", "chain"} {
			row := base()
			switch mode {
			case "amount":
				row.Amount = "NaN"
			case "from":
				row.From = []transactionParty{{Address: "0xwallet", Amount: "bad"}}
			case "to":
				row.To = []transactionParty{{Address: "0xwallet", Amount: "-1"}}
			case "mixed":
				row.To = []transactionParty{{Address: "0xwallet", Amount: "1"}, {Address: "0xwallet"}}
			case "missing":
				row.From = []transactionParty{{Address: "0xwallet"}}
				row.To = []transactionParty{{Address: "0xwallet", Amount: "1"}}
			case "time":
				row.Time = "bad"
			case "hash":
				row.Hash = ""
			case "symbol":
				row.Symbol = ""
			case "chain":
				row.ChainIndex = ""
			}
			_, err := transactionActivity("0xwallet", row, nil)
			Expect(err).To(HaveOccurred(), mode)
		}
	})
})
