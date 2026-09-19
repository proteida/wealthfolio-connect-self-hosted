// Package binance implements a BrokerClient that reads spot account
// balances from Binance via the official adshao/go-binance/v2 SDK.
//
// To compute USD valuation we hit the public /api/v3/ticker/price endpoint
// once per non-stable asset using its USDT pair (BTCUSDT, ETHUSDT, ...).
// Stablecoins skip the price lookup and are folded into the cash balance.
package binance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	binsdk "github.com/adshao/go-binance/v2"
	"github.com/rs/zerolog"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

// Fetcher abstracts the Binance SDK so tests can plug in a fake.
type Fetcher interface {
	Account(ctx context.Context) ([]RawBalance, error)
	Prices(ctx context.Context) (map[string]float64, error) // map["BTCUSDT"] = price
}

// RawBalance is the per-asset payload a Fetcher returns. Public so tests
// can construct it.
type RawBalance struct {
	Asset  string
	Free   float64
	Locked float64
}

// Client is the Binance BrokerClient.
type Client struct {
	apiKey, secret string
	fetcher        Fetcher
	log            zerolog.Logger
}

// New builds a client. Pass nil fetcher to use the real SDK.
func New(apiKey, secret string, f Fetcher) *Client {
	if f == nil {
		sdk := binsdk.NewClient(apiKey, secret)
		sdk.HTTPClient = &http.Client{Timeout: 15 * time.Second, Transport: newWeightTransport(http.DefaultTransport)}
		f = &realFetcher{client: sdk}
	}
	return &Client{apiKey: apiKey, secret: secret, fetcher: f, log: zerolog.Nop()}
}

// ConfigureHistory sets the durable source of trade cursors and optional exact
// trading pairs. Call during construction, before starting synchronization.
func (c *Client) ConfigureHistory(history repository.ActivityRepository, symbols []string) {
	if f, ok := c.fetcher.(*realFetcher); ok {
		f.history = history
		f.symbols = symbols
	}
}

// SnapshotCommitted publishes the pending trade cursor after the sync
// engine confirms the snapshot, so a failed run replays instead of
// skipping trades.
func (c *Client) SnapshotCommitted() {
	if f, ok := c.fetcher.(*realFetcher); ok {
		f.progress = f.pending
	}
}

// SetLogger attaches structured logging for incomplete history requests.
func (c *Client) SetLogger(log zerolog.Logger) { c.log = log }

// ID returns the slug used by sync orchestration.
func (c *Client) ID() string { return "binance" }

// Fetch pulls account balances + USDT-quoted prices and returns a snapshot.
func (c *Client) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	if c.apiKey == "" || c.secret == "" {
		return domainsync.BrokerSnapshot{}, errors.New("binance: api key/secret not configured")
	}
	balances, err := c.fetcher.Account(ctx)
	if err != nil {
		return domainsync.BrokerSnapshot{}, fmt.Errorf("binance: account: %w", err)
	}
	prices, err := c.fetcher.Prices(ctx)
	if err != nil {
		// Prices are best-effort: without them positions carry zero USD
		// valuation but owned assets stay visible, and the snapshot is
		// marked partial so it never replaces the last complete one.
		prices = map[string]float64{}
	}
	snapshot := buildSnapshot(balances, prices)
	if err != nil {
		snapshot.Partial = true
	}
	if tf, ok := c.fetcher.(TradeFetcher); ok {
		trades, tradeErr := tf.Trades(ctx, balances, prices)
		snapshot.Trades = trades
		snapshot.ActivitiesFetched = tradeErr == nil
		if errors.Is(tradeErr, errHistoryPending) {
			c.log.Info().Msg("binance: history budget reached; continuing from saved trade IDs on next sync")
		} else if tradeErr != nil {
			c.log.Warn().Err(tradeErr).Msg("binance: transaction history incomplete")
		}
	}
	return cexcommon.Translate("binance", "Binance", snapshot), nil
}

// buildSnapshot is exposed via BuildSnapshotForTest so external tests can
// drive the mapping pipeline.
func buildSnapshot(balances []RawBalance, prices map[string]float64) cexcommon.Snapshot {
	out := cexcommon.Snapshot{}
	for _, b := range balances {
		qty := b.Free + b.Locked
		if qty == 0 {
			continue
		}

		asset := cexcommon.NormalizeAsset(b.Asset)
		var price, usd float64
		if cexcommon.IsStablecoin(asset) {
			price = 1
			usd = qty
		} else {
			price = prices[asset+"USDT"]
			usd = price * qty
		}
		out.Balances = append(out.Balances, cexcommon.Balance{
			Asset:    asset,
			Quantity: qty,
			PriceUSD: price,
			USDValue: usd,
		})
	}
	return out
}

// ===================== real Binance SDK fetcher =====================

type realFetcher struct {
	client   *binsdk.Client
	history  repository.ActivityRepository
	symbols  []string
	progress historyProgress
	pending  historyProgress
}

// Account fetches all non-zero balances from the user's Binance spot account.
func (f *realFetcher) Account(ctx context.Context) ([]RawBalance, error) {
	acc, err := f.client.NewGetAccountService().OmitZeroBalances(true).Do(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RawBalance, 0, len(acc.Balances))
	for _, b := range acc.Balances {
		free, _ := strconv.ParseFloat(b.Free, 64)     //nolint:errcheck // SDK returns well-formed numeric strings; treat unparsable values as zero
		locked, _ := strconv.ParseFloat(b.Locked, 64) //nolint:errcheck // SDK returns well-formed numeric strings; treat unparsable values as zero
		if free == 0 && locked == 0 {
			continue
		}
		out = append(out, RawBalance{Asset: b.Asset, Free: free, Locked: locked})
	}
	return out, nil
}

// Prices returns a snapshot of every symbol's last traded price keyed by symbol.
func (f *realFetcher) Prices(ctx context.Context) (map[string]float64, error) {
	prices, err := f.client.NewListPricesService().Do(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(prices))
	for _, p := range prices {
		v, err := strconv.ParseFloat(p.Price, 64)
		if err == nil {
			out[p.Symbol] = v
		}
	}
	return out, nil
}
