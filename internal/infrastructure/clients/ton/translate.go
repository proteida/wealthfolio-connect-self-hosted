package ton

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

// tokenMeta is resolved symbol/decimals for one Jetton master address.
type tokenMeta struct {
	Address       string
	Symbol        string
	Name          string
	Decimals      int
	DecimalsKnown bool
}

// TonToken is one normalized holding: native TON or a Jetton balance in
// human units.
type TonToken struct {
	Symbol          string
	Name            string
	TokenAddr       string // "" for native TON, Jetton master address otherwise
	WalletAddr      string // Jetton wallet contract address, "" for native
	Quantity        float64
	Decimals        int
	DecimalsAssumed bool // true when the indexer gave no decimals and 9 was assumed
}

// TonWalletData bundles one wallet's payload for the translator.
type TonWalletData struct {
	Address           string // as configured
	Canonical         string // raw address echoed by the API
	Friendly          string // user-friendly address from the address book
	Tokens            []TonToken
	Activities        []brokerageActivity
	ActivitiesFetched bool
	// MasterOutflows accumulates valued outflows sent directly to Jetton
	// master contracts (vault/pool deposits), keyed by master address.
	MasterOutflows map[string]VaultSpend
	// AllowVaultFallback permits attributing basis to shares whose receipt
	// was never causally established: trace verification failed, or master
	// outflows provably lack an in-trace receipt (async mints). Verified
	// conversions never need it.
	AllowVaultFallback bool
	// Basis overrides the computed cost basis when set (non-nil). Used when
	// the current window is a slice of a multi-run backfill: lots are
	// reconciled against the stored ledger instead of the window alone.
	Basis map[string]float64
}

// VaultSpend is the valued spend into one master contract: total USD and the
// latest outflow time, used to attribute basis (and a synthetic buy leg) to
// shares that arrive without any transfer action.
type VaultSpend struct {
	Amount float64
	Time   int64
}

// brokerageActivity aliases the domain type for brevity.
type brokerageActivity = brokerage.Activity

// buildActivity builds the domain activity shared by every TON movement.
// Legs keep exact chain values (cleaned to 8dp against float tails); no
// cent-rounding is applied anywhere, so sub-cent transfers of any asset —
// stablecoin or otherwise — survive the ledger.
func buildActivity(wallet, symbol, tokenAddr string, qty float64, typ brokerage.ActivityType,
	rawType, counterparty, hash, group string, nowUnix int64, amount, price float64, note string,
) *brokerageActivity {
	if qty <= 0 || hash == "" || symbol == "" || nowUnix <= 0 {
		return nil
	}
	ticker := cexcommon.NormalizeAsset(symbol)
	// RawSymbol carries the display ticker, never the internal master
	// address; the master stays in the fingerprint input via tokenAddr.
	// Units, value and fee are rounded to 8dp so float serialization stays
	// clean (100.0, not 100.00000000000004) while IDs keep full precision.
	// Price keeps full float64 precision on purpose: the app canonicalizes
	// trade amounts as quantity × unit_price in exact decimal arithmetic, so
	// a rounded price re-times quantity into cash dust. Conversion legs
	// additionally carry a role bias (see biasedPrices) that guarantees the
	// stored buy total never exceeds the stored sell total.
	units, amount := round8(math.Abs(qty)), round8(amount)
	// Fingerprint immutable transfer content so repeated syncs and overlapping
	// pages produce the same ID. Direction is deliberately excluded: the
	// action hash plus sides already identify a leg, and this lets type-only
	// reclassifications update rows in place instead of orphaning them.
	identity := struct {
		Wallet, Symbol, Token, Amount, Hash, Counterparty string
	}{
		wallet, ticker, tokenAddr,
		strconv.FormatFloat(math.Abs(qty), 'g', -1, 64),
		hash, counterparty,
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return nil
	}
	key := fmt.Sprintf("%x", sha256.Sum256(raw))
	accountID := walletAccountID(wallet)
	description := "On-chain TON transfer; historical valuation unavailable"
	if note != "" {
		description += "; " + note
	}
	return &brokerageActivity{
		ID: idJoin(accountID, key), AccountID: accountID, SourceRecordID: key,
		SourceGroupID: group, ExternalReferenceID: hash,
		Type: typ, RawType: rawType, Units: units,
		Price: price, Amount: amount,
		Fee: 0, FeeAsset: "",
		TradeDate: time.Unix(nowUnix, 0).UTC(), Currency: brokerage.Currency{Code: "USD"},
		Symbol: &brokerage.Symbol{Symbol: ticker, RawSymbol: ticker,
			Type:     brokerage.SymbolType{Code: "CRYPTO", IsSupported: true},
			Exchange: brokerage.Exchange{Code: nativeTONSymbol}, Currency: brokerage.Currency{Code: "USD"}},
		ProviderType: "ton", SourceSystem: "ton", NeedsReview: true, Description: description,
	}
}

