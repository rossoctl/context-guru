package dash

import "testing"

// The property this whole file exists for: a real row rescued by a tagged ping, written
// before the real-row tagging code existed (so it carries keepalive_pings > 0 but a NULL
// keepalive_strategy_id), recovers the ping's own strategy id — not a guess, the exact value
// arrive() would have reported had the code existed at the time.
func TestBackfillRecoversStrategyFromThePingThatRescuedTheRow(t *testing.T) {
	db := emptyDB(t)

	prevReal := mkEvent(1_000, "s1", "claude", 100, 90)
	prevReal.TenantID = "t1"
	ping := mkEvent(2_000, "s1", "claude", 100, 90)
	ping.TenantID = "t1"
	ping.KeepAlive = true
	ping.KeepAliveStrategyID = "strat-a"
	rescued := mkEvent(3_000, "s1", "claude", 100, 90)
	rescued.TenantID = "t1"
	rescued.KeepAlivePings = 1
	rescued.KeepAliveSavedUSD = 0.42
	// KeepAliveStrategyID left "" — exactly the pre-fix historical shape.

	if err := db.insertBatch([]*Event{prevReal, ping, rescued}); err != nil {
		t.Fatal(err)
	}

	moved, err := db.backfillKeepAliveStrategyID(nil, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("moved %d rows, want 1", moved)
	}

	var got string
	if err := db.sql.QueryRow(`SELECT keepalive_strategy_id FROM requests WHERE ts = 3000`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "strat-a" {
		t.Errorf("recovered strategy = %q, want %q", got, "strat-a")
	}
	// The ping row and the earlier real row must be untouched.
	var pingStrat string
	if err := db.sql.QueryRow(`SELECT keepalive_strategy_id FROM requests WHERE ts = 2000`).Scan(&pingStrat); err != nil {
		t.Fatal(err)
	}
	if pingStrat != "strat-a" {
		t.Errorf("the ping row's own strategy id changed to %q", pingStrat)
	}
}

// A rescue with no matching tagged ping — plain account config or a session override, never a
// strategy — must stay NULL rather than being guessed at.
func TestBackfillLeavesAnUntaggedRescueAlone(t *testing.T) {
	db := emptyDB(t)

	ping := mkEvent(1_000, "s1", "claude", 100, 90)
	ping.TenantID = "t1"
	ping.KeepAlive = true
	// No KeepAliveStrategyID: an account-config ping, never a strategy.
	rescued := mkEvent(2_000, "s1", "claude", 100, 90)
	rescued.TenantID = "t1"
	rescued.KeepAlivePings = 1
	rescued.KeepAliveSavedUSD = 0.10

	if err := db.insertBatch([]*Event{ping, rescued}); err != nil {
		t.Fatal(err)
	}

	moved, err := db.backfillKeepAliveStrategyID(nil, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Fatalf("moved %d rows, want 0 — nothing here to recover", moved)
	}
	var got *string
	if err := db.sql.QueryRow(`SELECT keepalive_strategy_id FROM requests WHERE ts = 2000`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("recovered strategy = %q, want NULL (no tagged ping to attribute to)", *got)
	}
}

// A ping from an EARLIER, already-consumed idle span must never bleed into a LATER rescue in
// the same session — the "after this session's own previous real row" bound is what keeps
// this backfill from ever attributing a rescue to the wrong span's strategy.
func TestBackfillNeverReachesPastAnEarlierRealRow(t *testing.T) {
	db := emptyDB(t)

	oldPing := mkEvent(1_000, "s1", "claude", 100, 90)
	oldPing.TenantID = "t1"
	oldPing.KeepAlive = true
	oldPing.KeepAliveStrategyID = "strat-old"
	firstRescue := mkEvent(2_000, "s1", "claude", 100, 90)
	firstRescue.TenantID = "t1"
	firstRescue.KeepAlivePings = 1
	firstRescue.KeepAliveStrategyID = "strat-old" // already tagged correctly, e.g. by live code
	newPing := mkEvent(3_000, "s1", "claude", 100, 90)
	newPing.TenantID = "t1"
	newPing.KeepAlive = true
	newPing.KeepAliveStrategyID = "strat-new"
	secondRescue := mkEvent(4_000, "s1", "claude", 100, 90)
	secondRescue.TenantID = "t1"
	secondRescue.KeepAlivePings = 1
	// KeepAliveStrategyID left "" — this is the row under test.

	if err := db.insertBatch([]*Event{oldPing, firstRescue, newPing, secondRescue}); err != nil {
		t.Fatal(err)
	}

	moved, err := db.backfillKeepAliveStrategyID(nil, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("moved %d rows, want 1 (only the untagged rescue)", moved)
	}
	var got string
	if err := db.sql.QueryRow(`SELECT keepalive_strategy_id FROM requests WHERE ts = 4000`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "strat-new" {
		t.Errorf("recovered strategy = %q, want %q (the NEW span's ping, not the old span's)", got, "strat-new")
	}
}

// Run twice: the second run must be a true no-op, both in row count and in leaving the marker
// set, matching dedupetext.go's own idempotency guarantee.
func TestBackfillIsIdempotent(t *testing.T) {
	db := emptyDB(t)
	ping := mkEvent(1_000, "s1", "claude", 100, 90)
	ping.TenantID, ping.KeepAlive, ping.KeepAliveStrategyID = "t1", true, "strat-a"
	rescued := mkEvent(2_000, "s1", "claude", 100, 90)
	rescued.TenantID, rescued.KeepAlivePings = "t1", 1
	if err := db.insertBatch([]*Event{ping, rescued}); err != nil {
		t.Fatal(err)
	}

	if moved, err := db.backfillKeepAliveStrategyID(nil, 500, 0); err != nil || moved != 1 {
		t.Fatalf("first run: moved=%d err=%v, want 1/nil", moved, err)
	}
	done, err := db.keepAliveStrategyBackfillDone()
	if err != nil || !done {
		t.Fatalf("marker not set after a full sweep: done=%v err=%v", done, err)
	}
	if moved, err := db.backfillKeepAliveStrategyID(nil, 500, 0); err != nil || moved != 0 {
		t.Fatalf("second run: moved=%d err=%v, want 0/nil", moved, err)
	}
}
