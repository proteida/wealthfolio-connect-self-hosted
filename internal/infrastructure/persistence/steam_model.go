// Steam collectible persistence-objects: inventory snapshots, provenance
// events, market and trade rows, acquisition lots and per-asset records.
// Imports are idempotent (upserts keyed by external IDs); snapshots are
// append-only so a partial fetch can never erase history.

package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

// SteamSnapshotPO is one persisted inventory state.
type SteamSnapshotPO struct {
	ID       string    `gorm:"column:id;primaryKey;type:text"`
	SteamID  string    `gorm:"column:steam_id;type:text;not null;index:steam_snapshots_steam_idx"`
	TakenAt  time.Time `gorm:"column:taken_at;not null;index:steam_snapshots_steam_idx,priority:2"`
	Complete bool      `gorm:"column:complete;not null;default:false"`
}

// TableName pins the GORM-derived table name.
func (SteamSnapshotPO) TableName() string { return "steam_snapshots" }

// SteamSnapshotAssetPO is one asset line inside a snapshot.
type SteamSnapshotAssetPO struct {
	SnapshotID     string `gorm:"column:snapshot_id;type:text;not null;uniqueIndex:steam_snapshot_assets_uk,priority:1"`
	AssetID        string `gorm:"column:assetid;type:text;not null;uniqueIndex:steam_snapshot_assets_uk,priority:2"`
	ClassID        string `gorm:"column:classid;type:text;not null;default:''"`
	InstanceID     string `gorm:"column:instanceid;type:text;not null;default:''"`
	MarketHashName string `gorm:"column:market_hash_name;type:text;not null;default:''"`
	Amount         int    `gorm:"column:amount;not null;default:1"`
}

// TableName pins the GORM-derived table name.
func (SteamSnapshotAssetPO) TableName() string { return "steam_snapshot_assets" }

// SteamEventPO is one normalized inventory-history row. Matched flags rows
// consumed by a reconciliation; unmatched rows stay for later runs.
type SteamEventPO struct {
	SteamID        string    `gorm:"column:steam_id;type:text;not null;index:steam_events_steam_idx;uniqueIndex:steam_events_steam_uk,priority:1"`
	ExternalID     string    `gorm:"column:external_id;type:text;not null;uniqueIndex:steam_events_steam_uk,priority:2"`
	Timestamp      time.Time `gorm:"column:ts;not null"`
	Kind           string    `gorm:"column:kind;type:text;not null;default:''"`
	MarketHashName string    `gorm:"column:market_hash_name;type:text;not null;default:''"`
	Quantity       int       `gorm:"column:quantity;not null;default:1"`
	AssetID        string    `gorm:"column:assetid;type:text;not null;default:''"`
	Matched        bool      `gorm:"column:matched;not null;default:false"`
}

// TableName pins the GORM-derived table name.
func (SteamEventPO) TableName() string { return "steam_events" }

// SteamMarketTxPO is one normalized Community Market row.
type SteamMarketTxPO struct {
	SteamID        string    `gorm:"column:steam_id;type:text;not null;index:steam_market_txs_steam_idx;uniqueIndex:steam_market_txs_steam_uk,priority:1"`
	ExternalID     string    `gorm:"column:external_id;type:text;not null;uniqueIndex:steam_market_txs_steam_uk,priority:2"`
	Type           string    `gorm:"column:type;type:text;not null;default:''"`
	Timestamp      time.Time `gorm:"column:ts;not null"`
	MarketHashName string    `gorm:"column:market_hash_name;type:text;not null;default:''"`
	ClassID        string    `gorm:"column:classid;type:text;not null;default:''"`
	InstanceID     string    `gorm:"column:instanceid;type:text;not null;default:''"`
	Quantity       int       `gorm:"column:quantity;not null;default:1"`
	Gross          float64   `gorm:"column:gross;not null;default:0"`
	Net            *float64  `gorm:"column:net"`
	Currency       string    `gorm:"column:currency;type:text;not null;default:''"`
}

// TableName pins the GORM-derived table name.
func (SteamMarketTxPO) TableName() string { return "steam_market_transactions" }

