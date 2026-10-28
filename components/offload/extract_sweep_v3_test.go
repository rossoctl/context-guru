package offload

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// Tests for SWEEP V3 (#491): rules, protection, one big cut at a sweet spot.

const v3Base = "warm: true\nwarm_every_n: 3\nwarm_min_age_turns: 5\nwarm_max_turns: 200\nwarm_expected_turns: 200\n"
const v3Rule = v3Base + "selector: rule\nprotect_last_n: 0\n"
const v3Hybrid = v3Base + "selector: hybrid\nprotect_last_n: 0\n"

// v3T is one tool call and its output.
type v3T struct{ id, name, args, out string }

func v3Out(tag string) string {
	return strings.Repeat("candidate "+tag+" distinct line of output\n", 2700)
}

func tCallMsg(id, name, args string) bschemas.ChatMessage {
	n, i := name, id
	m := assistantMsg("")
	m.ChatAssistantMessage = &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{{
		ID: &i, Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &n, Arguments: args}}}}
	return m
}

func v3Conv(calls []v3T, filler int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("do the thing")}}
	req.Provider, req.Model = bschemas.Anthropic, "claude-sonnet-5"
	for _, c := range calls {
		req.Input = append(req.Input, tCallMsg(c.id, c.name, c.args), toolResultMsgWithID(c.id, c.out))
	}
	for i := 0; i < filler; i++ {
		req.Input = append(req.Input, assistantMsg("step done"), userMsg("ok"))
	}
	return req
}

func tReadArgs(p string) string { return `{"file_path":"` + p + `"}` }
func tCmdArgs(c string) string  { return `{"command":"` + c + `"}` }

// v3Calls: two rule drops (a.txt read twice, go test run twice) and two outputs no rule decides.
func tCalls() []v3T {
	return []v3T{
		{"t_ra1", "Read", tReadArgs("/w/a.txt"), v3Out("ra1")},
		{"t_rb", "Read", tReadArgs("/w/b.txt"), v3Out("rb")},
		{"t_go1", "Bash", tCmdArgs("go test ./..."), v3Out("go1")},
		{"t_ls", "Bash", tCmdArgs("ls -la"), v3Out("ls")},
		{"t_ra2", "Read", tReadArgs("/w/a.txt"), v3Out("ra2")},
		{"t_go2", "Bash", tCmdArgs("go  test ./..."), v3Out("go2")},
	}
}

type v3Rig struct {
	t       *testing.T
	e       *ExtractSweep
	session string
	st      store.Store
	asker   components.PrefixAsker
	calls   []v3T
	step    int
	c       int // the compaction point in the rig
	cSource string
	cutFrac float64
	window  int
	noKA    bool
}

func newV3Rig(t *testing.T, session, cfg string) *v3Rig {
	t.Helper()
	resetWarmForTest()
	resetSweepLedgerForTest()
	t.Cleanup(func() { resetWarmForTest(); resetSweepLedgerForTest() })
	t.Cleanup(func() { clearSweepPing(thread.Key(session, "")) })
	return &v3Rig{t: t, e: shippedSweep(t, cfg), session: session, st: newMemStore(), asker: pingAsker(),
		calls: tCalls(), step: 8, c: 200_000, window: 1_000_000}
}

// request sends the next request of the thread, with billed prompt P.
func (r *v3Rig) request(p int) (*bschemas.BifrostChatRequest, *components.Report) {
	r.t.Helper()
	req := v3Conv(r.calls, r.step)
	r.step++
	c := warmCtx(r.session, r.st)
	c.PrefixAsk = r.asker
	c.CtxWindow, c.CtxWindowExact, c.CompactionPoint = r.window, true, r.c
	c.CompactionPointSource, c.CutFrac = r.cSource, r.cutFrac
	c.PrevBilledInput, c.PrevBilledInputThread = p, p
	if r.noKA {
		c.CanKeepAlive = func() bool { return false }
	}
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := r.e.Offload(req, rep, c); err != nil {
		r.t.Fatal(err)
	}
	return req, rep
}

func (r *v3Rig) key() string { return thread.Key(r.session, "") }

func (r *v3Rig) forward() {
	r.t.Helper()
	(&warmRig{t: r.t, session: r.session}).forward()
}

func (r *v3Rig) refused(reason string) int64 { return v3Counters().Refused[reason] }

func dropped(req *bschemas.BifrostChatRequest) map[string]bool {
	out := map[string]bool{}
	for _, i := range toolIndices(req) {
		if strings.Contains(schema.MessageText(req.Input[i]), "<<cg:") {
			out[*req.Input[i].ChatToolMessage.ToolCallID] = true
		}
	}
	return out
}

func TestV3FillFollowsTheCutPoint(t *testing.T) {
	for _, c := range []struct {
		c    int
		want float64
	}{{100_000, 0.85}, {200_000, 0.85}, {500_000, 0.85 + (0.6-0.85)*300.0/800.0}, {1_000_000, 0.6}, {2_000_000, 0.6}} {
		if got := v3Fill(c.c, 0.85, 0.6); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("fill(%d) = %v, want %v", c.c, got, c.want)
		}
	}
}

