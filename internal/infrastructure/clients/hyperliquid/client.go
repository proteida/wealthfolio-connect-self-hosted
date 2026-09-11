// Package hyperliquid implements a BrokerClient that pulls a wallet's
// perpetuals + spot balances from Hyperliquid's public /info endpoint:
//
//	POST https://api.hyperliquid.xyz/info
//	{
//	  "type":  "spotClearinghouseState" | "clearinghouseState",
//	  "user":  "0x..."
//	}
//
// No API keys are required — the wallet address is the only credential.
package hyperliquid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

// HTTPDoer mirrors http.Client.Do.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

const defaultBaseURL = "https://api.hyperliquid.xyz"

// Client reads balances for a single Hyperliquid wallet.
type Client struct {
	wallet  string
	baseURL string
	http    HTTPDoer
}

// New builds a client targeting the supplied wallet address.
func New(wallet, baseURL string, h HTTPDoer) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if h == nil {
		h = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{wallet: wallet, baseURL: baseURL, http: h}
}

// ID returns the slug.
func (c *Client) ID() string { return "hyperliquid" }

// Fetch retrieves spot + perp state and folds them into a snapshot.
func (c *Client) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	if c.wallet == "" {
		return domainsync.BrokerSnapshot{}, errors.New("hyperliquid: wallet address not configured")
	}
	spot, spotPartial, spotErr := c.spot(ctx)
	perp, perpErr := c.perp(ctx)
	if spotErr != nil && perpErr != nil {
		return domainsync.BrokerSnapshot{}, fmt.Errorf("hyperliquid: %w", errors.Join(spotErr, perpErr))
	}
	snap := cexcommon.Snapshot{}
	// One failed component means the snapshot is incomplete and must never
	// replace the last complete holdings.
	snap.Partial = spotErr != nil || perpErr != nil || spotPartial
	snap.Balances = append(snap.Balances, spot...)
	// Perp account collateral is folded into a synthetic USDC cash balance.
	if perp > 0 {
		snap.Balances = append(
			snap.Balances, cexcommon.Balance{
				Asset:    "USDC",
				Quantity: perp,
				PriceUSD: 1,
				USDValue: perp,
			},
		)
	}
	return cexcommon.Translate("hyperliquid", "Hyperliquid", snap), nil
}

func (c *Client) spot(ctx context.Context) (out []cexcommon.Balance, metaFallback bool, err error) {
	marks, marksErr := c.spotMarks(ctx)
	if marksErr != nil {
		metaFallback = true
	}
	type assetRow struct {
		Coin     string `json:"coin"`
		Total    string `json:"total"`
		EntryNtl string `json:"entryNtl"`
	}
	type envelope struct {
		Balances []assetRow `json:"balances"`
	}
	var env envelope
	if err := c.postInfo(
		ctx, map[string]string{
			"type": "spotClearinghouseState",
			"user": c.wallet,
		}, &env,
	); err != nil {
		return nil, false, err
	}
	out = make([]cexcommon.Balance, 0, len(env.Balances))
	fallback := metaFallback
	for _, r := range env.Balances {
		qty := atof(r.Total)
		if qty == 0 {
			continue
		}
		ntl := atof(r.EntryNtl)
		var price, usd float64
		switch {
		case cexcommon.IsStablecoin(r.Coin):
			price = 1
			usd = qty
		case marks[strings.ToUpper(r.Coin)] > 0:
			// Market valuation from the spot meta context; entry
			// notional stays out of pricing entirely.
			price = marks[strings.ToUpper(r.Coin)]
			usd = price * qty
		case qty > 0 && ntl > 0:
			// Meta unavailable for this token: fall back to entry
			// notional and mark the snapshot partial.
			price = ntl / qty
			usd = ntl
			fallback = true
		default:
			// Positive quantity with zero entry notional: kept as an
			// unvalued position downstream, never dropped.
			fallback = true
		}
		out = append(
			out, cexcommon.Balance{
				Asset:    strings.ToUpper(r.Coin),
				Quantity: qty,
				PriceUSD: price,
				USDValue: usd,
			},
		)
	}
	if fallback {
		return out, true, nil
	}
	return out, false, nil
}

// spotMarks maps spot token names to their current markPx via the
// spotMetaAndAssetCtxs endpoint. The response is a two-element array
// [spotMeta, assetCtxs]: universe entries carry market names ("PURR/USDC",
// "@1") plus base/quote token indices, while assetCtxs aligns with the
// universe by position. Prices join balance coins through the meta token
// index, never through market names.
func (c *Client) spotMarks(ctx context.Context) (map[string]float64, error) {
	var env []json.RawMessage
	if err := c.postInfo(ctx, map[string]string{"type": "spotMetaAndAssetCtxs"}, &env); err != nil {
		return nil, err
	}
	if len(env) != 2 {
		return nil, fmt.Errorf("hyperliquid: spotMetaAndAssetCtxs returned %d elements, want 2", len(env))
	}
	var meta struct {
		Tokens []struct {
			Name  string `json:"name"`
			Index int    `json:"index"`
		} `json:"tokens"`
		Universe []struct {
			Tokens []int `json:"tokens"`
		} `json:"universe"`
	}
	if err := json.Unmarshal(env[0], &meta); err != nil {
		return nil, fmt.Errorf("hyperliquid: spot meta decode: %w", err)
	}
	var assetCtxs []struct {
		MarkPx string `json:"markPx"`
	}
	if err := json.Unmarshal(env[1], &assetCtxs); err != nil {
		return nil, fmt.Errorf("hyperliquid: spot asset contexts decode: %w", err)
	}
	names := make(map[int]string, len(meta.Tokens))
	for _, t := range meta.Tokens {
		names[t.Index] = t.Name
	}
	marks := make(map[string]float64)
	for i, u := range meta.Universe {
		if i >= len(assetCtxs) || len(u.Tokens) == 0 {
			continue
		}
		px := atof(assetCtxs[i].MarkPx)
		if px <= 0 {
			continue
		}
		if name := names[u.Tokens[0]]; name != "" {
			marks[strings.ToUpper(name)] = px
		}
	}
	return marks, nil
}

func (c *Client) perp(ctx context.Context) (float64, error) {
	type marginSummary struct {
		AccountValue string `json:"accountValue"`
	}
	type envelope struct {
		MarginSummary marginSummary `json:"marginSummary"`
	}
	var env envelope
	if err := c.postInfo(
		ctx, map[string]string{
			"type": "clearinghouseState",
			"user": c.wallet,
		}, &env,
	); err != nil {
		return 0, err
	}
	return atof(env.MarginSummary.AccountValue), nil
}

func (c *Client) postInfo(ctx context.Context, payload any, into any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/info", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, raw)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(raw, into)
}

func atof(s string) float64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64) //nolint:errcheck // exchange returns numeric strings; treat unparsable as zero
	return v
}
