package binance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	binsdk "github.com/adshao/go-binance/v2"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

const historyPageBudget = 20

var errHistoryPending = errors.New("binance: more history remains for a later sync")

type historyProgress struct {
	finished map[string]bool
	next     string
}

// TradeFetcher optionally extends balance fetchers with scoped Spot history.
type TradeFetcher interface {
	Trades(context.Context, []RawBalance, map[string]float64) ([]cexcommon.Trade, error)
}

// Trades queries only configured or account-relevant pairs. Each run makes at
// most 20 history requests, at most two pages per pair, rotating through the
// scope fairly. Progress is acknowledged only after snapshot persistence.
func (f *realFetcher) Trades(ctx context.Context, balances []RawBalance, prices map[string]float64) ([]cexcommon.Trade, error) {
	f.pending = historyProgress{finished: maps.Clone(f.progress.finished), next: f.progress.next}
	if f.pending.finished == nil {
		f.pending.finished = make(map[string]bool)
	}
	cursors, err := f.savedCursors(ctx)
	if err != nil {
		return nil, err
	}
	symbols, err := f.historySymbols(balances, prices, cursors)
	if err != nil {
		return nil, err
	}
	if len(symbols) == 0 {
		if len(f.symbols) > 0 {
			return nil, nil
		}
		// Heuristic discovery found no candidate pairs (e.g. a cash-only
		// account): it cannot distinguish empty from fully-sold, so the
		// history stays incomplete instead of reading as complete.
		// Completion for guessed scopes is reported via the pending
		// signal below once an explicit scope is configured.
		return nil, errHistoryPending
	}
	start := sort.SearchStrings(symbols, f.progress.next) % len(symbols)
	selected := make([]string, 0, min(len(symbols), historyPageBudget))
	for i := 0; i < cap(selected); i++ {
		selected = append(selected, symbols[(start+i)%len(symbols)])
	}
	// Never call exchangeInfo without a symbols filter. Metadata cannot expand
	// the scope, even if an upstream response includes additional markets.
	info, err := f.client.NewExchangeInfoService().Symbols(selected...).Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("binance selected market metadata: %w", err)
	}
	markets := make(map[string]binsdk.Symbol, len(info.Symbols))
	for _, market := range info.Symbols {
		markets[market.Symbol] = market
	}
	var all []cexcommon.Trade
	remaining := historyPageBudget
	for i, symbol := range selected {
		if remaining == 0 {
			break
		}
		f.pending.finished[symbol] = false
		market, ok := markets[symbol]
		if !ok {
			return all, fmt.Errorf("binance: no metadata for selected pair %s", symbol)
		}
		trades, done, used, fetchErr := f.symbolTrades(ctx, market, cursors[symbol], min(2, remaining))
		all = append(all, trades...)
		remaining -= used
		if fetchErr != nil {
			return all, fetchErr
		}
		f.pending.next = symbols[(start+i+1)%len(symbols)]
		f.pending.finished[symbol] = done
	}
	for _, symbol := range symbols {
		if !f.pending.finished[symbol] {
			return all, errHistoryPending
		}
	}
	if len(f.symbols) == 0 {
		// Heuristic pair discovery only guesses held-USDT pairs (plus
		// previously seen ones): it cannot see fully-sold assets, so one
		// nonempty guessed pair still does not establish account-wide
		// coverage. Explicit configured scope completes; guessed scope
		// stays pending until the user configures it.
		return all, errHistoryPending
	}
	return all, nil
}

