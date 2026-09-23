package offload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// THE QUESTION THIS ANSWERS, and why it is a test rather than a command: the econ gate's terms
// (prefixRewritePaysWith, turnsRemainingAfter) are unexported, so anything outside this package would
// have to RE-IMPLEMENT the arithmetic and would then agree with the copy rather than with the code that
// ships. That is the trap `cmd/cg-selarm`'s own header warns about. A test in-package calls the real
// functions.
//
// It reads a decision-point corpus and reports, PER CONTEXT WINDOW, how often the gate would authorise
// an ask and how much removable mass sits behind the ones it authorises. No model calls, no spend.
//
// Skipped unless CG_ECON_SIM_CORPUS points at a batches.jsonl, so it never runs in CI.
type simCandidate struct {
	Tokens           int    `json:"tokens"`
	MsgIdx           int    `json:"msg_idx"`
	FutureReferenced bool   `json:"future_referenced"`
	IndexVerdict     string `json:"index_verdict"`
}

type simBatch struct {
	Conv         string         `json:"conv"`
	DecisionAt   int            `json:"decision_at"`
	PrefixTokens int            `json:"prefix_tokens"`
	Candidates   []simCandidate `json:"candidates"`
	Transcript   []struct {
		I    int    `json:"i"`
		Role string `json:"role"`
		Text string `json:"text"`
	} `json:"transcript"`
}

// simReq rebuilds a request whose SHAPE drives every term the gate reads: MessagesTokens for reqNow,
// modelTurns (assistant messages only) for the growth rate, and per-message text for the cache-write
// span. Roles are carried through from the corpus rather than synthesised, because modelTurns counts
// assistant messages and a transcript rebuilt as all-user would report a growth rate of zero and make
// every horizon infinite.
func simReq(b simBatch) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{}
	for _, m := range b.Transcript {
		switch m.Role {
		case "assistant":
			req.Input = append(req.Input, asstMsg(m.Text, "bash", `{}`))
		case "tool":
			req.Input = append(req.Input, toolResultMsg(m.Text))
		default:
			req.Input = append(req.Input, userMsg(m.Text))
		}
	}
	return req
}

