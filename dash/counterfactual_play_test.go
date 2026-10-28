package dash_test

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	cf "github.com/rossoctl/context-guru/counterfactual"
	"github.com/rossoctl/context-guru/counterfactual/opscases"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// This file plays an opscases.Case through the current saving logic (dash.Event.Price) and
// compares the result with the case's expected figures, per request and per thread (design.md
// §10.4, "Agreement with the current code"). The current figure is computed each time the test
// runs and is never stored in a case.

// fixtureModel has no tokenizer correction, so BilledTokenFactor is 1 and the comparison is in
// the case's own tokens (design §11 question 3 covers the correction).
const fixtureModel = "fixture-model"

// Played is the current code's figure for one step of a case, in micro-dollars.
type Played struct {
	Standalone  bool
	BilledMicro int64            // what the request was billed (a ping: what it cost)
	NetMicro    int64            // cg's net saving: baseline - billed, plus the keep-alive credit, minus cg's own spend
	BaseMicro   int64            // what cg says the request would have been billed with no change, its keep-alive and cachesplit credits included
	ByChange    map[string]int64 // cg's per-change figure, keyed by ChangeID
}

func priceOf(r opscases.Rate) modelinfo.Price {
	return modelinfo.Price{
		Input:      float64(r.Input) / 1e12,
		CacheWrite: float64(r.CacheWrite5m) / 1e12,
		CacheRead:  float64(r.CacheRead) / 1e12,
		Output:     float64(r.Output) / 1e12,
	}
}

func micros(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

// Play runs a case through dash.Event.Price, one step after another, and returns cg's figure for
// each step.
//
// The old logic wants, on every request, the tokens removed (gross) and how many of them are a
// first removal (unique). The new model reports a change once and replays it, so Play does the
// bookkeeping the old pipeline did: it keeps every rewrite it has seen and lists all of them on
// each later request. The first request of a change is unique; a replay is gross only.
//
// A ping is a KeepAlive row, a rescued request carries KeepAlivePings, and a summarizer call is
// CGLLMCostUSD. The old code takes that cost as a given from the caller, so Play prices the call
// with the rates of the model the case names.
func Play(c opscases.Case) []Played {
	type change struct {
		id, operator string
		delta        int
	}
	var seen []change
	var out []Played

	var prevAt time.Time
	requests := 0
	pings, pingRead := 0, int64(0)

	for _, s := range c.Steps {
		if s.IsStandalone() {
			for _, op := range s.Standalone.Ops {
				if op.Kind != cf.KindSpend || op.Call == nil {
					continue
				}
				price := priceOf(c.ModelRate(op.Call.Model))
				u := op.Call.Usage
				e := &dash.Event{
					Model: fixtureModel, KeepAlive: true,
					FreshInput: int64(u.Input), CacheRead: int64(u.CacheRead), CacheWrite: int64(u.CacheWrite),
					OutputTokens: int64(u.Output),
				}
				e.Price(price, true)
				pings++
				pingRead = e.CacheRead
				p := Played{Standalone: true, BilledMicro: micros(e.CostUSD), NetMicro: -micros(e.CostUSD),
					ByChange: map[string]int64{op.ChangeID: -micros(e.CostUSD)}}
				out = append(out, p)
			}
			continue
		}

		first := map[string]bool{}
		var cgLLM float64
		var llmChange string
		for _, op := range s.Add {
			switch op.Kind {
			case cf.KindRewrite:
				seen = append(seen, change{op.ChangeID, op.Operator, op.SizeBefore.Tokens - op.SizeAfter.Tokens})
				first[op.ChangeID] = true
			case cf.KindSpend:
				if op.Call != nil {
					cgLLM += float64(c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)) / 1e12
					llmChange = op.ChangeID
				}
			}
		}

		price := priceOf(c.StepRate(s))
		at := s.Begin.Meta.At
		e := &dash.Event{
			Model:        fixtureModel,
			FreshInput:   int64(s.Usage.Input),
			CacheRead:    int64(s.Usage.CacheRead),
			CacheWrite:   int64(s.Usage.CacheWrite),
			CacheWrite1h: int64(s.Usage.CacheWrite1h),
			TokensBefore: opscases.Tokens(s.Begin.Client),
			TokensAfter:  opscases.Tokens(s.End.Forwarded),
			CGLLMCostUSD: cgLLM,
		}
		var ids []string
		for _, ch := range seen {
			gross, unique := max(ch.delta, 0), 0
			if first[ch.id] {
				unique = gross
			}
			e.Components = append(e.Components, dash.CompRow{
				Component: strings.TrimPrefix(ch.operator, "context-guru."), Kind: "rewrite", Acted: true, Mutated: true,
				SavedGross: gross, SavedUnique: unique,
			})
			e.SavedUnique += unique
			ids = append(ids, ch.id)
		}
		ttl, _ := time.ParseDuration(s.End.Facts.TTL)
		since := int64(0)
		if requests > 0 {
			since = at.Sub(prevAt).Milliseconds()
		}
		e.SinceLastMs = since
		e.KeepAlivePings = pings
		e.KeepAliveRefreshed = pingRead
		e.AttributeCache(requests > 0, requests > 0, since, ttl.Milliseconds(), "")
		e.Price(price, true)

		// cg books the keep-alive and cachesplit credits in their own columns, outside
		// BaselineCostUSD. Adding them back gives the bill cg says there would have been without
		// any of its levers, which is what a case's baselineUsage states.
		p := Played{BilledMicro: micros(e.CostUSD), ByChange: map[string]int64{},
			BaseMicro: micros(e.BaselineCostUSD + e.KeepAliveSavedUSD + e.CachesplitSavedUSD)}
		for j, comp := range e.Components {
			p.ByChange[ids[j]] += micros(comp.SavedUSD)
		}
		if e.KeepAliveSavedUSD > 0 {
			p.ByChange[s.End.Facts.PingWindow] += micros(e.KeepAliveSavedUSD)
		}
		if cgLLM > 0 {
			p.ByChange[llmChange] -= micros(cgLLM)
		}
		p.NetMicro = p.BaseMicro - p.BilledMicro - micros(cgLLM)
		out = append(out, p)

		prevAt, requests, pings, pingRead = at, requests+1, 0, 0
	}
	return out
}

