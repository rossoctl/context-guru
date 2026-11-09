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
	var last apply.Result
	var at time.Time
	for i, tools := range []int{2, 3, 5, 2, 4} { // 5 -> 2 is a compaction
		at = base.Add(time.Duration(i) * 7 * time.Minute) // past the 5m TTL: every turn after the first is cold
		a, b := with.turn(thread.Primary, tools, at), without.turn(thread.Primary, tools, at)
		if a.MaxCachedIdx != b.MaxCachedIdx || a.FrozenTokens != b.FrozenTokens || string(a.Body) != string(b.Body) {
			t.Fatalf("turn %d: with ThreadOf (idx %d, frozen %d) differs from without (idx %d, frozen %d)",
				i, a.MaxCachedIdx, a.FrozenTokens, b.MaxCachedIdx, b.FrozenTokens)
		}
		last = a
	}
	// Equal is not enough: both runs would also agree if the primary thread stopped keying by the
	// bare session id. The record must be under the session id itself, as before threads existed.
	for name, r := range map[string]*threadRun{"with ThreadOf": with, "without ThreadOf": without} {
		assertBareSessionRecord(t, name, r.o.Tracker, last, at)
	}
}

// assertBareSessionRecord checks that the tracker holds the last request's record under the bare
// session id: its message count and its time.
func assertBareSessionRecord(t *testing.T, name string, tr *modes.Tracker, last apply.Result, at time.Time) {
	t.Helper()
	n, prevAt := tr.TurnAt(last.Session, last.Messages, at.Add(time.Second).UnixMilli(), 0)
	if n != last.Messages || prevAt != at.UnixMilli() {
		t.Errorf("%s: record under the bare session id %q is (%d, %d), want (%d, %d)",
			name, last.Session, n, prevAt, last.Messages, at.UnixMilli())
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

// sharedPrefixBody is the main thread's transcript with `tools` tool outputs, plus `extra` user
// messages after it. declare adds a top-level `tools` array, which is how thread.Resolve tells the
// main agent from a tool-less side request.
func sharedPrefixBody(t *testing.T, tools int, extra []string, declare bool) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(threadBody(t, "main", tools, false), &doc); err != nil {
		t.Fatal(err)
	}
	msgs := doc["messages"].([]any)
	for _, e := range extra {
		msgs = append(msgs, map[string]any{"role": "user", "content": e})
	}
	doc["messages"] = msgs
	if declare {
		doc["tools"] = []any{map[string]any{"name": "Bash", "description": "run a command",
			"input_schema": map[string]any{"type": "object"}}}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A NEW thread whose first request extends the main thread's cached prefix must not start at
// MaxCachedIdx -1 (#487 review, finding 1). Claude Code's side requests (title, quota check,
// end-of-turn note) are tool-less and get their own p:<hash> thread; a fork names itself with an
// agent header. Both send the main thread's whole transcript, which the provider already holds.
// The thread ids here come from the real thread.New().Resolve.
//
// Without TurnFrom's parent read, both got MaxCachedIdx -1 and mask changed message 5 of the
// main thread's cached part.
func TestNewThreadOnASharedPrefixKeepsTheCachedPart(t *testing.T) {
	for _, tc := range []struct {
		name, agentID string
		declare       bool
		noTracker     bool // the store-backed path library callers use
	}{
		{"side request", "", false, false},
		{"fork", "fork1", true, false},
		{"side request without a tracker", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pipe(t, "pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 1, min_tokens: 50}\n")
			p, err := cfg.Build(nil)
			if err != nil {
				t.Fatal(err)
			}
			st, threads := store.NewMemory(store.Options{}), thread.New()
			var agent string
			var body []byte
			o := apply.Opts{Provider: bschemas.Anthropic, CacheMode: "on", Tracker: modes.NewTracker(0),
				ThreadOf: func(s string) thread.Result { return threads.Resolve(s, agent, body, false) }}
			if tc.noTracker {
				o.Tracker = nil
			}
			base := time.Unix(1_700_000_000, 0)
			send := func(b []byte, id string, at time.Time) apply.Result {
				body, agent = b, id
				o.Body, o.Now = b, at
				return apply.BodyOpts(context.Background(), p, st, o)
			}
			send(sharedPrefixBody(t, 4, nil, true), "", base)
			mainRes := send(sharedPrefixBody(t, 6, nil, true), "", base.Add(10*time.Second))
			if mainRes.Thread.ID != thread.Primary || mainRes.MaxCachedIdx != 5 {
				t.Fatalf("fixture: main turn 2 thread %q idx %d, want primary and 5", mainRes.Thread.ID, mainRes.MaxCachedIdx)
			}
			var cached []string
			for _, m := range gjson.GetBytes(mainRes.Body, "messages").Array() {
				cached = append(cached, m.Raw)
			}

			side := send(sharedPrefixBody(t, 6, []string{"one more question"}, tc.declare), tc.agentID, base.Add(20*time.Second))
			if _, ok := side.Thread.InheritsFrom(); !ok || side.Thread.ID == thread.Primary {
				t.Fatalf("fixture: the new thread %+v must be its own thread and inherit from the main one", side.Thread)
			}
			if side.MaxCachedIdx != len(cached)-1 {
				t.Errorf("new thread %q: MaxCachedIdx = %d, want %d (the main thread's cached part)",
					side.Thread.ID, side.MaxCachedIdx, len(cached)-1)
			}
			if side.FrozenTokens == 0 {
				t.Error("new thread of a warm main thread read cold or unprotected: frozen_tokens = 0")
			}
			sent := gjson.GetBytes(side.Body, "messages").Array()
			for i, was := range cached {
				if i >= len(sent) || sent[i].Raw != was {
					t.Errorf("new thread changed message %d of the main thread's cached part", i)
				}
			}

			// The parent's record is read, never written: the main thread's next request is still
			// measured against its OWN last request (8 messages), not the new thread's 9.
			if next := send(sharedPrefixBody(t, 7, nil, true), "", base.Add(30*time.Second)); next.MaxCachedIdx != 7 {
				t.Errorf("main turn 3: MaxCachedIdx = %d, want 7 (the new thread wrote the main thread's record)", next.MaxCachedIdx)
			}
		})
	}
}

// The Responses route keys by thread.Key too, so a single-thread session there is also unchanged:
// a ThreadOf that always answers the primary thread equals no ThreadOf at all.
func TestResponsesSingleThreadSessionIsUnchanged(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * 7 * time.Minute) }
	run := func(withThreadOf bool) ([]apply.Result, *modes.Tracker) {
		st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
		cfg := pipe(t, "pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 1, min_tokens: 50}\n")
		p, err := cfg.Build(nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []apply.Result
		for i, n := range []int{3, 5, 8, 2, 4} { // 8 -> 2 is a compaction
			var items []string
			for j := 0; j < n; j++ {
				items = append(items, fmt.Sprintf(`{"type":"function_call_output","call_id":"c%d","output":"output %d\n%s"}`,
					j, j, strings.Repeat(`payload line\n`, 60)))
			}
			o := apply.Opts{Provider: bschemas.OpenAI, API: "responses", CacheMode: "on", Session: "same-session",
				Body:    []byte(`{"model":"gpt-5.6","input":[{"role":"user","content":"go"},` + strings.Join(items, ",") + `]}`),
				Tracker: tr, Now: at(i)}
			if withThreadOf {
				o.ThreadOf = func(string) thread.Result { return thread.Result{ID: thread.Primary, Source: thread.SourceSession} }
			}
			out = append(out, apply.BodyOpts(context.Background(), p, st, o))
		}
		return out, tr
	}
	with, withTr := run(true)
	without, withoutTr := run(false)
	for i := range with {
		a, b := with[i], without[i]
		if a.MaxCachedIdx != b.MaxCachedIdx || a.FrozenTokens != b.FrozenTokens || string(a.Body) != string(b.Body) {
			t.Fatalf("turn %d: with ThreadOf (idx %d, frozen %d) differs from without (idx %d, frozen %d)",
				i, a.MaxCachedIdx, a.FrozenTokens, b.MaxCachedIdx, b.FrozenTokens)
		}
	}
	if !with[2].Changed {
		t.Fatal("fixture: mask acted on nothing, so the comparison proves little")
	}
	last := len(with) - 1
	assertBareSessionRecord(t, "with ThreadOf", withTr, with[last], at(last))
	assertBareSessionRecord(t, "without ThreadOf", withoutTr, without[last], at(last))
}