func TestEconGatePassRateByBand(t *testing.T) {
	path := os.Getenv("CG_ECON_SIM_CORPUS")
	if path == "" {
		t.Skip("set CG_ECON_SIM_CORPUS to a batches.jsonl to run the band simulation")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	var batches []simBatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var b simBatch
		if err := dec.Decode(&b); err != nil {
			break
		}
		if len(b.Candidates) > 0 && len(b.Transcript) > 0 {
			batches = append(batches, b)
		}
	}
	if len(batches) == 0 {
		t.Fatal("no usable decision points in corpus")
	}

	// SCALE CHECK FIRST, because every band comparison below is a ratio against the window and a
	// mis-scaled request would move every number without looking wrong. The corpus records
	// prefix_tokens from a chars/4 proxy; the gate reads schema.MessagesTokens, the real tokenizer. If
	// the two disagree by more than a little, the bands are not the bands they claim to be.
	var proxySum, realSum int
	for _, b := range batches {
		proxySum += b.PrefixTokens
		realSum += schema.MessagesTokens(simReq(b))
	}
	t.Logf("SCALE: corpus prefix_tokens sum %d | rebuilt MessagesTokens sum %d | ratio %.3f",
		proxySum, realSum, float64(realSum)/float64(proxySum))
	if realSum == 0 {
		t.Fatal("rebuilt requests carry no tokens: the transcript did not survive simReq")
	}

	// THE TWO APPROVAL VALUES ARE THE POINT OF THE SWEEP OF THEM. 1.0 is the warm-up prior the ledger
	// uses for its first minAskSamples asks; approvalFloor is what it reports once three thin asks have
	// been averaged. The gap between the two columns IS the #230 defect, priced.
	type arm struct {
		name     string
		approval float64
		premium  float64
		credit   bool
	}
	arms := []arm{
		{"prior approval=1.0, premium 20", 1.0, 20, true},
		{"floored approval=0.05, premium 20", approvalFloor, 20, true},
		{"floored approval=0.05, premium 1", approvalFloor, 1, true},
		{"floored, premium 20, NO credit", approvalFloor, 20, false},
		// THE ARM MY FIRST DESIGN MISSED. creditRemoval subtracts `expected = saved x approval`, so at a
		// floored approval it cannot matter by construction and "the credit is inert" would be a
		// conclusion about the floor, not about the credit. #232's claim has to be tested where the
		// credit has something to subtract.
		{"prior approval=1.0, premium 20, NO credit", 1.0, 20, false},
	}
	bands := []int{32000, 64000, 128000, 200000, 256000}

	for _, a := range arms {
		t.Logf("=== %s (askUSD from askCostPrior, per request)", a.name)
		t.Logf("%-9s %6s %7s %9s %9s %11s %11s", "window", "pts", "pass%", "meanInv", "meanCand", "passMass", "passDead")
		for _, w := range bands {
			var pts, passed, invSum, candSum int
			var passMass, passDead int
			for _, b := range batches {
				c := &components.Ctx{Ctx: context.Background(), CtxWindow: w}
				req := simReq(b)
				// saved is the WHOLE inventory mass and shallowest the earliest candidate index —
				// exactly what priceBatch passes.
				saved, shallowest := 0, len(req.Input)
				dead := 0
				for _, cd := range b.Candidates {
					saved += cd.Tokens
					if cd.MsgIdx < shallowest {
						shallowest = cd.MsgIdx
					}
					if !cd.FutureReferenced {
						dead += cd.Tokens
					}
				}
				if saved <= 0 || shallowest >= len(req.Input) {
					continue
				}
				pts++
				pricing := rewritePricing{askUSD: askCostPrior(req, c), approval: a.approval,
					premium: a.premium, creditRemoval: a.credit}
				_, _, ok := prefixRewritePaysWith(req, saved, shallowest, pricing, c)
				if ok {
					passed++
					invSum += len(b.Candidates)
					passMass += saved
					passDead += dead
				}
				candSum += len(b.Candidates)
			}
			if pts == 0 {
				continue
			}
			mi := 0.0
			if passed > 0 {
				mi = float64(invSum) / float64(passed)
			}
			t.Logf("%-9d %6d %6.0f%% %9.1f %9.1f %11d %11d", w, pts,
				100*float64(passed)/float64(pts), mi, float64(candSum)/float64(pts),
				passMass, passDead)
		}
	}

	// THE PROPOSED CONFIGURATION, END TO END. The sweeps above vary one term at a time, which cannot
	// answer "will it fire enough" -- that is a JOINT question over the inventory floor and the gate,
	// and multiplying a pass rate from one table by a qualification rate from another assumes they are
	// independent when the floor selects for large requests and the gate selects against them.
	//
	// Reported as a fraction of ALL decision points, because a firing rate conditioned on qualifying
	// hides exactly what min_inventory costs. Compared against iteration 024's measured 28% of runs
	// (626 asks over 2,207 requests), which is the only firing rate that ever produced reward.
	t.Logf("=== PROPOSED CONFIG END TO END: premium 20, credit on, min_inventory as shown")
	t.Logf("  %-8s %-6s %6s %8s %9s %12s %12s", "window", "minInv", "pts", "qualify", "ask/all", "askMass", "askDead")
	for _, w := range []int{64000, 128000} {
		for _, floor := range []int{0, 3, 10} {
			for _, ap := range []struct {
				label string
				v     float64
			}{{"prior 1.0", 1.0}, {"floored .05", approvalFloor}} {
				var pts, qual, ask, mass, dead int
				for _, b := range batches {
					c := &components.Ctx{Ctx: context.Background(), CtxWindow: w}
					req := simReq(b)
					saved, shallowest, dd := 0, len(req.Input), 0
					for _, cd := range b.Candidates {
						saved += cd.Tokens
						if cd.MsgIdx < shallowest {
							shallowest = cd.MsgIdx
						}
						if !cd.FutureReferenced {
							dd += cd.Tokens
						}
					}
					if saved <= 0 || shallowest >= len(req.Input) {
						continue
					}
					pts++
					if len(b.Candidates) < floor {
						continue // min_inventory declines before the gate is consulted
					}
					qual++
					pricing := rewritePricing{askUSD: askCostPrior(req, c), approval: ap.v,
						premium: 20, creditRemoval: true}
					if _, _, ok := prefixRewritePaysWith(req, saved, shallowest, pricing, c); ok {
						ask++
						mass += saved
						dead += dd
					}
				}
				if pts == 0 {
					continue
				}
				t.Logf("  %-8d %-6d %6d %7.0f%% %8.0f%% %12d %12d  [%s]",
					w, floor, pts, 100*float64(qual)/float64(pts), 100*float64(ask)/float64(pts),
					mass, dead, ap.label)
			}
		}
	}

	// WHAT THE GATE IS ANTI-CORRELATED WITH, measured rather than argued: bucket the decision points by
	// how full the request is and report the pass rate in each. If the gate refuses hardest where the
	// request is fullest, the mechanism cannot act when it matters, and that is a property of the
	// arithmetic rather than of any configuration.
	t.Logf("=== PASS RATE BY CONTEXT PRESSURE (window 128000, floored approval, premium 20)")
	w := 128000
	type bucket struct{ lo, hi float64 }
	bs := []bucket{{0, .25}, {.25, .5}, {.5, .75}, {.75, .9}, {.9, 2}}
	counts := make([][3]int, len(bs)) // pts, passed, meanInv accumulator
	for _, b := range batches {
		c := &components.Ctx{Ctx: context.Background(), CtxWindow: w}
		req := simReq(b)
		saved, shallowest := 0, len(req.Input)
		for _, cd := range b.Candidates {
			saved += cd.Tokens
			if cd.MsgIdx < shallowest {
				shallowest = cd.MsgIdx
			}
		}
		if saved <= 0 || shallowest >= len(req.Input) {
			continue
		}
		p := float64(schema.MessagesTokens(req)) / float64(w)
		for i, bk := range bs {
			if p >= bk.lo && p < bk.hi {
				counts[i][0]++
				counts[i][2] += len(b.Candidates)
				pricing := rewritePricing{askUSD: askCostPrior(req, c), approval: approvalFloor, premium: 20, creditRemoval: true}
				if _, _, ok := prefixRewritePaysWith(req, saved, shallowest, pricing, c); ok {
					counts[i][1]++
				}
				break
			}
		}
	}
	for i, bk := range bs {
		if counts[i][0] == 0 {
			continue
		}
		t.Logf("  pressure %4.0f-%4.0f%%  pts %4d  pass %5.0f%%  mean inventory %5.1f",
			100*bk.lo, 100*bk.hi, counts[i][0],
			100*float64(counts[i][1])/float64(counts[i][0]),
			float64(counts[i][2])/float64(counts[i][0]))
	}
}

