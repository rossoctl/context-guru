package dash_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/counterfactual/opscases"
	"github.com/rossoctl/context-guru/dash"
)

// This file plays every opscases.Case through the path the proxy uses (Trackers, the shared replay
// Request.Add (every op, replays included), Compute, Event.SetCounterfactual, Event.CounterfactualFigure) and
// compares the dollars with the case's expected figures. It lives in package dash_test, beside
// Play, because the helpers it reuses (priceOf, micros, expected, abs) are in that package.

// PlayedCF is the proxy path's figure for one step, in micro-dollars.
type PlayedCF struct {
	Standalone bool
	NetMicro   int64
	// ByComponent is CFFigure.ByComponent in micro-dollars: the ops plus the breaks charged to a
	// component. A per-ChangeID figure is not derivable from it, so the comparison is per component.
	ByComponent map[string]int64
	Unattrib    int64 // the unattributed break, in µ$ (a cost, positive): reported, not in NetMicro
	Unpriced    []string
}

// PlayCF drives the new path over a case. Net = DeltaUSD + KeepAliveSavedUSD + SplitUSD + TTLUSD,
// minus every spend op priced with its own model's rate (the host prices spends).
func PlayCF(c opscases.Case) []PlayedCF {
	trackers := cf.NewTrackers()
	var out []PlayedCF
	for _, s := range c.Steps {
		if s.IsStandalone() {
			_ = cf.ComputeStandalone(*s.Standalone) // the library returns the Call; the host prices it
			var spend int64
			for _, op := range s.Standalone.Ops {
				if op.Kind == cf.KindSpend && op.Call != nil {
					spend += c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)
				}
			}
			out = append(out, PlayedCF{Standalone: true, NetMicro: -opscases.RoundMicros(spend), ByComponent: map[string]int64{}})
			continue
		}
		req := trackers.BeginRequest(s.Begin.Meta, s.Begin.Client, s.Begin.ClientBreakpoints)
		var spend int64
		for _, op := range s.Add {
			req.Add(op)
			if op.Kind == cf.KindSpend && op.Call != nil {
				spend += c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)
			}
		}
		req.EndRequest(s.End.Thread, s.End.Forwarded, s.End.Breakpoints, s.End.Facts.EntryFacts())
		res := req.Compute(s.Usage)
		e := &dash.Event{}
		e.SetCounterfactual(res)
		fig := e.CounterfactualFigure(priceOf(c.StepRate(s)))
		p := PlayedCF{ByComponent: map[string]int64{}, Unattrib: micros(fig.UnattributedBreakUSD), Unpriced: fig.Unpriced}
		for k, v := range fig.ByComponent {
			p.ByComponent[k] = micros(v)
		}
		p.NetMicro = micros(fig.DeltaUSD+fig.KeepAliveSavedUSD+fig.SplitUSD+fig.TTLUSD) - opscases.RoundMicros(spend)
		out = append(out, p)
	}
	return out
}

// componentsOf maps every ChangeID a case reports to its component, across all steps (a replayed
// change appears in Expected.Ops of a later step, but only in Add of the step that reported it).
func componentsOf(c opscases.Case) map[string]string {
	m := map[string]string{}
	shared := map[string]bool{} // a ChangeID two components report: no per-component split is derivable
	for _, s := range c.Steps {
		ops := s.Add
		if s.Standalone != nil {
			ops = s.Standalone.Ops
		}
		for _, op := range ops {
			if op.Kind == cf.KindSpend || op.ChangeID == "" {
				continue
			}
			comp := strings.TrimPrefix(op.Operator, "context-guru.")
			if prev, ok := m[op.ChangeID]; ok && prev != comp {
				shared[op.ChangeID] = true
			}
			if _, ok := m[op.ChangeID]; !ok {
				m[op.ChangeID] = comp
			}
		}
	}
	for id := range shared {
		delete(m, id)
	}
	return m
}