// SteamTradePO is one normalized trade with both asset sides as JSON.
type SteamTradePO struct {
	SteamID      string    `gorm:"column:steam_id;type:text;not null;index:steam_trades_steam_idx;uniqueIndex:steam_trades_steam_uk,priority:1"`
	TradeID      string    `gorm:"column:trade_id;type:text;not null;uniqueIndex:steam_trades_steam_uk,priority:2"`
	Timestamp    time.Time `gorm:"column:ts;not null"`
	OtherSteamID string    `gorm:"column:other_steam_id;type:text;not null;default:''"`
	Status       string    `gorm:"column:status;type:text;not null;default:''"`
	GivenJSON    []byte    `gorm:"column:given_json;type:jsonb;not null"`
	ReceivedJSON []byte    `gorm:"column:received_json;type:jsonb;not null"`
}

// TableName pins the GORM-derived table name.
func (SteamTradePO) TableName() string { return "steam_trades" }

// SteamLotPO is one acquisition lot for indistinguishable items.
type SteamLotPO struct {
	SteamID        string    `gorm:"column:steam_id;type:text;not null;index:steam_lots_steam_idx;uniqueIndex:steam_lots_steam_uk,priority:1"`
	ID             string    `gorm:"column:id;type:text;not null;uniqueIndex:steam_lots_steam_uk,priority:2"`
	MarketHashName string    `gorm:"column:market_hash_name;type:text;not null;default:''"`
	Quantity       int       `gorm:"column:quantity;not null;default:0"`
	AcquiredAt     time.Time `gorm:"column:acquired_at;not null"`
	UnitCost       *float64  `gorm:"column:unit_cost"`
	CostCurrency   string    `gorm:"column:cost_currency;type:text;not null;default:''"`
	Source         string    `gorm:"column:source;type:text;not null;default:''"`
	Reference      string    `gorm:"column:reference;type:text;not null;default:''"`
}

// TableName pins the GORM-derived table name.
func (SteamLotPO) TableName() string { return "steam_acquisition_lots" }

// SteamAcquisitionPO binds one assetid to its acquisition evidence. Cost
// NULL means unknown (never zero, which means known-free).
type SteamAcquisitionPO struct {
	SteamID         string     `gorm:"column:steam_id;type:text;not null;index:steam_acquisitions_steam_idx;uniqueIndex:steam_acquisitions_steam_uk,priority:1"`
	AssetID         string     `gorm:"column:assetid;type:text;not null;uniqueIndex:steam_acquisitions_steam_uk,priority:2"`
	MarketHashName  string     `gorm:"column:market_hash_name;type:text;not null;default:''"`
	AcquiredAt      *time.Time `gorm:"column:acquired_at"`
	Type            string     `gorm:"column:type;type:text;not null;default:''"`
	Reference       string     `gorm:"column:reference;type:text;not null;default:''"`
	CostBasis       *float64   `gorm:"column:cost_basis"`
	CostCurrency    string     `gorm:"column:cost_currency;type:text;not null;default:''"`
	MatchMethod     string     `gorm:"column:match_method;type:text;not null;default:''"`
	MatchConfidence string     `gorm:"column:match_confidence;type:text;not null;default:''"`
}

// TableName pins the GORM-derived table name.
func (SteamAcquisitionPO) TableName() string { return "steam_asset_acquisitions" }

type steamAssetRepo struct{ db *gorm.DB }

// NewSteamAssetRepository wires a SteamAssetRepository backed by GORM.
func NewSteamAssetRepository(db *gorm.DB) repository.SteamAssetRepository {
	return &steamAssetRepo{db: db}
}

