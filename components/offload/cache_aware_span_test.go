package offload

import (
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

// #703. A session that reads this repo's own docs carries the TEXT `<<cg:HASH>>` from an early tool
// result on. The span trim treated that text as an offload marker and stopped the span there, so on
// the live proxy every commission over a 2,200-message thread covered 4 messages. The summary must
// cover the whole span, and the turn after it must splice it.
func TestCacheAwareSpanIsNotCutByMarkerTextInAnEarlyMessage(t *testing.T) {
	s := newCacheAware(t, caBaseCfg+"instruction_role: user\n")
	model := &capturingModel{out: "<summary>read the docs, fixed the handler.</summary>"}
	s.modelClient = model

	docs := "the summary marker looks like <<cg:HASH>> and " + expand.Marker("0123456789abcdef") +
		" in the docs\n" + strings.Repeat("filler line from the design doc\n", 40)
	in := []bschemas.ChatMessage{caMsg(bschemas.ChatMessageRoleUser, "TASK: fix the failing handler")}
	in = append(in, caToolPair("reading the docs", "read", `{"p":"docs"}`, "call_0", docs)...)
	in = append(in, caFixture()[1:]...)

	_, start, end := summarizeSpan(in, 1, s.keepLastTurns, nil)
	end = retreatPastLastToolExchange(in, nil, start, end) // the last tool exchange stays verbatim (#723)
	wantCovered := end - start
	if wantCovered < 4 {
		t.Fatalf("fixture span is %d messages; it must reach past the docs read", wantCovered)
	}

	c := caCtx("ca-marker-text")
	t1, rep1 := caTurn(t, s, c, in)
	if len(t1.Input) != len(in) {
		t.Fatalf("turn 1 spliced (%d -> %d); it must forward untouched", len(in), len(t1.Input))
	}
	if rep1.Events[EventSummaryStarted] != 1 {
		t.Fatalf("turn 1 started no summary: gates %v, events %v", rep1.Gates, rep1.Events)
	}
	if !WaitForSummaryForTest(convKey(c), 5*time.Second) {
		t.Fatal("the commissioned summary never landed")
	}
	cp, ok := loadCheckpoint(c)
	if !ok {
		t.Fatal("no checkpoint was written")
	}
	if cp.CoveredCount != wantCovered {
		t.Fatalf("checkpoint covers %d messages, want %d: the span stopped at the docs read's marker "+
			"TEXT instead of reaching the kept tail", cp.CoveredCount, wantCovered)
	}
	t2, _ := caTurn(t, s, c, in)
	if len(t2.Input) >= len(in) || !holdsText(t2.Input, "fixed the handler") {
		t.Fatalf("turn 2 did not splice the summary (%d -> %d messages)", len(in), len(t2.Input))
	}
}

// #703. /stats showed about 1,030 runs that declined with NO gate. Every way Offload returns without
// acting must say why, or "acted: 0" cannot tell a misfiring guard from a healthy wait.
func TestCacheAwareEveryDeclineRecordsAGateOrEvent(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		msgs func() []bschemas.ChatMessage
		ctx  func(*components.Ctx)
		set  func(*CacheAwareSummarizer)
		want string
	}{
		{name: "transcript shorter than keep_last_turns", cfg: caBaseCfg,
			msgs: func() []bschemas.ChatMessage { return caFixture()[:2] },
			want: "summary_span_empty"},
		{name: "span under min_tokens",
			cfg: "keep_last_turns: 2\nmin_tokens: 100000\nresummarize_tokens: 6000\n" +
				"trigger:\n  min_messages: 4\n  min_request_tokens: 10\n  min_request_frac: 0\n",
			msgs: caFixture, want: "summary_span_below_min_tokens"},
		{name: "no model at all", cfg: caBaseCfg, msgs: caFixture,
			set: func(s *CacheAwareSummarizer) { s.modelClient = nil }, want: "no_model"},
		{name: "first message expanded", cfg: caBaseCfg,
			msgs: caFixture,
			ctx: func(c *components.Ctx) {
				for _, m := range caFixture()[1:] {
					c.Store.Put(keptKey(contentKey(schema.MessageText(m))), []byte{1})
				}
			},
			want: "summary_span_empty_after_expand_trim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheAware(t, tc.cfg)
			s.modelClient = &capturingModel{out: "<summary>x</summary>"}
			if tc.set != nil {
				tc.set(s)
			}
			c := caCtx("ca-gate-" + strings.ReplaceAll(tc.name, " ", "-"))
			if tc.ctx != nil {
				tc.ctx(c)
			}
			req, rep := caTurn(t, s, c, tc.msgs())
			if len(req.Input) != len(tc.msgs()) {
				t.Fatalf("declining turn changed the request")
			}
			if rep.Gates[tc.want] != 1 {
				t.Fatalf("gates %v, events %v: want gate %q", rep.Gates, rep.Events, tc.want)
			}
		})
	}
}