// sharedComp reports whether the component reports a ChangeID that another component reports too,
// so its figure cannot be told from the other's by ChangeID (check/changeid-shared-between-operators).
func sharedComp(c opscases.Case, comp string) bool {
	byID := map[string]map[string]bool{}
	for _, s := range c.Steps {
		for _, op := range s.Add {
			if op.Kind == cf.KindSpend || op.ChangeID == "" {
				continue
			}
			if byID[op.ChangeID] == nil {
				byID[op.ChangeID] = map[string]bool{}
			}
			byID[op.ChangeID][strings.TrimPrefix(op.Operator, "context-guru.")] = true
		}
	}
	for _, set := range byID {
		if len(set) > 1 && set[comp] {
			return true
		}
	}
	return false
}

// expectedByComponent is the case's expected figure of each component, the way CFFigure.ByComponent
// builds it: the priced rewrite and setting ops (a spend is priced by the host, so it is left out)
// plus the counted breaks whose ChangeID a component owns. Rescue, split and TTL effects are their
// own CFFigure fields, not component figures; a break nobody owns is UnattributedBreakUSD.
func expectedByComponent(c opscases.Case, s opscases.Step, comps map[string]string) map[string]int64 {
	picos := map[string]int64{}
	rate := c.StepRate(s)
	for _, e := range s.Expected.Ops {
		if e.Call != nil {
			continue
		}
		if comp, ok := comps[e.ChangeID]; ok {
			picos[comp] += rate.Picos(e.Saved) - rate.Picos(e.Caused)
		}
	}
	for _, e := range s.Expected.Effects {
		if !e.Counted() || e.Kind != cf.EffectBreak {
			continue
		}
		if comp, ok := comps[e.ChangeID]; ok && e.ChangeID != "" {
			picos[comp] += rate.Picos(e.Saved) - rate.Picos(e.Caused)
		}
	}
	out := map[string]int64{}
	for k, v := range picos {
		out[k] = opscases.RoundMicros(v)
	}
	return out
}

// CompareCF lists the disagreements: the step net, the thread total, and the figure of each
// component. The legacy baseline bill is not compared: CFFigure has none. A per-ChangeID figure is
// not derivable from CFFigure (it sums by component), so the expected per-ChangeID figures are
// summed to their component, and a ChangeID that names no component (a keep-alive window, a
// spend) is skipped.
func CompareCF(c opscases.Case, played []PlayedCF) (diffs []diff) {
	comps := componentsOf(c)
	var wantTotal, gotTotal int64
	for i, s := range c.Steps {
		want, _ := expected(c, s)
		wantComp := expectedByComponent(c, s, comps)
		got := played[i]
		wantTotal += want
		gotTotal += got.NetMicro
		if abs(got.NetMicro-want) > 1 {
			diffs = append(diffs, diff{i, "", fmt.Sprintf("step %d: expected %d µ$, proxy path %d µ$", i, want, got.NetMicro)})
		}
		names := map[string]bool{}
		for k := range wantComp {
			names[k] = true
		}
		for k := range got.ByComponent {
			names[k] = true
		}
		var sorted []string
		for k := range names {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			if sharedComp(c, k) {
				continue // see componentsOf
			}
			if abs(got.ByComponent[k]-wantComp[k]) > 1 {
				diffs = append(diffs, diff{i, k, fmt.Sprintf("step %d component %s: expected %d µ$, proxy path %d µ$", i, k, wantComp[k], got.ByComponent[k])})
			}
		}
	}
	if abs(gotTotal-wantTotal) > 1 {
		diffs = append(diffs, diff{-1, "", fmt.Sprintf("thread total: expected %d µ$, proxy path %d µ$", wantTotal, gotTotal)})
	}
	return diffs
}

// cfPlace is a step and a component ("" for the step net). Step -1 is the thread total.
type cfPlace struct {
	step int
	comp string
}