func TestV3CutPointFollowsTheSummarizerThenTheCompactionPointThenTheWindow(t *testing.T) {
	e := shippedSweep(t, v3Rule)
	c := &components.Ctx{CtxWindow: 200_000, CtxWindowExact: true}
	check := func(name string, wantC int, wantSrc string) {
		t.Helper()
		if got, src := e.v3CutPoint(c); got != wantC || src != wantSrc {
			t.Errorf("%s: C = %d (%s), want %d (%s)", name, got, src, wantC, wantSrc)
		}
	}
	check("window only", 200_000, cSourceWindow)
	c.CompactionPoint, c.CompactionPointSource = 200_000, string(compactionpoint.WindowFallback)
	check("a window_fallback point is the window", 200_000, cSourceWindow)
	c.CompactionPoint, c.CompactionPointSource = 167_000, string(compactionpoint.Operator)
	check("operator point", 167_000, cSourceCompaction)
	c.CompactionPointSource = string(compactionpoint.Measured)
	check("measured point", 167_000, cSourceCompaction)
	c.CompactionPointSource = string(compactionpoint.Assumed)
	check("assumed point", 167_000, cSourceCompaction)
	c.CutFrac = 0.5
	check("summarizer", 83_500, cSourceCutFrac)
	c.CutFrac = 0
	c.CtxWindowExact = false
	check("window not exact", 0, "")
}

// The sweet spot is a share of the EFFECTIVE context: 0.85 x 167k, not 0.85 x the 200k window.
func TestV3SweetSpotUsesTheOperatorCompactionPoint(t *testing.T) {
	r := newV3Rig(t, "v3-eff", v3Rule)
	r.window, r.c, r.cSource = 200_000, 167_000, string(compactionpoint.Operator)
	r.request(0)
	_, rep := r.request(150_000)
	if rep.Events[eventAppliedWarm] != 2 || r.refused("not_in_sweet_spot") != 0 {
		t.Fatalf("operator C = 167k: 150k is in the sweet spot (0.85 x 167k = 142k). events %v refused %v", rep.Events, v3Counters().Refused)
	}
	// The same figure with no operator value: C is the 200k window, and 150k is under 0.85 x 200k.
	r = newV3Rig(t, "v3-eff2", v3Rule)
	r.window, r.c, r.cSource = 200_000, 200_000, string(compactionpoint.WindowFallback)
	r.request(0)
	r.request(150_000)
	r.request(150_000)
	_, rep = r.request(150_000)
	if rep.Events[eventAppliedWarm] != 0 || r.refused("not_in_sweet_spot") == 0 {
		t.Fatalf("window C = 200k: 150k is below 170k. events %v refused %v", rep.Events, v3Counters().Refused)
	}
}

func TestV3CSourceIsReportedForEachCase(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		cutFrac   float64
		point     int
		want      string
	}{
		{"window", string(compactionpoint.WindowFallback), 0, 200_000, cSourceWindow},
		{"compaction", string(compactionpoint.Operator), 0, 167_000, cSourceCompaction},
		{"cutfrac", string(compactionpoint.Operator), 0.9, 167_000, cSourceCutFrac},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newV3Rig(t, "v3-src-"+tc.name, v3Rule)
			r.window, r.c, r.cSource, r.cutFrac = 200_000, tc.point, tc.src, tc.cutFrac
			r.request(0)
			for i := 0; i < 6; i++ {
				r.request(10_000)
			}
			v := v3Counters()
			if v.CSource[tc.want] != 2 || len(v.CSource) != 1 {
				t.Fatalf("c_source = %v, want %q counted twice over 6 requests with warm_every_n 3 (paced)", v.CSource, tc.want)
			}
			d, ok := lastWarmDecision()
			if !ok || d.CSource != tc.want || d.CutPoint == 0 {
				t.Fatalf("the refusal log lacks C and its source: %+v", d)
			}
		})
	}
}

func TestV3AnAcceptedCutRecordsCAndItsSource(t *testing.T) {
	r := newV3Rig(t, "v3-cutrec", v3Rule)
	r.window, r.c, r.cSource = 200_000, 167_000, string(compactionpoint.Operator)
	r.request(0)
	r.request(150_000)
	v := v3Counters()
	if len(v.Recent) != 1 || v.Recent[0].C != 167_000 || v.Recent[0].CSource != cSourceCompaction {
		t.Fatalf("recent cuts = %+v", v.Recent)
	}
}

