package dash

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
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
// The facts carry no PromptTokens, so the previous prompt reads as unknown: the label behaves
// as it did before #496. usageTurn below covers the known-previous-prompt shape.
func causeMissWith(r *Recorder, session, threadID string, ts int64, f PrefixFacts, read int64, status int) *Event {
	f.TS = ts
	seenSession, seenModel, since, _ := r.ObserveThread("t1", session, threadID, f.Model, ts, 0)
	cause, modelGap, known, _, prevPrompt, _ := r.ObservePrefix(session, threadID, "", false, f, status >= 200 && status < 300)
	if known {
		since = modelGap
	}
	e := &Event{CacheWrite: 400_000, CacheRead: read}
	e.AttributeCache(seenSession, seenModel, since, fiveMin, cause, prevPrompt)
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
	b.Messages = 40
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
	if got, _, _, _, _, _ := r.ObservePrefix("s1", "p:fork", "", true, changed, true); got != CauseSystemChanged {
		t.Errorf("fork from the primary thread = %q, want %q", got, CauseSystemChanged)
	}
	r = newFactsRecorder()
	r.ObservePrefix("s1", "", "", false, base(), true)
	if got, _, _, _, _, _ := r.ObservePrefix("s1", "p:fork", "", false, changed, true); got != "" {
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
	e.AttributeCache(true, true, 60_000, fiveMin, CauseToolsChanged, 0)
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

// Issue #611. A short first request on a model is a cold start, not a model switch.
func TestAShortFirstRequestOnAModelIsColdStart(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	for _, tc := range []struct {
		messages   int
		wantReason string
		wantCause  string
	}{
		{1, CacheColdStart, ""},
		{2, CacheColdStart, ""},
		{3, CachePrefixChange, CauseModelChanged},
		{40, CachePrefixChange, CauseModelChanged},
	} {
		r := newFactsRecorder()
		r.seenModel["t1\x00m2"] = true // another session ran m2: only the thread rule applies
		causeMiss(r, "s1", "", t0, base())
		b := base()
		b.Model, b.Messages = "m2", tc.messages
		e := causeMiss(r, "s1", "", t0+60_000, b)
		if e.CacheMissReason != tc.wantReason || e.CacheMissCause != tc.wantCause {
			t.Errorf("%d messages: %q/%q, want %q/%q", tc.messages, e.CacheMissReason, e.CacheMissCause, tc.wantReason, tc.wantCause)
		}
	}
}

// A short request on a model the thread already ran, inside the TTL, is an ordinary miss.
func TestAShortRequestOnAKnownModelIsNotColdStart(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	a := base()
	a.Messages = 30
	causeMiss(r, "s1", "", t0, a)
	b := base()
	b.Messages = 2
	b.Tools = 21
	e := causeMiss(r, "s1", "", t0+60_000, b)
	if e.CacheMissReason != CachePrefixChange || e.CacheMissCause != CauseToolsChanged {
		t.Errorf("short request on a known model = %q/%q, want prefix_change/tools_changed", e.CacheMissReason, e.CacheMissCause)
	}
	if e := causeMissWith(r, "s1", "", t0+120_000, b, 5000, 200); e.CacheMissReason != CacheHit {
		t.Errorf("short request with a read = %q, want hit", e.CacheMissReason)
	}
}

// Issue #611, second case. A failed request writes no cache entry. The first success after it
// on that model is a cold start, however long the request is.
func TestTheFirstSuccessAfterAFailureIsColdStart(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	for _, status := range []int{400, 502} {
		r := newFactsRecorder()
		r.seenModel["t1\x00m2"] = true
		first := base()
		first.Messages = 100
		causeMiss(r, "s1", "", t0, first)
		long := base()
		long.Model, long.Messages = "m2", 178
		causeMissWith(r, "s1", "", t0+60_000, long, 0, status)
		e := causeMiss(r, "s1", "", t0+120_000, long)
		if e.CacheMissReason != CacheColdStart || e.CacheMissCause != "" {
			t.Errorf("status %d: first success = %q/%q, want cold_start", status, e.CacheMissReason, e.CacheMissCause)
		}
		// The success is now seen. A later miss is an ordinary one again.
		long.Tools = 21
		if e := causeMiss(r, "s1", "", t0+180_000, long); e.CacheMissReason != CachePrefixChange || e.CacheMissCause != CauseToolsChanged {
			t.Errorf("status %d: later miss = %q/%q, want prefix_change/tools_changed", status, e.CacheMissReason, e.CacheMissCause)
		}
	}
}

// A failure AFTER a success on the same model does not erase that success.
func TestAFailureAfterASuccessKeepsTheModelSeen(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	a := base()
	a.Messages = 50
	causeMiss(r, "s1", "", t0, a)
	causeMissWith(r, "s1", "", t0+60_000, a, 0, 502)
	a.Tools = 21
	if e := causeMiss(r, "s1", "", t0+120_000, a); e.CacheMissReason != CachePrefixChange || e.CacheMissCause != CauseToolsChanged {
		t.Errorf("after success then failure = %q/%q, want prefix_change/tools_changed", e.CacheMissReason, e.CacheMissCause)
	}
}

// usageTurn runs one request through the same calls proxy/dashcapture.go makes, with the
// request's own usage numbers, so PrefixFacts.PromptTokens (fresh + read + write) is the figure
// FactsOf records in production. This is the shape issue #496's partial reads have.
func usageTurn(r *Recorder, session, threadID string, ts int64, f PrefixFacts, fresh, read, write int64) *Event {
	f.TS = ts
	f.PromptTokens = fresh + read + write
	seenSession, seenModel, since, _ := r.ObserveThread("t1", session, threadID, f.Model, ts, 0)
	cause, modelGap, known, _, prevPrompt, _ := r.ObservePrefix(session, threadID, "", false, f, true)
	if known {
		since = modelGap
	}
	e := &Event{FreshInput: fresh, CacheRead: read, CacheWrite: write}
	e.AttributeCache(seenSession, seenModel, since, fiveMin, cause, prevPrompt)
	return e
}

// Issue #496. A read alone is not a hit: Claude Code keeps the system prompt and tools under a
// separate breakpoint, so a request that re-wrote the rest of the prompt still reads tens of
// thousands of tokens. A read under half the previous prompt, with more written than read, is a
// miss and takes the normal chain — cold start, then TTL, then the measured cause.
func TestALargePartialReadIsAMiss(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	toolsChanged := base()
	toolsChanged.DeclDigest = "d2"
	// The previous request wrote a 400k-token prompt; this one reads 50k of it and writes 350k.
	run := func(gapMs int64, second PrefixFacts, read, write int64) *Event {
		r := newFactsRecorder()
		usageTurn(r, "s1", "", t0, base(), 10_000, 0, 390_000)
		return usageTurn(r, "s1", "", t0+gapMs, second, 0, read, write)
	}
	cases := []struct {
		name                  string
		gapMs                 int64
		second                PrefixFacts
		read, write           int64
		wantReason, wantCause string
	}{
		{"62 s later, tools changed", 62_000, toolsChanged, 50_000, 350_000, CachePrefixChange, CauseToolsChanged},
		{"6 min later", 6 * 60_000, toolsChanged, 50_000, 350_000, CacheTTLExpiry, ""},
		{"no measured change", 62_000, base(), 50_000, 350_000, CacheUnknown, ""},
		{"a read that covers the prompt", 62_000, base(), 400_000, 1_000, CacheHit, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := run(tc.gapMs, tc.second, tc.read, tc.write)
			if e.CacheMissReason != tc.wantReason || e.CacheMissCause != tc.wantCause {
				t.Errorf("reason/cause = %q/%q, want %q/%q", e.CacheMissReason, e.CacheMissCause, tc.wantReason, tc.wantCause)
			}
		})
	}
	// The first request of a thread has no previous prompt, so a partial-read verdict is
	// impossible: it keeps today's label (a read is a hit).
	r := newFactsRecorder()
	if e := usageTurn(r, "s2", "", t0, base(), 0, 50_000, 350_000); e.CacheMissReason != CacheHit {
		t.Errorf("first request of a thread = %q, want hit (as today)", e.CacheMissReason)
	}
}

// ttlTurn mirrors proxy/dashcapture.go: the TTL comes from the billed tier of the thread's
// previous same-model request, as ObservePrefix returns it.
func ttlTurn(r *Recorder, ts int64, f PrefixFacts, read, write, write1h int64) *Event {
	ev := &Event{TS: ts, CacheRead: read, CacheWrite: write, CacheWrite1h: write1h}
	derived := FactsOf(ev)
	f.TS, f.OneHour, f.NoWrite, f.PromptTokens = ts, derived.OneHour, derived.NoWrite, derived.PromptTokens
	seenSession, seenModel, since, _ := r.ObserveThread("t1", "s", "th", f.Model, ts, 0)
	cause, modelGap, known, _, prevPrompt, prevOneHour := r.ObservePrefix("s", "th", "", false, f, true)
	if known {
		since = modelGap
	}
	ttl := int64(fiveMin)
	if prevOneHour {
		ttl = 3_600_000
	}
	e := &Event{CacheWrite: write, CacheRead: read, CacheWrite1h: write1h}
	e.AttributeCache(seenSession, seenModel, since, ttl, cause, prevPrompt)
	return e
}

func TestTTLExpiryUsesTheBilledTier(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	const min = int64(60_000)
	changed := func() PrefixFacts { f := base(); f.SystemHash = "s2"; return f }
	for _, tc := range []struct {
		name    string
		write1h int64
		gapMin  int64
		next    PrefixFacts
		want    string
	}{
		{"1h thread, miss after 20 min, changed prefix", 400_000, 20, changed(), CachePrefixChange},
		{"1h thread, miss after 20 min, no change", 400_000, 20, base(), CacheUnknown},
		{"1h thread, miss after 70 min", 400_000, 70, changed(), CacheTTLExpiry},
		{"1h asked, billed 5m, miss after 10 min", 0, 10, changed(), CacheTTLExpiry},
		{"5m thread, miss after 10 min", 0, 10, changed(), CacheTTLExpiry},
		{"5m thread, miss after 3 min", 0, 3, changed(), CachePrefixChange},
	} {
		r := newFactsRecorder()
		ttlTurn(r, t0, base(), 0, 400_000, tc.write1h)
		if got := ttlTurn(r, t0+tc.gapMin*min, tc.next, 0, 400_000, tc.write1h).CacheMissReason; got != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTTLTierSurvivesAReadOnlyTurn(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	const min = int64(60_000)
	r := newFactsRecorder()
	ttlTurn(r, t0, base(), 0, 400_000, 400_000)
	ttlTurn(r, t0+2*min, base(), 400_000, 0, 0) // read only: keeps the 1h tier
	f := base()
	f.SystemHash = "s2"
	if got := ttlTurn(r, t0+30*min, f, 0, 400_000, 400_000).CacheMissReason; got != CachePrefixChange {
		t.Errorf("reason = %q, want %q", got, CachePrefixChange)
	}
}

func TestSummarizerOnA1hThreadMissIsPricedAtTheRead(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	ttlTurn(r, t0, base(), 0, 400_000, 400_000)
	f := base()
	f.SystemHash = "s2"
	e := ttlTurn(r, t0+20*60_000, f, 0, 400_000, 400_000)
	p := modelinfo.Price{CacheRead: 1, CacheWrite: 10}
	if got := e.uniqueRate(p, SummarizerComponents[0]); got != p.CacheRead {
		t.Errorf("summarizer rate = %v, want the read rate %v", got, p.CacheRead)
	}
}

func TestTTLExpiryOnAMixedWriteKeepsFiveMinutes(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	r := newFactsRecorder()
	ttlTurn(r, t0, base(), 0, 50_000, 10_000) // 1h head (10k) over a 5m tail (40k)
	// Head still alive, tail re-created, nothing differed.
	if got := ttlTurn(r, t0+20*60_000, base(), 10_000, 40_000, 0).CacheMissReason; got != CacheTTLExpiry {
		t.Errorf("reason = %q, want %q", got, CacheTTLExpiry)
	}
}

func TestTTLTierIsInheritedFromTheParentThread(t *testing.T) {
	r := newFactsRecorder()
	f := base()
	f.TS, f.OneHour = 1_700_000_000_000, true
	r.ObservePrefix("s1", "", "", false, f, true)
	fork := base()
	fork.TS, fork.NoWrite = f.TS+60_000, true // a read-only first request of the fork
	if _, _, _, _, _, oneHour := r.ObservePrefix("s1", "p:fork", "", true, fork, true); !oneHour {
		t.Error("fork's first request did not inherit the parent's 1h tier")
	}
	next := base()
	next.TS, next.NoWrite = f.TS+120_000, true
	if _, _, _, _, _, oneHour := r.ObservePrefix("s1", "p:fork", "", false, next, true); !oneHour {
		t.Error("the fork's second request lost the inherited 1h tier")
	}
}

// restartedWith stores one request (the given cache write), restarts the recorder on the same
// database, seeds it, and returns the new recorder.
func restartedWith(t *testing.T, ts, read, write, write1h int64) *Recorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(&Event{TS: ts, TenantID: "t1", SessionID: "s", ThreadID: "th", Model: "m",
		CacheRead: read, CacheWrite: write, CacheWrite1h: write1h})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.Close()
	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec2.Close() })
	if n, err := rec2.SeedSessions(ts + 1_200_000); err != nil || n != 1 {
		t.Fatalf("SeedSessions = %d, %v; want 1", n, err)
	}
	return rec2
}

// After a restart, a miss 20 minutes later with a changed prefix is labelled from the seeded tier.
func TestSeededTierAfterARestart(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	changed := base()
	changed.DeclDigest = "d2"
	cases := []struct {
		name                 string
		read, write, write1h int64
		wantExpiry           bool
	}{
		{"1h thread is not an expiry", 0, 50_000, 50_000, false},
		{"5m thread is an expiry", 0, 50_000, 0, true},
		{"mixed write seeds 5m", 0, 50_000, 10_000, true},
		{"latest row wrote nothing seeds 5m", 50_000, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := restartedWith(t, t0, c.read, c.write, c.write1h)
			// A restart forgets which models were seen, so the first request on a model reads
			// cold_start. Another thread marks the model seen, as live traffic does within seconds.
			r.ObserveThread("t1", "other", "", "m", t0+60_000, 0)
			got := ttlTurn(r, t0+20*60_000, changed, 0, 50_000, 50_000).CacheMissReason
			if (got == CacheTTLExpiry) != c.wantExpiry {
				t.Errorf("reason = %q, want ttl_expiry=%v", got, c.wantExpiry)
			}
		})
	}
}
