package dash

import (
	"path/filepath"
	"testing"
)

func newFactsRecorder() *Recorder {
	return &Recorder{lastSeen: map[string]int64{}, lastTail: map[string]uint64{}, seenModel: map[string]bool{}}
}

// causeMiss runs one cache-missing request through the same calls proxy/dashcapture.go makes and
// returns the event, so the test sees the stored reason AND cause.
func causeMiss(r *Recorder, session, threadID string, ts int64, f PrefixFacts) *Event {
	seenSession, seenModel, since, _ := r.ObserveThread("t1", session, threadID, f.Model, ts, 0)
	cause := r.ObservePrefix(session, threadID, "", false, f, since)
	e := &Event{CacheWrite: 400_000}
	e.AttributeCache(seenSession, seenModel, since, fiveMin, cause)
	return e
}

func base() PrefixFacts {
	return PrefixFacts{Model: "m", DeclDigest: "d1", Tools: 20, SystemHash: "s1", SystemBlocks: 3, Effort: "high", Thinking: "adaptive"}
}

func TestPrefixCauses(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	const gap = 60_000 // inside the TTL, past the concurrent threshold
	rewrote := func(f PrefixFacts) PrefixFacts { f.Rewrote = true; return f }
	bypass := func(f PrefixFacts) PrefixFacts { f.Bypassed = true; return f }
	cases := []struct {
		name            string
		first, second   PrefixFacts
		gapMs           int64
		wantReason, why string
	}{
		{"tools digest changed", base(), func() PrefixFacts { f := base(); f.DeclDigest = "d2"; return f }(), gap, CachePrefixChange, CauseToolsChanged},
		{"tool count changed", base(), func() PrefixFacts { f := base(); f.Tools = 21; return f }(), gap, CachePrefixChange, CauseToolsChanged},
		{"system hash changed", base(), func() PrefixFacts { f := base(); f.SystemHash = "s2"; return f }(), gap, CachePrefixChange, CauseSystemChanged},
		{"system block count changed", base(), func() PrefixFacts { f := base(); f.SystemBlocks = 4; return f }(), gap, CachePrefixChange, CauseSystemChanged},
		{"effort changed", base(), func() PrefixFacts { f := base(); f.Effort = "low"; return f }(), gap, CachePrefixChange, CauseEffortChanged},
		{"thinking mode changed", base(), func() PrefixFacts { f := base(); f.Thinking = "disabled"; return f }(), gap, CachePrefixChange, CauseEffortChanged},
		{"bypass after a rewritten turn", rewrote(base()), bypass(base()), gap, CachePrefixChange, CauseCompactionAfterRewrite},
		{"bypass flip with no rewrite", base(), bypass(base()), gap, CachePrefixChange, CauseBypassFlip},
		{"no change is unknown", base(), base(), gap, CacheUnknown, ""},
		{"no change, under the concurrent gap", base(), base(), concurrentGapMs - 1, CacheUnknown, CauseConcurrent},
		{"no change, at the concurrent gap", base(), base(), concurrentGapMs, CacheUnknown, ""},
		{"an unread digest is no evidence", base(), func() PrefixFacts { f := base(); f.DeclDigest = ""; return f }(), gap, CacheUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFactsRecorder()
			causeMiss(r, "s1", "", t0, tc.first)
			e := causeMiss(r, "s1", "", t0+tc.gapMs, tc.second)
			if e.CacheMissReason != tc.wantReason || e.CacheMissCause != tc.why {
				t.Errorf("reason/cause = %q/%q, want %q/%q", e.CacheMissReason, e.CacheMissCause, tc.wantReason, tc.why)
			}
		})
	}
}

// A model switch on one thread is a model_changed prefix change. The cold-start rule fires
// only for a model never seen at all, so the first model is seen before the switch.
func TestAModelSwitchIsNamed(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	a := base()
	causeMiss(r, "s1", "", t0, a)
	b := base()
	b.Model = "m2"
	r.seenModel["t1\x00m2"] = true // m2 was used elsewhere, so this is not a cold start
	e := causeMiss(r, "s1", "", t0+60_000, b)
	if e.CacheMissReason != CachePrefixChange || e.CacheMissCause != CauseModelChanged {
		t.Errorf("reason/cause = %q/%q, want prefix_change/model_changed", e.CacheMissReason, e.CacheMissCause)
	}
}

