package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	domainsteam "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/steam"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
)

// Reconciliation binds current assetids to acquisition evidence at one of
// four confidence levels, never pretending an inference is exact:
//
//	exact  — a unique asset identifier agreed: snapshot-before shows the
//	         asset absent, snapshot-after shows it present, and a matching
//	         inventory-history event exists; or a trade's new_assetid
//	         equals the current assetid.
//	high   — inventory-history event + market-history row agree on
//	         timestamp, market_hash_name, quantity and price.
//	medium — classid/instanceid/market_hash_name, timestamp proximity and
//	         quantity agree, with no unique asset identifier.
//	low/unresolved — anything weaker, or fungible duplicates where unique
//	         assignment is impossible (see buildLots).
//
// Every input event survives either matched or in the unmatched store for
// later reconciliation; nothing silently disappears.

// Resolution is the resolver output for one asset.
type Resolution struct {
	Asset           InventoryItem
	AcquiredAt      *time.Time
	Type            domainsteam.AcquisitionType
	Reference       string
	CostBasis       *float64
	CostCurrency    string
	MatchMethod     domainsteam.MatchMethod
	MatchConfidence domainsteam.MatchConfidence
}

// Resolve binds owned assets to acquisition evidence. Parameters:
// before/after are the previous and current assetid sets (nil when
// unknown); events are normalized inventory-history rows; market maps
// external market IDs to transactions; trades are normalized trade
// records. Returned resolutions cover every owned asset; leftovers are
// reported separately for the unmatched store.
func Resolve(owned []InventoryItem, before map[string]bool, events []historyEvent, market map[string]domainsteam.MarketTransaction, trades []domainsteam.TradeRecord) (resolved []Resolution, unmatchedEvents []historyEvent) {
	byAsset := map[string][]historyEvent{}
	for _, ev := range events {
		if ev.AssetID != "" {
			byAsset[ev.AssetID] = append(byAsset[ev.AssetID], ev)
		}
	}
	consumed := map[string]bool{}
	for _, it := range owned {
		r := Resolution{Asset: it, Type: domainsteam.AcquisitionUnresolved,
			MatchMethod: domainsteam.MatchMethodUnresolved, MatchConfidence: domainsteam.MatchUnresolved}
		// Exact path 1: absent-before, present-after, matching event.
		if before != nil && !before[it.AssetID] {
			if ev, ok := matchEvent(byAsset[it.AssetID], it); ok {
				applyEvent(&r, ev)
				r.MatchMethod = domainsteam.MatchSnapshotEvent
				r.MatchConfidence = domainsteam.MatchExact
				consumed[ev.ExternalID] = true
			}
		}
		// Exact path 2: trade new_assetid echo.
		if r.MatchConfidence == domainsteam.MatchUnresolved {
			if tr, ok := matchNewAssetID(trades, it.AssetID); ok {
				r.AcquiredAt = &tr.Timestamp
				r.Type = domainsteam.AcquisitionTrade
				r.Reference = tr.TradeID
				r.MatchMethod = domainsteam.MatchNewAssetID
				r.MatchConfidence = domainsteam.MatchExact
			}
		}
		// High path: history event + market row agree.
		if r.MatchConfidence == domainsteam.MatchUnresolved {
			if ev, tx, ok := matchMarketPair(events, market, it); ok {
				applyEvent(&r, ev)
				r.Type = domainsteam.AcquisitionMarketBuy
				r.Reference = tx.ExternalID
				if tx.Currency != "" {
					r.CostCurrency = tx.Currency
				}
				basis := tx.Gross
				if tx.Quantity > 1 {
					basis = tx.Gross / float64(tx.Quantity)
				}
				r.CostBasis = &basis
				r.MatchMethod = domainsteam.MatchHistoryMarket
				r.MatchConfidence = domainsteam.MatchHigh
				consumed[ev.ExternalID] = true
			}
		}
		// Medium path: attribute proximity without unique identity.
		if r.MatchConfidence == domainsteam.MatchUnresolved {
			if ev, ok := matchAttributes(events, consumed, it); ok {
				applyEvent(&r, ev)
				r.MatchMethod = domainsteam.MatchAttributes
				r.MatchConfidence = domainsteam.MatchMedium
				consumed[ev.ExternalID] = true
			}
		}
		resolved = append(resolved, r)
	}
	for _, ev := range events {
		if !consumed[ev.ExternalID] {
			unmatchedEvents = append(unmatchedEvents, ev)
		}
	}
	return resolved, unmatchedEvents
}

