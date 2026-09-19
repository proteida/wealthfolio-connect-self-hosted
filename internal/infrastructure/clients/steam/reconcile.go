package steam

import (
	"context"
	"encoding/json"
	"errors"
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
// records; now anchors the medium-path recency window (weak attribute
// evidence older than mediumEventWindow cannot newly attach — long-held
// items keep their provenance from the stored acquisitions merged by the
// caller instead). Returned resolutions cover every owned asset;
// leftovers are reported separately for the unmatched store.
func Resolve(owned []InventoryItem, before map[string]bool, events []historyEvent, market map[string]domainsteam.MarketTransaction, trades []domainsteam.TradeRecord, now time.Time) (resolved []Resolution, unmatchedEvents []historyEvent) {
	byAsset := map[string][]historyEvent{}
	for _, ev := range events {
		if ev.AssetID != "" {
			byAsset[ev.AssetID] = append(byAsset[ev.AssetID], ev)
		}
	}
	consumed := map[string]bool{}
	consumedTx := map[string]bool{}
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
			if ev, tx, ok := matchMarketPair(events, market, consumed, consumedTx, it); ok {
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
				consumedTx[tx.ExternalID] = true
			}
		}
		// Medium path: attribute proximity without unique identity.
		if r.MatchConfidence == domainsteam.MatchUnresolved {
			if ev, ok := matchAttributes(events, consumed, it, now); ok {
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

// applyStoredAcquisitions reuses acquisition evidence already preserved in
// the database. Whichever record is stronger wins: a weak or missing
// fresh match never overrides a stored exact/high acquisition in the
// returned positions and activities (mirroring the upgrade-only rule
// SaveAcquisitions enforces on write), while stronger fresh evidence
// replaces stale stored rows. Equal ranks compare evidence richness, so
// a fresh match without cost, timestamp or reference cannot displace a
// richer stored record at the same confidence. Whole records win or
// lose: fields are never merged across evidences into chimeras.
func applyStoredAcquisitions(resolved []Resolution, acqs []repository.SteamAcquisitionRow) {
	byAsset := make(map[string]repository.SteamAcquisitionRow, len(acqs))
	for _, a := range acqs {
		byAsset[a.AssetID] = a
	}
	for i := range resolved {
		r := &resolved[i]
		st, ok := byAsset[r.Asset.AssetID]
		if !ok || st.MatchConfidence == "" || st.MatchConfidence == string(domainsteam.MatchUnresolved) {
			continue
		}
		freshRank, storedRank := acquisitionRank(r.MatchConfidence), acquisitionRank(domainsteam.MatchConfidence(st.MatchConfidence))
		if freshRank > storedRank {
			continue
		}
		if freshRank == storedRank &&
			acquisitionEvidenceScore(r.AcquiredAt, r.CostBasis, r.Reference) >= acquisitionEvidenceScore(st.AcquiredAt, st.CostBasis, st.Reference) {
			continue
		}
		r.AcquiredAt = st.AcquiredAt
		r.Type = domainsteam.AcquisitionType(st.Type)
		r.Reference = st.Reference
		r.CostBasis = st.CostBasis
		r.CostCurrency = st.CostCurrency
		r.MatchMethod = domainsteam.MatchMethod(st.MatchMethod)
		r.MatchConfidence = domainsteam.MatchConfidence(st.MatchConfidence)
	}
}

// acquisitionEvidenceScore counts present evidence fields, mirroring the
// persistence upgrade rule so merges and writes agree on richness.
func acquisitionEvidenceScore(acquiredAt *time.Time, cost *float64, ref string) int {
	n := 0
	if acquiredAt != nil && !acquiredAt.IsZero() {
		n++
	}
	if cost != nil {
		n++
	}
	if ref != "" {
		n++
	}
	return n
}

// acquisitionRank orders match confidence so merges keep the stronger
// evidence. It mirrors the persistence upgrade rule without importing it.
func acquisitionRank(conf domainsteam.MatchConfidence) int {
	switch conf {
	case domainsteam.MatchExact:
		return 4
	case domainsteam.MatchHigh:
		return 3
	case domainsteam.MatchMedium:
		return 2
	case domainsteam.MatchLow:
		return 1
	default:
		return 0
	}
}

func matchEvent(evs []historyEvent, it InventoryItem) (historyEvent, bool) {
	for _, ev := range evs {
		// Exact assignment needs acquisition evidence: removals, sales
		// and listings must never become acquisition records.
		if !isAcquisitionKind(ev.Kind) {
			continue
		}
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
// timestamp, name and quantity agree with an unconsumed market buy row.
// Both sides are consumed by the caller; events and transactions are
// visited in deterministic order so identical items cannot all reuse the
// same purchase.
func matchMarketPair(events []historyEvent, market map[string]domainsteam.MarketTransaction, consumedEvents map[string]bool, consumedTx map[string]bool, it InventoryItem) (historyEvent, domainsteam.MarketTransaction, bool) {
	orderedEvents := append([]historyEvent(nil), events...)
	sort.Slice(orderedEvents, func(i, j int) bool {
		if orderedEvents[i].Timestamp.Equal(orderedEvents[j].Timestamp) {
			return orderedEvents[i].ExternalID < orderedEvents[j].ExternalID
		}
		return orderedEvents[i].Timestamp.Before(orderedEvents[j].Timestamp)
	})
	orderedTx := make([]domainsteam.MarketTransaction, 0, len(market))
	for _, tx := range market {
		if consumedTx[tx.ExternalID] {
			continue
		}
		orderedTx = append(orderedTx, tx)
	}
	sort.Slice(orderedTx, func(i, j int) bool {
		if orderedTx[i].Timestamp.Equal(orderedTx[j].Timestamp) {
			return orderedTx[i].ExternalID < orderedTx[j].ExternalID
		}
		return orderedTx[i].Timestamp.Before(orderedTx[j].Timestamp)
	})
	bestGap := 25 * time.Hour
	var bestEv historyEvent
	var bestTx domainsteam.MarketTransaction
	found := false
	for _, ev := range orderedEvents {
		if consumedEvents[ev.ExternalID] || !isAcquisitionKind(ev.Kind) {
			continue
		}
		if ev.MarketHashName != "" && ev.MarketHashName != it.MarketHashName {
			continue
		}
		for _, tx := range orderedTx {
			if tx.Type != "buy" || tx.MarketHashName != it.MarketHashName {
				continue
			}
			if tx.Quantity != it.Amount && tx.Quantity != 1 {
				continue
			}
			gap := absDuration(ev.Timestamp.Sub(tx.Timestamp))
			if gap > 24*time.Hour || gap >= bestGap {
				continue
			}
			bestGap = gap
			bestEv, bestTx = ev, tx
			found = true
		}
	}
	return bestEv, bestTx, found
}

// mediumEventWindow bounds the medium path: attribute proximity without
// unique identity is only evidence while fresh. Older weak events cannot
// newly attach (long-held items keep provenance from stored acquisitions,
// merged by the caller before the snapshot is assembled).
const mediumEventWindow = 72 * time.Hour

// matchAttributes falls back to class/instance/name proximity within a
// three-day window of now. Quantity must agree; identity stays medium at
// best.
func matchAttributes(events []historyEvent, consumed map[string]bool, it InventoryItem, now time.Time) (historyEvent, bool) {
	for _, ev := range events {
		if consumed[ev.ExternalID] || !isAcquisitionKind(ev.Kind) {
			continue
		}
		if ev.Timestamp.IsZero() || absDuration(ev.Timestamp.Sub(now)) > mediumEventWindow {
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

// Cursor scopes for durable incremental progress. The inventory scope
// covers the snapshot itself; the others carry a provenanceCursor each
// (watermark plus provider resume offset) so a MaxPages-capped sync
// continues its backward walk instead of re-scanning only the newest
// pages forever.
// Cursor scope bases for durable incremental progress. Every scope is
// namespaced with the SteamID at use time (see steamCursorScope), so
// switching accounts never inherits another account's watermarks,
// offsets, or empty probes. Previously released unscoped cursors are
// treated as legacy state and never read: each account starts fresh
// (bounded, idempotent re-import) and persists scoped state going
// forward.
const (
	cursorScopeInventory = "steam-inventory"
	cursorScopeHistory   = "steam-history"
	cursorScopeMarket    = "steam-market"
	cursorScopeTrades    = "steam-trades"
	// cursorScopeEmptyProbe marks one unconfirmed complete-empty
	// observation. It is written eagerly (outside SnapshotCommitted,
	// which never runs for the failed sync) and read back by
	// confirmEmptyInventory: a probe newer than the latest complete
	// snapshot means no successful sync intervened, i.e. two
	// consecutive empty observations.
	cursorScopeEmptyProbe = "steam-empty-probe"
)

// steamCursorScope namespaces a cursor base with the account ID so sync
// state can never leak across Steam accounts sharing one database.
func steamCursorScope(base, steamID string) string {
	return base + ":" + strings.TrimSpace(steamID)
}

// provenanceCursor is the durable walk state for one provenance source:
// the watermark (newest timestamp fully covered by a complete run) plus
// the provider offset for continuing a MaxPages-capped walk. It is stored
// as JSON in the cursor Position, which stays an opaque string to the
// cursor store.
type provenanceCursor struct {
	Watermark time.Time       `json:"watermark"`
	Resume    json.RawMessage `json:"resume,omitempty"`
}

// provenanceOutcome is one source's single-run result: whether its range
// completed, the newest observed timestamp (zero when nothing was
// observed — the watermark must never advance on empty runs), and the
// provider offset to resume from when the page cap hit.
type provenanceOutcome struct {
	Complete bool
	Newest   time.Time
	Resume   json.RawMessage
}

// loadProvenanceCursor reads the durable walk state for scope. Bare
// RFC3339 positions written by previous releases decode as a watermark
// with no resume offset.
func (c *Client) loadProvenanceCursor(ctx context.Context, scope string) provenanceCursor {
	var pc provenanceCursor
	if c.cursors == nil {
		return pc
	}
	cur, err := c.cursors.Get(ctx, scope)
	if err != nil {
		return pc
	}
	if err := json.Unmarshal([]byte(cur.Position), &pc); err != nil {
		if t, terr := time.Parse(time.RFC3339, cur.Position); terr == nil {
			pc.Watermark = t
		}
	}
	return pc
}

// mergeProvenanceCursor folds a run outcome into durable state: the
// watermark advances only to actually observed timestamps (never to
// serve time), and a page-cap stop keeps its resume offset while any
// completed run clears it. An incomplete outcome with no resume offset
// (HTTP error, stalled cursor, failed source write) preserves the prior
// state entirely: advancing the watermark to partial rows would let the
// next fresh walk stop above the unfetched pages and skip them forever.
func mergeProvenanceCursor(prev provenanceCursor, out provenanceOutcome) provenanceCursor {
	if !out.Complete && len(out.Resume) == 0 {
		return prev
	}
	next := provenanceCursor{Watermark: prev.Watermark}
	if !out.Newest.IsZero() && out.Newest.After(next.Watermark) {
		next.Watermark = out.Newest
	}
	if !out.Complete && len(out.Resume) > 0 {
		next.Resume = out.Resume
	}
	return next
}

func (c *Client) storeProvenanceCursor(scope string, pc provenanceCursor, complete bool, now time.Time) {
	raw, err := json.Marshal(pc)
	if err != nil {
		return
	}
	c.pendingCursors = append(c.pendingCursors, repository.SyncCursor{
		Scope: scope, Position: string(raw), Complete: complete, UpdatedAt: now,
	})
	c.pendingDirty = true
}

// provenanceOutcomes carries one run's per-source results into
// persistence, where cursor advancement is decided.
type provenanceOutcomes struct {
	History provenanceOutcome
	Market  provenanceOutcome
	Trades  provenanceOutcome
}

// decodeMarketResume parses a stored market offset (ok=false when absent
// or corrupt: the walk restarts at zero).
func decodeMarketResume(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var mr marketResume
	if err := json.Unmarshal(raw, &mr); err != nil || mr.Offset < 0 {
		return 0, false
	}
	return mr.Offset, true
}

// decodeHistoryResume parses a stored provider offset back into a
// history cursor (nil when absent or corrupt: the walk restarts newest).
func decodeHistoryResume(raw json.RawMessage) *historyCursor {
	if len(raw) == 0 {
		return nil
	}
	var hc historyCursor
	if err := json.Unmarshal(raw, &hc); err != nil || hc.empty() {
		return nil
	}
	return &hc
}

// decodeTradeResume parses a stored provider offset back into a trade
// resume (nil when absent or corrupt).
func decodeTradeResume(raw json.RawMessage) *tradeResume {
	if len(raw) == 0 {
		return nil
	}
	var tr tradeResume
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil
	}
	return &tr
}

// mustMarshalResume encodes a resume offset for durable storage (nil
// when there is nothing to resume).
func mustMarshalResume(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case *historyCursor:
		if t == nil {
			return nil
		}
	case *tradeResume:
		if t == nil {
			return nil
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// confirmEmptyInventory decides whether a complete empty inventory may
// clear holdings and lots. It fails closed: any database read failure
// aborts the sync instead of authorizing a wipe. Only
// repository.ErrNotFound (no previous snapshot, nothing to erase) or an
// already-empty baseline permits at once; otherwise a previous
// unconfirmed probe newer than the latest complete snapshot confirms the
// second consecutive observation.
func (c *Client) confirmEmptyInventory(ctx context.Context) (bool, error) {
	if c.store == nil {
		return true, nil
	}
	prev, err := c.store.LatestSnapshot(ctx, c.cfg.SteamID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("steam: cannot confirm empty inventory: %w", err)
	}
	if len(prev.Assets) == 0 {
		return true, nil
	}
	if c.cursors != nil {
		if probe, err := c.cursors.Get(ctx, steamCursorScope(cursorScopeEmptyProbe, c.cfg.SteamID)); err == nil {
			if pt, terr := time.Parse(time.RFC3339, probe.Position); terr == nil && !pt.IsZero() && !prev.TakenAt.After(pt) {
				return true, nil
			}
		}
	}
	return false, nil
}

// stalePriceMaxAge bounds the stale-price fallback below: a stored
// quote older than this stays unvalued rather than misleading. Within
// the window, a quote that crossed the dust threshold since observation
// self-corrects on the next successful fetch.
const stalePriceMaxAge = 24 * time.Hour

// steamAccountID derives the stable brokerage account ID.
func steamAccountID(steamID string) string { return "steam-" + strings.TrimSpace(steamID) }

// Fetch assembles one inventory snapshot: current items, provenance,
// valuation and P&L. It never deletes history: partial results are marked
// partial (when the value filter is off, holdings keep zero-price
// positions, never erase), and unmatched events persist for later
// reconciliation. When MinItemValueUSD > 0, only stacks with a
// combined quantity × price strictly above the threshold are emitted as
// positions and activities (amounts aggregate by market name, so nine
// $4.43 cases count $39.87 together); unpriced stacks are excluded too
// (unknown value proves nothing), while every item stays in the
// persisted snapshot so a later price rise re-admits it.
func (c *Client) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	if strings.TrimSpace(c.cfg.SteamID) == "" {
		return domainsync.BrokerSnapshot{}, fmt.Errorf("steam: STEAM_ID not configured")
	}
	now := time.Now().UTC()
	accountID := steamAccountID(c.cfg.SteamID)

	// The price breaker is per-run: a throttled sync must not poison all
	// future syncs. Each Fetch starts half-open; repeated failures within
	// this run still trip it (see pricing loop below).
	c.priceFails.Store(0)
	c.emptyUnconfirmed = false

	if err := c.ensureAuth(ctx); err != nil {
		return domainsync.BrokerSnapshot{}, err
	}
	items, complete, err := c.fetchInventory(ctx)
	if err != nil && len(items) == 0 {
		return domainsync.BrokerSnapshot{}, err
	}
	if len(items) == 0 && complete && err == nil {
		// A complete empty inventory is either a wiped account or a
		// lying endpoint, and the two look identical. The first
		// sighting is recorded as an incomplete probe snapshot and
		// fails closed (holdings and lots stay untouched); a second
		// consecutive sighting confirms a genuinely emptied
		// inventory and proceeds. A first sync with no previous
		// snapshot has nothing to erase and proceeds at once.
		confirmed, cerr := c.confirmEmptyInventory(ctx)
		if cerr != nil {
			return domainsync.BrokerSnapshot{}, cerr
		}
		c.emptyUnconfirmed = !confirmed
	}
	hasSession := strings.TrimSpace(c.cfg.Session) != "" || c.auth != nil ||
		strings.TrimSpace(c.cfg.RefreshToken) != ""
	hasAPIKey := strings.TrimSpace(c.cfg.APIKey) != ""
	// Provenance is best-effort: inventory alone still yields a snapshot.
	// Failures are logged (paths and status codes only, never secrets) so
	// a dead session or revoked token is visible instead of silent.
	// Private endpoints are skipped when their credential is absent
	// instead of burning retries on guaranteed 403s.
	var events []historyEvent
	var descs map[string]historyDesc
	var histErr error
	histOutcome := provenanceOutcome{}
	if hasSession {
		histPrev := c.loadProvenanceCursor(ctx, steamCursorScope(cursorScopeHistory, c.cfg.SteamID))
		histStart, histStop := decodeHistoryResume(histPrev.Resume), histPrev.Watermark
		if histStart != nil {
			histStop = time.Time{}
		}
		var histResume *historyCursor
		events, descs, histOutcome.Complete, histResume, histErr = c.fetchHistory(ctx, histStart, histStop)
		histOutcome.Newest = newestEventTime(events)
		histOutcome.Resume = mustMarshalResume(histResume)
	} else {
		histErr = fmt.Errorf("steam: no session; skipping private inventory history")
	}
	if descs == nil {
		descs = map[string]historyDesc{}
	}
	names := make(map[string]string, len(descs))
	for k, d := range descs {
		names[k] = d.MarketHashName
	}
	var marketTxs []domainsteam.MarketTransaction
	var marketErr error
	marketOutcome := provenanceOutcome{}
	if hasSession {
		// Resume a capped backfill where it stopped, rewound by one
		// page so offset shifts cannot open a gap (the overlap
		// re-imports idempotently through external-ID upserts).
		mktStart := 0
		if off, ok := decodeMarketResume(c.loadProvenanceCursor(ctx, steamCursorScope(cursorScopeMarket, c.cfg.SteamID)).Resume); ok && off > 0 {
			mktStart = max(0, off-marketResumeOverlap)
		}
		var mktNext int
		marketTxs, marketOutcome.Complete, mktNext, marketErr = c.fetchMarketHistory(ctx, c.cfg.SteamID, names, mktStart)
		if marketOutcome.Complete {
			marketOutcome.Newest = now
		}
		if mktNext > 0 {
			marketOutcome.Resume = mustMarshalResume(marketResume{Offset: mktNext})
		}
	} else {
		marketErr = fmt.Errorf("steam: no session; skipping market history")
	}
	var trades []domainsteam.TradeRecord
	var tradeErr error
	tradeOutcome := provenanceOutcome{}
	if hasAPIKey {
		tradePrev := c.loadProvenanceCursor(ctx, steamCursorScope(cursorScopeTrades, c.cfg.SteamID))
		tradeStart, tradeStop := decodeTradeResume(tradePrev.Resume), tradePrev.Watermark
		if tradeStart != nil {
			tradeStop = time.Time{}
		}
		var tradeResumeOut *tradeResume
		trades, tradeOutcome.Complete, tradeResumeOut, tradeErr = c.fetchTrades(ctx, tradeStart, tradeStop)
		tradeOutcome.Newest = newestTradeTime(trades)
		tradeOutcome.Resume = mustMarshalResume(tradeResumeOut)
	} else {
		tradeErr = fmt.Errorf("steam: no API key; skipping trade history")
	}
	if histErr != nil {
		c.log.Warn().Err(histErr).Msg("steam inventory history unavailable; proceeding without provenance")
	}
	if marketErr != nil {
		c.log.Warn().Err(marketErr).Msg("steam market history unavailable; proceeding without cost basis")
	}
	if tradeErr != nil {
		c.log.Warn().Err(tradeErr).Msg("steam trade history unavailable; proceeding without trade evidence")
	}
	c.log.Info().
		Int("inventory", len(items)).
		Int("history_events", len(events)).
		Int("market_txs", len(marketTxs)).
		Int("trades", len(trades)).
		Bool("complete", complete).
		Msg("steam provenance fetched")
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
		// Durable provenance: previously unmatched events are reloaded
		// so evidence beyond MaxPages is not lost when each sync only
		// downloads the newest pages.
		if stored, err := c.store.UnmatchedEvents(ctx, c.cfg.SteamID); err == nil && len(stored) > 0 {
			seen := make(map[string]bool, len(events)+len(stored))
			for _, ev := range events {
				seen[ev.ExternalID] = true
			}
			for _, s := range stored {
				if seen[s.ExternalID] {
					continue
				}
				events = append(events, historyEvent{
					ExternalID: s.ExternalID, Timestamp: s.Timestamp, Kind: s.Kind,
					MarketHashName: s.MarketHashName, Quantity: s.Quantity, AssetID: s.AssetID,
				})
			}
		}
	}
	resolved, unmatched := Resolve(items, before, events, market, trades, now)
	if c.store != nil && len(items) > 0 {
		// Temporary provenance failures must not degrade known items:
		// stored acquisitions refill resolutions the fresh evidence
		// could not reach. The lookup is keyed by current asset IDs,
		// not by the complete snapshot baseline, so assets first seen
		// in a partial sync restore their evidence too.
		ids := make([]string, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.AssetID)
		}
		if acqs, err := c.store.AcquisitionsForAssets(ctx, c.cfg.SteamID, ids); err == nil {
			applyStoredAcquisitions(resolved, acqs)
		}
	}
	// Transaction sync is done only when inventory and every provenance
	// source completed. Sources skipped for missing credentials do not
	// block it: a public-only setup can still finish transaction sync.
	txComplete := complete &&
		(histOutcome.Complete || !hasSession) &&
		(marketOutcome.Complete || !hasSession) &&
		(tradeOutcome.Complete || !hasAPIKey)
	outcomes := provenanceOutcomes{History: histOutcome, Market: marketOutcome, Trades: tradeOutcome}
	// An unconfirmed empty probe is stored as an incomplete snapshot so
	// it can never become the reconciliation baseline; confirmation
	// travels separately through the empty-probe cursor below.
	if err := c.persistProvenance(ctx, items, complete && !c.emptyUnconfirmed, outcomes, now, events, marketTxs, trades, resolved, unmatched); err != nil {
		return domainsync.BrokerSnapshot{}, err
	}
	if c.emptyUnconfirmed {
		// Record the probe eagerly: SnapshotCommitted never runs for
		// this failed sync, so staged cursors would die with it. A
		// probe write failure only delays confirmation (fail closed
		// again), never authorizes a wipe.
		if c.cursors != nil {
			probe := repository.SyncCursor{
				Scope: steamCursorScope(cursorScopeEmptyProbe, c.cfg.SteamID), Position: now.UTC().Format(time.RFC3339), UpdatedAt: now,
			}
			if perr := c.cursors.Set(ctx, probe); perr != nil {
				c.log.Warn().Err(perr).Msg("steam empty probe not recorded; next empty sync will fail closed again")
			}
		}
		return domainsync.BrokerSnapshot{}, steamErr("inventory", fmt.Errorf("complete empty inventory unconfirmed: refusing to clear holdings"))
	}

	account := brokerage.Account{
		ID: accountID, Name: "Steam CS2",
		Type: brokerage.AccountTypeSecurities, RawType: "STEAM_CS2",
		Currency: "USD", BalanceCurrency: "USD",
		BrokerageAuthorization: "steam-auth", InstitutionName: "Steam",
		SyncEnabled: true, Status: "open",
		CreatedDate: now, LastHoldingsSync: &now,
		InitialHoldingsDone: true,
		InitialTxSyncDone:   txComplete,
	}
	if txComplete {
		account.LastTxSync = &now
	}
	positions := make([]brokerage.Position, 0, len(resolved))
	activities := make([]brokerage.Activity, 0, len(resolved))
	// retracted collects the activity source_record_ids of filtered items
	// so the sync engine can purge stale rows previously upserted for
	// them (UpsertBatch never deletes on its own).
	retracted := make([]string, 0)
	skippedDust := 0
	skippedUnpriced := 0
	skippedNames := make([]string, 0, 8)
	joinMissCount := 0
	joinMissSample := make([]string, 0, 8)
	// groupQty aggregates owned amounts by normalized market name so the
	// dust filter applies to the stack total (count × price), not to each
	// asset row: nine Gamma 2 Cases at $4.43 are $39.87 together, even
	// though every row alone is below the threshold.
	groupQty := make(map[string]int, len(resolved))
	for _, r := range resolved {
		if r.Asset.MarketHashName == "" {
			continue
		}
		groupQty[priceDedupeKey(r.Asset.MarketHashName)] += r.Asset.Amount
	}
	// One provider call per distinct name: duplicates share the quote.
	// The shared cache dedupes across syncs; this dedupes within one.
	// Observed times travel along for history-currency validation.
	// Dedupe on the normalized key (same mapping as PriceAssetKey) so
	// names differing only in case/whitespace share one quote.
	priced := map[string]pricedQuote{}
	priceOf := func(name string) (float64, string, bool) {
		key := priceDedupeKey(name)
		if v, ok := priced[key]; ok {
			return v.price, v.priceType, v.ok
		}
		// Transient fetch failures already trip the run breaker inside
		// CurrentPrice; here they surface as unvalued, never as erasures.
		// The error is debug-logged per name so throttled items can be
		// told apart from truly unlisted ones when diagnosing syncs.
		price, priceType, observed, ok, perr := c.CurrentPrice(ctx, name)
		if perr != nil {
			c.log.Debug().Err(perr).Str("name", name).Msg("steam price fetch failed transiently")
		}
		if !ok {
			// The live quote failed: fall back to a recent stored
			// quote so one flaky sync neither flaps the dust filter
			// nor zeroes a known price. Anything older than
			// stalePriceMaxAge (or never observed) stays unvalued.
			price, priceType, ok = c.stalePrice(ctx, name)
			observed = time.Time{}
		}
		priced[key] = pricedQuote{price: price, priceType: priceType, observed: observed, ok: ok}
		return price, priceType, ok
	}
	pricing := true
	unpriced := 0
	for _, r := range resolved {
		if !r.Asset.HasDescription {
			joinMissCount++
			if len(joinMissSample) < 8 {
				joinMissSample = append(joinMissSample, r.Asset.AssetID+"/"+r.Asset.ClassID+"/"+r.Asset.InstanceID)
			}
		}
		if pricing && c.priceFails.Load() >= priceFailBreaker {
			c.log.Warn().Msg("steam price provider failing repeatedly; leaving remaining items unvalued")
			pricing = false
		}
		var price float64
		var priceType string
		var valued bool
		if pricing {
			if !pricable(r.Asset) {
				// Unmarketable collectibles (Veteran Coins, drops, empty
				// names from partial pages) never have a market quote:
				// leave unvalued without burning a provider call or
				// tripping the breaker.
				unpriced++
			} else {
				price, priceType, valued = priceOf(r.Asset.MarketHashName)
			}
		} else if pricable(r.Asset) {
			// Breaker tripped: no live calls, but cheap stored quotes
			// still stabilize the filter and the display instead of
			// zeroing everything behind the failure.
			price, priceType, valued = c.stalePrice(ctx, r.Asset.MarketHashName)
		} else {
			unpriced++
		}
		if c.cfg.MinItemValueUSD > 0 {
			// Strict dust filter on the stack total: only groups whose
			// combined quantity × price sits strictly above the
			// threshold are emitted. No grandfathering (a price drop
			// must hide the stack again) and no unpriced exception
			// (unknown value is not proven value; zero-price positions
			// would leak dust into the UI). Filtered items stay in the
			// persisted snapshot, so a later price rise re-admits them
			// and history is never lost.
			groupTotal := 0.0
			if valued {
				groupTotal = float64(groupQty[priceDedupeKey(r.Asset.MarketHashName)]) * price
			}
			if !valued || groupTotal <= c.cfg.MinItemValueUSD {
				if valued {
					skippedDust++
				} else {
					skippedUnpriced++
				}
				if len(skippedNames) < 8 {
					skippedNames = append(skippedNames, r.Asset.MarketHashName)
				}
				retracted = append(retracted, "steam:"+r.Asset.AssetID)
				continue
			}
		}
		view := domainsteam.PnL(toDomainAsset(c.cfg.SteamID, r, now), optFloat(valued, price), priceType)
		positions = append(positions, toPosition(r, view))
		if act, ok := toActivity(r, view, accountID, now); ok {
			activities = append(activities, act)
		}
	}
	if skippedDust+skippedUnpriced > 0 {
		c.log.Info().Int("skipped_dust", skippedDust).Int("skipped_unpriced", skippedUnpriced).Float64("threshold", c.cfg.MinItemValueUSD).Strs("names", skippedNames).Msg("steam items below value threshold excluded")
	}
	if unpriced > 0 {
		c.log.Info().Int("unpriced_unmarketable", unpriced).Msg("steam items without market quotes left unvalued")
	}
	if joinMissCount > 0 {
		c.log.Warn().Int("join_misses", joinMissCount).Strs("sample", joinMissSample).Msg("steam assets without descriptions kept out of positions when filter is on")
	}
	phased := time.Now()
	c.backfillPriceHistory(ctx, resolved, priced)
	c.log.Info().Int("positions", len(positions)).Int("activities", len(activities)).Int("retracted", len(retracted)).Dur("assemble", time.Since(phased)).Msg("steam snapshot assembled")
	snap := domainsync.BrokerSnapshot{
		Connection: brokerage.Connection{
			ID: "steam-conn", AuthorizationID: "steam-auth",
			BrokerageName: "Steam", BrokerageSlug: "steam",
			DisplayName: "Steam", Name: "Steam",
			Status: brokerage.ConnectionActive, UpdatedAt: now,
		},
		Accounts:   []brokerage.Account{account},
		Holdings:   []brokerage.Holdings{{AccountID: accountID, Positions: positions, CapturedAt: now, Partial: !complete}},
		Activities: map[string][]brokerage.Activity{accountID: activities},
	}
	if len(retracted) > 0 && complete {
		// Retractions only leave on complete snapshots: a truncated
		// inventory cannot tell filtered dust from items lost to a
		// failed page, and deleting their activities would destroy
		// valid history the next complete sync would have kept.
		snap.RetractedActivities = map[string][]string{accountID: retracted}
	}
	return snap, nil
}

// pricedQuote is one run's quote for a name with its observation time.
// The timestamp distinguishes live observations (which validate
// backfilled history currency) from stale fallbacks (which must not).
type pricedQuote struct {
	price     float64
	priceType string
	observed  time.Time
	ok        bool
}

// stalePrice returns the newest stored quote for name regardless of
// TTL, for filter/display stability when the live endpoint fails. Only
// quotes observed within stalePriceMaxAge qualify: older ones stay
// unvalued rather than misleading, and names never observed have
// nothing to fall back to.
func (c *Client) stalePrice(ctx context.Context, marketHashName string) (float64, string, bool) {
	if c.priceHistory == nil || strings.TrimSpace(marketHashName) == "" {
		return 0, "", false
	}
	now := time.Now().UTC()
	asset, curr := priceKey(marketHashName, c.cfg.Currency)
	p, err := c.priceHistory.Get(ctx, asset, curr, now)
	if err != nil || p.Timestamp.IsZero() || p.Timestamp.After(now.Add(time.Minute)) {
		return 0, "", false
	}
	if now.Sub(p.Timestamp) > stalePriceMaxAge {
		return 0, "", false
	}
	return p.Price, "steam_history", true
}

// pricable reports whether an item can ever have a Steam market quote:
// it needs a name and the marketable flag from its description.
// Unmarketable collectibles (Veteran Coins, non-tradable drops) and
// description-less stragglers from partial pages stay zero-price by
// design and must never consume provider calls, breaker budget or
// history budget.
func pricable(it InventoryItem) bool {
	return it.MarketHashName != "" && it.Marketable
}

// backfillPriceHistory pulls full histories for up to HistoryBudget
// distinct names lacking recent coverage. Each name costs one provider
// call; covered names (a stored point within historyLookback) cost only a
// database read. Unmarketable names cost nothing and consume no budget.
// The fetched series is validated against this run's fresh quote before
// storing, so a session-currency mismatch can never pollute the USD store.
func (c *Client) backfillPriceHistory(ctx context.Context, resolved []Resolution, priced map[string]pricedQuote) {
	if c.priceHistory == nil || c.cfg.HistoryBudget <= 0 {
		return
	}
	seen := map[string]bool{}
	budget := c.cfg.HistoryBudget
	now := time.Now().UTC()
	for _, r := range resolved {
		if !pricable(r.Asset) {
			continue
		}
		name := r.Asset.MarketHashName
		// Dedupe on the normalized key (same mapping as the quote
		// cache above) so case/whitespace variants share one budget
		// slot instead of each burning a history fetch.
		dedupe := priceDedupeKey(name)
		if seen[dedupe] {
			continue
		}
		seen[dedupe] = true
		if budget <= 0 {
			continue
		}
		asset, curr := PriceAssetKey(name, c.cfg.Currency)
		// A single recent current-quote row (written by pricing every
		// sync) must NOT count as history coverage, or the backfill
		// would never fire for priced items. Covered means a real
		// series is stored: rows sourced from pricehistory. Legacy
		// pre-currency rows are purged on startup, so anything
		// non-quote here is a validated USD series.
		covered := false
		if pts, err := c.priceHistory.List(ctx, asset, curr, now.Add(-historyLookback), now); err == nil {
			series := 0
			for _, p := range pts {
				if p.Source != priceSourceQuote {
					series++
				}
			}
			covered = series >= minHistoryPoints
		}
		if covered {
			continue
		}
		quote, haveQuote := 0.0, false
		if pq, ok := priced[priceDedupeKey(name)]; ok && pq.ok && pq.price > 0 && !pq.observed.IsZero() {
			quote, haveQuote = pq.price, true
		}
		if !haveQuote {
			// No fresh quote to validate the series currency
			// against: skip the fetch rather than storing blind.
			// Budget is not consumed; the next sync retries.
			c.log.Info().Str("name", name).Msg("steam price history skipped: no fresh quote for validation")
			continue
		}
		budget--
		n, err := c.SyncPriceHistory(ctx, name, c.priceHistory, quote, haveQuote)
		if err != nil {
			c.log.Warn().Err(err).Str("name", name).Msg("steam price history backfill failed")
			continue
		}
		c.log.Info().Str("name", name).Int("points", n).Msg("steam price history backfilled")
	}
}

// historyLookback bounds the coverage check window. minHistoryPoints is
// how many stored points in that window count as a real series: current
// quotes alone (1-2 rows from pricing) never satisfy it, so backfill
// keeps firing until a full pricehistory lands.
const historyLookback = 30 * 24 * time.Hour

const minHistoryPoints = 10

// snapshot write fails the sync (durable state matters); provenance rows
// are best-effort and only warn, so a flaky history endpoint never blocks
// holdings. Unmatched events always persist for later reconciliation.
// persistProvenance writes the sync outcome to the Steam store. The
// snapshot write fails the sync (durable state matters); provenance rows
// are best-effort and only warn, so a flaky history endpoint never blocks
// holdings. Unmatched events always persist for later reconciliation.
func (c *Client) persistProvenance(ctx context.Context, items []InventoryItem, complete bool, out provenanceOutcomes, now time.Time, events []historyEvent, marketTxs []domainsteam.MarketTransaction, trades []domainsteam.TradeRecord, resolved []Resolution, unmatched []historyEvent) error {
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
		// Nanosecond precision: two syncs within one second (fast
		// retry, manual double-run) must not collide on the
		// append-only primary key. The .999… layout trims trailing
		// zeros, so idle-second IDs keep the old short shape.
		ID:      "steam:" + steamID + ":" + now.UTC().Format("20060102T150405.999999999Z"),
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
	eventsWriteErr := c.store.RecordEvents(ctx, steamID, evRows)
	warn("events", eventsWriteErr)
	mktRows := make([]repository.SteamMarketRow, 0, len(marketTxs))
	for _, tx := range marketTxs {
		mktRows = append(mktRows, repository.SteamMarketRow{
			ExternalID: tx.ExternalID, Type: tx.Type, Timestamp: tx.Timestamp,
			MarketHashName: tx.MarketHashName, Quantity: tx.Quantity,
			Gross: tx.Gross, Net: tx.Net, Currency: tx.Currency,
		})
	}
	marketWriteErr := c.store.SaveMarketTransactions(ctx, steamID, mktRows)
	warn("market", marketWriteErr)
	trRows := make([]repository.SteamTradeRow, 0, len(trades))
	for _, tr := range trades {
		given, _ := json.Marshal(tr.Given)       //nolint:errcheck // persistence stores []
		received, _ := json.Marshal(tr.Received) //nolint:errcheck // persistence stores []
		trRows = append(trRows, repository.SteamTradeRow{
			TradeID: tr.TradeID, Timestamp: tr.Timestamp, OtherSteamID: tr.OtherSteamID,
			Status: tr.Status, GivenJSON: given, ReceivedJSON: received,
		})
	}
	tradesWriteErr := c.store.SaveTrades(ctx, steamID, trRows)
	warn("trades", tradesWriteErr)
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
	if complete {
		warn("lots", c.store.SaveLots(ctx, steamID, toLotRows(buildLots(resolved))))
	} else {
		// A partial inventory must never replace lots: the truncated
		// snapshot would erase valid lots and cost basis. Existing lots
		// stay until a confirmed complete snapshot rebuilds them.
		c.log.Info().Bool("complete", complete).Msg("steam snapshot keeping existing lots")
	}
	acqs := make([]repository.SteamAcquisitionRow, 0, len(resolved))
	for _, r := range resolved {
		acqs = append(acqs, repository.SteamAcquisitionRow{
			AssetID: r.Asset.AssetID, MarketHashName: r.Asset.MarketHashName,
			AcquiredAt: r.AcquiredAt, Type: string(r.Type), Reference: r.Reference,
			CostBasis: r.CostBasis, CostCurrency: r.CostCurrency,
			MatchMethod: string(r.MatchMethod), MatchConfidence: string(r.MatchConfidence),
		})
	}
	acqErr := c.store.SaveAcquisitions(ctx, steamID, acqs)
	warn("acquisitions", acqErr)
	if acqErr != nil {
		// Acquisition evidence is not durable: consumed events must
		// stay unmatched so the next sync re-resolves and retries the
		// write instead of losing older evidence beyond the
		// pagination window. SaveAcquisitions itself is atomic
		// (single transaction), so success means every row landed.
		c.log.Warn().Msg("steam acquisitions not durable; keeping events unmatched for retry")
	} else {
		warn("matched", c.store.MarkEventsMatched(ctx, steamID, matched))
	}
	// Cursors advance only for sources whose rows reached the database.
	// A failed source write zeroes its outcome, which preserves the
	// prior walk state (see mergeProvenanceCursor) so the next sync
	// re-imports the missing rows instead of stopping above them.
	histEff, tradeEff, mktEff := out.History, out.Trades, out.Market
	if eventsWriteErr != nil {
		histEff = provenanceOutcome{}
	}
	if tradesWriteErr != nil {
		tradeEff = provenanceOutcome{}
	}
	if marketWriteErr != nil {
		mktEff = provenanceOutcome{}
	}
	c.pendingCursors = []repository.SyncCursor{
		{Scope: steamCursorScope(cursorScopeInventory, c.cfg.SteamID), Position: now.UTC().Format(time.RFC3339), Complete: complete, UpdatedAt: now},
	}
	c.storeProvenanceCursor(steamCursorScope(cursorScopeHistory, c.cfg.SteamID),
		mergeProvenanceCursor(c.loadProvenanceCursor(ctx, steamCursorScope(cursorScopeHistory, c.cfg.SteamID)), histEff),
		histEff.Complete, now)
	c.storeProvenanceCursor(steamCursorScope(cursorScopeMarket, c.cfg.SteamID),
		mergeProvenanceCursor(c.loadProvenanceCursor(ctx, steamCursorScope(cursorScopeMarket, c.cfg.SteamID)), mktEff),
		mktEff.Complete, now)
	c.storeProvenanceCursor(steamCursorScope(cursorScopeTrades, c.cfg.SteamID),
		mergeProvenanceCursor(c.loadProvenanceCursor(ctx, steamCursorScope(cursorScopeTrades, c.cfg.SteamID)), tradeEff),
		tradeEff.Complete, now)
	return nil
}

// newestEventTime returns the newest inventory-history timestamp (zero
// when nothing was observed: the watermark must never advance on empty
// runs, or newer evidence would be skipped).
func newestEventTime(events []historyEvent) time.Time {
	newest := time.Time{}
	for _, ev := range events {
		if ev.Timestamp.After(newest) {
			newest = ev.Timestamp
		}
	}
	return newest
}

// newestTradeTime returns the newest trade timestamp (zero when nothing
// was observed).
func newestTradeTime(trades []domainsteam.TradeRecord) time.Time {
	newest := time.Time{}
	for _, tr := range trades {
		if tr.Timestamp.After(newest) {
			newest = tr.Timestamp
		}
	}
	return newest
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

// titleOf returns the human-readable title for an item: the market name
// first, then the inventory name, never a bare classid/instanceid pair
// (and never empty — downstream UIs fall back to raw identifiers when the
// title fields are blank).
func titleOf(r Resolution) string {
	if r.Asset.MarketHashName != "" {
		return r.Asset.MarketHashName
	}
	if r.Asset.Name != "" {
		return r.Asset.Name
	}
	return "Steam item " + r.Asset.AssetID
}

// toPosition maps one resolved asset to a holding line. Unknown prices
// stay zero-price positions (visible, never erased) when the value filter
// is off; when MinItemValueUSD > 0 such assets never reach this function.
// Unknown basis stays zero per the Position contract (never the market
// price).
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
		// RawSymbol is the display string by codebase convention (never
		// the internal master): it carries the title, while
		// classid/instanceid live in the steam tables and source IDs.
		Symbol: brokerage.Symbol{
			Symbol: titleOf(r), RawSymbol: titleOf(r),
			Name: titleOf(r), Description: titleOf(r),
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
			Symbol: titleOf(r), RawSymbol: titleOf(r),
			Name: titleOf(r), Description: titleOf(r),
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
			if r.CostCurrency != "USD" {
				// Cost in another currency than the valuation: P&L
				// mixes denominations until FX conversion exists.
				act.NeedsReview = true
			}
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
