package apply_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// Each thread of a session has its own provider cache (#423, #426), so the record apply reads to
// find the cached part (MaxCachedIdx) and to decide ColdCache must be the thread's own, not the
// session's (#484). These tests interleave a main thread and a subagent under ONE session id.

const subThread = "agent:sub"

// threadBody is one thread's transcript under the shared session id (bobSession). head names the
// thread, so each thread has its own system prompt and first user message; tools is how many tool
// outputs it carries, each one distinct so dedup never matches across them. hourTier marks the
// thread's first user message with the 1-hour cache tier.
func threadBody(t *testing.T, head string, tools int, hourTier bool) []byte {
	t.Helper()
	msgs := []map[string]any{
		{"role": "system", "content": "You are " + head + "."},
		{"role": "user", "content": "the task for " + head},
	}
	if hourTier { // the thread asks for the provider's 1-hour cache tier
		msgs[1]["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "1h"}
	}
	for i := 0; i < tools; i++ {
		msgs = append(msgs, map[string]any{
			"role":         "tool",
			"tool_call_id": fmt.Sprintf("%s-%d", head, i),
			"content":      fmt.Sprintf("%s tool output #%d\n%s", head, i, strings.Repeat("payload line\n", 60)),
		})
	}
	b, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"metadata": map[string]any{"taskId": bobSession},
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// threadRun sends turns of several threads of one session through the real entry point, with one
// store, one tracker and one pipeline, and remembers what each thread last forwarded: that is what
// the provider holds in the thread's cache.
type threadRun struct {
	t    *testing.T
	o    apply.Opts
	st   store.Store
	p    *components.Pipeline
	cur  string
	sent map[string][]string
	// hourTier lists the threads whose requests ask for the 1-hour cache tier.
	hourTier map[string]bool
}

func newThreadRun(t *testing.T, threads bool) *threadRun {
	cfg := pipe(t, "pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 1, min_tokens: 50}\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &threadRun{t: t, st: store.NewMemory(store.Options{}), p: p, sent: map[string][]string{},
		o: apply.Opts{Provider: bschemas.Anthropic, CacheMode: "on", Tracker: modes.NewTracker(0)}}
	if threads {
		r.o.ThreadOf = func(string) thread.Result { return thread.Result{ID: r.cur, Source: thread.SourceHeader} }
	}
	return r
}

// turn sends one request of thread th (thread.Primary for the main thread) at time at.
func (r *threadRun) turn(th string, tools int, at time.Time) apply.Result {
	r.t.Helper()
	head := "main"
	if th != thread.Primary {
		head = "sub"
	}
	r.cur = th
	o := r.o
	o.Body, o.Now = threadBody(r.t, head, tools, r.hourTier[th]), at
	return apply.BodyOpts(context.Background(), r.p, r.st, o)
}

// unchangedCachedPart reports every message of thread th's previous forwarded request that this
// request forwards with different bytes. A different byte there breaks the thread's cache.
func (r *threadRun) unchangedCachedPart(name, th string, res apply.Result) {
	r.t.Helper()
	var now []string
	for _, m := range gjson.GetBytes(res.Body, "messages").Array() {
		now = append(now, m.Raw)
	}
	for i, prev := range r.sent[th] {
		if i >= len(now) || now[i] != prev {
			r.t.Errorf("%s: message %d of the thread's cached part changed (MaxCachedIdx=%d)\n was: %.90s\n now: %.90s",
				name, i, res.MaxCachedIdx, prev, now[min(i, len(now)-1)])
		}
	}
	r.sent[th] = now
}

// Effect 2 of #484. A short subagent request between two main-thread requests set the main
// thread's cached boundary from the SUBAGENT's message count, so mask was free to rewrite main
// messages the provider had cached. The other way round, a subagent request after a longer main
// request looked like a compaction, reset the boundary to -1, and mask rewrote the subagent's own
// cached part.
//
// Measured before the fix (session-keyed tracker), all 3 later turns broke their thread's cache:
// "main 2" got MaxCachedIdx=2 against a 4-message cached part and mask changed message 3; "sub 2"
// got -1 and mask changed message 2; "main 3" got 3 against 6 and mask changed message 5. 2 false
// compaction resets were counted. The frozen-decision replay did not hide it: each changed message
// had never been masked before, so there was nothing frozen to replay.
func TestThreadsKeepTheirOwnCachedPart(t *testing.T) {
	r := newThreadRun(t, true)
	base := time.Unix(1_700_000_000, 0)
	before := modes.CompactionResets()

	for _, s := range []struct {
		name  string
		th    string
		tools int
		want  int // MaxCachedIdx: the thread's own previous message count - 1
	}{
		{"main 1", thread.Primary, 2, -1},
		{"sub 1", subThread, 1, -1},
		{"main 2", thread.Primary, 4, 3},
		{"sub 2", subThread, 2, 2},
		{"main 3", thread.Primary, 5, 5},
	} {
		res := r.turn(s.th, s.tools, base.Add(time.Duration(len(r.sent))*10*time.Second))
		if res.MaxCachedIdx != s.want {
			t.Errorf("%s: MaxCachedIdx = %d, want %d (the thread's own previous turn)", s.name, res.MaxCachedIdx, s.want)
		}
		r.unchangedCachedPart(s.name, s.th, res)
	}
	if got := modes.CompactionResets() - before; got != 0 {
		t.Errorf("%d compaction resets counted; no thread shrank", got)
	}
}

// Effect 1 of #484. The main thread is idle for 10 minutes while a subagent of the same session
// sends a request every minute. The main thread's cache expired, so its next request is cold and
// mask may act at any depth: nothing is frozen.
func TestIdleMainThreadIsColdWhileASubagentRuns(t *testing.T) {
	//
	// The main thread is the LONGER one on purpose. A shorter main request after the subagent's
	// reads as a compaction and resets the boundary to -1, which also gives 0 frozen tokens and
	// would make this test pass for the wrong reason.
	r := newThreadRun(t, true)
	base := time.Unix(1_700_000_000, 0)
	r.turn(thread.Primary, 20, base)
	for m := 1; m <= 10; m++ {
		r.turn(subThread, m, base.Add(time.Duration(m)*time.Minute))
	}
	res := r.turn(thread.Primary, 21, base.Add(10*time.Minute+5*time.Second))
	if res.FrozenTokens != 0 {
		t.Fatalf("main thread idle 10m: frozen_tokens = %d (MaxCachedIdx=%d), want 0 on a cold request",
			res.FrozenTokens, res.MaxCachedIdx)
	}

	// The control: a main thread idle for 2 minutes is warm, whatever the subagent does.
	r = newThreadRun(t, true)
	r.turn(thread.Primary, 20, base)
	r.turn(subThread, 1, base.Add(time.Minute))
	if res := r.turn(thread.Primary, 21, base.Add(2*time.Minute)); res.FrozenTokens == 0 {
		t.Fatal("fixture broken: a main thread idle for 2 minutes read cold")
	}
}

// A single-thread session behaves exactly as it did before threads: a ThreadOf that always answers
// the primary thread gives the same boundary, frozen tokens and bytes as no ThreadOf at all.
func TestSingleThreadSessionIsUnchanged(t *testing.T) {
	with, without := newThreadRun(t, true), newThreadRun(t, false)
	base := time.Unix(1_700_000_000, 0)
	for i, tools := range []int{2, 3, 5, 2, 4} { // 5 -> 2 is a compaction
		at := base.Add(time.Duration(i) * 7 * time.Minute) // past the 5m TTL: every turn after the first is cold
		a, b := with.turn(thread.Primary, tools, at), without.turn(thread.Primary, tools, at)
		if a.MaxCachedIdx != b.MaxCachedIdx || a.FrozenTokens != b.FrozenTokens || string(a.Body) != string(b.Body) {
			t.Fatalf("turn %d: with ThreadOf (idx %d, frozen %d) differs from without (idx %d, frozen %d)",
				i, a.MaxCachedIdx, a.FrozenTokens, b.MaxCachedIdx, b.FrozenTokens)
		}
	}
}

// The Responses route reads the same record, so it must key it the same way.
func TestResponsesThreadsKeepTheirOwnCachedPart(t *testing.T) {
	st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
	p := components.NewPipeline(nil, nil)
	base := time.Unix(1_700_000_000, 0)
	before := modes.CompactionResets()
	for i, s := range []struct {
		th    string
		items int
		want  int
	}{
		{thread.Primary, 6, -1},
		{subThread, 2, -1},
		{thread.Primary, 8, 5},
		{subThread, 3, 1},
	} {
		head := "main"
		if s.th != thread.Primary {
			head = "sub"
		}
		var items []string
		for j := 0; j < s.items; j++ {
			items = append(items, fmt.Sprintf(`{"role":"user","content":"%s item %d"}`, head, j))
		}
		th := s.th
		res := apply.BodyOpts(context.Background(), p, st, apply.Opts{
			Provider: bschemas.OpenAI, API: "responses", CacheMode: "on", Session: "same-session",
			Body:    []byte(`{"model":"gpt-5.6","input":[` + strings.Join(items, ",") + `]}`),
			Tracker: tr, Now: base.Add(time.Duration(i) * 10 * time.Second),
			ThreadOf: func(string) thread.Result { return thread.Result{ID: th, Source: thread.SourceHeader} },
		})
		if res.MaxCachedIdx != s.want {
			t.Errorf("turn %d (%q): MaxCachedIdx = %d, want %d", i, s.th, res.MaxCachedIdx, s.want)
		}
	}
	if got := modes.CompactionResets() - before; got != 0 {
		t.Errorf("%d compaction resets counted; no thread shrank", got)
	}
}

// The TTL record is the thread's own. The main thread asks for the 1-hour tier and the subagent
// does not, so a subagent idle for 10 minutes is cold, whatever the main thread asked for. Keyed
// by session, the main thread's 1h record made the subagent read warm: frozen_tokens > 0.
func TestThreadTTLRecordIsPerThread(t *testing.T) {
	r := newThreadRun(t, true)
	r.hourTier = map[string]bool{thread.Primary: true}
	base := time.Unix(1_700_000_000, 0)
	r.turn(subThread, 20, base)
	for m := 1; m <= 10; m++ {
		r.turn(thread.Primary, m, base.Add(time.Duration(m)*time.Minute))
	}
	if res := r.turn(subThread, 21, base.Add(10*time.Minute+5*time.Second)); res.FrozenTokens != 0 {
		t.Fatalf("subagent idle 10m on the 5m tier: frozen_tokens = %d, want 0 (another thread's 1h tier made it warm)",
			res.FrozenTokens)
	}

	// Monotone toward warm within one thread: the main thread asked for 1h once, so 10 minutes
	// later it is still warm even when this request carries no ttl.
	r = newThreadRun(t, true)
	r.hourTier = map[string]bool{thread.Primary: true}
	r.turn(thread.Primary, 20, base)
	r.hourTier = nil
	if res := r.turn(thread.Primary, 21, base.Add(10*time.Minute)); res.FrozenTokens == 0 {
		t.Fatal("a thread that asked for the 1h tier read cold after 10 minutes")
	}
}