// expected is the case's expected figure for one step, in micro-dollars.
func expected(c opscases.Case, s opscases.Step) (net int64, byChange map[string]int64) {
	byChange = map[string]int64{}
	rate := c.StepRate(s)
	for _, e := range s.Expected.Ops {
		picos := rate.Picos(e.Saved) - rate.Picos(e.Caused)
		if e.Call != nil {
			picos -= c.ModelRate(e.Call.Model).CallPicos(e.Call.Usage)
		}
		byChange[e.ChangeID] += opscases.RoundMicros(picos)
		net += picos
	}
	for _, e := range s.Expected.Effects { // breaks and rescues are cache effects, not operations
		picos := rate.Picos(e.Saved) - rate.Picos(e.Caused)
		byChange[e.ChangeID] += opscases.RoundMicros(picos)
		net += picos
	}
	return opscases.RoundMicros(net), byChange
}

// diff is one disagreement. change is the ChangeID it is about, or "" when it is about a step or
// the thread.
type diff struct {
	step   int // -1 for the thread total
	change string
	text   string
}

// Compare lists every disagreement between a case's expected figures and cg's, per step and per
// thread. A difference of one micro-dollar is rounding (§10.4).
func Compare(c opscases.Case, played []Played) (report string, diffs []diff) {
	var b strings.Builder
	var wantTotal, gotTotal int64
	fmt.Fprintf(&b, "%-5s %12s %12s %12s\n", "step", "expected µ$", "cg µ$", "difference")
	for i, s := range c.Steps {
		want, wantBy := expected(c, s)
		got := played[i]
		wantTotal += want
		gotTotal += got.NetMicro
		fmt.Fprintf(&b, "%-5d %12d %12d %12d\n", i, want, got.NetMicro, got.NetMicro-want)
		if abs(got.NetMicro-want) > 1 {
			diffs = append(diffs, diff{i, "", fmt.Sprintf("step %d: expected %d µ$, cg %d µ$", i, want, got.NetMicro)})
		}
		if !got.Standalone && s.Expected.BaselineUsage != nil {
			bill := opscases.RoundMicros(c.StepRate(s).UsagePicos(*s.Expected.BaselineUsage))
			if abs(bill-got.BaseMicro) > 1 {
				diffs = append(diffs, diff{i, "", fmt.Sprintf("step %d: baseline bill expected %d µ$, cg %d µ$", i, bill, got.BaseMicro)})
			}
		}
		ids := map[string]bool{}
		for id := range wantBy {
			ids[id] = true
		}
		for id := range got.ByChange {
			ids[id] = true
		}
		var sorted []string
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		for _, id := range sorted {
			if abs(got.ByChange[id]-wantBy[id]) > 1 {
				diffs = append(diffs, diff{i, id, fmt.Sprintf("step %d change %s: expected %d µ$, cg %d µ$", i, id, wantBy[id], got.ByChange[id])})
			}
		}
	}
	fmt.Fprintf(&b, "%-5s %12d %12d %12d\n", "sum", wantTotal, gotTotal, gotTotal-wantTotal)
	if abs(gotTotal-wantTotal) > 1 {
		diffs = append(diffs, diff{-1, "", fmt.Sprintf("thread total: expected %d µ$, cg %d µ$", wantTotal, gotTotal)})
	}
	return b.String(), diffs
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestCasesAgreeWithCurrentLogic runs every case through cg. A case that is not in
// differences.json must agree; one that is listed must really differ, so a stale entry fails.
func TestCasesAgreeWithCurrentLogic(t *testing.T) {
	differences := opscases.Differences()
	for _, c := range opscases.All() {
		t.Run(c.ID, func(t *testing.T) {
			report, diffs := Compare(c, Play(c))
			t.Logf("%s\n%s", c.Description, report)
			d, listed := differences[c.ID]
			if !listed {
				for _, x := range diffs {
					t.Error(x.text)
				}
				return
			}
			// A listed case must really differ, so a stale entry fails. Only the places the entry
			// lists may differ: any other difference still fails.
			type place struct {
				step   int
				change string
			}
			allowed := map[place]bool{}
			for _, a := range d.Allow {
				allowed[place{a.Step, a.Change}] = true
			}
			used := map[place]bool{}
			for _, x := range diffs {
				pl := place{x.step, x.change}
				switch {
				case x.step == -1 && d.Total, allowed[pl]:
					used[pl] = true
					t.Logf("%s: %s", d.Kind, x.text)
				default:
					t.Error(x.text)
				}
			}
			for pl := range allowed {
				if !used[pl] {
					t.Errorf("%s allows step %d %q to differ but cg agrees there: remove it", c.ID, pl.step, pl.change)
				}
			}
			if len(diffs) == 0 {
				t.Errorf("listed as %q (%s) but cg agrees: remove the entry", d.Kind, d.Reason)
			}
		})
	}
}