// ---------------------------------------------------------------------------------------------------
// THE SEQUENTIAL SIMULATION, and why the static sweep above cannot answer the question it looks like it
// answers. A fixed approval value shows what the gate does AT that value; it cannot show an estimator
// driving itself to that value and then refusing the evidence that would revise it. The observed
// failure is dynamic: three thin asks completed, the pooled ratio fell to 0.0124, and the component
// stopped asking for the remaining thirteen minutes of the run. Reproducing THAT needs history.
//
// FAIRNESS RULE, and the whole comparison rests on it: both estimators face the SAME outcome model, so
// nothing below depends on that model being right. It only has to be identical across arms. The model
// is stated rather than tuned, and swept, because inventing a favourable one is exactly how a
// simulation flatters the design it is testing.
type approvalEstimator interface {
	// name for the log.
	name() string
	// value is what the estimator would report for an ask about to be priced, given inventory size.
	value(inventoryCandidates int) float64
	// observe books the outcome of one completed ask.
	observe(offered, approved, inventoryCandidates int)
}

// shippedEstimator is askLedger's arithmetic, restated here because askLedger's own fields are private
// to it and the simulation needs to drive many independent copies. Kept deliberately literal: asks
// counted, mass pooled, floored — and a mutation of THIS is not a mutation of the shipped code, which
// is why the static sweep above calls prefixRewritePaysWith directly rather than trusting this.
type shippedEstimator struct {
	asks              int
	offered, approved int
}

