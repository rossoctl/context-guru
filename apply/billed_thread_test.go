package apply

import (
	"testing"

	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
)

// A session with a main thread plus a side request: the session figure follows whoever answered last,
// the per-thread figure follows the thread. The billed-growth horizon reads the per-thread one, so the
// main thread's run stays steady however many side requests come between its turns.
func TestBilledInputIsRecordedPerThread(t *testing.T) {
	st := store.NewMemory(store.Options{})
	const sess = "s1"
	var run []int
	for i, main := range []int64{100_000, 104_000, 108_000, 112_000} {
		RecordBilledInput(st, sess, main)
		RecordBilledInputThread(st, sess, "", main)
		// A side request (a title, a monitor) answers between the main thread's turns, much smaller.
		side := int64(3_000 + i)
		RecordBilledInput(st, sess, side)
		RecordBilledInputThread(st, sess, "side", side)
		run = append(run, prevBilledInputThread(st, sess, ""))
		if got := prevBilledInput(st, sess); got != int(side) {
			t.Fatalf("the session figure is the last answer, %d; got %d", side, got)
		}
		if got := prevBilledInputThread(st, sess, "side"); got != int(side) {
			t.Fatalf("side thread figure %d, want %d", got, side)
		}
	}
	want := []int{100_000, 104_000, 108_000, 112_000}
	for i := range want {
		if run[i] != want[i] {
			t.Fatalf("main thread figures %v, want %v: a side request moved them", run, want)
		}
	}
}

func TestBilledInputThreadUnknownIsZero(t *testing.T) {
	st := store.NewMemory(store.Options{})
	if got := prevBilledInputThread(st, "s", "t"); got != 0 {
		t.Fatalf("unknown thread = %d, want 0", got)
	}
	RecordBilledInputThread(st, "", "t", 5) // no session: no-op
	RecordBilledInputThread(st, "s", "t", 0)
	if got := prevBilledInputThread(st, "s", "t"); got != 0 {
		t.Fatalf("no-op writes stored %d", got)
	}
	if got := prevBilledInputThread(nil, "s", "t"); got != 0 {
		t.Fatalf("nil store = %d", got)
	}
}

// The thread's request ratio (#1533) is two counts per thread. A turn with none, and a value that does not read,
// both mean "unknown": the previous ratio stays and a reader keeps the table.
func TestRatioIsRecordedPerThreadAndUnknownIsZero(t *testing.T) {
	st := store.NewMemory(store.Options{})
	if tok, by := prevRatioThread(st, "s", "t"); tok != 0 || by != 0 {
		t.Fatalf("unknown thread = %d, %d, want 0, 0", tok, by)
	}
	RecordRatioThread(st, "s", "", 3000, 6000)
	RecordRatioThread(st, "s", "side", 100, 900)
	RecordRatioThread(st, "s", "", 0, 0)    // a turn with no ratio: the last one stays
	RecordRatioThread(st, "", "t", 5, 5)    // no session: no-op
	RecordRatioThread(st, "s", "t", -1, 10) // not a ratio
	if tok, by := prevRatioThread(st, "s", ""); tok != 3000 || by != 6000 {
		t.Fatalf("main = %d, %d, want 3000, 6000", tok, by)
	}
	if tok, by := prevRatioThread(st, "s", "side"); tok != 100 || by != 900 {
		t.Fatalf("side = %d, %d, want 100, 900: a side request moved the main figure", tok, by)
	}
	if tok, by := prevRatioThread(st, "s", "t"); tok != 0 || by != 0 {
		t.Fatalf("thread t = %d, %d, want 0, 0", tok, by)
	}
	st.Put(store.RatioThreadPrefix+thread.Key("s", "bad"), []byte("x:y"))
	if tok, by := prevRatioThread(st, "s", "bad"); tok != 0 || by != 0 {
		t.Fatalf("a bad value read %d, %d, want 0, 0", tok, by)
	}
}
