package dash

import (
	"testing"
	"time"
)

func bypassEvent(ts int64, accounting string, count func() (int, int)) *Event {
	e := mkEvent(ts, "s-bypass", "m", 0, 0)
	e.Mode, e.TokenAccounting, e.BypassCount = ModeBypass, accounting, count
	return e
}

func waitWritten(t *testing.T, r *Recorder, n int64) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if r.Stats().Written >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d of %d rows written", r.Stats().Written, n)
}

func rowBySession(t *testing.T, r *Recorder, session string) *Event {
	t.Helper()
	page, err := r.DB().Requests(Filter{Session: session}, 0, 10)
	if err != nil || len(page.Requests) != 1 {
		t.Fatalf("rows for %s = %v, err %v", session, page, err)
	}
	return page.Requests[0]
}

// A big count must not hold up the writer: other rows keep being saved while it runs, and the
// counted row is updated when it finishes.
func TestBypassCountDoesNotBlockTheWriter(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	release := make(chan struct{})
	r.Record(bypassEvent(1000, AccountingMissing, func() (int, int) { <-release; return 500, 400 }))
	waitWritten(t, r, 1)
	r.Record(mkEvent(1001, "s-other", "m", 100, 90))
	waitWritten(t, r, 2) // written while the count above is still blocked

	if got := rowBySession(t, r, "s-bypass"); got.TokensBefore != 0 {
		t.Fatalf("the row has %d tokens_before before its count finished, want 0", got.TokensBefore)
	}
	close(release)
	var got *Event
	for i := 0; i < 400; i++ {
		if got = rowBySession(t, r, "s-bypass"); got.TokensBefore > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got.TokensBefore != 500 || got.TokensAfter != 500 || got.AttemptedTokens != 400 || got.FrozenTokens != 100 {
		t.Errorf("before/after/attempted/frozen = %d/%d/%d/%d, want 500/500/400/100",
			got.TokensBefore, got.TokensAfter, got.AttemptedTokens, got.FrozenTokens)
	}
}

// The row was priced while its count was unknown, which labels it "missing"; once counted it is
// "partial", as it was when the count was taken inline. A complete row stays complete.
func TestBypassCountFlipsMissingToPartialOnly(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	count := func() (int, int) { return 300, 300 }
	missing := bypassEvent(2000, AccountingMissing, count)
	missing.SessionID = "s-missing"
	complete := bypassEvent(2001, AccountingComplete, count)
	complete.SessionID = "s-complete"
	r.Record(missing)
	r.Record(complete)
	waitWritten(t, r, 2)
	for _, c := range []struct{ session, want string }{{"s-missing", AccountingPartial}, {"s-complete", AccountingComplete}} {
		var got *Event
		for i := 0; i < 400; i++ {
			if got = rowBySession(t, r, c.session); got.TokensBefore > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if got.TokensBefore != 300 || got.TokenAccounting != c.want {
			t.Errorf("%s: tokens_before %d accounting %q, want 300 / %q", c.session, got.TokensBefore, got.TokenAccounting, c.want)
		}
	}
}

// A full queue leaves the row uncounted (tokens_before 0 reads as unknown) and never blocks.
func TestBypassCountQueueFullDropsTheCount(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer func() { close(release); r.Close() }()
	block := func() (int, int) { <-release; return 1, 1 }
	total := countQueue + 5
	for i := 0; i < total; i++ {
		e := bypassEvent(int64(3000+i), AccountingMissing, block)
		r.Record(e)
		waitWritten(t, r, int64(i+1))
	}
	// One job is held by the worker, countQueue sit in the queue, the rest are dropped.
	if got := r.countsDropped.Load(); got < 1 {
		t.Errorf("countsDropped = %d, want at least 1", got)
	}
	if got := r.Stats().CountsDropped; got != r.countsDropped.Load() {
		t.Errorf("Stats().CountsDropped = %d, want %d", got, r.countsDropped.Load())
	}
}

// The queue is bounded by the total body size it holds, not just by job count, because a queued
// job pins its parsed request. One job is always accepted, however large.
func TestBypassCountQueueIsBoundedByBytes(t *testing.T) {
	r, err := NewRecorder(Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer func() { close(release); r.Close() }()
	block := func() (int, int) { <-release; return 1, 1 }

	big := bypassEvent(4000, AccountingMissing, block)
	big.BypassBytes = 3 * countMaxBytes // larger than the whole bound: still accepted when nothing is queued
	r.Record(big)
	waitWritten(t, r, 1)
	if got := r.countsDropped.Load(); got != 0 {
		t.Fatalf("a lone oversized job was dropped (%d)", got)
	}
	small := bypassEvent(4001, AccountingMissing, block)
	small.BypassBytes = 1 << 20
	r.Record(small)
	waitWritten(t, r, 2)
	// The writer counts a row as written BEFORE it hands the row to enqueueCount, so Written
	// reaching 2 does not mean the drop has been counted yet. Wait for the drop itself.
	for i := 0; i < 400 && r.countsDropped.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.countsDropped.Load(); got != 1 {
		t.Errorf("countsDropped = %d, want 1: a job that would exceed the byte bound was queued", got)
	}
}