func (e *shippedEstimator) name() string { return "shipped: pooled ratio, floor 0.05, minAskSamples 3" }
func (e *shippedEstimator) value(int) float64 {
	if e.asks < minAskSamples {
		return 1.0 // the warm-up prior
	}
	if e.offered <= 0 {
		return 1.0
	}
	a := float64(e.approved) / float64(e.offered)
	if a < approvalFloor {
		a = approvalFloor
	}
	return a
}
func (e *shippedEstimator) observe(offered, approved, _ int) {
	e.asks++
	e.offered += offered
	e.approved += approved
}

// shrunkEstimator is the proposal: mass-weighted shrinkage toward an optimistic prior, bucketed by
// inventory size.
//
//	approval = (approved + k*p0) / (offered + k)
//
// THREE PROPERTIES, each answering one measured defect:
//
//  1. k is in TOKENS, not asks. Three 2k-token asks are three units of evidence to the shipped
//     estimator and 6k units to this one, which is the right unit for a mass ratio — the precision of
//     approved/offered scales with offered, not with how many calls produced it.
//  2. There is no switch and no floor. The estimate leaves p0 continuously as mass accumulates and can
//     never reach zero while k > 0, so the ratchet guard the floor exists to provide is structural
//     rather than bolted on.
//  3. Bucketed by inventory size, each bucket shrinking toward the POOLED estimate rather than toward
//     p0 once pooled evidence exists. That is #230 stated as arithmetic: approval is a curve in batch
//     size, so a 12-candidate ask must not be priced by 3-candidate history.
type shrunkEstimator struct {
	k         float64
	p0        float64
	buckets   map[int][2]int // bucket -> {offered, approved}
	toffered  int
	tapproved int
	// floored keeps approvalFloor UNDER the shrunk estimate. The first run of this simulation showed
	// the two are not alternatives: at a genuinely low true rate the shrunk form converges BELOW 0.05
	// and asks LESS than the shipped estimator, because the floor is an optimistic bias that keeps the
	// component asking. Separated so the floor's contribution is attributable rather than bundled.
	floored bool
	// bucketed selects per-inventory-size history (issue #230) versus one pooled ratio. Separated for
	// the same reason: "the proposal helps" is not a finding if it cannot say which half helped.
	bucketed bool
}

func newShrunkOpts(k, p0 float64, floored, bucketed bool) *shrunkEstimator {
	return &shrunkEstimator{k: k, p0: p0, buckets: map[int][2]int{}, floored: floored, bucketed: bucketed}
}

func newShrunk(k, p0 float64) *shrunkEstimator { return newShrunkOpts(k, p0, false, true) }

// bucketOf splits at the inflection the component's own measurements report (extract_sweep.go: batch
// 3-6 dropped a genuinely-spent output 2 times in 4; batch 10 dropped it 4 in 4). Three buckets, not a
// continuous fit: with this much data a fitted curve would be noise given a shape.
func bucketOf(n int) int {
	switch {
	case n < 7:
		return 0
	case n < 10:
		return 1
	default:
		return 2
	}
}

func (e *shrunkEstimator) name() string {
	f, b := "no-floor", "pooled"
	if e.floored {
		f = "floored"
	}
	if e.bucketed {
		b = "bucketed"
	}
	return fmt.Sprintf("shrunk k=%.0fk p0=%.2f %s %s", e.k/1000, e.p0, f, b)
}

func (e *shrunkEstimator) value(n int) float64 {
	// The prior for a bucket is the POOLED rate once any mass has been seen anywhere, so a bucket with
	// no history of its own inherits the component's overall experience rather than the fixed optimism.
	prior := e.p0
	if e.toffered > 0 {
		pooled := (float64(e.tapproved) + e.k*e.p0) / (float64(e.toffered) + e.k)
		prior = pooled
	}
	off, app := e.toffered, e.tapproved
	if e.bucketed {
		b := e.buckets[bucketOf(n)]
		off, app = b[0], b[1]
	}
	v := (float64(app) + e.k*prior) / (float64(off) + e.k)
	if e.floored && v < approvalFloor {
		v = approvalFloor
	}
	return v
}

