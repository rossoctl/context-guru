package dash

import (
	"math"
	"path/filepath"
	"testing"
)

func newFactsRecorder() *Recorder {
	return &Recorder{lastSeen: map[string]int64{}, lastTail: map[string]uint64{}, seenModel: map[string]bool{}}
}

// causeMiss runs one cache-missing request through the same calls proxy/dashcapture.go makes and
// returns the event, so the test sees the stored reason AND cause.
func causeMiss(r *Recorder, session, threadID string, ts int64, f PrefixFacts) *Event {
	return causeMissWith(r, session, threadID, ts, f, 0, 200)
}

// causeMissWith is causeMiss with a cache read and an HTTP status, and it mirrors the gap rule
// in proxy/dashcapture.go: the TTL test uses the gap since the last request on the SAME model.
func causeMissWith(r *Recorder, session, threadID string, ts int64, f PrefixFacts, read int64, status int) *Event {
	f.TS = ts
	seenSession, seenModel, since, _ := r.ObserveThread("t1", session, threadID, f.Model, ts, 0)
	cause, modelGap, known, _ := r.ObservePrefix(session, threadID, "", false, f, status >= 200 && status < 300)
	if known {
		since = modelGap
	}
	e := &Event{CacheWrite: 400_000, CacheRead: read}
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

// A model switch is named model_changed when the thread has no earlier request on the new model.
func TestAModelSwitchIsNamed(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	causeMiss(r, "s1", "", t0, base())
	b := base()
	b.Model = "m2"
	r.seenModel["t1\x00m2"] = true // m2 ran in another session, so this is not a cold start
	e := causeMiss(r, "s1", "", t0+60_000, b)
	if e.CacheMissReason != CachePrefixChange || e.CacheMissCause != CauseModelChanged {
		t.Errorf("reason/cause = %q/%q, want prefix_change/model_changed", e.CacheMissReason, e.CacheMissCause)
	}
}

// A, B, C on one thread: opus, sonnet, opus. The cache entry belongs to a model, so B does not
// refresh A's entry. The gap C measures is from A, not from B.
func TestAnEntryBelongsToItsModel(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	other := base()
	other.Model = "m2"
	run := func(cAfter int64, cRead int64) *Event {
		r := newFactsRecorder()
		r.seenModel["t1\x00m2"] = true
		causeMiss(r, "s1", "", t0, base())
		causeMiss(r, "s1", "", t0+60_000, other)
		return causeMissWith(r, "s1", "", t0+cAfter, base(), cRead, 200)
	}
	// C is 6 min after A but only 5 min after B. Measured from B (the thread's last request) the
	// entry looks live. Measured from A, the last request on C's model, it expired.
	if e := run(6*60_000, 0); e.CacheMissReason != CacheTTLExpiry || e.CacheMissCause != "" {
		t.Errorf("C at 6 min = %q/%q, want ttl_expiry with no cause", e.CacheMissReason, e.CacheMissCause)
	}
	// 2 min after A: the entry is live. A read is a hit, and a miss has no model_changed cause.
	if e := run(2*60_000, 5000); e.CacheMissReason != CacheHit {
		t.Errorf("C at 2 min with a read = %q, want hit", e.CacheMissReason)
	}
	if e := run(2*60_000, 0); e.CacheMissCause == CauseModelChanged || e.CacheMissReason != CacheUnknown {
		t.Errorf("C at 2 min, missed = %q/%q, want unknown and never model_changed", e.CacheMissReason, e.CacheMissCause)
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
	r.ObservePrefix("s1", "", "", false, base(), true)
	if got, _, _, _ := r.ObservePrefix("s1", "p:fork", "", true, changed, true); got != CauseSystemChanged {
		t.Errorf("fork from the primary thread = %q, want %q", got, CauseSystemChanged)
	}
	r = newFactsRecorder()
	r.ObservePrefix("s1", "", "", false, base(), true)
	if got, _, _, _ := r.ObservePrefix("s1", "p:fork", "", false, changed, true); got != "" {
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

// Rewrite, then two normal turns, then a bypass: the earlier rewrite still explains the bypass.
// Only the sticky RewroteEarlier flag can say so, since neither turn before the bypass was rewritten.
func TestAnEarlierRewriteStillExplainsALaterBypass(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	rw := base()
	rw.Rewrote = true
	by := base()
	by.Bypassed = true
	causeMiss(r, "s1", "", t0, rw)
	causeMiss(r, "s1", "", t0+60_000, base())
	causeMiss(r, "s1", "", t0+120_000, base())
	if e := causeMiss(r, "s1", "", t0+180_000, by); e.CacheMissCause != CauseCompactionAfterRewrite {
		t.Errorf("cause = %q, want %q", e.CacheMissCause, CauseCompactionAfterRewrite)
	}
}

// A failed request does not become the thread's baseline.
func TestAFailedRequestDoesNotReplaceTheBaseline(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	causeMiss(r, "s1", "", t0, base())
	bad := base()
	bad.DeclDigest = "d-bad"
	causeMissWith(r, "s1", "", t0+60_000, bad, 0, 400)
	// Same tools as the first request: nothing changed against the last GOOD request.
	if e := causeMiss(r, "s1", "", t0+120_000, base()); e.CacheMissReason != CacheUnknown || e.CacheMissCause != "" {
		t.Errorf("after a failed request = %q/%q, want unknown with no cause", e.CacheMissReason, e.CacheMissCause)
	}
}

// A miss that wrote no cache is not a prefix change, even when something differs.
func TestAMissThatWroteNothingIsNotAPrefixChange(t *testing.T) {
	e := &Event{CacheWrite: 0}
	e.AttributeCache(true, true, 60_000, fiveMin, CauseToolsChanged)
	if e.CacheMissReason != CacheUnknown || e.CacheMissCause != "" {
		t.Errorf("= %q/%q, want unknown with no cause", e.CacheMissReason, e.CacheMissCause)
	}
}

// Overview's prefix_change cost counts the measured bucket only. A miss that is now unknown
// leaves it by design (issue #600): the figure no longer holds guesses.
func TestOverviewPrefixChangeCostCountsOnlyMeasuredRows(t *testing.T) {
	db := openTestDB(t)
	mk := func(ts int64, reason, cause string, cost float64) *Event {
		e := mkEvent(ts, "s1", "m", 10_000, 10_000)
		e.CostUSD, e.BaselineCostUSD = cost, cost
		e.CacheMissReason, e.CacheMissCause = reason, cause
		return e
	}
	if err := db.insertBatch([]*Event{
		mk(1_000, CacheHit, "", 0.10),
		mk(2_000, CachePrefixChange, CauseToolsChanged, 0.50),
		mk(3_000, CacheUnknown, "", 0.70),
		mk(4_000, CacheUnknown, CauseConcurrent, 0.90),
	}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if o.PrefixChangeRequestsAll != 1 || math.Abs(o.PrefixChangeCostAll-0.50) > 1e-12 {
		t.Errorf("prefix_change all = %d rows $%.2f, want 1 row $0.50", o.PrefixChangeRequestsAll, o.PrefixChangeCostAll)
	}
}
