// Historical-price persistence-object: durable market quotes without
// expiration, plus finalized daily candles.

package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
)

// HistoricalPricePO is the GORM mapping for the historical_prices table.
// One row per (asset, quote time, currency, source); re-UPSERTing the same
// key overwrites the price so provider corrections land on the old row.
type HistoricalPricePO struct {
	Asset     string    `gorm:"column:asset;type:text;not null;uniqueIndex:historical_prices_lookup,priority:1"`
	Timestamp time.Time `gorm:"column:ts;not null;uniqueIndex:historical_prices_lookup,priority:2"`
	Currency  string    `gorm:"column:currency;type:text;not null;uniqueIndex:historical_prices_lookup,priority:3"`
	Price     float64   `gorm:"column:price;not null"`
	Source    string    `gorm:"column:source;type:text;not null;uniqueIndex:historical_prices_lookup,priority:4;default:''"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

// TableName pins the GORM-derived table name.
func (HistoricalPricePO) TableName() string { return "historical_prices" }

// ToDomain converts a price PO into its domain counterpart.
func (p HistoricalPricePO) ToDomain() repository.HistoricalPrice {
	return repository.HistoricalPrice{
		Asset:     p.Asset,
		Timestamp: p.Timestamp,
		Currency:  p.Currency,
		Price:     p.Price,
		Source:    p.Source,
		UpdatedAt: p.UpdatedAt,
	}
}

type priceHistoryRepo struct{ db *gorm.DB }

// NewPriceHistoryRepository wires a PriceHistoryRepository backed by GORM.
func NewPriceHistoryRepository(db *gorm.DB) repository.PriceHistoryRepository {
	return &priceHistoryRepo{db: db}
}

// normalizePrice trims codes to upper case and defaults an empty currency
// to USD so equivalent lookups share one key.
func normalizePrice(asset, currency string) (string, string) {
	asset = strings.ToUpper(strings.TrimSpace(asset))
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = "USD"
	}
	return asset, currency
}

// Get returns the latest stored quote at or before at, or
// repository.ErrNotFound.
func (r *priceHistoryRepo) Get(ctx context.Context, asset, currency string, at time.Time) (repository.HistoricalPrice, error) {
	asset, currency = normalizePrice(asset, currency)
	var po HistoricalPricePO
	err := r.db.WithContext(ctx).
		Where("asset = ? AND currency = ? AND ts <= ?", asset, currency, at.UTC()).
		Order("ts DESC").
		First(&po).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.HistoricalPrice{}, repository.ErrNotFound
	}
	if err != nil {
		return repository.HistoricalPrice{}, fmt.Errorf("price history get: %w", err)
	}
	return po.ToDomain(), nil
}

// List returns stored quotes in [from, to], oldest first.
func (r *priceHistoryRepo) List(ctx context.Context, asset, currency string, from, to time.Time) ([]repository.HistoricalPrice, error) {
	asset, currency = normalizePrice(asset, currency)
	var pos []HistoricalPricePO
	if err := r.db.WithContext(ctx).
		Where("asset = ? AND currency = ? AND ts >= ? AND ts <= ?", asset, currency, from.UTC(), to.UTC()).
		Order("ts ASC").
		Find(&pos).Error; err != nil {
		return nil, fmt.Errorf("price history list: %w", err)
	}
	out := make([]repository.HistoricalPrice, 0, len(pos))
	for _, po := range pos {
		out = append(out, po.ToDomain())
	}
	return out, nil
}

// Upsert inserts quotes, overwriting price/source/updated_at on the same
// (asset, timestamp, currency, source) so provider corrections update rows
// instead of duplicating them.
func (r *priceHistoryRepo) Upsert(ctx context.Context, prices []repository.HistoricalPrice) error {
	if len(prices) == 0 {
		return nil
	}
	pos := make([]HistoricalPricePO, 0, len(prices))
	now := time.Now().UTC()
	for _, p := range prices {
		asset, currency := normalizePrice(p.Asset, p.Currency)
		updated := p.UpdatedAt
		if updated.IsZero() {
			updated = now
		}
		pos = append(pos, HistoricalPricePO{
			Asset: asset, Timestamp: p.Timestamp.UTC(), Currency: currency,
			Price: p.Price, Source: strings.TrimSpace(p.Source), UpdatedAt: updated,
		})
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "asset"}, {Name: "ts"}, {Name: "currency"}, {Name: "source"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"price", "source", "updated_at",
		}),
	}).Create(&pos).Error
	if err != nil {
		return fmt.Errorf("price history upsert: %w", err)
	}
	return nil
}
