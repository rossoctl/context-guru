package dash_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/accountant/test/cases"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
)

// This file plays a cases.Case through the current saving logic (dash.Event.Price) and
// compares the result with the case's expected figures, per request and per thread (testing.md
// §10.4, "Agreement with the current code"). The current figure is computed each time the test
// runs and is never stored in a case.

// fixtureModel has no tokenizer correction, so BilledTokenFactor is 1 and the comparison is in
// the case's own tokens (design §11 question 3 covers the correction).
const fixtureModel = "fixture-model"

// threadKey is the key of the removals a request replays: its session and its thread. A change
// reported on one thread is replayed on that thread only, never on another thread of the session.
func threadKey(s cases.Step) string { return s.Begin.Meta.Session + "\x00" + s.End.Thread.ID }

// Played is the current code's figure for one step of a case, in micro-dollars.
type Played struct {
	Standalone  bool
	BilledMicro int64            // what the request was billed (a ping: what it cost)
	NetMicro    int64            // cg's net saving: baseline - billed, plus the keep-alive credit, minus cg's own spend
	BaseMicro   int64            // what cg says the request would have been billed with no change, its keep-alive and cachesplit credits included
	ByChange    map[string]int64 // cg's per-change figure, keyed by ChangeID
}

func priceOf(r cases.Rate) modelinfo.Price {
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
// first removal (unique). The new model reports a change once and replays it, so Play keeps every
// rewrite it has seen, per thread, and lists all of them on each later request. Whether a removal
// is unique is not decided here: Play asks the live Recorder.MarkUnique, so a case catches any
// scoping that code has.
//
// A ping is a KeepAlive row, a rescued request carries KeepAlivePings, and a summarizer call is
// CGLLMCostUSD. The old code takes that cost as a given from the caller, so Play prices the call
// with the rates of the model the case names.
func Play(t testing.TB, c cases.Case) []Played {
	// The unique-versus-replay split is the live recorder's own: Recorder.MarkUnique, which the proxy
	// calls for every component of every request. Running it here, and not a copy of its rule, is
	// what lets a case catch a scoping difference in it (for example a change that main reported
	// being seen as a replay on another thread).
	rec, err := dash.NewRecorder(dash.Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })

	type change struct {
		id, operator string
		delta        int
	}
	seen := map[string][]change{} // per thread (see threadKey)
	var out []Played

	pings, pingRead := 0, int64(0)
	threads := map[string]bool{} // the threads this case has already sent a request on

	for _, s := range c.Steps {
		if s.IsStandalone() {
			for _, op := range s.Standalone.Ops {
				if op.Kind != accountant.KindSpend || op.Call == nil {
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

		var cgLLM float64
		var llmChange string
		for _, op := range s.Add {
			switch op.Kind {
			case accountant.KindRewrite:
				sess := threadKey(s)
				// the fixtures use 4 bytes a token
				seen[sess] = append(seen[sess], change{op.ChangeID, op.Operator, (op.SizeBefore.Bytes - op.SizeAfter.Bytes) / 4})
			case accountant.KindSpend:
				if op.Call != nil {
					cgLLM += float64(c.ModelRate(op.Call.Model).CallPicos(op.Call.Usage)) / 1e12
					llmChange = op.ChangeID
				}
			}
		}

		// An OpenAI model keeps its name so that cg picks its own provider's keep-alive floor
		// (keepaliveCreditFloorMs); every other case stays on the fixture model.
		model, provider := fixtureModel, ""
		if strings.Contains(s.Begin.Meta.Model, "gpt-") {
			model, provider = s.Begin.Meta.Model, string(bschemas.OpenAI)
		}
		cacheTTL := ""
		if s.End.Facts.TTL == "1h" {
			cacheTTL = apply.TTLEphemeral1h
		}

		price := priceOf(c.StepRate(s))
		at := s.Begin.Meta.At
		e := &dash.Event{
			Model:        model,
			Provider:     provider,
			CacheTTL:     cacheTTL,
			FreshInput:   int64(s.Usage.Input),
			CacheRead:    int64(s.Usage.CacheRead),
			CacheWrite:   int64(s.Usage.CacheWrite),
			CacheWrite1h: int64(s.Usage.CacheWrite1h),
			TokensBefore: cases.Tokens(s.Begin.Client),
			TokensAfter:  cases.Tokens(s.End.Forwarded),
			CGLLMCostUSD: cgLLM,
		}
		// cachesplit: the forwarded system block it split is the stable half, and the block after
		// it is the volatile tail. A session's first request counts as a changed tail.
		var tailHash uint64
		for _, op := range s.Add {
			if op.Kind != accountant.KindRewrite || op.Operator != "context-guru.cachesplit" {
				continue
			}
			i := s.End.Forwarded.Start[1] + op.At.Index
			if i+1 >= len(s.End.Forwarded.Pos) {
				continue
			}
			tailHash = s.End.Forwarded.Pos[i+1].Hash
			e.SplitStableTokens = cases.Tokens(accountant.Body{Pos: s.End.Forwarded.Pos[i : i+1]})
		}
		var ids []string
		for _, ch := range seen[threadKey(s)] {
			gross := max(ch.delta, 0)
			// the content key of a removal is the change's own ID: the operator derives it from the
			// removed content, as the live pipeline's CacheKeys are
			component := strings.TrimPrefix(ch.operator, "context-guru.")
			unique := rec.MarkUnique("t1", component, []string{ch.id}, gross)
			e.Components = append(e.Components, dash.CompRow{
				Component: component, Kind: "rewrite", Acted: true, Mutated: true,
				SavedGross: gross, SavedUnique: unique,
			})
			e.SavedUnique += unique
			ids = append(ids, ch.id)
		}
		ttl, _ := time.ParseDuration(s.End.Facts.TTL)
		// Recency, "seen this thread", "seen this model" and the tail comparison are the live
		// recorder's, per thread: the library's "main" is the live primary thread, the empty id.
		tid := s.End.Thread.ID
		if tid == "main" {
			tid = thread.Primary
		}
		// The first request of a thread forked from another is that conversation's next turn with a
		// changed prefix, not a cold start (thread.Result.Inherits): the recorder measures its gap
		// from the parent. The live code finds the parent by content; a case names it.
		var seenThread, seenModel, tailChanged bool
		var since int64
		key := threadKey(s)
		if parent := s.End.Thread.Parent; parent != "" && !threads[key] {
			if parent == "main" {
				parent = thread.Primary
			}
			seenThread, seenModel, since, tailChanged = rec.ObserveThreadFrom("t1", s.Begin.Meta.Session, tid, parent, s.Begin.Meta.Model, at.UnixMilli(), tailHash)
		} else {
			seenThread, seenModel, since, tailChanged = rec.ObserveThread("t1", s.Begin.Meta.Session, tid, s.Begin.Meta.Model, at.UnixMilli(), tailHash)
		}
		threads[key] = true
		e.TailChanged = tailChanged
		e.SinceLastMs = since
		e.KeepAlivePings = pings
		e.KeepAliveRefreshed = pingRead
		e.AttributeCache(seenThread, seenModel, since, ttl.Milliseconds(), "")
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
		if e.CachesplitSavedUSD > 0 {
			for _, op := range s.Add {
				if op.Operator == "context-guru.cachesplit" {
					p.ByChange[op.ChangeID] += micros(e.CachesplitSavedUSD)
				}
			}
		}
		if cgLLM > 0 {
			p.ByChange[llmChange] -= micros(cgLLM)
		}
		p.NetMicro = p.BaseMicro - p.BilledMicro - micros(cgLLM)
		out = append(out, p)

		pings, pingRead = 0, 0
	}
	return out
}

// expected is the case's expected figure for one step, in micro-dollars.
func expected(c cases.Case, s cases.Step) (net int64, byChange map[string]int64) {
	byChange = map[string]int64{}
	rate := c.StepRate(s)
	for _, e := range s.Expected.Ops {
		picos := rate.Picos(e.Saved) - rate.Picos(e.Caused)
		if e.Call != nil {
			picos -= c.ModelRate(e.Call.Model).CallPicos(e.Call.Usage)
		}
		byChange[e.ChangeID] += cases.RoundMicros(picos)
		net += picos
	}
	for _, e := range s.Expected.Effects { // breaks and rescues are cache effects, not operations
		if !e.Counted() {
			continue
		}
		picos := rate.Picos(e.Saved) - rate.Picos(e.Caused)
		byChange[e.ChangeID] += cases.RoundMicros(picos)
		net += picos
	}
	return cases.RoundMicros(net), byChange
}

// diff is one disagreement. change is the ChangeID it is about, or "" when it is about a step or
// the thread.
type diff struct {
	step   int // -1 for the thread total
	change string
	text   string
}

// Compare lists every disagreement between a case's expected figures and cg's, per step and per
// thread. A difference of one micro-dollar is rounding (testing.md §10.4).
func Compare(c cases.Case, played []Played) (report string, diffs []diff) {
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
			bill := cases.RoundMicros(c.StepRate(s).UsagePicos(*s.Expected.BaselineUsage))
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

// Difference marks a case where the current code is meant to give a different figure (testing.md
// §10.2). The list is context-guru's own, in testdata/accountant/known-differences.json: it compares the shared cases
// with cg code, so it does not belong in the accountant repository.
type Difference struct {
	Case   string `json:"case"`
	Kind   string `json:"kind"`   // "differs" | "notCovered"
	Reason string `json:"reason"` // cites the spec section that causes it
	// Allow lists exactly where the current code may differ: any difference not listed here fails.
	Allow []Allowed `json:"allow"`
	// Total allows the thread total to differ. It follows from the steps, so it is listed once.
	Total bool `json:"total,omitempty"`
}

// Allowed is one place the current code may differ from the case.
type Allowed struct {
	Step int `json:"step"`
	// Change is the ChangeID whose figure differs. Empty means the step's own figures differ: its
	// net saving and its baseline bill.
	Change string `json:"change,omitempty"`
}

func loadDifferences(t testing.TB) map[string]Difference {
	t.Helper()
	b, err := os.ReadFile("testdata/accountant/known-differences.json")
	if err != nil {
		t.Fatal(err)
	}
	var list []Difference
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("testdata/accountant/known-differences.json: %v", err)
	}
	out := map[string]Difference{}
	for _, d := range list {
		out[d.Case] = d
	}
	return out
}

// TestCasesAgreeWithCurrentLogic runs every case through cg. A case that is not in
// known-differences.json must agree; one that is listed must really differ, so a stale entry fails.
func TestCasesAgreeWithCurrentLogic(t *testing.T) {
	differences := loadDifferences(t)
	// An entry whose case no longer exists (the case was renamed or removed in accountant, or the
	// entry has a typo) would otherwise never be visited.
	known := map[string]bool{}
	for _, c := range cases.All() {
		known[c.ID] = true
	}
	for id := range differences {
		if !known[id] {
			t.Errorf("testdata/accountant/known-differences.json lists %q, which is not a case of the accountant module in go.mod", id)
		}
	}
	for _, c := range cases.All() {
		t.Run(c.ID, func(t *testing.T) {
			report, diffs := Compare(c, Play(t, c))
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
