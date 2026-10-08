package offload

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
)

// fakePrefixAsker is the components.PrefixAsker cache_aware_summarizer calls through when no
// MessagesModel is available — the Anthropic incoming-model path (#275: internal/cheapmodel.
// Anthropic implements only Complete). Records what it was asked so a test can assert on the
// REQUEST, the same discipline capturingModel uses for the MessagesModel path.
type fakePrefixAsker struct {
	calls           int64
	gotSession, got string
	out             string
	usage           components.PrefixUsage
	err             error
}

func (f *fakePrefixAsker) Ask(_ context.Context, session, ask string) (string, components.PrefixUsage, error) {
	atomic.AddInt64(&f.calls, 1)
	f.gotSession, f.got = session, ask
	if f.err != nil {
		return "", components.PrefixUsage{}, f.err
	}
	return f.out, f.usage, nil
}

// caCtxNoModel is caCtx plus a PrefixAsker and NO model at all — model.source: incoming on a
// client with no MessagesModel is the shape this test fixture stands in for.
func caCtxNoModel(session string, asker components.PrefixAsker) *components.Ctx {
	c := caCtx(session)
	c.PrefixAsk = asker
	return c
}

var errPrefixAskUpstream = errors.New("prefix ask upstream refused")

// ⭐ THE WHOLE POINT OF #275's SECOND HALF: a session with no MessagesModel at all must still
// compact, through PrefixAsk, rather than declining every turn of what is this deployment's only
// real traffic shape (Anthropic, model.source: incoming).
func TestCacheAwareUsesPrefixAskWhenNoMessagesModelIsAvailable(t *testing.T) {
	before := CacheAwareSummarizerPrefixAskUsed()
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	// s.modelClient is nil and the Ctx below carries no components.Model either — model
	// resolution finds nothing, which is exactly the state a MessagesModel-less client leaves it
	// in. Only PrefixAsk is reachable.
	asker := &fakePrefixAsker{out: "<summary>explored the handler via prefix ask.</summary>"}
	ctx := caCtxNoModel("ca-prefixask", asker)

	in := caFixture()
	t1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep components.Report
	if _, err := s.Offload(t1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the commissioned summary never landed")
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Fatalf("PrefixAsker.Ask called %d times, want exactly 1", asker.calls)
	}
	if asker.gotSession != ctx.Session {
		t.Errorf("asked with session %q, want %q", asker.gotSession, ctx.Session)
	}
	if !strings.Contains(asker.got, "OPERATOR INSTRUCTION") {
		t.Errorf("the ask text is not the user-variant instruction: %.80q", asker.got)
	}
	if CacheAwareSummarizerPrefixAskUsed() != before+1 {
		t.Error("the PrefixAsk path was not counted")
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Fatal("no checkpoint was committed via the PrefixAsk path")
	}

	// Turn 2 must splice the checkpoint, exactly as the MessagesModel path does.
	t2 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	rep = components.Report{}
	if _, err := s.Offload(t2, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if len(t2.Input) == len(in) {
		t.Error("turn 2 did not splice the PrefixAsk-produced checkpoint")
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Errorf("asker called %d times by turn 2, want 1 — turn 2 must REUSE the checkpoint", asker.calls)
	}
}

// A session's first turn has nothing stashed yet (components.ErrNoPrefix) — the ordinary,
// once-per-session case, which must be counted apart from a real failure and must fail open
// (no checkpoint, transcript forwarded untouched, next turn gets another chance).
func TestCacheAwarePrefixAskNoPrefixIsNotAFailure(t *testing.T) {
	beforeNoPrefix := CacheAwareSummarizerNoPrefix()
	beforeErrors := CacheAwareSummarizerErrors()
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &fakePrefixAsker{err: components.ErrNoPrefix}
	ctx := caCtxNoModel("ca-prefixask-noprefix", asker)

	in := caFixture()
	t1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), in...)}
	var rep components.Report
	if _, err := s.Offload(t1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the detached call never resolved")
	}
	if CacheAwareSummarizerNoPrefix() != beforeNoPrefix+1 {
		t.Error("ErrNoPrefix was not counted as the no-prefix case")
	}
	if CacheAwareSummarizerErrors() != beforeErrors {
		t.Error("ErrNoPrefix was counted as a real failure — the two mean opposite things to an operator")
	}
	if _, ok := loadCheckpoint(ctx); ok {
		t.Error("a checkpoint was committed despite no prefix to read")
	}
	if len(t1.Input) != len(in) {
		t.Error("the transcript was modified on a turn with nothing to read yet")
	}
}