// cfKnownDifferences are the places where CounterfactualFigure deliberately differs from a case.
// The test tolerates exactly these places and fails if one stops differing.
var cfKnownDifferences = map[string]struct {
	Reason string
	At     []cfPlace
	// Unattrib asserts that the step net differs by exactly the unattributed break CFFigure reports
	// (a cost the case counts in its net and CFFigure shows apart): the steps listed here.
	Unattrib []int
}{
	"rewrite/decision-flips-inside-prefix/hot/5m": {
		Reason:   "a break with no ChangeID is UnattributedBreakUSD, outside the net (cfprice.go: ownerless breaks are shown, never charged)",
		At:       []cfPlace{{3, ""}},
		Unattrib: []int{3},
	},
	"rewrite/restore-in-place/hot/5m": {
		Reason:   "a break with no ChangeID is UnattributedBreakUSD, outside the net (cfprice.go: ownerless breaks are shown, never charged)",
		At:       []cfPlace{{2, ""}},
		Unattrib: []int{2},
	},
	"setting/nil-position/penalty-unknown": {
		Reason:   "the setting is unpriced, so it owns nothing and its break is UnattributedBreakUSD (an unpriced op is not a zero saving)",
		At:       []cfPlace{{1, ""}, {1, "inference-router"}},
		Unattrib: []int{1},
	},
}

// cfLibraryDisagrees are cases where Compute itself returns something other than the case expects.
// They are skipped, with the reason, so the suite stays green and the finding stays visible.
var cfLibraryDisagrees = map[string]string{}

func TestProxyPathMatchesOpscases(t *testing.T) {
	for _, c := range opscases.All() {
		t.Run(c.ID, func(t *testing.T) {
			diffs := CompareCF(c, PlayCF(c))
			if why, ok := cfLibraryDisagrees[c.ID]; ok {
				t.Skipf("library disagrees with its own case: %s", why)
			}
			known, listed := cfKnownDifferences[c.ID]
			allowed := map[cfPlace]bool{}
			for _, p := range known.At {
				allowed[cfPlace{p.step, p.comp}] = true
			}
			played := PlayCF(c)
			// Where a case is listed, the gap at each Unattrib step must be exactly the unattributed
			// break CFFigure reports, and the thread total may differ only by the sum of those.
			var wantTotalGap int64
			for _, st := range known.Unattrib {
				want, _ := expected(c, c.Steps[st])
				gap := played[st].NetMicro - want
				if gap == 0 || abs(gap) <= 1 && played[st].Unattrib == 0 {
					t.Errorf("step %d is listed as unattributed but the net agrees: remove it", st)
				} else if abs(gap-played[st].Unattrib) > 1 {
					t.Errorf("step %d: net gap %d µ$ is not the unattributed break %d µ$", st, gap, played[st].Unattrib)
				}
				wantTotalGap += played[st].Unattrib
			}
			used := map[cfPlace]bool{}
			for _, x := range diffs {
				pl := cfPlace{x.step, x.change}
				if listed && (allowed[pl] || x.step == -1 && len(known.Unattrib) > 0) {
					used[pl] = true
					t.Logf("known difference (%s): %s", known.Reason, x.text)
					continue
				}
				t.Error(x.text)
			}
			if listed && len(known.Unattrib) > 0 {
				var wantT, gotT int64
				for i, st := range c.Steps {
					w, _ := expected(c, st)
					wantT += w
					gotT += played[i].NetMicro
				}
				if abs((gotT-wantT)-wantTotalGap) > 1 {
					t.Errorf("thread total gap %d µ$ is not the unattributed sum %d µ$", gotT-wantT, wantTotalGap)
				}
			}
			for pl := range allowed {
				if !used[pl] {
					t.Errorf("%s allows step %d %q to differ but it agrees: remove it", c.ID, pl.step, pl.comp)
				}
			}
			if listed && len(diffs) == 0 {
				t.Errorf("listed as a known difference (%s) but it agrees: remove the entry", known.Reason)
			}
		})
	}
}
