package opscases

import (
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

// computeImplemented flips to true when Compute prices. Until then the cases are specifications
// that are checked for shape only. Flip it and every case must pass.
const computeImplemented = true

func TestCasesAreWellFormed(t *testing.T) {
	cases := All()
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.ID] {
			t.Errorf("duplicate id %q", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Description) == "" {
			t.Errorf("%s: no description", c.ID)
		}
		if _, ok := Rates[c.Rates]; !ok {
			t.Errorf("%s: unknown rates %q", c.ID, c.Rates)
		}
		if len(c.Steps) == 0 {
			t.Errorf("%s: no steps", c.ID)
		}
		for i, s := range c.Steps {
			checkStep(t, c, i, s)
		}
	}
}

func checkStep(t *testing.T, c Case, i int, s Step) {
	t.Helper()
	at := func(msg string, a ...any) { t.Errorf("%s step %d: "+msg, append([]any{c.ID, i}, a...)...) }

	for _, op := range s.Add {
		checkOp(at, op)
	}
	for _, e := range s.Expected.Ops {
		for _, tt := range []cf.TierTokens{e.Saved, e.Caused} {
			if tt.Input < 0 || tt.CacheRead < 0 || tt.CacheWrite5m < 0 || tt.CacheWrite1h < 0 {
				at("negative tokens for %q (§6.3)", e.ChangeID)
			}
		}
	}
	for _, e := range s.Expected.Effects {
		for _, tt := range []cf.TierTokens{e.Saved, e.Caused} {
			if tt.Input < 0 || tt.CacheRead < 0 || tt.CacheWrite5m < 0 || tt.CacheWrite1h < 0 {
				at("negative tokens for %s effect %q (§6.3)", e.Kind, e.ChangeID)
			}
		}
		if e.Kind == cf.EffectRescue && e.ChangeID == "" {
			at("a rescue has no changeId: it belongs to a keep-alive window")
		}
	}
	if s.IsStandalone() {
		for _, op := range s.Standalone.Ops {
			if op.Kind != cf.KindSpend {
				at("a standalone RequestOps holds only spend operations, got %q", op.Kind)
			}
		}
		return
	}
	if s.Begin == nil || s.End == nil {
		at("a request step needs begin and end")
		return
	}
	if s.Usage.CacheWrite1h > s.Usage.CacheWrite {
		at("CacheWrite1h %d exceeds CacheWrite %d: it is a part of it, never an addition", s.Usage.CacheWrite1h, s.Usage.CacheWrite)
	}
	if s.End.Facts.TTL == "" {
		at("facts have no ttl")
	} else if _, err := time.ParseDuration(s.End.Facts.TTL); err != nil {
		at("facts ttl: %v", err)
	}

	// The billed prompt tokens are the forwarded body's tokens: input + read + write.
	billed := s.Usage.Input + s.Usage.CacheRead + s.Usage.CacheWrite
	if fwd := Tokens(s.End.Forwarded); billed != fwd {
		at("billed %d tokens but the forwarded body holds %d (fixtures use 4 bytes/token)", billed, fwd)
	}
	// The baseline is the client body's tokens.
	if s.Expected.BaselineUsage == nil {
		at("no baselineUsage")
		return
	}
	b := *s.Expected.BaselineUsage
	if client := Tokens(s.Begin.Client); b.Input+b.CacheRead+b.CacheWrite != client {
		at("baseline %d tokens but the client body holds %d", b.Input+b.CacheRead+b.CacheWrite, client)
	}

	// Conservation (§6.3, check 6): baseline bill - billed bill = sum of Saved - Caused over the
	// content changes, breaks and state changes. A spend is not part of the bill difference.
	rate := c.StepRate(s)
	var net int64
	for _, e := range s.Expected.Ops {
		if e.Call != nil {
			continue
		}
		net += rate.Picos(e.Saved) - rate.Picos(e.Caused)
	}
	for _, e := range s.Expected.Effects {
		if e.Counted() {
			net += rate.Picos(e.Saved) - rate.Picos(e.Caused)
		}
	}
	if slices.Contains(s.Expected.Checks, "conservation") {
		return // the case states that this step fails the check, so the sums differ on purpose
	}
	if got := rate.UsagePicos(b) - rate.UsagePicos(s.Usage); got != net {
		at("baseline minus billed is %d pico-dollars, but the expected ops net %d", got, net)
	}
}

