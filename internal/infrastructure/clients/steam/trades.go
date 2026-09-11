package steam

import (
	"context"
	"net/url"
	"strconv"
	"time"

	domainsteam "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/steam"
)

// GetTradeHistory parameters follow the official IEconService shape. Only
// documented fields are sent; Steam decides what it returns.
type tradeHistoryPage struct {
	Response struct {
		More         bool       `json:"more"`
		TotalTrades  int        `json:"total_trades"`
		Trades       []rawTrade `json:"trades"`
		Descriptions []any      `json:"descriptions"`
	} `json:"response"`
}

type rawTrade struct {
	TradeID        any             `json:"tradeid"`
	SteamIDOther   any             `json:"steamid_other"`
	TimeInit       any             `json:"time_init"`
	Status         any             `json:"status"`
	AssetsGiven    []rawTradeAsset `json:"assets_given"`
	AssetsReceived []rawTradeAsset `json:"assets_received"`
}

type rawTradeAsset struct {
	AppID      any `json:"appid"`
	ContextID  any `json:"contextid"`
	AssetID    any `json:"assetid"`
	ClassID    any `json:"classid"`
	InstanceID any `json:"instanceid"`
	Amount     any `json:"amount"`
	// Older records expose the post-trade identity; modern CS2 trades
	// usually do not (see normalizeTradeAsset).
	NewAssetID   any `json:"new_assetid"`
	NewContextID any `json:"new_contextid"`
}

func normalizeTradeAsset(a rawTradeAsset) domainsteam.TradeAsset {
	qty := intVal(a.Amount)
	if qty == 0 {
		qty = 1
	}
	return domainsteam.TradeAsset{
		AssetID:    strVal(a.AssetID),
		ClassID:    strVal(a.ClassID),
		InstanceID: strVal(a.InstanceID),
		Quantity:   qty,
		NewAssetID: strVal(a.NewAssetID),
		NewContext: strVal(a.NewContextID),
	}
}

// fetchTrades pages trade history (max 100 per call) until Steam reports
// no more, pages stop advancing, or the page cap hits. Failed trades are
// included: exclusion would hide evidence a later reconciliation needs.
func (c *Client) fetchTrades(ctx context.Context, since time.Time) ([]domainsteam.TradeRecord, bool, error) {
	const pageSize = 100
	var all []domainsteam.TradeRecord
	seen := map[string]bool{}
	afterTime := since.Unix()
	afterID := ""
	for page := 0; page < c.cfg.MaxPages; page++ {
		q := url.Values{
			"max_trades":       {"100"},
			"get_descriptions": {"1"},
			"language":         {"english"},
			"include_failed":   {"1"},
			"include_total":    {"1"},
		}
		if afterTime > 0 {
			q.Set("start_after_time", strconv.FormatInt(afterTime, 10))
		}
		if afterID != "" {
			q.Set("start_after_tradeid", afterID)
		}
		var env tradeHistoryPage
		if err := c.getStore(ctx, "/IEconService/GetTradeHistory/v1/", q, &env); err != nil {
			return all, false, err
		}
		trades := env.Response.Trades
		if len(trades) == 0 {
			return all, true, nil
		}
		for _, t := range trades {
			id := strVal(t.TradeID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			rec := domainsteam.TradeRecord{
				TradeID:      id,
				Timestamp:    parseSteamTime(strVal(t.TimeInit)),
				OtherSteamID: strVal(t.SteamIDOther),
				Status:       strVal(t.Status),
			}
			for _, a := range t.AssetsGiven {
				rec.Given = append(rec.Given, normalizeTradeAsset(a))
			}
			for _, a := range t.AssetsReceived {
				rec.Received = append(rec.Received, normalizeTradeAsset(a))
			}
			all = append(all, rec)
			afterID = id
			if ts := rec.Timestamp.Unix(); ts > 0 {
				afterTime = ts
			}
		}
		if !env.Response.More {
			return all, true, nil
		}
	}
	return all, false, nil
}