// LatestSnapshot returns the newest complete snapshot with its assets.
func (r *steamAssetRepo) LatestSnapshot(ctx context.Context, steamID string) (repository.SteamInventorySnapshot, error) {
	var snap SteamSnapshotPO
	err := r.db.WithContext(ctx).
		Where("steam_id = ? AND complete = ?", steamID, true).
		Order("taken_at DESC").First(&snap).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.SteamInventorySnapshot{}, repository.ErrNotFound
	}
	if err != nil {
		return repository.SteamInventorySnapshot{}, fmt.Errorf("steam snapshot get: %w", err)
	}
	var rows []SteamSnapshotAssetPO
	if err := r.db.WithContext(ctx).Where("snapshot_id = ?", snap.ID).Find(&rows).Error; err != nil {
		return repository.SteamInventorySnapshot{}, fmt.Errorf("steam snapshot assets: %w", err)
	}
	out := repository.SteamInventorySnapshot{
		ID: snap.ID, SteamID: snap.SteamID, TakenAt: snap.TakenAt, Complete: snap.Complete,
	}
	for _, row := range rows {
		out.Assets = append(out.Assets, repository.SteamAssetRow{
			AssetID: row.AssetID, ClassID: row.ClassID, InstanceID: row.InstanceID,
			MarketHashName: row.MarketHashName, Amount: row.Amount,
		})
	}
	return out, nil
}

// SaveSnapshot stores a snapshot with its asset rows atomically.
func (r *steamAssetRepo) SaveSnapshot(ctx context.Context, snap repository.SteamInventorySnapshot) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&SteamSnapshotPO{
			ID: snap.ID, SteamID: snap.SteamID, TakenAt: snap.TakenAt, Complete: snap.Complete,
		}).Error; err != nil {
			return fmt.Errorf("steam snapshot save: %w", err)
		}
		rows := make([]SteamSnapshotAssetPO, 0, len(snap.Assets))
		for _, a := range snap.Assets {
			rows = append(rows, SteamSnapshotAssetPO{
				SnapshotID: snap.ID, AssetID: a.AssetID, ClassID: a.ClassID,
				InstanceID: a.InstanceID, MarketHashName: a.MarketHashName, Amount: a.Amount,
			})
		}
		if len(rows) > 0 {
			if err := tx.Create(&rows).Error; err != nil {
				return fmt.Errorf("steam snapshot assets save: %w", err)
			}
		}
		return nil
	})
}

// RecordEvents stores normalized rows, skipping known external IDs.
func (r *steamAssetRepo) RecordEvents(ctx context.Context, steamID string, events []repository.SteamEventRow) error {
	for _, ev := range events {
		po := SteamEventPO{
			SteamID: steamID, ExternalID: ev.ExternalID, Timestamp: ev.Timestamp,
			Kind: ev.Kind, MarketHashName: ev.MarketHashName, Quantity: ev.Quantity,
			AssetID: ev.AssetID, Matched: ev.Matched,
		}
		if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "steam_id"}, {Name: "external_id"}},
			DoNothing: true,
		}).Create(&po).Error; err != nil {
			return fmt.Errorf("steam events record: %w", err)
		}
	}
	return nil
}

// UnmatchedEvents returns stored events not yet bound to an asset.
func (r *steamAssetRepo) UnmatchedEvents(ctx context.Context, steamID string) ([]repository.SteamEventRow, error) {
	var pos []SteamEventPO
	if err := r.db.WithContext(ctx).
		Where("steam_id = ? AND matched = ?", steamID, false).
		Order("ts ASC").Find(&pos).Error; err != nil {
		return nil, fmt.Errorf("steam unmatched events: %w", err)
	}
	out := make([]repository.SteamEventRow, 0, len(pos))
	for _, po := range pos {
		out = append(out, repository.SteamEventRow{
			ExternalID: po.ExternalID, Timestamp: po.Timestamp, Kind: po.Kind,
			MarketHashName: po.MarketHashName, Quantity: po.Quantity,
			AssetID: po.AssetID, Matched: po.Matched,
		})
	}
	return out, nil
}

// MarkEventsMatched flags events consumed by a reconciliation.
func (r *steamAssetRepo) MarkEventsMatched(ctx context.Context, steamID string, externalIDs []string) error {
	if len(externalIDs) == 0 {
		return nil
	}
	if err := r.db.WithContext(ctx).Model(&SteamEventPO{}).
		Where("steam_id = ? AND external_id IN ?", steamID, externalIDs).
		Update("matched", true).Error; err != nil {
		return fmt.Errorf("steam events mark matched: %w", err)
	}
	return nil
}