// TranslateTON folds every wallet's tokens into a single connection with one
// account per wallet. Unpriced holdings are kept as positions with zero price
// (dust included), mirroring the other on-chain integration.
func TranslateTON(wallets []TonWalletData) domainsync.BrokerSnapshot {
	now := time.Now().UTC()
	connection := brokerage.Connection{
		ID:              "ton-conn",
		AuthorizationID: "ton-auth",
		BrokerageName:   nativeTONSymbol,
		BrokerageSlug:   "ton",
		DisplayName:     "TON (Toncoin)",
		Name:            nativeTONSymbol,
		Status:          brokerage.ConnectionActive,
		UpdatedAt:       now,
	}
	accounts := make([]brokerage.Account, 0, len(wallets))
	holdings := make([]brokerage.Holdings, 0, len(wallets))
	activities := make(map[string][]brokerage.Activity, len(wallets))
	for _, w := range wallets {
		accountID := walletAccountID(canonicalWallet(w))
		var totalUSD, stableCash float64
		// Basis recomputed from a truncated window omits the middle of
		// history and would publish a wrong number: publish unknown (zero)
		// until a complete window proves the full ledger. A caller-supplied
		// Basis (reconciled against stored history) takes precedence.
		basis := w.Basis
		if basis == nil {
			basis = make(map[string]float64)
			if w.ActivitiesFetched {
				basis = averageBasis(w.Activities)
			}
		}
		if w.AllowVaultFallback {
			for _, attr := range vaultAttributions(w) {
				if basis[attr.symbol] != 0 {
					continue
				}
				basis[attr.symbol] = attr.price
				if act := attr.buy(canonicalWallet(w)); act != nil {
					w.Activities = append(w.Activities, *act)
				}
			}
		}
		var positions []brokerage.Position
		for _, t := range w.Tokens {
			if t.Quantity <= 0 {
				continue
			}
			symbol := cexcommon.NormalizeAsset(t.Symbol)
			if isStablecoin(symbol) {
				// Stablecoins are valued at an explicit $1 policy into
				// cash, matching the CEX convention. They must never
				// vanish: unknown portfolio value is not a zero balance.
				stableCash += t.Quantity
				totalUSD += t.Quantity
				continue
			}
			positions = append(positions, brokerage.Position{
				Symbol: brokerage.Symbol{
					Symbol:      symbol,
					RawSymbol:   symbol,
					Name:        t.Name,
					Description: t.Name,
					Type:        brokerage.SymbolType{Code: "CRYPTO", IsSupported: true},
					Exchange:    brokerage.Exchange{Code: nativeTONSymbol, Name: nativeTONSymbol},
					Currency:    brokerage.Currency{Code: "USD"},
				},
				Units:                t.Quantity,
				AveragePurchasePrice: basis[symbol],
				Currency:             brokerage.Currency{Code: "USD"},
			})
		}
		label := "Wallet " + shortAddr(displayWallet(w))
		acc := brokerage.Account{
			ID:                     accountID,
			Name:                   label,
			AccountNumber:          shortAddr(displayWallet(w)),
			Type:                   brokerage.AccountTypeCryptocurrency,
			RawType:                "CRYPTO_WALLET",
			Currency:               "USD",
			BalanceTotal:           totalUSD,
			BalanceCurrency:        "USD",
			BrokerageAuthorization: "ton-auth",
			InstitutionName:        nativeTONSymbol,
			SyncEnabled:            true,
			Status:                 "open",
			CreatedDate:            now,
			LastHoldingsSync:       &now,
			InitialHoldingsDone:    true,
		}
		if w.ActivitiesFetched {
			acc.InitialTxSyncDone = true
			acc.LastTxSync = &now
		}
		if len(w.Activities) > 0 {
			activities[accountID] = w.Activities
		}
		accounts = append(accounts, acc)
		holdings = append(holdings, brokerage.Holdings{
			AccountID: accountID,
			Balances: []brokerage.Balance{
				{Currency: brokerage.Currency{Code: "USD"}, Cash: stableCash},
			},
			Positions:  positions,
			CapturedAt: now,
		})
	}
	return domainsync.BrokerSnapshot{
		Connection: connection,
		Accounts:   accounts,
		Holdings:   holdings,
		Activities: activities,
	}
}