// ⭐ A rules-only cut: gates pass, the rule drops go in one cut, the ledger has the numbers.
func TestV3RuleSelectorCutsOnceAtTheSweetSpotAndMakesNoModelCall(t *testing.T) {
	asker := pingAsker()
	r := newV3Rig(t, "v3-rule", v3Rule)
	r.asker = asker
	r.request(0)
	req, rep := r.request(175_000)
	r.forward()
	if rep.Events[eventAppliedWarm] != 2 {
		t.Fatalf("events = %v, want 2 applied warm; v3 = %+v", rep.Events, v3Counters())
	}
	got := dropped(req)
	if len(got) != 2 || !got["t_ra1"] || !got["t_go1"] {
		t.Fatalf("dropped = %v, want t_ra1 and t_go1", got)
	}
	v := v3Counters()
	if v.Cuts != 1 || v.RuleDrops[ruleFileReread] != 1 || v.RuleDrops[ruleBuildRerun] != 1 || v.JudgeDrops != 0 {
		t.Fatalf("v3 ledger = %+v", v)
	}
	if len(v.Recent) != 1 || v.Recent[0].DShareP < 0.15 || v.Recent[0].PShareC < 0.87 || v.Recent[0].PShareC > 0.88 {
		t.Fatalf("recent cut = %+v, want D/P >= 0.15 and P/C = 0.875", v.Recent)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 0 {
		t.Fatalf("selector rule made %d model calls", n)
	}
	d, _ := lastWarmDecision()
	if d.DShareP <= 0 || d.PShareC <= 0 {
		t.Fatalf("the decision log lacks D/P and P/C: %+v", d)
	}
}

func TestV3NotInTheSweetSpotIsRefusedAndPassesAtTheSpot(t *testing.T) {
	r := newV3Rig(t, "v3-spot", v3Rule)
	r.request(0)
	r.request(100_000)
	req, rep := r.request(100_000)
	if rep.Events[eventAppliedWarm] != 0 || len(dropped(req)) != 0 || r.refused("not_in_sweet_spot") != 1 {
		t.Fatalf("below the spot: events %v refused %v", rep.Events, v3Counters().Refused)
	}
	_, rep = r.request(175_000)
	if rep.Events[eventAppliedWarm] != 2 {
		t.Fatalf("at the spot: events %v refused %v", rep.Events, v3Counters().Refused)
	}
}

func TestV3ADropThatIsTooSmallIsRefused(t *testing.T) {
	r := newV3Rig(t, "v3-small", v3Rule+"min_drop_frac: 0.9\n")
	r.request(0)
	req, rep := r.request(175_000)
	if rep.Events[eventAppliedWarm] != 0 || len(dropped(req)) != 0 || r.refused("drop_too_small") != 1 {
		t.Fatalf("events %v refused %v", rep.Events, v3Counters().Refused)
	}
}

func TestV3ACutThatDoesNotPayIsRefusedAndDoesNotStartTheGrowthWait(t *testing.T) {
	r := newV3Rig(t, "v3-pay", strings.Replace(v3Rule, "warm_expected_turns: 200", "warm_expected_turns: 5", 1))
	r.request(0)
	for i := 0; i < 2; i++ {
		_, rep := r.request(175_000)
		if rep.Events[eventAppliedWarm] != 0 {
			t.Fatalf("a cut that does not pay was applied: %v", rep.Events)
		}
	}
	if r.refused("does_not_pay") != 2 || r.refused("waiting_for_growth") != 0 {
		t.Fatalf("refused = %v, want 2 does_not_pay and no growth wait", v3Counters().Refused)
	}
}

// ⭐ One cut per round: after an accepted cut, wait until P grew by min_drop_frac x C.
func TestV3TheWaitHoldsUntilPHasGrownByMinDropFracOfC(t *testing.T) {
	r := newV3Rig(t, "v3-wait2", v3Rule)
	r.request(0)
	r.request(175_000)
	base := 176_000 // the first size seen after the cut is the base
	r.request(base)
	r.request(base + 10_000)
	if got := r.refused("waiting_for_growth"); got != 2 {
		t.Fatalf("waiting_for_growth = %d, want 2 (%v)", got, v3Counters().Refused)
	}
	r.request(base + 30_000) // 0.15 x 200k = 30k: the wait is over
	if got := r.refused("waiting_for_growth"); got != 2 {
		t.Fatalf("waiting_for_growth = %d after the growth, want it still 2", got)
	}
	if r.refused("no_rule_drops") != 1 {
		t.Fatalf("after the wait the gates must reach the rules: %v", v3Counters().Refused)
	}
}

func TestV3AShrunkPromptStartsANewRound(t *testing.T) {
	r := newV3Rig(t, "v3-shrink", v3Rule)
	r.request(0)
	r.request(175_000)
	r.request(176_000)
	r.request(180_000) // waiting
	r.request(172_000) // the prompt fell: a client compaction, a new round
	r.request(172_000)
	if got := r.refused("waiting_for_growth"); got != 2 {
		t.Fatalf("waiting_for_growth = %d, want 2 (the shrink must end the wait): %v", got, v3Counters().Refused)
	}
}

// ---- the rules ----

func v3PlanOf(t *testing.T, cfg string, calls []v3T) (v3Plan, *ExtractSweep, *bschemas.BifrostChatRequest) {
	t.Helper()
	e := shippedSweep(t, cfg)
	req := v3Conv(calls, 2)
	c := warmCtx("v3-plan", newMemStore())
	return e.v3Plan(c, req), e, req
}

func ruleIDs(p v3Plan) map[string]string {
	out := map[string]string{}
	for _, h := range p.rules {
		out[h.toolID] = h.rule
	}
	return out
}

func TestV3FileRuleReadThenReadOrWrite(t *testing.T) {
	big := v3Out
	cases := []struct {
		name  string
		calls []v3T
		want  map[string]string
	}{
		{"read then read the same path", []v3T{{"a", "Read", tReadArgs("/w/a"), big("a")}, {"b", "Read", tReadArgs("/w/a"), big("b")}},
			map[string]string{"a": ruleFileReread}},
		{"read then edit the same path", []v3T{{"a", "Read", tReadArgs("/w/a"), big("a")}, {"e", "Edit", tReadArgs("/w/a"), "ok"}},
			map[string]string{"a": ruleFileReread}},
		{"read then write the same path", []v3T{{"a", "Read", tReadArgs("/w/a"), big("a")}, {"e", "Write", tReadArgs("/w/a"), "ok"}},
			map[string]string{"a": ruleFileReread}},
		{"read then read ANOTHER path", []v3T{{"a", "Read", tReadArgs("/w/a"), big("a")}, {"b", "Read", tReadArgs("/w/b"), big("b")}},
			map[string]string{}},
		{"edit then read the same path", []v3T{{"e", "Edit", tReadArgs("/w/a"), "ok"}, {"a", "Read", tReadArgs("/w/a"), big("a")}},
			map[string]string{}},
		{"a read that is the last one", []v3T{{"a", "Read", tReadArgs("/w/a"), big("a")}}, map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := v3PlanOf(t, v3Rule, tc.calls)
			got := ruleIDs(p)
			if len(got) != len(tc.want) {
				t.Fatalf("rules = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("rules = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestV3BuildRuleNeedsALaterRunOfTheSameCommand(t *testing.T) {
	big := v3Out
	cases := []struct {
		name  string
		calls []v3T
		want  int
	}{
		{"same command twice", []v3T{{"a", "Bash", tCmdArgs("go test ./..."), big("a")}, {"b", "Bash", tCmdArgs("go test ./..."), big("b")}}, 1},
		{"white space differs", []v3T{{"a", "Bash", tCmdArgs("make   test"), big("a")}, {"b", "Bash", tCmdArgs("make test"), big("b")}}, 1},
		{"different command", []v3T{{"a", "Bash", tCmdArgs("go test ./a"), big("a")}, {"b", "Bash", tCmdArgs("go test ./b"), big("b")}}, 0},
		{"not a build command", []v3T{{"a", "Bash", tCmdArgs("ls"), big("a")}, {"b", "Bash", tCmdArgs("ls"), big("b")}}, 0},
		{"not a shell tool", []v3T{{"a", "Grep", tCmdArgs("go test ./..."), big("a")}, {"b", "Grep", tCmdArgs("go test ./..."), big("b")}}, 0},
		{"a single run", []v3T{{"a", "Bash", tCmdArgs("pytest -x"), big("a")}}, 0},
		{"three runs: the first two", []v3T{{"a", "Bash", tCmdArgs("npm test"), big("a")}, {"b", "Bash", tCmdArgs("npm test"), big("b")}, {"c", "Bash", tCmdArgs("npm test"), big("c")}}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := v3PlanOf(t, v3Rule, tc.calls)
			if len(p.rules) != tc.want {
				t.Fatalf("rules = %v, want %d", ruleIDs(p), tc.want)
			}
		})
	}
}

func TestV3AnOutputWithAMarkerOrExpandedIsNeverARuleCandidate(t *testing.T) {
	calls := []v3T{{"a", "Read", tReadArgs("/w/a"), v3Out("a")}, {"b", "Read", tReadArgs("/w/a"), v3Out("b")}}
	e := shippedSweep(t, v3Rule)
	st := newMemStore()
	c := warmCtx("v3-mark", st)
	if p := e.v3Plan(c, v3Conv(calls, 2)); len(p.rules) != 1 {
		t.Fatalf("fixture: want 1 rule, got %v", ruleIDs(p))
	}
	// A real marker: its key is in the Store.
	st.Put("abc123", []byte("original"))
	marked := []v3T{{"a", "Read", tReadArgs("/w/a"), v3Out("a") + "\n<<cg:abc123>>"}, calls[1]}
	if p := e.v3Plan(c, v3Conv(marked, 2)); len(p.rules) != 0 {
		t.Fatalf("a marked output was a rule candidate: %v", ruleIDs(p))
	}
	// Expanded in this session.
	MarkKeptVerbatim(st, "v3-mark", v3Out("a"))
	if p := e.v3Plan(c, v3Conv(calls, 2)); len(p.rules) != 0 {
		t.Fatalf("an expanded output was a rule candidate: %v", ruleIDs(p))
	}
}

func TestV3ProtectionsNeverDrop(t *testing.T) {
	calls := []v3T{
		{"a", "Read", tReadArgs("/w/docs/plan.md"), v3Out("a")}, {"b", "Read", tReadArgs("/w/docs/plan.md"), v3Out("b")},
		{"c", "TodoWrite", `{"x":1}`, v3Out("c")},
		{"d", "Bash", tCmdArgs("go test ./..."), v3Out("d")}, {"e", "Bash", tCmdArgs("go test ./..."), v3Out("e")},
	}
	t.Run("none", func(t *testing.T) {
		p, _, _ := v3PlanOf(t, v3Rule, calls)
		if got := ruleIDs(p); len(got) != 2 {
			t.Fatalf("rules = %v, want a and d", got)
		}
	})
	t.Run("protect_paths", func(t *testing.T) {
		p, _, _ := v3PlanOf(t, v3Rule+"protect_paths: ['docs/*.md']\n", calls)
		if _, ok := ruleIDs(p)["a"]; ok || !p.protectedID["a"] || len(p.rules) != 1 {
			t.Fatalf("rules = %v protected = %v", ruleIDs(p), p.protectedID)
		}
	})
	t.Run("protect_tools", func(t *testing.T) {
		p, _, _ := v3PlanOf(t, v3Rule+"protect_tools: ['bash']\n", calls)
		if _, ok := ruleIDs(p)["d"]; ok || !p.protectedID["d"] || !p.protectedID["e"] || len(p.rules) != 1 {
			t.Fatalf("rules = %v protected = %v", ruleIDs(p), p.protectedID)
		}
	})
	t.Run("protect_last_n", func(t *testing.T) {
		cfg := strings.Replace(v3Rule, "protect_last_n: 0", "protect_last_n: 4", 1)
		p, _, _ := v3PlanOf(t, cfg, calls)
		// The newest 4 of the 5 results are protected: only "a" can be a rule drop.
		if len(p.protectedID) != 4 || p.protectedID["a"] || len(p.rules) != 1 || ruleIDs(p)["a"] != ruleFileReread {
			t.Fatalf("rules = %v protected = %v", ruleIDs(p), p.protectedID)
		}
	})
	t.Run("protect_last_n defaults to 10 when v3 is on", func(t *testing.T) {
		e := shippedSweep(t, v3Base+"selector: rule\n")
		if e.protectLastN != 10 {
			t.Fatalf("protect_last_n = %d, want 10", e.protectLastN)
		}
		p := e.v3Plan(warmCtx("v3-n", newMemStore()), v3Conv(calls, 2))
		if len(p.rules) != 0 {
			t.Fatalf("5 results are all in the newest 10: no rule drop, got %v", ruleIDs(p))
		}
	})
}

func TestV3GlobMatching(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"docs/plan.md", "/w/docs/plan.md", true},
		{"docs/plan.md", "/w/xdocs/plan.md", false},
		{"*.md", "/w/docs/plan.md", true},
		{"docs/*.md", "/w/docs/sub/plan.md", false},
		{"docs/**/*.md", "/w/docs/sub/plan.md", true},
		{"/w/**", "/w/a/b/c", true},
		{"/w/*.go", "/other/w/x.go", false},
		{"plan.md", "/w/plan.md", true},
	} {
		re, err := globRegexp(c.glob)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("glob %q on %q = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

// ---- hybrid ----

func TestV3HybridAsksTheJudgeOnlyAboutWhatTheRulesDidNotDecide(t *testing.T) {
	r := newV3Rig(t, "v3-hyb-ask", v3Hybrid)
	a := pingAsker()
	r.asker = a
	r.request(0)
	r.request(175_000)
	r.request(175_000) // the third: the question is registered
	r.forward()
	warmMu.Lock()
	h := warmThreads[r.key()].held
	warmMu.Unlock()
	if h == nil {
		t.Fatalf("no answer is waiting: %+v", v3Counters())
	}
	got := map[string]bool{}
	for _, o := range h.Outputs {
		got[o.ToolUseID] = true
	}
	if len(got) != 4 || got["t_ra1"] || got["t_go1"] {
		t.Fatalf("the judge was asked about %v, want the 4 outputs no rule decided (not t_ra1, t_go1)", got)
	}
}

// ⭐ Hybrid is ONE cut: nothing is applied while the question is out, then rules and judge go together.
func TestV3HybridMakesOneCutWithRulesAndJudgeTogether(t *testing.T) {
	r := newV3Rig(t, "v3-hyb-one", v3Hybrid)
	r.request(0)
	for i := 0; i < 2; i++ {
		req, rep := r.request(175_000)
		if rep.Events[eventAppliedWarm] != 0 || len(dropped(req)) != 0 {
			t.Fatalf("a rule drop went in before the judge answered: %v", rep.Events)
		}
	}
	r.forward()
	req, rep := r.request(175_000)
	if rep.Events[eventAppliedWarm] != 6 {
		t.Fatalf("events = %v, want ONE cut of 2 rule + 4 judge drops; v3 = %+v", rep.Events, v3Counters())
	}
	if got := dropped(req); len(got) != 6 {
		t.Fatalf("dropped = %v", got)
	}
	v := v3Counters()
	if v.Cuts != 1 || v.RuleDrops[ruleFileReread]+v.RuleDrops[ruleBuildRerun] != 2 || v.JudgeDrops != 4 || v.HybridRulesOnly != 0 {
		t.Fatalf("ledger = %+v", v)
	}
}

func TestV3HybridWithoutAnAnswerFallsBackToRulesAloneAndCountsIt(t *testing.T) {
	r := newV3Rig(t, "v3-hyb-fb", v3Hybrid)
	r.asker = &fakeAsker{err: errors.New("down")}
	r.request(0)
	r.request(175_000)
	r.request(175_000)
	r.forward()
	req, rep := r.request(175_000)
	if rep.Events[eventAppliedWarm] != 2 || len(dropped(req)) != 2 {
		t.Fatalf("events = %v v3 = %+v, want a rules-only cut of 2", rep.Events, v3Counters())
	}
	if v := v3Counters(); v.HybridRulesOnly != 1 || v.JudgeDrops != 0 {
		t.Fatalf("ledger = %+v", v)
	}
}

func TestV3HybridRulesAloneStillPassTheSizeGate(t *testing.T) {
	r := newV3Rig(t, "v3-hyb-small", v3Hybrid+"min_drop_frac: 0.9\n")
	r.asker = &fakeAsker{err: errors.New("down")}
	r.request(0)
	r.request(175_000)
	r.request(175_000)
	r.forward()
	req, rep := r.request(175_000)
	if rep.Events[eventAppliedWarm] != 0 || len(dropped(req)) != 0 || r.refused("drop_too_small") == 0 {
		t.Fatalf("events %v refused %v", rep.Events, v3Counters().Refused)
	}
}

func TestV3NoQuestionDuringTheGrowthWait(t *testing.T) {
	a := pingAsker()
	r := newV3Rig(t, "v3-nowait-ask", v3Hybrid)
	r.asker = a
	r.request(0)
	for i := 0; i < 2; i++ {
		r.request(175_000)
	}
	r.forward()
	r.request(175_000) // one cut: every candidate is dropped
	// New outputs the judge could be asked about. A question would be paid for them if the wait did not stop it.
	r.calls = append(r.calls,
		v3T{"t_n1", "Bash", tCmdArgs("ls /a"), v3Out("n1")},
		v3T{"t_n2", "Bash", tCmdArgs("ls /b"), v3Out("n2")})
	before := atomic.LoadInt64(&a.calls)
	for i := 0; i < 10; i++ {
		r.request(176_000 + i*1000) // inside the wait: under 30k of growth
		r.forward()
	}
	if got := atomic.LoadInt64(&a.calls); got != before {
		t.Fatalf("the judge was asked %d times during the growth wait", got-before)
	}
	// Control: once P has grown enough the wait is over, and the same fixture does ask.
	for i := 0; i < 6; i++ {
		r.request(210_000)
		r.forward()
	}
	if got := atomic.LoadInt64(&a.calls); got == before {
		t.Fatal("fixture: after the wait the judge was never asked, so the test above proves nothing")
	}
}

func TestV3TheJudgeDoesNotSeeProtectedOutputs(t *testing.T) {
	e := shippedSweep(t, v3Base+"selector: judge\nprotect_tools: ['bash']\nprotect_last_n: 0\n")
	c := warmCtx("v3-judge-prot", newMemStore())
	cand := e.snapshotCandidate(c, v3Conv(tCalls(), 8))
	cand.warm, cand.minAge = true, 5
	cands := cand.collect(&components.Report{Component: "extract_llm_sweep"})
	for _, cd := range cands {
		if strings.HasPrefix(cd.toolID, "t_go") || cd.toolID == "t_ls" {
			t.Fatalf("a protected output was offered: %s", cd.toolID)
		}
	}
	if len(cands) != 3 {
		t.Fatalf("offered %d, want the 3 file reads", len(cands))
	}
}

// ---- config ----

func TestV3IsOffByDefaultAndTheSettingsAreChecked(t *testing.T) {
	e := shippedSweep(t, warmCfg)
	if e.v3 || e.selector != selectorJudge {
		t.Fatalf("v3 is on with the default config: %+v", e)
	}
	if e := shippedSweep(t, warmCfg+"selector: judge\n"); e.v3 {
		t.Fatal("selector: judge alone must not switch v3 on")
	}
	for _, cfg := range []string{"selector: rule\n", "fill_small: 0.8\n", "protect_last_n: 3\n", "protect_tools: [x]\n"} {
		c, err := newExtractSweep([]byte("min_tokens: 2000\n" + cfg))
		if err != nil || c.(*ExtractSweep).v3 {
			t.Errorf("%q without warm: true must be accepted and do nothing (err %v)", cfg, err)
		}
	}
	if _, err := newExtractSweep([]byte("min_tokens: 2000\nwarm: true\nselector: bogus\n")); err == nil {
		t.Error("an unknown selector was accepted")
	}
	e = shippedSweep(t, warmCfg+"selector: rule\n")
	if !e.v3 || e.fillSmall != 0.85 || e.fillLarge != 0.6 || e.minDropFrac != 0.15 || e.protectLastN != 10 {
		t.Fatalf("v3 defaults wrong: %+v", e)
	}
}

// ⭐ With the default config the warm path is #720's: no v3 counter, the same bytes.
func TestV3DefaultConfigIsUnchangedFromTheWarmPath(t *testing.T) {
	run := func(cfg string) ([]string, int) {
		r := newWarmRig(t, "v3-default", cfg, payingTail(), 1.0)
		r.toHeld()
		req, rep := r.request(nil)
		if v := SweepWarmCounters().V3; v.Any() {
			t.Fatalf("v3 counted something with the default config: %+v", v)
		}
		return texts(req), rep.Events[eventAppliedWarm]
	}
	base, n := run(warmCfg)
	if n != 3 {
		t.Fatalf("fixture: the warm path applied %d, want 3", n)
	}
	same, n2 := run(warmCfg + "selector: judge\n")
	if n2 != n || strings.Join(base, "\x00") != strings.Join(same, "\x00") {
		t.Fatal("selector: judge changed the bytes of the warm path")
	}
}

// ---- the keep-alive ping question and the Store answer (round 1) ----

func TestV3PingQuestionNeverOffersAProtectedOutput(t *testing.T) {
	e := shippedSweep(t, v3Base+"selector: judge\nprotect_last_n: 1\n")
	key, _ := registered(t, e, "v3-ping-prot", pingAsker(), newMemStore(), refConversation(3))
	_, info, reason, ok := SweepPingSubstitute(key)
	if !ok || info.Offered != 2 {
		t.Fatalf("ping offered %d (ok %v, reason %q), want 2: the newest output is protected", info.Offered, ok, reason)
	}
}

func TestV3RuleSelectorMakesNoModelCallAtThePingAndKeepsNoCopy(t *testing.T) {
	resetV3ForTest()
	asker := pingAsker()
	e := shippedSweep(t, v3Rule)
	key, _ := registered(t, e, "v3-ping-rule", asker, newMemStore(), refConversation(3))
	sweepPingMu.Lock()
	_, kept := sweepPingCand[key]
	sweepPingMu.Unlock()
	if kept {
		t.Fatal("selector rule kept a copy of the conversation for a ping that is always refused")
	}
	dispatch, _, _, ok := SweepPingSubstitute(key)
	if ok || dispatch != nil {
		t.Fatal("selector rule offered a ping question")
	}
	if n := atomic.LoadInt64(&asker.calls); n != 0 {
		t.Fatalf("selector rule made %d model calls at the ping", n)
	}
}

// A candidate registered anyway (a config change) is still refused at the ping.
func TestV3RuleSelectorRefusesARegisteredPingCandidate(t *testing.T) {
	resetV3ForTest()
	asker := pingAsker()
	key, _ := registered(t, shippedSweep(t, ""), "v3-ping-rule2", asker, newMemStore(), refConversation(3))
	sweepPingMu.Lock()
	sweepPingCand[key].e = shippedSweep(t, v3Rule)
	sweepPingMu.Unlock()
	_, _, reason, ok := SweepPingSubstitute(key)
	if ok || reason != SweepReasonV3Rule || v3Counters().Refused["ping_refused_rule"] != 1 || atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatalf("ok %v reason %q refused %v", ok, reason, v3Counters().Refused)
	}
}

func TestV3HybridPingDoesNotOfferRuleDecidedOutputs(t *testing.T) {
	e := shippedSweep(t, v3Hybrid)
	c := warmCtx("v3-ping-hyb", newMemStore())
	cand := e.snapshotCandidate(c, v3Conv(tCalls(), 8))
	cands := cand.collect(&components.Report{Component: "extract_llm_sweep"})
	for _, cd := range cands {
		if cd.toolID == "t_ra1" || cd.toolID == "t_go1" {
			t.Fatalf("a rule-decided output was offered at the ping: %s", cd.toolID)
		}
	}
	if len(cands) != 4 {
		t.Fatalf("offered %d, want the 4 outputs no rule decides", len(cands))
	}
}

// A stored answer (made before a config change, or by a ping) that names a protected output must not drop it.
func TestV3ColdApplyOfAStoredAnswerSkipsProtectedOutputs(t *testing.T) {
	st, _ := heldFor(t, shippedSweep(t, ""), "v3-cold-prot", refConversation(3))
	e := shippedSweep(t, v3Base+"selector: judge\nprotect_last_n: 1\n")
	req := refConversation(3)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, coldCtx("v3-cold-prot", st)); err != nil {
		t.Fatal(err)
	}
	if got := dropped(req); len(got) != 2 || got["toolu_wire_2"] {
		t.Fatalf("dropped = %v, want toolu_wire_0 and _1 only: _2 is the newest and protected", got)
	}
	if v3Counters().Refused["cold_protected_filtered"] != 1 {
		t.Fatalf("the filter is not counted: %+v", v3Counters())
	}
}

func TestV3ACountedCutOnAColdApplyStartsTheGrowthWait(t *testing.T) {
	resetWarmForTest()
	st, _ := heldFor(t, shippedSweep(t, ""), "v3-cold-cut", refConversation(3))
	e := shippedSweep(t, v3Base+"selector: judge\nprotect_last_n: 0\n")
	c := coldCtx("v3-cold-cut", st)
	c.CtxWindow, c.CtxWindowExact, c.CompactionPoint, c.PrevBilledInputThread = 1_000_000, true, 200_000, 150_000
	if _, err := e.Offload(refConversation(3), &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
		t.Fatal(err)
	}
	warmMu.Lock()
	await := warmThreads[thread.Key("v3-cold-cut", "")].v3Await
	warmMu.Unlock()
	if v3Counters().Cuts != 1 || !await {
		t.Fatalf("cuts %d await %v: a cold cut must be counted and start the wait", v3Counters().Cuts, await)
	}
}

func TestV3PacedRefusalsAreCountedOncePerQuestionInterval(t *testing.T) {
	r := newV3Rig(t, "v3-pace", v3Rule)
	for i := 0; i < 3; i++ {
		r.request(0) // no billed size: nothing counted, even at the pace
	}
	if v3Counters().Refused["not_in_sweet_spot"] != 0 {
		t.Fatal("a request with P = 0 was counted")
	}
	for i := 0; i < 6; i++ {
		r.request(100_000)
	}
	if got := r.refused("not_in_sweet_spot"); got != 2 {
		t.Fatalf("not_in_sweet_spot = %d, want 2 over 6 requests with warm_every_n 3", got)
	}
}

func TestV3RuleDropsRecordTheirOwnTokenCount(t *testing.T) {
	p, e, req := v3PlanOf(t, v3Rule, []v3T{{"a", "Read", tReadArgs("/w/a"), v3Out("a")}, {"b", "Read", tReadArgs("/w/a"), v3Out("b")}})
	src := e.newV3Held(warmCtx("v3-tok", newMemStore()), "k", p, req.Input, false)
	if len(src.rules) != 1 || src.rules[0].Tokens != schema.TextTokens(v3Out("a")) || src.rules[0].Tokens <= src.rules[0].Saves {
		t.Fatalf("rule output = %+v, want Tokens = the output's own size, above Saves", src.rules)
	}
}

func TestV3AColdRequestWithNoStoredAnswerBuildsNoPlan(t *testing.T) {
	e := shippedSweep(t, v3Base+"selector: judge\nprotect_last_n: 1\n")
	before := v3PlanBuilds.Load()
	req := refConversation(3)
	if _, err := e.Offload(req, &components.Report{Component: "extract_llm_sweep"}, coldCtx("v3-noplan", newMemStore())); err != nil {
		t.Fatal(err)
	}
	if got := v3PlanBuilds.Load() - before; got != 0 {
		t.Fatalf("a cold request with no stored answer built %d plans", got)
	}
	st, _ := heldFor(t, shippedSweep(t, ""), "v3-noplan2", refConversation(3))
	before = v3PlanBuilds.Load()
	if _, err := e.Offload(refConversation(3), &components.Report{Component: "extract_llm_sweep"}, coldCtx("v3-noplan2", st)); err != nil {
		t.Fatal(err)
	}
	if got := v3PlanBuilds.Load() - before; got != 1 {
		t.Fatalf("with a stored answer the plan was built %d times, want 1 (control)", got)
	}
}

func TestV3ColdCutSplitsRuleDropsFromJudgeDrops(t *testing.T) {
	resetV3ForTest()
	e := shippedSweep(t, v3Rule)
	req := v3Conv(tCalls(), 2)
	marked := func(id string) {
		i, _ := rawToolMessage(req.Input, id)
		txt := schema.MessageText(req.Input[i]) + "\n<<cg:zz>>"
		req.Input[i].Content = &bschemas.ChatMessageContent{ContentStr: &txt}
	}
	st := newMemStore()
	st.Put("zz", []byte("original"))
	cx := warmCtx("v3-split", st)
	before := v3MarkedIDs(cx, req)
	marked("t_ra1")
	marked("t_rb")
	marked("t_ls") // claimed, but marked before: not a drop of this apply
	before["t_ls"] = true
	e.v3ColdCut(cx, req, "k-split", []string{"t_ra1", "t_rb", "t_ls", "t_go1"}, before,
		map[string]string{"t_ra1": ruleFileReread})
	v := v3Counters()
	if v.RuleDrops[ruleFileReread] != 1 || v.JudgeDrops != 1 || v.Cuts != 1 {
		t.Fatalf("ledger = %+v, want 1 rule drop, 1 judge drop (t_go1 was never marked, t_ls was marked before)", v)
	}
}

func TestV3AColdApplyThatChangesNothingStillClearsTheHybridQuestion(t *testing.T) {
	resetWarmForTest()
	e := shippedSweep(t, v3Hybrid)
	tkey := thread.Key("v3-cold-clear", "")
	warmMu.Lock()
	warmFor(tkey).v3Asked = true
	warmMu.Unlock()
	// A waiting answer that names an output the conversation does not have: the recheck drops it, wn = 0.
	warmHold(tkey, heldAnswer{V: heldVersion, Thread: tkey, AskedAtMs: time.Now().UnixMilli(), Judged: 3,
		Outputs: []heldOutput{{ToolUseID: "gone", Hash: "x", Tokens: 100, Saves: 90, Drop: true}}})
	c := coldCtx("v3-cold-clear", newMemStore())
	c.CanKeepAlive = nil
	req := refConversation(3)
	_, changed := e.v3Dispatch(req, &components.Report{Component: "extract_llm_sweep"}, c, tkey,
		v3In{cold: true, hasHeld: true})
	warmMu.Lock()
	asked := warmThreads[tkey].v3Asked
	warmMu.Unlock()
	if changed != 0 || asked {
		t.Fatalf("changed %d, v3Asked %v: a used answer must clear the question even when nothing changed", changed, asked)
	}
}