// The rewrite effect ends once a bypassed request has re-sent the original history: the next
// bypass has nothing to blame on an earlier rewrite.
func TestRewriteEffectEndsAfterABypass(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	rw := base()
	rw.Rewrote = true
	by := base()
	by.Bypassed = true
	causeMiss(r, "s1", "", t0, rw)
	if e := causeMiss(r, "s1", "", t0+60_000, by); e.CacheMissCause != CauseCompactionAfterRewrite {
		t.Fatalf("first bypass cause = %q, want %q", e.CacheMissCause, CauseCompactionAfterRewrite)
	}
	if e := causeMiss(r, "s1", "", t0+120_000, by); e.CacheMissReason != CacheUnknown || e.CacheMissCause != "" {
		t.Errorf("second bypass = %q/%q, want unknown with no cause", e.CacheMissReason, e.CacheMissCause)
	}
}

// Interleaved threads: the main thread (tools d1) and a subagent (tools d2) alternate. With a
// thread id each thread compares with its own previous request, so no change is found. With no
// thread id both share one key, and the alternation reads as a tools change on every request:
// the control that shows why the thread id matters, and that the fixture reaches the branch.
func TestInterleavedThreadsCompareWithTheirOwnPreviousRequest(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	main := base()
	sub := base()
	sub.DeclDigest, sub.Tools = "d2", 5
	run := func(mainThread, subThread string) (mainEv, subEv *Event) {
		r := newFactsRecorder()
		causeMiss(r, "s1", mainThread, t0, main)
		causeMiss(r, "s1", subThread, t0+10_000, sub)
		subEv = causeMiss(r, "s1", subThread, t0+70_000, sub)
		mainEv = causeMiss(r, "s1", mainThread, t0+130_000, main)
		return mainEv, subEv
	}
	m, s := run("", "a:S")
	for name, e := range map[string]*Event{"main": m, "sub": s} {
		if e.CacheMissReason != CacheUnknown || e.CacheMissCause != "" {
			t.Errorf("%s thread with ids: %q/%q, want unknown with no cause", name, e.CacheMissReason, e.CacheMissCause)
		}
	}
	m, _ = run("", "")
	if m.CacheMissReason != CachePrefixChange || m.CacheMissCause != CauseToolsChanged {
		t.Fatalf("control: both threads on one key gave %q/%q; the fixture no longer interleaves", m.CacheMissReason, m.CacheMissCause)
	}
}

// A new thread that inherits from another compares with that thread's last request, so a fork
// whose system prompt changed is named. Without the parent it has nothing to compare with.
func TestAnInheritingThreadComparesWithItsParent(t *testing.T) {
	changed := base()
	changed.SystemHash = "s2"
	r := newFactsRecorder()
	r.ObservePrefix("s1", "", "", false, base(), 0)
	if got := r.ObservePrefix("s1", "p:fork", "", true, changed, 60_000); got != CauseSystemChanged {
		t.Errorf("fork from the primary thread = %q, want %q", got, CauseSystemChanged)
	}
	r = newFactsRecorder()
	r.ObservePrefix("s1", "", "", false, base(), 0)
	if got := r.ObservePrefix("s1", "p:fork", "", false, changed, 60_000); got != "" {
		t.Errorf("fork with no parent = %q, want no cause", got)
	}
}

// The cause is stored and read back, and an older file without the column gains it with an
// empty default: the additive migration path.
func TestCacheMissCausePersistsAndMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := mkEvent(1000, "s1", "m", 100, 90)
	e.CacheMissReason, e.CacheMissCause = CachePrefixChange, CauseToolsChanged
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Request(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheMissReason != CachePrefixChange || got.CacheMissCause != CauseToolsChanged {
		t.Errorf("round trip = %q/%q", got.CacheMissReason, got.CacheMissCause)
	}
	// Drop the column to look like a file from before this change, then reopen.
	if _, err := db.sql.Exec(`ALTER TABLE requests DROP COLUMN cache_miss_cause`); err != nil {
		t.Fatalf("simulate an old file: %v", err)
	}
	db.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen an old file: %v", err)
	}
	defer db2.Close()
	old, err := db2.Request(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if old.CacheMissReason != CachePrefixChange || old.CacheMissCause != "" {
		t.Errorf("old row after migration = %q/%q, want prefix_change with an empty cause", old.CacheMissReason, old.CacheMissCause)
	}
}