// A genuine ask failure (not ErrNoPrefix) must fail open exactly like a MessagesModel error does,
// and must be counted as a real error, distinct from the no-prefix case.
func TestCacheAwarePrefixAskFailureFailsOpenAndCounts(t *testing.T) {
	beforeErrors := CacheAwareSummarizerErrors()
	s := newCacheAware(t, caBaseCfg+"instruction_role: auto\n")
	asker := &fakePrefixAsker{err: errPrefixAskUpstream}
	ctx := caCtxNoModel("ca-prefixask-fail", asker)

	t1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caFixture()...)}
	var rep components.Report
	if _, err := s.Offload(t1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if !WaitForSummaryForTest(ctx.Session, 5*time.Second) {
		t.Fatal("the detached call never resolved")
	}
	if CacheAwareSummarizerErrors() != beforeErrors+1 {
		t.Error("a genuine prefix-ask failure was not counted")
	}
	if _, ok := loadCheckpoint(ctx); ok {
		t.Error("a checkpoint was committed despite the ask failing")
	}
}

// A pinned `instruction_role: system` cannot be honoured through PrefixAsk at all (it always
// sends a trailing USER message) — this must DECLINE loudly rather than silently send the
// user-variant text under a config that asked for the system channel.
func TestCacheAwarePinnedSystemRoleDeclinesOnThePrefixAskPath(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: system\nmodel_id: some-verified-model\n")
	asker := &fakePrefixAsker{out: "<summary>should never be reached</summary>"}
	ctx := caCtxNoModel("ca-prefixask-system", asker)

	t1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caFixture()...)}
	var rep components.Report
	if _, err := s.Offload(t1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["system_role_unsupported_via_prefix_ask"] == 0 {
		t.Fatalf("did not decline a pinned system role on the PrefixAsk path (gates: %v)", rep.Gates)
	}
	if atomic.LoadInt64(&asker.calls) != 0 {
		t.Errorf("the asker was called %d times despite the role being unsendable", asker.calls)
	}
}

// The keep-alive substitution must work identically off a PrefixAsk-built candidate: this is
// what makes cache_aware_summarizer able to feed the keep-alive on Anthropic incoming-model
// traffic at all, since that is also the traffic with no MessagesModel.
func TestCacheAwareKeepAliveCandidateWorksViaPrefixAsk(t *testing.T) {
	s := newCacheAware(t, "keep_last_turns: 2\nmin_tokens: 10\nresummarize_tokens: 6000\n"+
		"instruction_role: auto\n"+
		"trigger:\n  min_messages: 4\n  min_request_tokens: 10\n  min_request_frac: 0\n  cache_state: pre_expiry\n")
	asker := &fakePrefixAsker{out: "<summary>ok via prefix ask</summary>"}
	ctx := caCtxNoModel("ca-prefixask-ka", asker)
	// Warm, not pre-expiry: cache_state: pre_expiry must decline to fire THIS turn so the
	// registration (not the immediate fire) is what this test is about.
	const ttl = 5 * 60 * 1000
	ctx.CacheTTLMs, ctx.IdleMs = ttl, 30*1000

	t1 := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage(nil), caFixture()...)}
	var rep components.Report
	if _, err := s.Offload(t1, &rep, ctx); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if rep.Gates["cache_state_declined_warm"] == 0 {
		t.Fatalf("precondition: this turn must be declined by cache_state (gates: %v)", rep.Gates)
	}
	dispatch, _, _, ok := KeepAliveSubstitute(ctx.Session)
	if !ok {
		t.Fatal("no keep-alive candidate was registered for a turn with real PrefixAsk commission material")
	}
	res := dispatch(5 * time.Second)
	if !res.Committed {
		t.Fatal("the PrefixAsk-backed substitute did not commit a summary")
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Errorf("asker called %d times, want 1", asker.calls)
	}
	if _, ok := loadCheckpoint(ctx); !ok {
		t.Error("the substitute dispatch committed nothing to the checkpoint")
	}
}
