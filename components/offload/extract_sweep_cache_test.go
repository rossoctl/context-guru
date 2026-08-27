package offload

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// THE SIBLING BATCHES OF ONE SWEEP SHARE A PREFIX, and whether they can actually read it decides how
// the calls are ordered.
//
// The defect these tests were written against: every batch launched concurrently under one semaphore
// with no cache coordination, so each sent the shared contract as fresh input and none could read
// another's write because all were in flight at once. cheapmodel.claimCacheWrite deliberately
// suppresses the breakpoint on concurrent siblings — a cache entry only ever written is worse than no
// breakpoint — so the concurrent case cannot accidentally work.
//
// The fix is one writer then readers, and it is CONDITIONAL on the write being earnable: serializing
// costs a whole gateway queue round (~2-4 s p50, tail 12-16 s), and on a model whose minimum
// cacheable prefix the adjudication prefix cannot clear, that round buys nothing at all.

// orderModel records, at the moment the FIRST call returns, how many calls have started. That is the
// signal that distinguishes the two orderings deterministically: with the first batch serialized,
// nothing else has been launched yet, so the count is 1. Without it every batch's goroutine is
// launched immediately and will have entered well inside the delay below.
type orderModel struct {
	delay                time.Duration
	calls                int64
	mu                   sync.Mutex
	entered              int
	startedWhenFirstDone int
	firstDone            bool
	maxSystemSeen        int
}

var outputRe = regexp.MustCompile(`=== OUTPUT (\d+) `)

func (m *orderModel) Complete(_ context.Context, prompt string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	m.mu.Lock()
	m.entered++
	m.mu.Unlock()
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	if !m.firstDone {
		m.firstDone, m.startedWhenFirstDone = true, m.entered
	}
	m.mu.Unlock()
	labels := outputRe.FindAllStringSubmatch(prompt, -1)
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, `{"i":`+l[1]+`,"needed_by":"none","quote":"","verdict":"keep"}`)
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

// CompleteBlocks makes this a SystemBlocksModel, so the split is exercised through the same capability
// the real client offers. The system half is joined into the prompt for label extraction only — what
// matters here is the ordering, and the split's own placement is asserted in internal/extract.
func (m *orderModel) CompleteBlocks(ctx context.Context, system []string, user string) (string, error) {
	m.mu.Lock()
	if len(system) > m.maxSystemSeen {
		m.maxSystemSeen = len(system)
	}
	m.mu.Unlock()
	return m.Complete(ctx, strings.Join(system, "\n")+"\n"+user)
}

func (m *orderModel) startedAtFirstReturn() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startedWhenFirstDone
}

func (m *orderModel) systemBlocks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxSystemSeen
}

// bigGoalReq is manyCandidates plus a long opening instruction, so the conversation context the sweep
// carries is large enough to lift the shared prefix over a sonnet-class minimum cacheable size. The
// candidates themselves are unchanged — they are the user half and never cacheable.
func bigGoalReq(n int) *bschemas.BifrostChatRequest {
	req := manyCandidates(n)
	req.Input[0] = userMsg("Work through this backlog carefully. " +
		strings.Repeat("Each item needs its own verification pass before you move on. ", 40))
	return req
}

