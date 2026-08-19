package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// TestDiagStarlarkOnRealOutput runs the real code-strategy prompt against the real gateway
// on a real captured tool output and prints exactly what came back and what the sandbox did
// with it. It exists because RunExtractionDetail collapses "no reply", "transport error" and
// "the sandbox refused the program" into one reason string.
func TestDiagStarlarkOnRealOutput(t *testing.T) {
	if os.Getenv("CG_DIAG") == "" {
		t.Skip("set CG_DIAG=1")
	}
	body := os.Getenv("CG_DIAG_BODY")
	if body == "" {
		t.Skip("set CG_DIAG_BODY to a file holding one tool output")
	}
	raw, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	m := cheapmodel.Anthropic{
		BaseURL:    os.Getenv("CHEAP_MODEL_BASE"),
		APIKey:     os.Getenv("CHEAP_MODEL_AUTH"),
		Model:      os.Getenv("CHEAP_MODEL"),
		AuthScheme: "bearer",
	}
	sys, user := buildCodePromptSplit(string(raw), "explore the repository", nil, true, false, AggroMedium)
	fmt.Printf("system blocks=%d total=%d chars   user=%d chars\n", len(sys), totalLen(sys), len(user))
	src, err := completeSplit(context.Background(), m, sys, user)
	fmt.Printf("reply err=%v len=%d\n", err, len(src))
	fmt.Printf("---- raw reply ----\n%s\n---- end ----\n", clip(src, 1500))
	stripped := stripFences(src)
	out, summary := execStarlarkSummary(context.Background(), string(raw), stripped)
	fmt.Printf("sandbox: out_len=%d (input %d) summary=%q\n", len(out), len(raw), summary)
	if out == "" {
		fmt.Printf("SANDBOX PRODUCED NOTHING — stripped source was:\n%s\n", clip(stripped, 1200))
	}
	b, _ := json.Marshal(map[string]any{"in": len(raw), "out": len(out)})
	fmt.Println(string(b))
}

func totalLen(s []string) (n int) {
	for _, x := range s {
		n += len(x)
	}
	return
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[clipped]"
}

// --- corpus harness -------------------------------------------------------------
//
// TestDiagCodeLegCorpus runs the CODE leg alone over a directory of real captured tool
// outputs and prints the acceptance rate broken down by rejection slug. It is the only
// honest way to state "the code leg is accepted N of M": the production counter mixes
// strategies, and a single diagnostic call (above) proves nothing about a rate.
//
// Corpus layout: one JSON file per case, {"id":…,"goal":…,"body":…} — bodies captured
// from real Claude Code traffic. Env:
//
//	CG_DIAG=1 CG_DIAG_CORPUS=/tmp/xcorpus  CHEAP_MODEL_BASE/_AUTH/CHEAP_MODEL
//	CG_DIAG_REWRITE=0   run the deletion-only contract instead of the default
//	CG_DIAG_REPAIR=0    disable the one syntax-repair round-trip (cost ablation)
//	CG_DIAG_AGGRO=low|medium|high
func TestDiagCodeLegCorpus(t *testing.T) {
	if os.Getenv("CG_DIAG") == "" || os.Getenv("CG_DIAG_CORPUS") == "" {
		t.Skip("set CG_DIAG=1 CG_DIAG_CORPUS=<dir of {goal,body} json files>")
	}
	files, err := filepath.Glob(filepath.Join(os.Getenv("CG_DIAG_CORPUS"), "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus files: %v", err)
	}
	sort.Strings(files)
	m := cheapmodel.Anthropic{
		BaseURL: os.Getenv("CHEAP_MODEL_BASE"), APIKey: os.Getenv("CHEAP_MODEL_AUTH"),
		Model: os.Getenv("CHEAP_MODEL"), AuthScheme: "bearer",
		MaxTokens: cheapmodel.DefaultMaxTokens,
	}
	cfg := DefaultCfg()
	cfg.Mode, cfg.AllowDeterministic, cfg.AllowedStrategies = "code", false, []string{"code"}
	cfg.Rewrite = os.Getenv("CG_DIAG_REWRITE") != "0"
	cfg.Aggressiveness, _ = ParseAggressiveness(os.Getenv("CG_DIAG_AGGRO"))
	repairSyntax = os.Getenv("CG_DIAG_REPAIR") != "0"
	price := cheapmodel.PricingFromEnv()

	type row struct {
		id            string
		before, after int
		slug, reason  string
		derived       float64
		cost          float64
		calls         int64
		ok            bool
	}
	rows := make([]row, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, f string) {
			defer wg.Done()
			defer func() { <-sem }()
			var c struct{ ID, Goal, Body string }
			raw, err := os.ReadFile(f)
			if err != nil || json.Unmarshal(raw, &c) != nil {
				rows[i] = row{id: filepath.Base(f), slug: "corpus error"}
				return
			}
			keep := HarvestIdentifiers(c.Goal, 40)
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			ctx, sink := cheapmodel.WithCallSink(ctx)
			out, _, strat, why := RunExtractionDetail(ctx, c.Body, c.Goal, keep, tokens.Count(c.Body), cfg, m)
			calls, in, o := sink.Totals()
			cw, cr := sink.CacheTotals()
			rows[i] = row{
				id: c.ID, before: tokens.Count(c.Body), after: tokens.Count(out),
				slug: slugOf(why), reason: why, ok: strat == "code",
				derived: derivationRatio(out, c.Body),
				cost:    price.Cost(in, o, cw, cr), calls: calls,
			}
		}(i, f)
	}
	wg.Wait()

	byslug := map[string]int{}
	var acc, totBefore, totAfter int
	var totCost float64
	var totCalls int64
	for _, r := range rows {
		fmt.Printf("%-14s %6d -> %-6d %-6v derived=%.2f %-26s %s\n",
			r.id, r.before, r.after, r.ok, r.derived, r.slug, clip(r.reason, 90))
		totBefore += r.before
		totCalls += r.calls
		totCost += r.cost
		if r.ok {
			acc++
			totAfter += r.after
			continue
		}
		totAfter += r.before
		byslug[r.slug]++
	}
	fmt.Printf("\nACCEPTED %d/%d (%.0f%%)  model=%s rewrite=%v repair=%v aggro=%s\n",
		acc, len(rows), 100*float64(acc)/float64(len(rows)), m.Model, cfg.Rewrite, repairSyntax, cfg.Aggressiveness)
	fmt.Printf("tokens %d -> %d (removed %d, %.1f%%)  calls=%d cost=$%.4f ($%.4f per accepted)\n",
		totBefore, totAfter, totBefore-totAfter, 100*float64(totBefore-totAfter)/float64(totBefore),
		totCalls, totCost, totCost/float64(max1(acc)))
	tried, fixed := RepairStats()
	fmt.Printf("syntax repair round-trips: %d attempted, %d produced a runnable program\n", tried, fixed)
	fmt.Println("rejections by slug:")
	slugs := make([]string, 0, len(byslug))
	for s := range byslug {
		slugs = append(slugs, s)
	}
	sort.Slice(slugs, func(i, j int) bool { return byslug[slugs[i]] > byslug[slugs[j]] })
	for _, s := range slugs {
		fmt.Printf("  %-24s %d\n", s, byslug[s])
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// slugOf reduces a per-strategy reason list to the code leg's category, so the harness
// counts causes rather than message text.
func slugOf(reason string) string {
	if reason == "" {
		return "accepted"
	}
	r := strings.TrimPrefix(strings.Split(reason, "; ")[0], "code: ")
	if i := strings.Index(r, ":"); i > 0 {
		r = r[:i]
	}
	return r
}