func checkOp(at func(string, ...any), op cf.Operation) {
	if op.Kind == cf.KindRewrite && op.ChangeID == "" {
		at("rewrite %q has no changeId", op.ID)
	}
	if len(op.Spans) > 0 { // §6.5 check 4
		var b, a int
		for _, s := range op.Spans {
			b += s.SizeBefore.Bytes
			a += s.SizeAfter.Bytes
		}
		if b != op.SizeBefore.Bytes || a != op.SizeAfter.Bytes {
			at("op %q: spans sum to %d/%d, op says %d/%d", op.ID, b, a, op.SizeBefore.Bytes, op.SizeAfter.Bytes)
		}
	}
}

func TestComputeMeetsEveryCase(t *testing.T) {
	if !computeImplemented {
		t.Skip("Compute is not implemented; see counterfactual/compute.go")
	}
	for _, c := range All() {
		t.Run(c.ID, func(t *testing.T) {
			ts := cf.NewTrackers()
			for i, s := range c.Steps {
				var res cf.Result
				if s.IsStandalone() {
					res = cf.ComputeStandalone(*s.Standalone)
				} else {
					r := ts.BeginRequest(s.Begin.Meta, s.Begin.Client, s.Begin.ClientBreakpoints)
					for _, op := range s.Add {
						r.Add(op)
					}
					r.EndRequest(s.End.Thread, s.End.Forwarded, s.End.Breakpoints, s.End.Facts.EntryFacts())
					res = r.Compute(s.Usage)
				}
				compare(t, i, s.Expected, res)
			}
		})
	}
}

func compare(t *testing.T, i int, want Expected, got cf.Result) {
	t.Helper()
	if want.Phase != "" && got.Phase != want.Phase {
		t.Errorf("step %d: phase %q, want %q", i, got.Phase, want.Phase)
	}
	if want.Boundary != nil && (got.Boundary == nil || *got.Boundary != *want.Boundary) {
		t.Errorf("step %d: boundary %v, want %v", i, got.Boundary, *want.Boundary)
	}
	var priced []cf.PricedOp
	for _, p := range got.Ops {
		if p.Saved != (cf.TierTokens{}) || p.Caused != (cf.TierTokens{}) || p.Call != nil {
			priced = append(priced, p)
		}
	}
	if len(priced) != len(want.Ops) {
		t.Fatalf("step %d: %d priced ops, want %d", i, len(priced), len(want.Ops))
	}
	for j, w := range want.Ops {
		p := priced[j]
		if p.ChangeID != w.ChangeID || p.Saved != w.Saved || p.Caused != w.Caused {
			t.Errorf("step %d op %d: got %s saved=%+v caused=%+v, want %s saved=%+v caused=%+v",
				i, j, p.ChangeID, p.Saved, p.Caused, w.ChangeID, w.Saved, w.Caused)
		}
		if (w.Call == nil) != (p.Call == nil) {
			t.Errorf("step %d op %d: call presence differs", i, j)
		}
		if p.Estimated != w.Estimated {
			t.Errorf("step %d op %d: estimated %v, want %v", i, j, p.Estimated, w.Estimated)
		}
	}
	if len(got.CacheEffects) != len(want.Effects) {
		t.Fatalf("step %d: %d cache effects, want %d: %+v", i, len(got.CacheEffects), len(want.Effects), got.CacheEffects)
	}
	for j, w := range want.Effects {
		g := got.CacheEffects[j]
		if g.Kind != w.Kind || g.ChangeID != w.ChangeID || g.Saved != w.Saved || g.Caused != w.Caused ||
			(w.TTL != "" && g.TTL != w.TTL) {
			t.Errorf("step %d effect %d: got %s %s saved=%+v caused=%+v, want %s %s saved=%+v caused=%+v",
				i, j, g.Kind, g.ChangeID, g.Saved, g.Caused, w.Kind, w.ChangeID, w.Saved, w.Caused)
		}
		if w.From != nil && (g.From == nil || *g.From != *w.From) {
			t.Errorf("step %d effect %d: from %v, want %v", i, j, g.From, *w.From)
		}
		if w.To != nil && (g.To == nil || *g.To != *w.To) {
			t.Errorf("step %d effect %d: to %v, want %v", i, j, g.To, *w.To)
		}
	}
	var failed []string
	for _, c := range got.Checks {
		failed = append(failed, c.Check)
	}
	sort.Strings(failed)
	wantChecks := append([]string(nil), want.Checks...)
	sort.Strings(wantChecks)
	if strings.Join(failed, ",") != strings.Join(wantChecks, ",") {
		t.Errorf("step %d: failed checks %v, want %v", i, failed, wantChecks)
	}
	reasons := map[string]bool{}
	for _, u := range got.Unpriced {
		reasons[u.OpID+"|"+u.Reason] = true
	}
	for _, w := range want.Unpriced {
		if !reasons[w.OpID+"|"+w.Reason] {
			t.Errorf("step %d: missing unpriced %q for op %q", i, w.Reason, w.OpID)
		}
	}
}

