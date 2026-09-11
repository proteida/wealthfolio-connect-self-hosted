// Sync-cursor persistence-object: durable incremental-sync progress.

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

// SyncStatePO is the GORM mapping for the sync_state table: one row per
// incremental-sync scope (e.g. "okx-fills", "ton-tail-<wallet>").
type SyncStatePO struct {
	Scope     string    `gorm:"column:scope;primaryKey;type:text"`
	Position  string    `gorm:"column:position;type:text;not null;default:''"`
	Complete  bool      `gorm:"column:complete;not null;default:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

// TableName pins the GORM-derived table name.
func (SyncStatePO) TableName() string { return "sync_state" }

// ToDomain converts a sync-state PO into its domain counterpart.
func (p SyncStatePO) ToDomain() repository.SyncCursor {
	return repository.SyncCursor{
		Scope:     p.Scope,
		Position:  p.Position,
		Complete:  p.Complete,
		UpdatedAt: p.UpdatedAt,
	}
}

type cursorRepo struct{ db *gorm.DB }

// NewCursorRepository wires a CursorRepository backed by GORM.
func NewCursorRepository(db *gorm.DB) repository.CursorRepository {
	return &cursorRepo{db: db}
}

// Get returns the cursor for scope, or repository.ErrNotFound when absent.
func (r *cursorRepo) Get(ctx context.Context, scope string) (repository.SyncCursor, error) {
	var po SyncStatePO
	err := r.db.WithContext(ctx).First(&po, "scope = ?", scope).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.SyncCursor{}, repository.ErrNotFound
	}
	if err != nil {
		return repository.SyncCursor{}, fmt.Errorf("cursor get: %w", err)
	}
	return po.ToDomain(), nil
}

// Set creates or replaces the cursor for scope.
func (r *cursorRepo) Set(ctx context.Context, c repository.SyncCursor) error {
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now().UTC()
	}
	po := SyncStatePO{Scope: c.Scope, Position: c.Position, Complete: c.Complete, UpdatedAt: c.UpdatedAt}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope"}},
		DoUpdates: clause.AssignmentColumns([]string{"position", "complete", "updated_at"}),
	}).Create(&po).Error
	if err != nil {
		return fmt.Errorf("cursor set: %w", err)
	}
	return nil
}

// Delete clears the cursor for scope.
func (r *cursorRepo) Delete(ctx context.Context, scope string) error {
	if err := r.db.WithContext(ctx).Delete(&SyncStatePO{}, "scope = ?", scope).Error; err != nil {
		return fmt.Errorf("cursor delete: %w", err)
	}
	return nil
}
