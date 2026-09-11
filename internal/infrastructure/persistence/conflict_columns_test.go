// White-box assertions on upsert conflict column lists: what survives a
// re-sync is a data-correctness property, so it is pinned here directly.

package persistence

import (
	"slices"
	"testing"
)

func TestActivityConflictColumnsRefreshNormalization(t *testing.T) {
	cols := activityConflictColumns()
	for _, want := range []string{
		"price", "units", "amount", "currency_code", "currency_name",
		"type", "subtype", "raw_type", "description",
		"trade_date", "fee", "fee_asset",
		"symbol_ticker", "symbol_raw",
		"source_group_id", "needs_review", "is_external",
	} {
		if !slices.Contains(cols, want) {
			t.Errorf("activityConflictColumns is missing %q: re-syncs would leave it stale", want)
		}
	}
	for _, immutable := range []string{"id", "account_id", "source_record_id", "external_reference_id"} {
		if slices.Contains(cols, immutable) {
			t.Errorf("activityConflictColumns must not contain identity column %q", immutable)
		}
	}
}