// SaveMarketTransactions stores normalized market rows, skipping known IDs.
func (r *steamAssetRepo) SaveMarketTransactions(ctx context.Context, steamID string, txs []repository.SteamMarketRow) error {
	for _, tx := range txs {
		po := SteamMarketTxPO{
			SteamID: steamID, ExternalID: tx.ExternalID, Type: tx.Type,
			Timestamp: tx.Timestamp, MarketHashName: tx.MarketHashName,
			ClassID: tx.ClassID, InstanceID: tx.InstanceID,
			Quantity: tx.Quantity, Gross: tx.Gross, Net: tx.Net, Currency: tx.Currency,
		}
		if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "steam_id"}, {Name: "external_id"}},
			DoNothing: true,
		}).Create(&po).Error; err != nil {
			return fmt.Errorf("steam market txs save: %w", err)
		}
	}
	return nil
}

// SaveTrades stores normalized trades, skipping known trade IDs.
func (r *steamAssetRepo) SaveTrades(ctx context.Context, steamID string, trades []repository.SteamTradeRow) error {
	for _, tr := range trades {
		given := nonNilJSON(tr.GivenJSON)
		received := nonNilJSON(tr.ReceivedJSON)
		po := SteamTradePO{
			SteamID: steamID, TradeID: tr.TradeID, Timestamp: tr.Timestamp,
			OtherSteamID: tr.OtherSteamID, Status: tr.Status,
			GivenJSON: given, ReceivedJSON: received,
		}
		if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "steam_id"}, {Name: "trade_id"}},
			DoNothing: true,
		}).Create(&po).Error; err != nil {
			return fmt.Errorf("steam trades save: %w", err)
		}
	}
	return nil
}

// SaveLots replaces the whole lot set: stale names absent from the new set
// are cleared so sold lots cannot linger.
func (r *steamAssetRepo) SaveLots(ctx context.Context, steamID string, lots []repository.SteamLotRow) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("steam_id = ?", steamID).Delete(&SteamLotPO{}).Error; err != nil {
			return fmt.Errorf("steam lots clear: %w", err)
		}
		for _, l := range lots {
			po := SteamLotPO{
				SteamID: steamID, ID: l.ID, MarketHashName: l.MarketHashName,
				Quantity: l.Quantity, AcquiredAt: l.AcquiredAt, UnitCost: l.UnitCost,
				CostCurrency: l.CostCurrency, Source: l.Source, Reference: l.Reference,
			}
			if err := tx.Create(&po).Error; err != nil {
				return fmt.Errorf("steam lots save: %w", err)
			}
		}
		return nil
	})
}

// SaveAcquisitions upserts per-asset acquisition records but never
// downgrades: existing exact/high evidence survives a later sync with
// weaker or missing provenance (e.g. a temporary Steam failure).
func (r *steamAssetRepo) SaveAcquisitions(ctx context.Context, steamID string, acquisitions []repository.SteamAcquisitionRow) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, a := range acquisitions {
			var existing SteamAcquisitionPO
			err := tx.Where("steam_id = ? AND assetid = ?", steamID, a.AssetID).First(&existing).Error
			if err == nil {
				if confidenceRank(existing.MatchConfidence) > confidenceRank(a.MatchConfidence) {
					continue
				}
				if confidenceRank(existing.MatchConfidence) == confidenceRank(a.MatchConfidence) &&
					!strongerOrEqualAcquisition(existing, a) {
					continue
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("steam acquisitions read: %w", err)
			}
			po := SteamAcquisitionPO{
				SteamID: steamID, AssetID: a.AssetID, MarketHashName: a.MarketHashName,
				AcquiredAt: a.AcquiredAt, Type: a.Type, Reference: a.Reference,
				CostBasis: a.CostBasis, CostCurrency: a.CostCurrency,
				MatchMethod: a.MatchMethod, MatchConfidence: a.MatchConfidence,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "steam_id"}, {Name: "assetid"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"market_hash_name", "acquired_at", "type", "reference",
					"cost_basis", "cost_currency", "match_method", "match_confidence",
				}),
			}).Create(&po).Error; err != nil {
				return fmt.Errorf("steam acquisitions save: %w", err)
			}
		}
		return nil
	})
}