// A model whose floor the prefix CAN clear gets the earn-then-read ordering: the first batch runs
// alone so its prefix takes the cache write, and only then do the rest run concurrently to read it.
func TestSweepSerializesTheFirstBatchWhenThePrefixIsCacheable(t *testing.T) {
	model := &orderModel{delay: 40 * time.Millisecond}
	// sonnet-class, whose minimum cacheable prefix is 1024 provider tokens against haiku's 4096.
	e := newSweep(t, model, "model:\n  model: claude-sonnet-5\n")
	rep := &components.Report{}
	if _, err := e.Offload(bigGoalReq(36), rep,
		sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION 1: there really were sibling batches. With one batch there is nothing to order
	// and nothing to share, and every assertion below would be vacuous.
	if got := atomic.LoadInt64(&model.calls); got < 2 {
		t.Fatalf("only %d batch call(s), so there were no siblings to share a prefix (gates: %v)",
			got, rep.Gates)
	}
	// PRECONDITION 2: the prefix was judged cacheable. If it was not, the component is CORRECT to
	// stay concurrent and this test is asserting the wrong branch.
	if rep.Gates["sweep_prefix_uncacheable"] != 0 {
		t.Fatalf("the prefix was judged uncacheable on sonnet-class, so the ordering under test "+
			"was deliberately skipped (gates: %v)", rep.Gates)
	}
	// PRECONDITION 3: the split was actually used, so there IS a prefix to write.
	if model.systemBlocks() < 2 {
		t.Fatalf("only %d system block(s): the goal did not join the cacheable prefix, so siblings "+
			"have nothing to read", model.systemBlocks())
	}
	if got := model.startedAtFirstReturn(); got != 1 {
		t.Fatalf("%d calls had started when the first returned: the first batch did not run alone, "+
			"so claimCacheWrite suppressed its breakpoint and no sibling can read a write", got)
	}
}

// AND THE OTHER BRANCH, which is the shipped one. On haiku-class the adjudication prefix cannot clear
// the 4096-provider-token minimum — measured at ~537 o200k against a 3413 floor — so a breakpoint
// would be silently ignored and serializing the first batch would buy a queue round for nothing. The
// component stays fully concurrent and SAYS SO, because an operator reading a duplicated-input bill
// needs to know which of the two causes it is.
func TestSweepStaysConcurrentAndCountsItWhenThePrefixCannotBeCached(t *testing.T) {
	model := &orderModel{delay: 40 * time.Millisecond}
	e := newSweep(t, model, "model:\n  model: claude-haiku-4-5\n")
	rep := &components.Report{}
	if _, err := e.Offload(bigGoalReq(36), rep,
		sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&model.calls); got < 2 {
		t.Fatalf("only %d batch call(s), so there were no siblings (gates: %v)", got, rep.Gates)
	}
	if rep.Gates["sweep_prefix_uncacheable"] != 1 {
		t.Fatalf("haiku-class cannot cache this prefix and the component did not record it "+
			"(gates: %v)", rep.Gates)
	}
	if got := model.startedAtFirstReturn(); got < 2 {
		t.Errorf("the first batch was serialized on a model that cannot cache the prefix: the "+
			"queue round bought nothing (started at first return = %d)", got)
	}
}

// A SINGLE BATCH MUST NOT PAY FOR A CACHE WRITE. There is no sibling to read the entry and a write
// costs 1.25x fresh, so marking the prefix would be a 25% loss. Observable through the split: with one
// batch the goal stays in the user half, so there is only the contract block.
func TestSweepDoesNotCacheTheGoalForASingleBatch(t *testing.T) {
	model := &orderModel{}
	e := newSweep(t, model, "model:\n  model: claude-sonnet-5\n")
	rep := &components.Report{}
	if _, err := e.Offload(bigGoalReq(3), rep,
		sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: exactly one batch, which is what makes a cache write worthless here.
	if got := atomic.LoadInt64(&model.calls); got != 1 {
		t.Fatalf("expected one batch call, got %d (gates: %v)", got, rep.Gates)
	}
	if got := model.systemBlocks(); got != 1 {
		t.Fatalf("a single-batch sweep sent %d system blocks; the goal must stay in the user half "+
			"so no 1.25x write is paid for an entry nothing reads", got)
	}
	if rep.Gates["sweep_prefix_uncacheable"] != 0 {
		t.Errorf("a single-batch sweep reported a prefix-sharing failure it never attempted (gates: %v)",
			rep.Gates)
	}
	if rep.Gates["sweep_prefix_cache_read_ZERO"] != 0 {
		t.Errorf("a single-batch sweep counted a sibling read (gates: %v)", rep.Gates)
	}
}

// THE JUSTIFICATION FOR THE ORDERING, COUNTED. A sibling that reads nothing from cache paid fresh for
// the prefix anyway and the serialized round bought a queue wait for nothing — indistinguishable from
// a working one except on the bill, which is why it needs a counter. The stub reports no cache usage,
// so every sibling here is a zero-read one.
func TestSweepCountsASiblingThatReadNothingFromCache(t *testing.T) {
	model := &orderModel{}
	e := newSweep(t, model, "model:\n  model: claude-sonnet-5\n")
	rep := &components.Report{}
	if _, err := e.Offload(bigGoalReq(36), rep,
		sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	calls := atomic.LoadInt64(&model.calls)
	if calls < 2 {
		t.Fatalf("only %d batch call(s), so there were no siblings to check (gates: %v)", calls, rep.Gates)
	}
	// Every batch after the first is a sibling, and none of them read anything here.
	if got, want := rep.Gates["sweep_prefix_cache_read_ZERO"], int(calls)-1; got != want {
		t.Fatalf("sweep_prefix_cache_read_ZERO = %d, want %d: a sibling paying fresh for the shared "+
			"prefix would be invisible (gates: %v)", got, want, rep.Gates)
	}
}