func (e *shrunkEstimator) observe(offered, approved, n int) {
	b := e.buckets[bucketOf(n)]
	e.buckets[bucketOf(n)] = [2]int{b[0] + offered, b[1] + approved}
	e.toffered += offered
	e.tapproved += approved
}

func TestApprovalEstimatorDynamics(t *testing.T) {
	path := os.Getenv("CG_ECON_SIM_CORPUS")
	if path == "" {
		t.Skip("set CG_ECON_SIM_CORPUS to a batches.jsonl to run the estimator simulation")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	var batches []simBatch
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var b simBatch
		if err := dec.Decode(&b); err != nil {
			break
		}
		if len(b.Candidates) > 0 && len(b.Transcript) > 0 {
			batches = append(batches, b)
		}
	}
	// Chronological within a conversation, which is the order a live session would meet them. Sorted
	// rather than assumed: the corpus is grouped by conversation but nothing guarantees decision_at is
	// ascending within a group, and an estimator fed history out of order is not being tested.
	sort.SliceStable(batches, func(i, j int) bool {
		if batches[i].Conv != batches[j].Conv {
			return batches[i].Conv < batches[j].Conv
		}
		return batches[i].DecisionAt < batches[j].DecisionAt
	})

	// THE OUTCOME MODEL, stated and swept. `w` is the fraction of INDEX-REMOVABLE mass an ask actually
	// converts into a removal. Index-removable rather than oracle-dead, because a real selector at 10%
	// false-drop is what the component has; w then covers everything between "the model refuses nearly
	// all of it" (0.05, the probe's measured 1.24% rounded up) and "it takes all of it" (1.0).
	for _, w := range []float64{0.05, 0.30, 1.00} {
		for _, window := range []int{64000, 128000} {
			t.Logf("=== outcome model w=%.2f (fraction of index-removable mass converted), window %d", w, window)
			t.Logf("  %-56s %6s %6s %12s %9s %s", "estimator", "asks", "pass%", "removedTok", "finalAppr", "shutdownAfterAsk")
			ests := []approvalEstimator{
				&shippedEstimator{},
				newShrunk(300000, 1.0),                  // no floor, bucketed
				newShrunkOpts(300000, 1.0, true, true),  // + floor  -> isolates the floor
				newShrunkOpts(300000, 1.0, true, false), // + floor, pooled -> isolates bucketing
				newShrunkOpts(1000000, 1.0, true, true), // is more shrinkage still better?
			}
			for _, est := range ests {
				var asks, evals, removed int
				lastAskAt, evalsAfterLast := -1, 0
				for i, b := range batches {
					c := &components.Ctx{Ctx: context.Background(), CtxWindow: window}
					req := simReq(b)
					saved, shallowest, idxMass := 0, len(req.Input), 0
					for _, cd := range b.Candidates {
						saved += cd.Tokens
						if cd.MsgIdx < shallowest {
							shallowest = cd.MsgIdx
						}
						if cd.IndexVerdict == "unreferenced" {
							idxMass += cd.Tokens
						}
					}
					if saved <= 0 || shallowest >= len(req.Input) {
						continue
					}
					evals++
					n := len(b.Candidates)
					pricing := rewritePricing{askUSD: askCostPrior(req, c), approval: est.value(n),
						premium: 20, creditRemoval: true}
					if _, _, ok := prefixRewritePaysWith(req, saved, shallowest, pricing, c); !ok {
						continue
					}
					asks++
					got := int(w * float64(idxMass))
					removed += got
					est.observe(saved, got, n)
					lastAskAt, evalsAfterLast = i, 0
					_ = lastAskAt
				}
				// How many evaluations went by after the FINAL ask without another one: the shutdown
				// signature. A healthy estimator keeps asking to the end; the observed failure asked
				// three times and then never again.
				if lastAskAt >= 0 {
					for _, b := range batches[lastAskAt+1:] {
						if len(b.Candidates) > 0 {
							evalsAfterLast++
						}
					}
				}
				t.Logf("  %-56s %6d %5.0f%% %12d %9.3f %d",
					est.name(), asks, 100*float64(asks)/float64(max(1, evals)), removed,
					est.value(10), evalsAfterLast)
			}
		}
	}
}
