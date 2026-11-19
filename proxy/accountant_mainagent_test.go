package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/store"
)

// #1243. A main-thread request with a 1-hour breakpoint on the HISTORY (a message, not the head) is
// priced: the premium of the 1h write on the history, and the rescue of a read after the 5-minute
// cache would have expired. The request goes through the real apply relabel, the real envelope
// reporter and the real accountant library.

// mainAgentTurn is a Claude Code shaped body: a system block and a history of `turns` user messages,
// the last one carrying the breakpoint. Padded past the 200,000-byte gate.
func mainAgentTurn(turns int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"aws/claude-opus-5","system":[{"type":"text","text":"head","cache_control":{"type":"ephemeral"}}],"messages":[`)
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteString(`,`)
		}
		mark := ``
		if i == turns-1 {
			mark = `,"cache_control":{"type":"ephemeral"}`
		}
		b.WriteString(`{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("history ", 14000) + `"` + mark + `}]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// relabelled runs the body through apply as the main thread and returns the forwarded body and every
// operation the proxy would report: apply's own (messages) and the envelope diff (tools, system).
func relabelled(t *testing.T, client []byte, threadID string) ([]byte, []accountant.Operation) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("pipeline: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: client, MainAgentTTL1h: true, HeadTTLMinBytes: 200000,
		IsSubagent: keepalive.IsSubagentThread,
		ThreadOf:   func(string) thread.Result { return thread.Result{ID: threadID, Source: thread.SourceHeader} },
	})
	fwd := res.Body
	if fwd == nil {
		fwd = client
	}
	return fwd, append(envelopeOps(client, fwd), res.Trace.Ops...)
}

func TestMainAgentHistoryBreakpointIsPricedPremiumThenRescue(t *testing.T) {
	st := newSavingsTracker()
	t0 := time.Unix(1_700_000_000, 0)
	run := func(id string, at time.Time, client []byte, u Usage) accountant.Result {
		fwd, ops := relabelled(t, client, thread.Primary)
		if !apply.AllBreakpointsAskExtendedTTL(fwd) {
			t.Fatalf("%s: not every breakpoint asks 1h", id)
		}
		req := st.begin(accountant.RequestMeta{RequestID: id, Session: "s", Model: "m", At: at}, client)
		st.end(req, accountant.Thread{ID: "main"}, fwd, ops, accountant.EntryFacts{TTL: time.Hour, Lifetime: accountant.LifetimeExact})
		res := st.compute(req, u)
		if len(res.Checks) != 0 {
			t.Fatalf("%s: checks failed (an unexplained byte): %+v", id, res.Checks)
		}
		return res
	}

	// Request 1 writes the whole history at 1h. The client body would have written it at 5m, so the
	// premium is the 5m -> 1h move of those tokens.
	r1 := run("r1", t0, mainAgentTurn(2), Usage{CacheWrite: 20000, CacheWrite1h: 20000})
	var premium *accountant.CacheEffect
	for i, e := range r1.CacheEffects {
		if e.Kind == accountant.EffectTTLPremium {
			premium = &r1.CacheEffects[i]
		}
	}
	if premium == nil {
		t.Fatalf("no ttlPremium for a 1h write on the history: %+v", r1.CacheEffects)
	}
	if premium.Saved.CacheWrite5m <= 0 || premium.Caused.CacheWrite1h != premium.Saved.CacheWrite5m {
		t.Fatalf("premium moves %d tokens 5m -> %d at 1h; want the same positive count", premium.Saved.CacheWrite5m, premium.Caused.CacheWrite1h)
	}
	// Priced: Opus-like rates, input 1.00 per unit, 5m write 1.25, 1h write 2.00. Moving N tokens costs 0.75N.
	cost := 2.00*float64(premium.Caused.CacheWrite1h) - 1.25*float64(premium.Saved.CacheWrite5m)
	if want := 0.75 * float64(premium.Saved.CacheWrite5m); cost != want || cost <= 0 {
		t.Errorf("premium cost %.2f, want %.2f", cost, want)
	}

	// Request 2, 10 minutes later: it reads the history. With 5m entries the cache would be gone.
	r2 := run("r2", t0.Add(10*time.Minute), mainAgentTurn(3), Usage{CacheRead: 19000, CacheWrite: 1000, CacheWrite1h: 1000})
	var rescue *accountant.CacheEffect
	for i, e := range r2.CacheEffects {
		if e.Kind == accountant.EffectTTLRescue {
			rescue = &r2.CacheEffects[i]
		}
	}
	if rescue == nil {
		t.Fatalf("no ttlRescue for a read after 10 minutes of a 1h history: %+v", r2.CacheEffects)
	}
	// The rescue covers the history that was read, which is more than the head alone: the system
	// block is a few tokens, so a head-only rescue would be near 0.
	if rescue.Saved.CacheWrite5m < 10000 || rescue.Saved.CacheWrite5m != rescue.Caused.CacheRead {
		t.Errorf("rescue saved %d 5m-write tokens (read %d); want the history, over 10000", rescue.Saved.CacheWrite5m, rescue.Caused.CacheRead)
	}
	// Saving: a 5m write (1.25) avoided and a read (0.10) paid, per token.
	if gain := 1.25*float64(rescue.Saved.CacheWrite5m) - 0.10*float64(rescue.Caused.CacheRead); gain <= 0 {
		t.Errorf("rescue gain %.2f, want a positive saving", gain)
	}
}

// A subagent request is not relabelled, so it books no 1h premium.
func TestSubagentRequestBooksNoTTLPremium(t *testing.T) {
	client := mainAgentTurn(2)
	fwd, ops := relabelled(t, client, "a:agent1")
	if string(fwd) != string(client) || len(ops) != 0 {
		t.Fatalf("a subagent request was changed (ops %d)", len(ops))
	}
}
