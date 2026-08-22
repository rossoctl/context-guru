package offload

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/store"
)

// gatewayCard is the operator rate card this deployment actually bills at
// (/etc/context-guru/prices.yaml, claude-haiku family), per token.
var gatewayCard = modelinfo.Price{
	Input: 0.76 / 1e6, Output: 3.80 / 1e6,
	CacheRead: 0.076 / 1e6, CacheWrite: 0.95 / 1e6,
}

// THE PRICE-MAP RECONCILIATION. extraction_calls.cost_usd is priced by the component from
// cheapmodel.Pricing; requests.cg_llm_cost_usd is priced by the dashboard from the operator's
// modelinfo.Price. On production the same 93 calls were booked at $0.7946 and $0.6039 — 32%
// apart, because the component fell back to haiku LIST rates. With the card reachable through
// Ctx.RatesFor the two must agree to the cent on the same tokens.
func TestBothCostPathsAgreeOnTheOperatorCard(t *testing.T) {
	e := &ExtractLLM{modelName: "claude-haiku-4-5", pricing: cheapmodel.HaikuPricing()}
	c := components.Ctx{RatesFor: func(string) components.TokenRates {
		return components.TokenRates{
			Input: gatewayCard.Input, Output: gatewayCard.Output,
			CacheRead: gatewayCard.CacheRead, CacheWrite: gatewayCard.CacheWrite,
		}
	}}
	const in, out, cw, cr int64 = 3785, 519, 988, 9265
	component := e.pricingFor(c).Cost(in, out, cw, cr)
	dashboard := gatewayCard.Cost(in, cr, cw, out)
	if math.Abs(component-dashboard) > 1e-12 {
		t.Fatalf("the two cost paths must price one call identically: component=$%.8f dashboard=$%.8f",
			component, dashboard)
	}
	// And the list-rate fallback is what they used to disagree by, so it must not be reached
	// when the card can answer.
	if list := cheapmodel.HaikuPricing().Cost(in, out, cw, cr); math.Abs(component-list) < 1e-9 {
		t.Fatal("pricingFor still used the built-in list rates with a card available")
	}
	if e.cardPriced(c) != true {
		t.Fatal("cardPriced must report that the named model is on the card")
	}
	// With no card, the gate must SAY it is guessing rather than pretend.
	if e.cardPriced(components.Ctx{}) {
		t.Fatal("cardPriced must be false without a resolver")
	}
}

// The reuse prior is the component's own realized amortization, not a recurrence rate.
// production ledger: saved_gross 73,911 / saved_unique 46,380 = 1.59x realized.
func TestReusePriorMatchesTheMeasuredLedger(t *testing.T) {
	if got := 1 + expectedReuses(false, 5); got < 1.2 || got > 2.0 {
		t.Errorf("first-sight multiplier = %.2fx, ledger measured 1.59x", got)
	}
	if expectedReuses(true, 5) <= expectedReuses(false, 5) {
		t.Error("recurring content must still be valued above first sight")
	}
	if expectedReuses(false, 40) >= expectedReuses(false, 5) {
		t.Error("late in a session there are fewer turns left to amortize over")
	}
}

// A replayed removal is a cache-READ token, not a cache-write one. Collapsing the two is what
// over-credited every cold-turn call by ~12x.
func TestColdTurnPricesReplaysAtTheReadRate(t *testing.T) {
	v := savedTokenValue(&components.Ctx{CacheAware: true, ColdCache: true})
	if v.perToken <= v.repeatPerToken {
		t.Fatalf("a cold turn writes at 1.25x and replays at 0.1x: first=%g repeat=%g",
			v.perToken, v.repeatPerToken)
	}
	if r := v.perToken / v.repeatPerToken; math.Abs(r-12.5) > 0.5 {
		t.Errorf("write/read ratio = %.2f, expected 12.5", r)
	}
	warm := savedTokenValue(&components.Ctx{CacheAware: true})
	if warm.perToken != warm.repeatPerToken {
		t.Error("on a warm turn both the removal and its replays are cache reads")
	}
}