// confidenceRank orders match confidence so stored evidence is only
// replaced by equal-or-stronger evidence.
func confidenceRank(conf string) int {
	switch conf {
	case "exact":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// strongerOrEqualAcquisition prefers rows that actually carry evidence
// (timestamp, cost, reference) when confidence ties.
func strongerOrEqualAcquisition(existing SteamAcquisitionPO, next repository.SteamAcquisitionRow) bool {
	score := func(acquiredAt *time.Time, cost *float64, ref string) int {
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
	return score(next.AcquiredAt, next.CostBasis, next.Reference) >=
		score(existing.AcquiredAt, existing.CostBasis, existing.Reference)
}

// CurrentAssets returns asset states from the newest complete snapshot
// joined with acquisition records. Partial snapshots are ignored for the
// join: an asset omitted by a truncated page keeps its persisted
// acquisition evidence instead of losing it until it reappears.
//
// NOTE: assets first discovered in a partial sync are absent from the
// complete baseline by design. Fetch restores their evidence through
// AcquisitionsForAssets instead, which is snapshot-independent.
func (r *steamAssetRepo) CurrentAssets(ctx context.Context, steamID string) ([]repository.SteamAssetState, error) {
	var snap SteamSnapshotPO
	err := r.db.WithContext(ctx).
		Where("steam_id = ? AND complete = ?", steamID, true).
		Order("taken_at DESC").First(&snap).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, repository.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("steam current snapshot: %w", err)
	}
	var rows []SteamSnapshotAssetPO
	if err := r.db.WithContext(ctx).Where("snapshot_id = ?", snap.ID).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("steam current assets: %w", err)
	}
	var acqs []SteamAcquisitionPO
	if err := r.db.WithContext(ctx).Where("steam_id = ?", steamID).Find(&acqs).Error; err != nil {
		return nil, fmt.Errorf("steam current acquisitions: %w", err)
	}
	byAsset := make(map[string]SteamAcquisitionPO, len(acqs))
	for _, a := range acqs {
		byAsset[a.AssetID] = a
	}
	out := make([]repository.SteamAssetState, 0, len(rows))
	for _, row := range rows {
		st := repository.SteamAssetState{
			SteamAssetRow: repository.SteamAssetRow{
				AssetID: row.AssetID, ClassID: row.ClassID, InstanceID: row.InstanceID,
				MarketHashName: row.MarketHashName, Amount: row.Amount,
			},
			FirstSeenAt: snap.TakenAt, LastSeenAt: snap.TakenAt,
		}
		if a, ok := byAsset[row.AssetID]; ok {
			st.AcquiredAt = a.AcquiredAt
			st.Type = a.Type
			st.Reference = a.Reference
			st.CostBasis = a.CostBasis
			st.CostCurrency = a.CostCurrency
			st.MatchMethod = a.MatchMethod
			st.MatchConfidence = a.MatchConfidence
		}
		out = append(out, st)
	}
	return out, nil
}

// AcquisitionsForAssets returns stored acquisition records for the given
// asset IDs, independently of any snapshot baseline.
func (r *steamAssetRepo) AcquisitionsForAssets(ctx context.Context, steamID string, assetIDs []string) ([]repository.SteamAcquisitionRow, error) {
	if len(assetIDs) == 0 {
		return nil, nil
	}
	var pos []SteamAcquisitionPO
	if err := r.db.WithContext(ctx).
		Where("steam_id = ? AND assetid IN ?", steamID, assetIDs).
		Find(&pos).Error; err != nil {
		return nil, fmt.Errorf("steam acquisitions lookup: %w", err)
	}
	out := make([]repository.SteamAcquisitionRow, 0, len(pos))
	for _, po := range pos {
		out = append(out, repository.SteamAcquisitionRow{
			AssetID: po.AssetID, MarketHashName: po.MarketHashName,
			AcquiredAt: po.AcquiredAt, Type: po.Type, Reference: po.Reference,
			CostBasis: po.CostBasis, CostCurrency: po.CostCurrency,
			MatchMethod: po.MatchMethod, MatchConfidence: po.MatchConfidence,
		})
	}
	return out, nil
}
