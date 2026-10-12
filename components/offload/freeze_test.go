package offload

import (
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

func tool(s string) bschemas.ChatMessage {
	t := s
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		Content: &bschemas.ChatMessageContent{ContentStr: &t}}
}

// A Nop store (no FrozenLoser) must degrade to the legacy behavior rather than panic or
// mutate at depth on every message.
func TestRepairLostFreezeNoopStore(t *testing.T) {
	c := &components.Ctx{Session: "s", Store: store.Nop{}}
	if repairLostFreeze(c, "mask", "anything") {
		t.Fatal("a store that cannot report losses must not authorize depth mutation")
	}
}

// The replay counters have to move, or the fix is unverifiable in a benchmark run.
func TestFrozenCountersMove(t *testing.T) {
	h0, m0 := FrozenStats()
	st := store.NewMemory(store.Options{})
	c := &components.Ctx{Session: "sCount", Store: st}
	msg := tool("some tool output")
	var rep components.Report
	reapplyFrozen(c, &rep, "mask", &msg) // miss: nothing frozen yet
	freeze(c, "mask", "some tool output", "short")
	msg2 := tool("some tool output")
	reapplyFrozen(c, &rep, "mask", &msg2) // hit
	h1, m1 := FrozenStats()
	if h1 <= h0 || m1 <= m0 {
		t.Fatalf("hits/misses must both advance: %d->%d, %d->%d", h0, h1, m0, m1)
	}
}

// The result cache (cg:res:) is extract_llm's replay namespace. It IS pinned against
// eviction — losing it un-compacts an already-cached message like any other replay
// decision — but it deliberately gets NO depth repair, because re-deriving it means a
// sampled model call (see repairLostFreeze).
func TestResultCachePinnedAgainstEviction(t *testing.T) {
	st := store.NewMemory(store.Options{MaxEntries: 4})
	c := &components.Ctx{Session: "s", Store: st}

	putResult(c, "id1", "compacted", "one-line summary")
	// Ordinary rewind stashes churn through the cache; the replay decision must survive.
	for i := 0; i < 20; i++ {
		st.Put("rewindhash"+string(rune('a'+i)), []byte("big original payload"))
	}
	got, ok := getResult(c, "id1")
	if !ok {
		t.Fatal("a result-cache decision must be pinned against LRU eviction")
	}
	if got.Projected != "compacted" || got.Summary != "one-line summary" {
		t.Fatalf("projection and summary must survive together, got %+v", got)
	}
}

// The projection and its summary line must live and die as ONE key. As two independently
// TTL'd/pinned keys, losing only the summary made the replay HIT and silently emit
// different bytes (the "[summary] " segment vanishing) inside the cached prefix.
func TestResultAndSummaryShareOneKey(t *testing.T) {
	now := time.Unix(0, 0)
	st := store.NewMemory(store.Options{TTLSeconds: 10})
	st.SetClock(func() time.Time { return now })
	c := &components.Ctx{Session: "s", Store: st}
	putResult(c, "id1", "compacted", "summary")

	// Whatever the store drops, a replay either returns BOTH parts or misses entirely —
	// it can never return a projection with the summary silently missing.
	now = now.Add(11 * time.Second)
	if got, ok := getResult(c, "id1"); ok {
		t.Fatalf("expired decision must miss outright, got %+v", got)
	}
	// A half-written / unreadable payload is also treated as absent, never spliced.
	st.Put(resultKey("s", "id2"), []byte("{not json"))
	if got, ok := getResult(c, "id2"); ok {
		t.Fatalf("unreadable decision must miss, got %+v", got)
	}
}

// The counters must also move on extract_llm's replay path (getResult), not just
// reapplyFrozen — the shipped coding config does ALL of its replay through the result
// cache, so counting only reapplyFrozen would report zero freeze activity on exactly the
// traffic this fix targets.
func TestResultCacheFeedsCounters(t *testing.T) {
	h0, m0 := FrozenStats()
	c := &components.Ctx{Session: "sRC", Store: store.NewMemory(store.Options{})}
	if _, ok := getResult(c, "idz"); ok {
		t.Fatal("nothing cached yet")
	}
	putResult(c, "idz", "compacted", "")
	if _, ok := getResult(c, "idz"); !ok {
		t.Fatal("expected a replay hit")
	}
	h1, m1 := FrozenStats()
	if h1 <= h0 || m1 <= m0 {
		t.Fatalf("result-cache replay must feed hits/misses: %d->%d, %d->%d", h0, h1, m0, m1)
	}
}