// averageBasis derives per-symbol remaining-position cost basis under the
// average-cost method: valued inbound legs (buys, deposits, inbound
// transfers) add lots chronologically, outbound legs (sells, outbound
// transfers) relieve quantity and cost proportionally. Averaging lifetime
// purchases without disposal relief overstates the basis after a full exit
// (buy 10 @ $1, sell all, buy 10 @ $3 must yield $3, not $2). Symbols
// without a feed stay at zero (unknown, never market price).
func averageBasis(activities []brokerage.Activity) map[string]float64 {
	ordered := append([]brokerage.Activity(nil), activities...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].TradeDate.Before(ordered[j].TradeDate)
	})
	type lot struct{ qty, cost float64 }
	lots := make(map[string]*lot)
	for _, a := range ordered {
		if a.Symbol == nil || a.Units <= 0 {
			continue
		}
		sym := cexcommon.NormalizeAsset(a.Symbol.Symbol)
		l, ok := lots[sym]
		if !ok {
			l = &lot{}
			lots[sym] = l
		}
		switch a.Type {
		case brokerage.ActivityBuy, brokerage.ActivityDeposit, brokerage.ActivityTransferIn:
			if a.Amount <= 0 {
				continue
			}
			l.qty += a.Units
			l.cost += a.Amount
		case brokerage.ActivitySell, brokerage.ActivityTransferOut:
			if l.qty <= 0 {
				continue
			}
			relief := a.Units
			if relief > l.qty {
				relief = l.qty
			}
			l.cost -= l.cost * relief / l.qty
			l.qty -= relief
		default:
			continue
		}
	}
	out := make(map[string]float64, len(lots))
	for symbol, l := range lots {
		if l.qty > 0 && l.cost > 0 {
			out[symbol] = round8(l.cost / l.qty)
		}
	}
	return out
}

// vaultAttribution pairs held shares that history never shows inbound with
// the valued master-bound outflows that must have funded them (async mints
// emit no transfer action).
type vaultAttribution struct {
	symbol, master string
	qty, amount    float64
	price          float64
	time           int64
}

// buy renders the attribution as a BUY leg dated at the latest funding
// outflow. Identity is the wallet/master pair only — never the amounts — so
// growing or shrinking a position updates the row in place (via the upsert
// conflict columns) instead of inserting a duplicate acquisition every sync.
func (v vaultAttribution) buy(wallet string) *brokerageActivity {
	act := buildActivity(wallet, v.symbol, v.master, v.qty,
		brokerage.ActivityBuy, "STAKE_BUY", v.master,
		"vault:"+v.master, v.master, v.time, v.amount, v.price,
		fmt.Sprintf("vault share acquisition of %g %s attributed from $%s deposits; no mint transfer indexed",
			v.qty, v.symbol, formatUSD(v.amount)))
	if act == nil {
		return nil
	}
	key := "vault:" + v.master
	act.ID = idJoin(walletAccountID(wallet), key)
	act.SourceRecordID = key
	return act
}

// vaultAttributions finds held tokens with zero inbound history and valued
// master-bound outflows. Genuinely free tokens (gifts with no payment) keep
// their zero basis.
func vaultAttributions(w TonWalletData) []vaultAttribution {
	inbound := make(map[string]bool)
	for _, a := range w.Activities {
		if a.Symbol == nil {
			continue
		}
		switch a.Type {
		case brokerage.ActivityBuy, brokerage.ActivityDeposit, brokerage.ActivityTransferIn:
			inbound[cexcommon.NormalizeAsset(a.Symbol.Symbol)] = true
		default:
			// Other activity types never mark inbound history.
		}
	}
	var out []vaultAttribution
	for _, t := range w.Tokens {
		if t.Quantity <= 0 || t.TokenAddr == "" {
			continue
		}
		symbol := cexcommon.NormalizeAsset(t.Symbol)
		if inbound[symbol] {
			continue
		}
		spend, ok := w.MasterOutflows[t.TokenAddr]
		if !ok || spend.Amount <= 0 {
			continue
		}
		out = append(out, vaultAttribution{
			symbol: symbol, master: t.TokenAddr,
			qty: t.Quantity, amount: round8(spend.Amount),
			price: round8(spend.Amount / t.Quantity), time: spend.Time,
		})
	}
	return out
}

// round8 trims float serialization tails; sub-dust results keep the
// original value so data is never destroyed.
func round8(v float64) float64 {
	if r := math.Round(v*1e8) / 1e8; r != 0 || v == 0 {
		return r
	}
	return v
}

// canonicalWallet prefers the indexer-echoed raw address so the same wallet
// configured in different forms still maps to one account. Raw addresses are
// case-sensitive and must never be lower-cased.
func canonicalWallet(w TonWalletData) string {
	if w.Canonical != "" {
		return w.Canonical
	}
	return w.Address
}

// displayWallet prefers the user-friendly form for labels.
func displayWallet(w TonWalletData) string {
	if w.Friendly != "" {
		return w.Friendly
	}
	return canonicalWallet(w)
}

// walletAccountID derives the stable per-wallet account slug.
func walletAccountID(wallet string) string { return "ton-" + wallet }

// idJoin joins account and fingerprint IDs.
func idJoin(accountID, key string) string { return accountID + ":" + key }

// shortAddr truncates long addresses for display, passing short ones through.
func shortAddr(a string) string {
	if len(a) < 10 {
		return a
	}
	return a[:6] + "..." + a[len(a)-4:]
}

// isStablecoin mirrors the shared CEX helper for the few USD-pegged Jettons
// without importing the CEX package into the wallet client.
func isStablecoin(symbol string) bool {
	switch strings.ToUpper(symbol) {
	case "USDT", "USDC", "DAI", "USDD", "PYUSD":
		return true
	}
	return false
}
