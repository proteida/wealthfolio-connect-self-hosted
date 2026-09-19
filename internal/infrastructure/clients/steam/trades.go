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

// tradeResume is the provider offset for continuing a MaxPages-capped
// trade walk: the API pages backward from (time, id).
type tradeResume struct {
	AfterTime time.Time `json:"after_time"`
	AfterID   string    `json:"after_id"`
}

// fetchTrades pages trade history (max 100 per call) from the newest page
// backwards until Steam reports no more, the watermark is reached, pages
// stop advancing, or the page cap hits. start resumes a capped walk (nil
// starts at the newest page); stopBefore ends a fresh walk as complete
// once a page reaches already-covered territory (zero disables the stop).
// start_after_* are backward-pagination anchors, so the newest imported
// timestamp must never seed them — newness is detected by walking from
// the top instead. Failed trades are included: exclusion would hide
// evidence a later reconciliation needs. Incomplete walks return the
// offset to resume from (nil unless the page cap hit).
func (c *Client) fetchTrades(ctx context.Context, start *tradeResume, stopBefore time.Time) ([]domainsteam.TradeRecord, bool, *tradeResume, error) {
	const pageSize = 100
	var all []domainsteam.TradeRecord
	seen := map[string]bool{}
	var afterTime int64
	var afterID string
	if start != nil {
		afterTime = start.AfterTime.Unix()
		afterID = start.AfterID
	}
	// A resumed walk continues below the watermark, so the watermark
	// stop applies to fresh walks only; otherwise it would fire at once.
	resume := start != nil
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
			return all, false, nil, err
		}
		trades := env.Response.Trades
		if len(trades) == 0 {
			return all, true, nil, nil
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
		if !resume && !stopBefore.IsZero() {
			if min := pageMinTradeTime(trades); !min.IsZero() && !min.After(stopBefore.Add(time.Second)) {
				return all, true, nil, nil
			}
		}
		if !env.Response.More {
			return all, true, nil, nil
		}
	}
	if afterID == "" && afterTime == 0 {
		// Capped without any consumable offset: nothing to resume from.
		return all, false, nil, nil
	}
	return all, false, &tradeResume{AfterTime: time.Unix(afterTime, 0).UTC(), AfterID: afterID}, nil
}

// pageMinTradeTime returns the oldest parseable trade timestamp in raw
// rows (zero when none parse, which must never trigger a watermark stop).
func pageMinTradeTime(trades []rawTrade) time.Time {
	min := time.Time{}
	for _, t := range trades {
		at := parseSteamTime(strVal(t.TimeInit))
		if at.IsZero() {
			continue
		}
		if min.IsZero() || at.Before(min) {
			min = at
		}
	}
	return min
}
