package persistence

import (
	"testing"
	"time"
)

func TestFutuLegIDPattern(t *testing.T) {
	m := futuLegIDPattern.FindStringSubmatch("futu-281000123-hk")
	if len(m) != 3 || m[1] != "281000123" || m[2] != "hk" {
		t.Fatalf("pattern mismatch: %v", m)
	}
	for _, id := range []string{"futu-281000123", "futu-abc-hk", "ibkr-U1", "futu-1-", ""} {
		if futuLegIDPattern.FindStringSubmatch(id) != nil {
			t.Fatalf("pattern must reject %q", id)
		}
	}
}

func TestFutuSlugPriority(t *testing.T) {
	if !(futuSlugPriority("hk") < futuSlugPriority("us") &&
		futuSlugPriority("us") < futuSlugPriority("cn") &&
		futuSlugPriority("cn") < futuSlugPriority("sg")) {
		t.Fatal("market priority order broken")
	}
	if futuSlugPriority("xx") != 99 {
		t.Fatal("unknown slugs sort last")
	}
}

func TestPlanFutuActivityMoves(t *testing.T) {
	olds := []futuActivityKey{
		{ID: "h1", AccountID: "futu-1-hk", SourceRecordID: "f1"},
		{ID: "u1", AccountID: "futu-1-us", SourceRecordID: "f1"},
		{ID: "u2", AccountID: "futu-1-us", SourceRecordID: "f2"},
		{ID: "h3", AccountID: "futu-1-hk", SourceRecordID: "f3"},
	}
	move, drop := planFutuActivityMoves(olds, "futu-1-hk", map[string]bool{"f3": true})
	has := func(ids []string, want string) bool {
		for _, id := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
	// f1 collapses to the primary leg's copy; f2 moves (only copy);
	// f3 already imported under the new scheme, old copy drops.
	if !(has(move, "h1") && has(move, "u2") && len(move) == 2) {
		t.Fatalf("move = %v", move)
	}
	if !(has(drop, "u1") && has(drop, "h3") && len(drop) == 2) {
		t.Fatalf("drop = %v", drop)
	}
}

func TestUnionFutuHoldings(t *testing.T) {
	mkSnap := func(account string, balances string, positions string, captured time.Time) HoldingsSnapshotPO {
		return HoldingsSnapshotPO{
			AccountID: account, CapturedAt: captured,
			Balances: []byte(balances), Positions: []byte(positions), Options: []byte("[]"),
		}
	}
	legs := []futuLeg{
		{account: AccountPO{ID: "futu-1-hk"}, slug: "hk"},
		{account: AccountPO{ID: "futu-1-us"}, slug: "us"},
	}
	snaps := []HoldingsSnapshotPO{
		mkSnap("futu-1-hk",
			`[{"Currency":{"Code":"HKD"},"Cash":5000}]`,
			`[{"Symbol":{"Symbol":"0700.HK"},"Units":100}]`,
			time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
		mkSnap("futu-1-us",
			`[{"Currency":{"Code":"USD"},"Cash":700}]`,
			`[{"Symbol":{"Symbol":"0700.HK"},"Units":100},{"Symbol":{"Symbol":"AAPL"},"Units":10}]`,
			time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)),
	}
	po, ok := unionFutuHoldings("futu-1", legs, snaps)
	if !ok {
		t.Fatal("expected a merged snapshot")
	}
	h, err := po.ToDomain()
	if err != nil {
		t.Fatalf("merged snapshot undecodable: %v", err)
	}
	// Primary-leg balances only (never summed across display currencies).
	if len(h.Balances) != 1 || h.Balances[0].Currency.Code != "HKD" {
		t.Fatalf("balances = %+v", h.Balances)
	}
	// Union of positions by symbol: 0700.HK once, AAPL added.
	if len(h.Positions) != 2 {
		t.Fatalf("positions = %+v", h.Positions)
	}
	if !h.CapturedAt.Equal(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("captured_at = %v", h.CapturedAt)
	}
	if _, ok := unionFutuHoldings("futu-1", legs, nil); ok {
		t.Fatal("no snapshots must report false")
	}
}

func TestMergeFutuAccountRowCompletion(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mkLeg := func(sync, txDone, holdDone bool, txSync *time.Time) futuLeg {
		return futuLeg{account: AccountPO{
			ID: "x", SyncEnabled: sync, InitialTxSyncDone: txDone,
			InitialHoldingsDone: holdDone, LastTxSync: txSync,
			BalanceTotal: 100,
		}, slug: "hk"}
	}
	// One incomplete leg keeps the merged account incomplete with no
	// completion timestamps, even though the other leg completed.
	got := mergeFutuAccountRow("futu-1", []futuLeg{
		mkLeg(true, true, true, &t1),
		mkLeg(true, false, true, nil),
	})
	if got.ID != "futu-1" {
		t.Fatalf("id = %q", got.ID)
	}
	if got.InitialTxSyncDone {
		t.Error("partial history must not promote InitialTxSyncDone")
	}
	if got.LastTxSync != nil {
		t.Errorf("LastTxSync must stay unset while incomplete, got %v", got.LastTxSync)
	}
	if !got.InitialHoldingsDone {
		t.Error("unanimous holdings completion must survive")
	}
	if !got.SyncEnabled {
		t.Error("unanimously enabled legs must stay enabled")
	}
	// All legs complete: flags and latest timestamps carry over.
	got = mergeFutuAccountRow("futu-1", []futuLeg{
		mkLeg(true, true, true, &t1),
		mkLeg(false, true, true, &t2),
	})
	if !got.InitialTxSyncDone || got.LastTxSync == nil || !got.LastTxSync.Equal(t2) {
		t.Errorf("complete merge lost progress: %+v", got)
	}
	if got.SyncEnabled {
		t.Error("a disabled leg must survive the merge")
	}
}

func TestFutuLegTimes(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	legs := []futuLeg{
		{account: AccountPO{LastTxSync: &t2}},
		{account: AccountPO{LastTxSync: &t1}},
		{account: AccountPO{}},
	}
	if got := maxTimeOf(legs, func(a AccountPO) *time.Time { return a.LastTxSync }); !got.Equal(t2) {
		t.Fatalf("max = %v", got)
	}
	if got := minTimeOf(legs, func(a AccountPO) *time.Time { return a.LastTxSync }); !got.Equal(t1) {
		t.Fatalf("min = %v", got)
	}
	if maxTimeOf(nil, func(a AccountPO) *time.Time { return a.LastTxSync }) != nil {
		t.Fatal("empty legs must yield nil")
	}
}

func TestChunkStrings(t *testing.T) {
	got := chunkStrings([]string{"a", "b", "c", "d", "e"}, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 {
		t.Fatalf("chunks = %v", got)
	}
	if len(chunkStrings(nil, 2)) != 0 {
		t.Fatal("nil must yield no chunks")
	}
}
