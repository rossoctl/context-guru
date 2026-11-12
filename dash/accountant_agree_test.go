package dash_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/accountant/test/cases"
	"github.com/rossoctl/context-guru/dash"
)

// This file plays every cases.Case through the path the proxy uses (Trackers, Request.Add of
// every op with replays included, Compute, Event.SetAccountant, Event.AccountantFigure) and
// compares the dollars with the case's expected figures. It lives in package dash_test, with
// the helpers it reuses (priceOfAcc, microsAcc, expectedAcc, absAcc) in accountant_agree_helpers_test.go.

// PlayedAcc is the proxy path's figure for one step, in micro-dollars.
type PlayedAcc struct {
	Standalone bool
	NetMicro   int64
	// ByComponent is AccFigure.ByComponent in micro-dollars: the ops plus the breaks charged to a
	// component. A per-ChangeID figure is not derivable from it, so the comparison is per component.
	ByComponent map[string]int64
	Unpriced    []string
}

// PlayAcc drives the new path over a case. Net = DeltaUSD + KeepAliveSavedUSD + SplitUSD + TTLUSD,
// minus every spend op priced with its own model's rate (the host prices spends).
func PlayAcc(c cases.Case) []PlayedAcc {
	trackers := accountant.NewTrackers()
	var out []PlayedAcc
	for _, s := range c.Steps {
		if s.IsStandalone() {
			_ = accountant.ComputeStandalone(*s.Standalone) // the library returns the Call; the host prices it
			var spend int64
			for _, op := range s.Standalone.Ops {
				if op.Kind == accountant.KindSpend && op.Call != nil {
					spend += c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)
				}
			}
			out = append(out, PlayedAcc{Standalone: true, NetMicro: -cases.RoundMicros(spend), ByComponent: map[string]int64{}})
			continue
		}
		req := trackers.BeginRequest(s.Begin.Meta, s.Begin.Client, s.Begin.ClientBreakpoints)
		var spend int64
		for _, op := range s.Add {
			req.Add(op)
			if op.Kind == accountant.KindSpend && op.Call != nil {
				spend += c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)
			}
		}
		req.EndRequest(s.End.Thread, s.End.Forwarded, s.End.Breakpoints, s.End.Facts.EntryFacts())
		res := req.Compute(s.Usage)
		e := &dash.Event{}
		e.SetAccountant(res)
		fig := e.AccountantFigure(priceOfAcc(c.StepRate(s)))
		p := PlayedAcc{ByComponent: map[string]int64{}, Unpriced: fig.Unpriced}
		for k, v := range fig.ByComponent {
			p.ByComponent[k] = microsAcc(v)
		}
		p.NetMicro = microsAcc(fig.DeltaUSD+fig.KeepAliveSavedUSD+fig.SplitUSD+fig.TTLUSD) - cases.RoundMicros(spend)
		out = append(out, p)
	}
	return out
}

// componentsOf maps every ChangeID a case reports to its component, across all steps (a replayed
// change appears in Expected.Ops of a later step, but only in Add of the step that reported it).
func componentsOf(c cases.Case) map[string]string {
	m := map[string]string{}
	shared := map[string]bool{} // a ChangeID two components report: no per-component split is derivable
	for _, s := range c.Steps {
		ops := s.Add
		if s.Standalone != nil {
			ops = s.Standalone.Ops
		}
		for _, op := range ops {
			if op.Kind == accountant.KindSpend || op.ChangeID == "" {
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
func sharedComp(c cases.Case, comp string) bool {
	byID := map[string]map[string]bool{}
	for _, s := range c.Steps {
		for _, op := range s.Add {
			if op.Kind == accountant.KindSpend || op.ChangeID == "" {
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

// expectedByComponent is the case's expected figure of each component, the way AccFigure.ByComponent
// builds it: the priced rewrite and setting ops (a spend is priced by the host, so it is left out)
// plus the counted breaks whose ChangeID a component owns, plus each mark premium and payback (on the owner of its
// ChangeID, else on the tail breakpoint). Rescue, split and TTL effects are their own AccFigure fields,
// not component figures; a counted break nobody owns is EndedBreakUSD.
func expectedByComponent(c cases.Case, s cases.Step, comps map[string]string) map[string]int64 {
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
		if !e.Counted() {
			continue
		}
		switch e.Kind {
		case accountant.EffectBreak:
			if comp, ok := comps[e.ChangeID]; ok && e.ChangeID != "" {
				picos[comp] += rate.Picos(e.Saved) - rate.Picos(e.Caused)
			}
		case accountant.EffectMarkPremium, accountant.EffectMarkPayback:
			// The premium of a breakpoint the host added, and the payback of it on the next request: a cost
			// and a saving on the component whose span covers the mark, else on the tail breakpoint
			// (AccountantFigure does the same).
			comp, ok := comps[e.ChangeID]
			if !ok || e.ChangeID == "" {
				comp = "tailbreakpoint"
			}
			picos[comp] += rate.Picos(e.Saved) - rate.Picos(e.Caused)
		}
	}
	out := map[string]int64{}
	for k, v := range picos {
		out[k] = cases.RoundMicros(v)
	}
	return out
}

// CompareAcc lists the disagreements: the step net, the thread total, and the figure of each
// component. The legacy baseline bill is not compared: AccFigure has none. A per-ChangeID figure is
// not derivable from AccFigure (it sums by component), so the expected per-ChangeID figures are
// summed to their component, and a ChangeID that names no component (a keep-alive window, a
// spend) is skipped.
func CompareAcc(c cases.Case, played []PlayedAcc) (diffs []diffAcc) {
	comps := componentsOf(c)
	var wantTotal, gotTotal int64
	for i, s := range c.Steps {
		want, _ := expectedAcc(c, s)
		wantComp := expectedByComponent(c, s, comps)
		got := played[i]
		wantTotal += want
		gotTotal += got.NetMicro
		if absAcc(got.NetMicro-want) > 1 {
			diffs = append(diffs, diffAcc{i, "", fmt.Sprintf("step %d: expected %d µ$, proxy path %d µ$", i, want, got.NetMicro)})
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
			if absAcc(got.ByComponent[k]-wantComp[k]) > 1 {
				diffs = append(diffs, diffAcc{i, k, fmt.Sprintf("step %d component %s: expected %d µ$, proxy path %d µ$", i, k, wantComp[k], got.ByComponent[k])})
			}
		}
	}
	if absAcc(gotTotal-wantTotal) > 1 {
		diffs = append(diffs, diffAcc{-1, "", fmt.Sprintf("thread total: expected %d µ$, proxy path %d µ$", wantTotal, gotTotal)})
	}
	return diffs
}

// accPlace is a step and a component ("" for the step net). Step -1 is the thread total.
type accPlace struct {
	step int
	comp string
}

// accLibraryDisagrees are cases where Compute itself returns something other than the case expects.
// They are skipped, with the reason, so the suite stays green and the finding stays visible.
var accLibraryDisagrees = map[string]string{}

// TestProxyPathMatchesTheAccountantCases plays every shared case through the proxy path and wants
// every step net, every component figure and the thread total to match. No difference is allowed.
func TestProxyPathMatchesTheAccountantCases(t *testing.T) {
	for _, c := range cases.All() {
		t.Run(c.ID, func(t *testing.T) {
			if why, ok := accLibraryDisagrees[c.ID]; ok {
				t.Skipf("library disagrees with its own case: %s", why)
			}
			for _, x := range CompareAcc(c, PlayAcc(c)) {
				t.Error(x.text)
			}
		})
	}
}