func matchEvent(evs []historyEvent, it InventoryItem) (historyEvent, bool) {
	for _, ev := range evs {
		if ev.ClassID != "" && ev.ClassID != it.ClassID {
			continue
		}
		if ev.MarketHashName != "" && ev.MarketHashName != it.MarketHashName {
			continue
		}
		return ev, true
	}
	return historyEvent{}, false
}

func matchNewAssetID(trades []domainsteam.TradeRecord, assetID string) (domainsteam.TradeRecord, bool) {
	for _, tr := range trades {
		for _, a := range tr.Received {
			if a.NewAssetID != "" && a.NewAssetID == assetID {
				return tr, true
			}
		}
	}
	return domainsteam.TradeRecord{}, false
}

// matchMarketPair finds an unconsumed acquisition-typed event whose
// timestamp, name and quantity agree with a market buy row.
func matchMarketPair(events []historyEvent, market map[string]domainsteam.MarketTransaction, it InventoryItem) (historyEvent, domainsteam.MarketTransaction, bool) {
	for _, ev := range events {
		if !isAcquisitionKind(ev.Kind) {
			continue
		}
		if ev.MarketHashName != "" && ev.MarketHashName != it.MarketHashName {
			continue
		}
		for _, tx := range market {
			if tx.Type != "buy" || tx.MarketHashName != it.MarketHashName {
				continue
			}
			if tx.Quantity != it.Amount && tx.Quantity != 1 {
				continue
			}
			if absDuration(ev.Timestamp.Sub(tx.Timestamp)) > 24*time.Hour {
				continue
			}
			return ev, tx, true
		}
	}
	return historyEvent{}, domainsteam.MarketTransaction{}, false
}

// matchAttributes falls back to class/instance/name proximity within a
// three-day window. Quantity must agree; identity stays medium at best.
func matchAttributes(events []historyEvent, consumed map[string]bool, it InventoryItem) (historyEvent, bool) {
	for _, ev := range events {
		if consumed[ev.ExternalID] || !isAcquisitionKind(ev.Kind) {
			continue
		}
		if ev.ClassID != "" && ev.ClassID != it.ClassID {
			continue
		}
		if ev.InstanceID != "" && ev.InstanceID != it.InstanceID {
			continue
		}
		if ev.MarketHashName != "" && ev.MarketHashName != it.MarketHashName {
			continue
		}
		if ev.Quantity != 0 && ev.Quantity != it.Amount {
			continue
		}
		return ev, true
	}
	return historyEvent{}, false
}

func isAcquisitionKind(kind string) bool {
	switch kind {
	case EventMarketBuy, EventTraded, EventReceived, EventCrafted, EventEarned, EventMarketReturn:
		return true
	default:
		return false
	}
}

func applyEvent(r *Resolution, ev historyEvent) {
	if !ev.Timestamp.IsZero() {
		t := ev.Timestamp
		r.AcquiredAt = &t
	}
	r.Type = acquisitionForKind(ev.Kind)
	r.Reference = ev.ExternalID
}

