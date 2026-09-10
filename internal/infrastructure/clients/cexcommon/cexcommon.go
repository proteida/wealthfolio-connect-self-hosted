// Package cexcommon contains the shared translation helpers used by every
// CEX client (Binance, OKX, Bitget, Hyperliquid). Each individual client
// only needs to fetch raw balances and let cexcommon turn them into a
// BrokerSnapshot using a uniform set of conventions:
//
//   - Stablecoins (USDT, USDC, DAI, ...) collapse into the cash balance.
//   - Tiny dust positions (USD value < $1) are dropped.
//   - One brokerage Account per exchange (slug + "-spot").
package cexcommon

import (
	"strings"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
)

// Balance is the normalised per-asset row that each CEX client produces.
type Balance struct {
	Asset    string  // e.g. "BTC"
	Quantity float64 // free + locked
	PriceUSD float64
	USDValue float64 // priceUSD * quantity, supplied by the upstream when available
}

// Trade is one historical trade row. Optional — most CEX clients only fill
// balances on the initial pass.
type Trade struct {
	ID         string
	Symbol     string // e.g. "BTC-USDT"; used as a fallback when assets are unset
	BaseAsset  string
	QuoteAsset string
	Side       string // "buy" or "sell"
	Price      float64
	Quantity   float64
	Fee        float64
	FeeAsset   string
	// Note carries non-ledger facts (e.g. a maker rebate excluded from Fee)
	// into the activity description.
	Note      string
	Timestamp time.Time
}

// Snapshot bundles balances + optional trades for one CEX.
type Snapshot struct {
	Balances          []Balance
	Trades            []Trade
	ActivitiesFetched bool
	// Partial marks a snapshot with a failed upstream component (e.g. the
	// price feed). Partial snapshots keep unpriced assets as zero-price
	// positions and must never replace the last complete holdings.
	Partial bool
}

// Translate folds a CEX snapshot into a BrokerSnapshot. The resulting
// connection/account IDs are derived from the supplied slug ("okx",
// "binance", ...) so callers can produce stable rows.
func Translate(slug, displayName string, s Snapshot) domainsync.BrokerSnapshot {
	now := time.Now().UTC()
	accountID := slug + "-spot"

	var totalUSD float64
	for _, b := range s.Balances {
		totalUSD += b.USDValue
	}

	account := brokerage.Account{
		ID:                     accountID,
		Name:                   displayName + " Spot",
		Type:                   brokerage.AccountTypeCryptocurrency,
		RawType:                "CRYPTO_SPOT",
		Currency:               "USD",
		BalanceTotal:           totalUSD,
		BalanceCurrency:        "USD",
		BrokerageAuthorization: slug + "-auth",
		InstitutionName:        displayName,
		SyncEnabled:            true,
		Status:                 "open",
		CreatedDate:            now,
		LastHoldingsSync:       &now,
		InitialTxSyncDone:      s.ActivitiesFetched,
		InitialHoldingsDone:    true,
	}
	if s.ActivitiesFetched {
		account.LastTxSync = &now
	}

	connection := brokerage.Connection{
		ID:              slug + "-conn",
		AuthorizationID: slug + "-auth",
		BrokerageName:   displayName,
		BrokerageSlug:   slug,
		DisplayName:     displayName,
		Name:            displayName,
		Status:          brokerage.ConnectionActive,
		UpdatedAt:       now,
	}

	cashBalance := brokerage.Balance{
		Currency: brokerage.Currency{Code: "USD"},
	}
	positions := make([]brokerage.Position, 0, len(s.Balances))
	for _, b := range s.Balances {
		if b.Quantity == 0 {
			continue
		}
		asset := NormalizeAsset(b.Asset)
		if IsStablecoin(asset) {
			cashBalance.Cash += b.USDValue
			continue
		}
		// Owned but unpriced assets stay visible as zero-price positions:
		// unknown market value must not imply zero quantity. Only assets
		// with a known (even tiny) valuation are subject to the $1 dust
		// filter.
		if b.PriceUSD == 0 && b.USDValue == 0 {
			positions = append(positions, unvaluedPosition(slug, displayName, asset, b.Quantity))
			continue
		}
		if b.USDValue < 1 {
			continue
		}
		positions = append(positions, brokerage.Position{
			Symbol: brokerage.Symbol{
				Symbol:      asset,
				RawSymbol:   asset,
				Description: asset,
				Name:        asset,
				Type:        brokerage.SymbolType{Code: "CRYPTO", IsSupported: true, Description: "Cryptocurrency"},
				Exchange:    brokerage.Exchange{Code: strings.ToUpper(slug), Name: displayName},
				Currency:    brokerage.Currency{Code: "USD"},
			},
			Units: b.Quantity,
			Price: b.PriceUSD,
			// AveragePurchasePrice stays zero (unknown): the balance feed
			// carries market valuation, not acquisition history. Stamping
			// the market price as basis would reset the apparent cost
			// basis on every sync.
			Currency: brokerage.Currency{Code: "USD"},
		})
	}

	holding := brokerage.Holdings{
		AccountID:  accountID,
		Balances:   []brokerage.Balance{cashBalance},
		Positions:  positions,
		CapturedAt: now,
		Partial:    s.Partial,
	}

	activities := map[string][]brokerage.Activity{}
	if len(s.Trades) > 0 {
		acts := make([]brokerage.Activity, 0, len(s.Trades))
		for _, t := range s.Trades {
			base, quote := t.BaseAsset, t.QuoteAsset
			if base == "" || quote == "" {
				base, quote = splitPair(t.Symbol)
			}
			if base == "" || quote == "" {
				continue
			}
			// Feed the resolved pair back into the trade: normalization
			// below reads t.QuoteAsset for the value denomination, and a
			// symbol-only trade must not fall back to a fabricated USD.
			t.BaseAsset, t.QuoteAsset = base, quote
			value := t.Price * t.Quantity
			buy := !strings.EqualFold(t.Side, "sell")
			quoteType, baseType := brokerage.ActivityBuy, brokerage.ActivitySell
			if buy {
				quoteType, baseType = brokerage.ActivitySell, brokerage.ActivityBuy
			}
			quoteLeg := newTradeActivity(t, accountID, NormalizeAsset(quote), quoteType, value, value, 0, slug, "quote")
			baseLeg := newTradeActivity(t, accountID, NormalizeAsset(base), baseType, t.Quantity, value, t.Fee, slug, "base")
			// Emit the source leg first so every fill reads as
			// SELL-source then BUY-destination: buys are USDT/C→TOKEN,
			// sells are TOKEN→USDT/C, swaps are TOKEN1→TOKEN2.
			if buy {
				acts = append(acts, quoteLeg, baseLeg)
			} else {
				acts = append(acts, baseLeg, quoteLeg)
			}
		}
		activities[accountID] = acts
	}

	return domainsync.BrokerSnapshot{
		Connection: connection,
		Accounts:   []brokerage.Account{account},
		Holdings:   []brokerage.Holdings{holding},
		Activities: activities,
	}
}