func (f *realFetcher) historySymbols(balances []RawBalance, prices map[string]float64, cursors map[string]int64) ([]string, error) {
	scope := make(map[string]bool)
	if len(f.symbols) > 0 {
		for _, symbol := range f.symbols {
			if symbol = strings.ToUpper(strings.TrimSpace(symbol)); symbol != "" {
				scope[symbol] = true
			}
		}
	} else {
		if prices == nil {
			return nil, errors.New("binance: prices unavailable for automatic history pair selection")
		}
		for symbol := range cursors {
			scope[symbol] = true
		}
		for _, balance := range balances {
			asset := strings.ToUpper(balance.Asset)
			if balance.Free+balance.Locked <= 0 || cexcommon.IsStablecoin(asset) {
				continue
			}
			symbol := asset + "USDT"
			if _, exists := prices[symbol]; exists {
				scope[symbol] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(scope)), nil
}

// savedCursors derives progress exclusively from durable activities. A crash or
// DB write failure therefore retries missing fills instead of losing them.
func (f *realFetcher) savedCursors(ctx context.Context) (map[string]int64, error) {
	cursors := make(map[string]int64)
	if f.history == nil {
		return cursors, nil
	}
	for offset := 0; ; {
		rows, total, err := f.history.List(ctx, repository.ActivityFilter{AccountID: "binance-spot", Offset: offset, Limit: 1000})
		if err != nil {
			return nil, fmt.Errorf("binance saved history: %w", err)
		}
		for _, row := range rows {
			parts := strings.Split(row.SourceRecordID, ":")
			if len(parts) < 3 || parts[0] != "binance" {
				continue
			}
			idPart := strings.TrimSuffix(parts[2], "-quote")
			idPart = strings.TrimSuffix(idPart, "-base")
			id, parseErr := strconv.ParseInt(idPart, 10, 64)
			if parseErr != nil || id < 0 || id == math.MaxInt64 {
				return nil, fmt.Errorf("binance: invalid saved trade ID %q", row.SourceRecordID)
			}
			cursors[parts[1]] = max(cursors[parts[1]], id+1)
		}
		offset += len(rows)
		if offset >= total {
			return cursors, nil
		}
		if len(rows) == 0 {
			return nil, errors.New("binance saved history pagination stalled")
		}
	}
}

func (f *realFetcher) symbolTrades(ctx context.Context, market binsdk.Symbol, fromID int64, budget int) (trades []cexcommon.Trade, complete bool, used int, resultErr error) {
	seen := make(map[string]bool)
	for pageNumber := 0; pageNumber < budget; pageNumber++ {
		page, err := f.client.NewListTradesService().Symbol(market.Symbol).FromID(fromID).Limit(1000).Do(ctx)
		if err != nil {
			return trades, false, pageNumber + 1, fmt.Errorf("binance trades %s: %w", market.Symbol, err)
		}
		nextID := fromID
		for _, raw := range page {
			trade, mapErr := mapTrade(raw, market)
			if mapErr != nil {
				return trades, false, pageNumber + 1, mapErr
			}
			if raw.ID == math.MaxInt64 {
				return trades, false, pageNumber + 1, fmt.Errorf("binance trade ID overflow for %s", market.Symbol)
			}
			if raw.ID < fromID || seen[trade.ID] {
				continue
			}
			trades = append(trades, trade)
			seen[trade.ID] = true
			nextID = max(nextID, raw.ID+1)
		}
		if len(page) < 1000 {
			return trades, true, pageNumber + 1, nil
		}
		if nextID <= fromID {
			return trades, false, pageNumber + 1, fmt.Errorf("binance trades %s: pagination did not advance", market.Symbol)
		}
		fromID = nextID
	}
	return trades, false, budget, nil
}

func mapTrade(raw *binsdk.TradeV3, market binsdk.Symbol) (cexcommon.Trade, error) {
	if raw == nil || raw.ID < 0 || raw.Time <= 0 || market.BaseAsset == "" || market.QuoteAsset == "" {
		return cexcommon.Trade{}, fmt.Errorf("binance: invalid trade for %s", market.Symbol)
	}
	values := make([]float64, 3)
	for i, value := range []string{raw.Price, raw.Quantity, raw.Commission} {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return cexcommon.Trade{}, fmt.Errorf("binance: invalid trade numeric value %q for %s", value, market.Symbol)
		}
		values[i] = n
	}
	side := "sell"
	if raw.IsBuyer {
		side = "buy"
	}
	return cexcommon.Trade{
		ID:     "binance:" + market.Symbol + ":" + strconv.FormatInt(raw.ID, 10),
		Symbol: market.Symbol, BaseAsset: market.BaseAsset, QuoteAsset: market.QuoteAsset,
		Side: side, Price: values[0], Quantity: values[1], Fee: values[2], FeeAsset: raw.CommissionAsset,
		Timestamp: time.UnixMilli(raw.Time).UTC(),
	}, nil
}