// TestExamplesPriceAsDocumented pins the dollar figures of examples.md to the cases, so a case
// cannot drift from the document it came from. Each figure is in micro-dollars, rounded once at
// the end (§10.2, "Rounding").
func TestExamplesPriceAsDocumented(t *testing.T) {
	byID := map[string]Case{}
	for _, c := range All() {
		byID[c.ID] = c
	}
	total := func(id string) int64 {
		c, ok := byID[id]
		if !ok {
			t.Fatalf("no case %q", id)
		}
		var sum int64
		for _, s := range c.Steps {
			sum += c.StepPicos(s)
		}
		return RoundMicros(sum)
	}
	for _, w := range []struct {
		id     string
		micros int64
		doc    string
	}{
		{"rewrite/single/after-bstar/hit/5m", 26_250, "example 1: $0.02625"},
		{"rewrite/replay/hot-hot-cold/5m", 54_600, "example 2: $0.05460"},
		{"rewrite/tools-spans/first-hot-cold/5m", 8_000*3_750/1000 + 8_000*300/1000 + 8_000*3_750/1000, "example 3: $0.03000 + $0.00240 + $0.03000"},
		{"rewrite+spend/summary/break-then-payback/5m", 16_750, "example 4: +$0.01675 after three requests, call included"},
		{"keepalive/ping-rescue/exact/5m", 31_455, "example 8: +$0.03146 (the ping costs $0.003045)"},
	} {
		if got := total(w.id); got != w.micros {
			t.Errorf("%s: %s: total = %d µ$, want %d", w.id, w.doc, got, w.micros)
		}
	}
	// Example 7: coref on turn 7 nets -$0.05960 by itself.
	c := byID["rewrite/coref-break-inside-prefix/hot/5m"]
	var coref int64
	last := c.Steps[len(c.Steps)-1]
	for _, e := range last.Expected.Ops {
		if e.ChangeID == "cg:coref:5e2d" {
			coref += c.RateTable().Picos(e.Saved) - c.RateTable().Picos(e.Caused)
		}
	}
	for _, e := range last.Expected.Effects { // the break is a cache effect, not an operation
		if e.ChangeID == "cg:coref:5e2d" {
			coref += c.RateTable().Picos(e.Saved) - c.RateTable().Picos(e.Caused)
		}
	}
	if got := roundDoc(coref); got != -59_600 {
		t.Errorf("example 7: coref nets %d µ$ at the document's rounding, want -59600 (-$0.05960)", got)
	}
	// Example 6: the four operations net $0.03012 on turn 5.
	c = byID["rewrite/four-operations/hot/5m"]
	if got := roundDoc(c.StepPicos(c.Steps[len(c.Steps)-1])); got != 30_120 {
		t.Errorf("example 6: turn 5 nets %d µ$ at the document's rounding, want 30120 ($0.03012)", got)
	}
	for _, c := range byID {
		if !strings.Contains(c.Source, "examples.md") && !strings.Contains(c.Source, "issue_cases.md") && !strings.Contains(c.Source, "advanced-offload.md") {
			t.Errorf("%s: source %q does not name its document", c.ID, c.Source)
		}
	}
}

// roundDoc rounds pico-dollars to $0.00001 (10 µ$), half up: the rounding examples.md prints.
func roundDoc(picos int64) int64 {
	n := picos + 5_000_000
	q := n / 10_000_000
	if n%10_000_000 < 0 {
		q--
	}
	return q * 10
}

// TestClientSizeFollowsFromResult pins the identity the summarizer size gate needs (issue #555): the
// client body's tokens are the billed tokens plus the tokens the operations saved minus the tokens
// they caused. Cache effects move tokens between tiers and cancel, and a spend adds none.
func TestClientSizeFollowsFromResult(t *testing.T) {
	for _, c := range All() {
		for i, s := range c.Steps {
			if s.IsStandalone() || slices.Contains(s.Expected.Checks, "conservation") {
				continue
			}
			var saved, caused int
			for _, e := range s.Expected.Ops {
				saved += e.Saved.Input + e.Saved.CacheRead + e.Saved.CacheWrite5m + e.Saved.CacheWrite1h
				caused += e.Caused.Input + e.Caused.CacheRead + e.Caused.CacheWrite5m + e.Caused.CacheWrite1h
			}
			billed := s.Usage.Input + s.Usage.CacheRead + s.Usage.CacheWrite
			if client := Tokens(s.Begin.Client); client != billed+saved-caused {
				t.Errorf("%s step %d: client %d tokens, but billed %d + saved %d - caused %d = %d",
					c.ID, i, client, billed, saved, caused, billed+saved-caused)
			}
		}
	}
}