// The content classes must discriminate: at a cold-turn valuation and the measured per-call
// cost, prose and directory listings clear break-even at reachable sizes and JSON, ANSI CLI
// output, grep output and test logs do not — for the whole reachable size range, because the
// agent caps every tool result near 7,399 tokens.
func TestContentClassesGateOnExpectedYield(t *testing.T) {
	cold := savedTokenValue(&components.Ctx{CacheAware: true, ColdCache: true,
		SelfRates: components.TokenRates{Input: 3.80 / 1e6, Output: 19.0 / 1e6,
			CacheRead: 0.38 / 1e6, CacheWrite: 4.75 / 1e6}})
	pricing := cheapmodel.Pricing{InputPerMTok: 0.76, OutputPerMTok: 3.80,
		CacheReadPerMTok: 0.076, CacheWritePerMTok: 0.95}
	const ceiling = 7399 // the largest tool output this workload can produce
	allowedAt := func(cls string, size int) bool {
		var ratio float64
		for _, c := range contentClasses {
			if c.name == cls {
				ratio = c.ratio
			}
		}
		if ratio == 0 {
			t.Fatalf("unknown class %q", cls)
		}
		// 700 = the variable prompt overhead at context_messages: 2 (200 for the keep-list
		// and labels, ~500 for two rendered messages).
		return evaluateGate(size, ratio, cold, callCost(pricing, size, 700), true, 5, false, true).allow
	}
	// THE PREFILTER, as arithmetic rather than as a list of banned classes. These four cannot
	// pay at ANY size this workload can produce, because the agent caps every tool result near
	// 7,399 tokens: JSON needs 55,100 tokens to break even, grep 23,100, ANSI CLI output
	// 19,300, and a test log never does. Together JSON and ANSI are 31% of the reachable mass.
	for _, cls := range []string{"json_blob", "test_result_log", "grep_output", "ansi_cli_output"} {
		if allowedAt(cls, ceiling) {
			t.Errorf("%s compresses too little to pay even at the %d-token ceiling", cls, ceiling)
		}
	}
	// And these do pay, at sizes that really occur — which is what turns a flat 474-token mean
	// yield into a selected population above break-even.
	for _, cls := range []string{"ls_listing", "markdown_doc", "multi_file_bundle"} {
		if !allowedAt(cls, 4500) {
			t.Errorf("%s at 4,500 tokens should clear break-even", cls)
		}
	}
	// A directory listing shrinks 65.5% and pays from 1,400 tokens; it is the one class that
	// pays well inside the common size range.
	if !allowedAt("ls_listing", 2000) {
		t.Error("ls_listing should pay from ~1,400 tokens")
	}
}

// The classifier must put real production shapes in the right class — the ratios are only
// worth anything if the class is right.
func TestContentClassRecognisesRealShapes(t *testing.T) {
	cases := []struct{ want, body string }{
		{"json_blob", `{"runs":[{"id":1,"status":"ok"},{"id":2,"status":"fail"}]}`},
		{"grep_output", "scripts/submit-run.sh:316: timeout_sec=900\nscripts/lib/common.sh:42: set -e\n"},
		{"ls_listing", "total 48\ndrwxr-xr-x  4 itayn staff 128 Aug 21 09:10 runs\n"},
		{"ansi_cli_output", "\x1b[32mPASS\x1b[0m building module\nlinking\n"},
		{"read_with_line_numbers", "   1\timport json\n   2\timport sys\n   3\t\n   4\tdef main():\n"},
		{"markdown_doc", "# Title\n\n- first\n- second\n- third\n\nsome prose here\n"},
		{"source_code", "package offload\n\nfunc helper() int { return 1 }\n"},
	}
	for _, tc := range cases {
		got, _, ok := contentClass(tc.body)
		if !ok || got != tc.want {
			t.Errorf("classified %q as %q (ok=%v), want %q", headLine(tc.body), got, ok, tc.want)
		}
	}
	// Plain prose is deliberately unrecognised: the pooled learned ratio must govern there
	// rather than a class ratio invented for it.
	if _, _, ok := contentClass("The benchmark finished and the numbers moved the wrong way."); ok {
		t.Error("prose must fall through to the learned ratio, not be classified")
	}
}

func headLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// A sweep must have a bound. Production made 27 calls on one request against a tenant cap of
// 2, spent $0.229 and added 76.6 s to a turn — the sweep does not draw on the hot path's caps,
// so its own default was the only brake and it was "unlimited".
func TestColdSweepIsBoundedByDefault(t *testing.T) {
	c, err := newExtractLLM([]byte("per_output: false\ncold_cache:\n  enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := c.(*ExtractLLM)
	if e.cold.MaxCalls <= 0 || e.cold.MaxCalls > llmConcurrency {
		t.Fatalf("default sweep cap = %d, want a bound at or below one concurrency round (%d)",
			e.cold.MaxCalls, llmConcurrency)
	}
	// An operator can still opt out, explicitly.
	uc, err := newExtractLLM([]byte("per_output: false\ncold_cache:\n  enabled: true\n  max_calls: -1\n"))
	if err != nil {
		t.Fatal(err)
	}
	un := uc.(*ExtractLLM)
	if un.cold.MaxCalls != 0 {
		t.Fatalf("max_calls: -1 must mean unlimited, got %d", un.cold.MaxCalls)
	}
}

// The cross-session extraction cache holds results that were already paid for with a model
// call, in a store whose default cap is shared with the large expand stashes. Losing an entry
// costs a call.
func TestCrossSessionResultKeysArePinned(t *testing.T) {
	key := extract.ResultKey("id", "claude-haiku-4-5", extract.DefaultCfg())
	pinned := false
	for _, p := range store.DefaultPinPrefixes {
		if strings.HasPrefix(key, p) {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("extraction result key %q is not in a pinned namespace %v", key, store.DefaultPinPrefixes)
	}
}

// context_messages is the per-call cost lever; its default must stay small enough that the
// prompt is mostly the candidate rather than mostly the conversation.
func TestContextMessagesDefaultIsSmall(t *testing.T) {
	if defaultContextMessages > 3 {
		t.Fatalf("defaultContextMessages = %d: production sent 3,785 prompt tokens to compress "+
			"a 2,700-token candidate at 7", defaultContextMessages)
	}
}

// A class whose measured reduction cannot support a fixed-size window must not be offered one.
// FOUND LIVE on a cold sweep through the proxy: a 6,906-token `grep -n` result came back as its
// first 37 of 158 lines with "22,081 characters elided" — accepted, because marking the cut made
// it honest, and useless, because every line of a grep result is a distinct fact. The economic
// gate had already refused three sibling grep candidates; this one arrived through the
// exploration budget, which is where the gate's verdict does not apply.
func TestFactDenseContentIsNotOfferedTheWindow(t *testing.T) {
	var grep strings.Builder
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&grep, "./components/offload/extract_econ_test.go:%d:func TestSomething%d(t *testing.T) {\n", 100+i, i)
	}
	cls, ratio, ok := contentClass(grep.String())
	if !ok || cls != "grep_output" {
		t.Fatalf("classified as %q (ok=%v), want grep_output", cls, ok)
	}
	if ratio >= minWindowRatio {
		t.Fatalf("grep output measured at %.3f is above the window floor %.2f", ratio, minWindowRatio)
	}
	// With the window withheld the projection cannot shrink, so the extractor refuses instead
	// of returning a truncation. That is the whole behaviour under test.
	cfg := extract.DefaultCfg()
	cfg.Mode, cfg.Rewrite = "deterministic", true
	cfg.MaxChars = 0
	out, _, strat, why := extract.RunExtractionDetail(context.Background(), grep.String(),
		"find the tests", nil, len(grep.String())/4, cfg, nil)
	if strat != "none" || out != "" {
		t.Fatalf("a fact-dense body must not be windowed: strategy=%q out=%d chars", strat, len(out))
	}
	if why == "" {
		t.Fatal("the refusal must carry a reason")
	}
	// And the classes that DO support a window still get one.
	for _, c := range contentClasses {
		switch c.name {
		case "ls_listing", "markdown_doc", "multi_file_bundle", "yaml_config",
			"read_with_line_numbers", "source_code":
			if c.ratio < minWindowRatio {
				t.Errorf("%s (%.3f) should still be allowed a window", c.name, c.ratio)
			}
		case "json_blob", "test_result_log", "grep_output", "ansi_cli_output":
			if c.ratio >= minWindowRatio {
				t.Errorf("%s (%.3f) is fact-dense and must not be windowed", c.name, c.ratio)
			}
		}
	}
}