func newTradeActivity(t Trade, accountID, asset string, activityType brokerage.ActivityType, units, amount, fee float64, slug, leg string) brokerage.Activity {
	price := 0.0
	if units != 0 {
		price = amount / units
	}
	feeAsset := ""
	if leg == "base" {
		feeAsset = t.FeeAsset
	}
	// Amounts are denominated in the pair's quote asset. Relabeling them USD
	// fabricates valuations (1 ETH for 0.05 BTC is not a $0.05 purchase), so
	// non-USD quotes keep their own currency and are flagged for review
	// until a historical quote/USD conversion exists.
	currency := brokerage.Currency{Code: "USD"}
	needsReview := false
	description := ""
	if q := NormalizeAsset(t.QuoteAsset); q != "" && q != "USD" && !IsStablecoin(q) {
		currency = brokerage.Currency{Code: q}
		needsReview = true
		description = "Quoted in " + q + "; no USD conversion applied"
	}
	if t.Note != "" {
		if description != "" {
			description += "; "
		}
		description += t.Note
		needsReview = true
	}
	return brokerage.Activity{
		ID:        t.ID + "-" + leg,
		AccountID: accountID,
		Type:      activityType,
		TradeDate: t.Timestamp,
		Price:     price,
		Units:     units,
		Amount:    amount,
		Fee:       fee,
		FeeAsset:  feeAsset,
		Currency:  currency,
		Symbol: &brokerage.Symbol{
			Symbol:    asset,
			RawSymbol: asset,
			Type:      brokerage.SymbolType{Code: "CRYPTO", IsSupported: true},
			Exchange:  brokerage.Exchange{Code: strings.ToUpper(slug)},
			Currency:  currency,
		},
		Description:    description,
		NeedsReview:    needsReview,
		RawType:        strings.ToUpper(string(activityType)),
		ProviderType:   slug,
		SourceSystem:   slug,
		SourceRecordID: t.ID + "-" + leg,
		SourceGroupID:  t.ID,
	}
}

// unvaluedPosition keeps an owned but unpriced asset visible with zero
// price and unknown basis instead of erasing the holding.
func unvaluedPosition(slug, displayName, asset string, qty float64) brokerage.Position {
	return brokerage.Position{
		Symbol: brokerage.Symbol{
			Symbol:      asset,
			RawSymbol:   asset,
			Description: asset,
			Name:        asset,
			Type:        brokerage.SymbolType{Code: "CRYPTO", IsSupported: true, Description: "Cryptocurrency"},
			Exchange:    brokerage.Exchange{Code: strings.ToUpper(slug), Name: displayName},
			Currency:    brokerage.Currency{Code: "USD"},
		},
		Units:    qty,
		Currency: brokerage.Currency{Code: "USD"},
	}
}

func splitPair(pair string) (string, string) {
	pair = strings.ToUpper(strings.ReplaceAll(pair, "-", ""))
	for _, quote := range []string{"USD₮0", "USDT", "USDC", "BUSD", "USDS", "USD", "BTC", "ETH", "BNB", "SOL"} {
		if strings.HasSuffix(pair, quote) && len(pair) > len(quote) {
			return strings.TrimSuffix(pair, quote), quote
		}
	}
	return "", ""
}

func NormalizeAsset(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	// Tether's stylized symbols (USD₮ on TON, USD₮0 LayerZero omnichain)
	// all denote USDT.
	if s == "USD₮0" || s == "USD₮" {
		return "USDT"
	}
	// Affluent vault shares (affUSDe across vault versions) are tracked as
	// one AFFSENTORAENT position.
	if s == "AFFUSDE" {
		return "AFFSENTORAENT"
	}
	return s
}

// IsStablecoin returns true for the most common USD-pegged stablecoins.
func IsStablecoin(s string) bool {
	switch NormalizeAsset(s) {
	case "USDT", "USDC", "DAI", "BUSD", "TUSD", "FRAX", "USD", "USDD", "PYUSD":
		return true
	}
	return false
}