func acquisitionForKind(kind string) domainsteam.AcquisitionType {
	switch kind {
	case EventMarketBuy:
		return domainsteam.AcquisitionMarketBuy
	case EventTraded, EventReceived:
		return domainsteam.AcquisitionTrade
	case EventCrafted:
		return domainsteam.AcquisitionCraft
	case EventEarned:
		return domainsteam.AcquisitionDrop
	case EventMarketReturn:
		return domainsteam.AcquisitionReturned
	default:
		return domainsteam.AcquisitionUnresolved
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// buildLots groups mutually indistinguishable assets (same market_hash_name
// with no per-asset evidence above medium) into acquisition lots instead of
// forcing arbitrary per-asset assignments.
func buildLots(resolved []Resolution) []domainsteam.AcquisitionLot {
	byName := map[string][]Resolution{}
	for _, r := range resolved {
		if r.MatchConfidence == domainsteam.MatchExact || r.MatchConfidence == domainsteam.MatchHigh {
			continue
		}
		if r.Asset.MarketHashName == "" {
			continue
		}
		byName[r.Asset.MarketHashName] = append(byName[r.Asset.MarketHashName], r)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []domainsteam.AcquisitionLot
	for _, n := range names {
		group := byName[n]
		if len(group) < 2 {
			continue
		}
		qty := 0
		for _, r := range group {
			qty += r.Asset.Amount
		}
		out = append(out, domainsteam.AcquisitionLot{
			ID:             "lot:" + n,
			MarketHashName: n,
			Quantity:       qty,
			Source:         domainsteam.AcquisitionUnresolved,
			Reference:      fmt.Sprintf("%d indistinguishable assets", len(group)),
		})
	}
	return out
}

// steamAccountID derives the stable brokerage account ID.
func steamAccountID(steamID string) string { return "steam-" + strings.TrimSpace(steamID) }

// Fetch assembles one inventory snapshot: current items, provenance,
// valuation and P&L. It never deletes history: partial results are marked
// partial (holdings keep zero-price positions, never erase), and unmatched
// events persist for later reconciliation.
func (c *Client) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	if strings.TrimSpace(c.cfg.SteamID) == "" {
		return domainsync.BrokerSnapshot{}, fmt.Errorf("steam: STEAM_ID not configured")
	}
	now := time.Now().UTC()
	accountID := steamAccountID(c.cfg.SteamID)

	if err := c.ensureAuth(ctx); err != nil {
		return domainsync.BrokerSnapshot{}, err
	}
	items, complete, err := c.fetchInventory(ctx)
	if err != nil && len(items) == 0 {
		return domainsync.BrokerSnapshot{}, err
	}
	// Provenance is best-effort: inventory alone still yields a snapshot.
	events, _, _ := c.fetchHistory(ctx, 0)          //nolint:errcheck // partial history stays usable
	marketTxs, _, _ := c.fetchMarketHistory(ctx)    //nolint:errcheck // partial history stays usable
	trades, _, _ := c.fetchTrades(ctx, time.Time{}) //nolint:errcheck // partial history stays usable
	market := make(map[string]domainsteam.MarketTransaction, len(marketTxs))
	for _, tx := range marketTxs {
		market[tx.ExternalID] = tx
	}
	var before map[string]bool
	if c.store != nil {
		if prev, err := c.store.LatestSnapshot(ctx, c.cfg.SteamID); err == nil {
			before = make(map[string]bool, len(prev.Assets))
			for _, a := range prev.Assets {
				before[a.AssetID] = true
			}
		}
	}
	resolved, unmatched := Resolve(items, before, events, market, trades)
	if err := c.persistProvenance(ctx, items, complete, now, events, marketTxs, trades, resolved, unmatched); err != nil {
		return domainsync.BrokerSnapshot{}, err
	}

	account := brokerage.Account{
		ID: accountID, Name: "Steam CS2",
		Type: brokerage.AccountTypeSecurities, RawType: "STEAM_CS2",
		Currency: "USD", BalanceCurrency: "USD",
		BrokerageAuthorization: "steam-auth", InstitutionName: "Steam",
		SyncEnabled: true, Status: "open",
		CreatedDate: now, LastHoldingsSync: &now,
		InitialHoldingsDone: true,
		InitialTxSyncDone:   complete,
	}
	if complete {
		account.LastTxSync = &now
	}
	positions := make([]brokerage.Position, 0, len(resolved))
	activities := make([]brokerage.Activity, 0, len(resolved))
	for _, r := range resolved {
		price, priceType, valued := c.CurrentPrice(ctx, r.Asset.MarketHashName)
		view := domainsteam.PnL(toDomainAsset(c.cfg.SteamID, r, now), optFloat(valued, price), priceType)
		positions = append(positions, toPosition(r, view))
		if act, ok := toActivity(r, view, accountID, now); ok {
			activities = append(activities, act)
		}
	}
	return domainsync.BrokerSnapshot{
		Connection: brokerage.Connection{
			ID: "steam-conn", AuthorizationID: "steam-auth",
			BrokerageName: "Steam", BrokerageSlug: "steam",
			DisplayName: "Steam", Name: "Steam",
			Status: brokerage.ConnectionActive, UpdatedAt: now,
		},
		Accounts:   []brokerage.Account{account},
		Holdings:   []brokerage.Holdings{{AccountID: accountID, Positions: positions, CapturedAt: now, Partial: !complete}},
		Activities: map[string][]brokerage.Activity{accountID: activities},
	}, nil
}

// persistProvenance writes the sync outcome to the Steam store. The
// snapshot write fails the sync (durable state matters); provenance rows
// are best-effort and only warn, so a flaky history endpoint never blocks
// holdings. Unmatched events always persist for later reconciliation.
func (c *Client) persistProvenance(ctx context.Context, items []InventoryItem, complete bool, now time.Time, events []historyEvent, marketTxs []domainsteam.MarketTransaction, trades []domainsteam.TradeRecord, resolved []Resolution, unmatched []historyEvent) error {
	if c.store == nil {
		return nil
	}
	steamID := c.cfg.SteamID
	rows := make([]repository.SteamAssetRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, repository.SteamAssetRow{
			AssetID: it.AssetID, ClassID: it.ClassID, InstanceID: it.InstanceID,
			MarketHashName: it.MarketHashName, Amount: it.Amount,
		})
	}
	if err := c.store.SaveSnapshot(ctx, repository.SteamInventorySnapshot{
		ID:      "steam:" + steamID + ":" + now.UTC().Format("20060102T150405Z"),
		SteamID: steamID, TakenAt: now, Complete: complete, Assets: rows,
	}); err != nil {
		return fmt.Errorf("steam: snapshot save: %w", err)
	}
	warn := func(op string, err error) {
		if err != nil {
			c.log.Warn().Err(err).Str("op", op).Msg("steam provenance best-effort write failed")
		}
	}
	evRows := make([]repository.SteamEventRow, 0, len(events))
	for _, ev := range events {
		evRows = append(evRows, repository.SteamEventRow{
			ExternalID: ev.ExternalID, Timestamp: ev.Timestamp, Kind: ev.Kind,
			MarketHashName: ev.MarketHashName, Quantity: ev.Quantity, AssetID: ev.AssetID,
		})
	}
	warn("events", c.store.RecordEvents(ctx, steamID, evRows))
	mktRows := make([]repository.SteamMarketRow, 0, len(marketTxs))
	for _, tx := range marketTxs {
		mktRows = append(mktRows, repository.SteamMarketRow{
			ExternalID: tx.ExternalID, Type: tx.Type, Timestamp: tx.Timestamp,
			MarketHashName: tx.MarketHashName, Quantity: tx.Quantity,
			Gross: tx.Gross, Net: tx.Net, Currency: tx.Currency,
		})
	}
	warn("market", c.store.SaveMarketTransactions(ctx, steamID, mktRows))
	trRows := make([]repository.SteamTradeRow, 0, len(trades))
	for _, tr := range trades {
		given, _ := json.Marshal(tr.Given)       //nolint:errcheck // persistence stores []
		received, _ := json.Marshal(tr.Received) //nolint:errcheck // persistence stores []
		trRows = append(trRows, repository.SteamTradeRow{
			TradeID: tr.TradeID, Timestamp: tr.Timestamp, OtherSteamID: tr.OtherSteamID,
			Status: tr.Status, GivenJSON: given, ReceivedJSON: received,
		})
	}
	warn("trades", c.store.SaveTrades(ctx, steamID, trRows))
	matched := make([]string, 0, len(events)-len(unmatched))
	unmatchedSet := make(map[string]bool, len(unmatched))
	for _, ev := range unmatched {
		unmatchedSet[ev.ExternalID] = true
	}
	for _, ev := range events {
		if !unmatchedSet[ev.ExternalID] {
			matched = append(matched, ev.ExternalID)
		}
	}
	warn("matched", c.store.MarkEventsMatched(ctx, steamID, matched))
	warn("lots", c.store.SaveLots(ctx, steamID, toLotRows(buildLots(resolved))))
	acqs := make([]repository.SteamAcquisitionRow, 0, len(resolved))
	for _, r := range resolved {
		acqs = append(acqs, repository.SteamAcquisitionRow{
			AssetID: r.Asset.AssetID, MarketHashName: r.Asset.MarketHashName,
			AcquiredAt: r.AcquiredAt, Type: string(r.Type), Reference: r.Reference,
			CostBasis: r.CostBasis, CostCurrency: r.CostCurrency,
			MatchMethod: string(r.MatchMethod), MatchConfidence: string(r.MatchConfidence),
		})
	}
	warn("acquisitions", c.store.SaveAcquisitions(ctx, steamID, acqs))
	c.pendingCursor = repository.SyncCursor{
		Scope: "steam-inventory", Position: now.UTC().Format(time.RFC3339),
		Complete: complete, UpdatedAt: now,
	}
	c.pendingDirty = true
	return nil
}

// lotList centralizes lot building for persistence.
func lotList(resolved []Resolution) []domainsteam.AcquisitionLot {
	return buildLots(resolved)
}

func toLotRows(lots []domainsteam.AcquisitionLot) []repository.SteamLotRow {
	out := make([]repository.SteamLotRow, 0, len(lots))
	for _, l := range lots {
		out = append(out, repository.SteamLotRow{
			ID: l.ID, MarketHashName: l.MarketHashName, Quantity: l.Quantity,
			AcquiredAt: l.AcquiredAt, UnitCost: l.UnitCost, CostCurrency: l.CostCurrency,
			Source: string(l.Source), Reference: l.Reference,
		})
	}
	return out
}
func toDomainAsset(steamID string, r Resolution, now time.Time) domainsteam.SteamAsset {
	return domainsteam.SteamAsset{
		SteamID: steamID, AssetID: r.Asset.AssetID,
		ClassID: r.Asset.ClassID, InstanceID: r.Asset.InstanceID,
		MarketHashName: r.Asset.MarketHashName, Amount: r.Asset.Amount,
		FirstSeenAt: now, LastSeenAt: now,
		AcquiredAt: r.AcquiredAt, AcquisitionType: r.Type,
		AcquisitionRef: r.Reference, CostBasis: r.CostBasis,
		CostCurrency: r.CostCurrency, MatchMethod: r.MatchMethod,
		MatchConfidence: r.MatchConfidence,
	}
}

func optFloat(ok bool, v float64) *float64 {
	if !ok {
		return nil
	}
	return &v
}

// toPosition maps one resolved asset to a holding line. Unknown prices
// stay zero-price positions (visible, never erased); unknown basis stays
// zero per the Position contract (never the market price).
func toPosition(r Resolution, view domainsteam.PricedAsset) brokerage.Position {
	price := 0.0
	if view.CurrentPrice != nil {
		price = *view.CurrentPrice
	}
	basis := 0.0
	if view.Asset.CostBasis != nil {
		basis = *view.Asset.CostBasis
	}
	pnl := 0.0
	if view.UnrealizedPnL != nil {
		pnl = *view.UnrealizedPnL
	}
	return brokerage.Position{
		Symbol: brokerage.Symbol{
			Symbol: r.Asset.MarketHashName, RawSymbol: r.Asset.ClassID + "/" + r.Asset.InstanceID,
			Name:     r.Asset.Name,
			Type:     brokerage.SymbolType{Code: "COLLECTIBLE", IsSupported: true, Description: "Steam collectible"},
			Exchange: brokerage.Exchange{Code: "STEAM", Name: "Steam Community Market"},
			Currency: brokerage.Currency{Code: "USD"},
		},
		Units: float64(r.Asset.Amount), Price: price, OpenPnL: pnl,
		AveragePurchasePrice: basis, Currency: brokerage.Currency{Code: "USD"},
	}
}

// toActivity emits acquisition history only when evidence produced a cost
// basis or a transfer: market buys become BUYs, everything else
// TRANSFER_INs. Unresolved assets emit nothing rather than fabricated
// history. Low confidence is flagged for review.
func toActivity(r Resolution, view domainsteam.PricedAsset, accountID string, now time.Time) (brokerage.Activity, bool) {
	if r.MatchConfidence == domainsteam.MatchUnresolved {
		return brokerage.Activity{}, false
	}
	act := brokerage.Activity{
		ID:        "steam:" + r.Asset.AssetID,
		AccountID: accountID,
		Symbol: &brokerage.Symbol{
			Symbol: r.Asset.MarketHashName, RawSymbol: r.Asset.ClassID + "/" + r.Asset.InstanceID,
			Type:     brokerage.SymbolType{Code: "COLLECTIBLE", IsSupported: true},
			Exchange: brokerage.Exchange{Code: "STEAM"},
			Currency: brokerage.Currency{Code: "USD"},
		},
		Units:          float64(r.Asset.Amount),
		Currency:       brokerage.Currency{Code: "USD"},
		RawType:        string(r.Type),
		Description:    fmt.Sprintf("Steam %s (%s/%s)", r.Type, r.MatchMethod, r.MatchConfidence),
		ProviderType:   "steam",
		SourceSystem:   "steam",
		SourceRecordID: "steam:" + r.Asset.AssetID,
		SourceGroupID:  string(r.MatchMethod),
		NeedsReview:    r.MatchConfidence == domainsteam.MatchLow || r.MatchConfidence == domainsteam.MatchMedium,
	}
	act.Type = brokerage.ActivityTransferIn
	if r.Type == domainsteam.AcquisitionMarketBuy && view.Asset.CostBasis != nil {
		act.Type = brokerage.ActivityBuy
		act.Price = *view.Asset.CostBasis
		act.Amount = act.Price * act.Units
		if r.CostCurrency != "" {
			act.Currency = brokerage.Currency{Code: r.CostCurrency}
		}
	}
	if r.AcquiredAt != nil {
		act.TradeDate = *r.AcquiredAt
	} else {
		act.TradeDate = now
		act.NeedsReview = true
	}
	if r.Reference != "" {
		act.ExternalReferenceID = r.Reference
	}
	return act, true
}
