package kvcache

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// The tests that would have caught the three defects an independent review found in this
// package, plus the determinism rule its own comments promise.
//
// The finding underneath those defects is what this file is really for: all three were fixable
// without a single existing test changing colour. Every one of them lived at a seam where two
// things are meant to be the same thing — the simulator against the dynamic program's transition
// weight, the Go against the Python port, the cost model against the hit accounting — and a
// fixture that exercises only one side of a seam cannot see across it. So each test below is
// built to fail on the specific mistake, and the fixtures deliberately carry the property the
// old ones lacked: more than one model.

// ── the exhaustive check: is the "exact ceiling" actually exact ────────────

// bruteForce scores EVERY action sequence through Simulate itself and returns the cheapest.
//
// Through Simulate, never through a reimplementation: a brute force that priced plans its own way
// would prove the two agree with each other and nothing about what the product bills. That is
// exactly how the model-switch defect stayed invisible — the DP and the simulator each priced a
// keep-alive at a different model, and no check compared either against a third opinion.
func bruteForce(t *testing.T, reqs []*Request, cfg Config) (float64, string) {
	t.Helper()
	ids := make([]int64, len(reqs))
	for i, r := range reqs {
		ids[i] = r.ID
	}
	total := 1
	for range reqs {
		total *= len(Actions)
	}
	plan := make(map[int64]Action, len(reqs))
	cheapest, at := math.Inf(1), 0
	for code := 0; code < total; code++ {
		n := code
		for i := range ids {
			plan[ids[i]] = Actions[n%len(Actions)]
			n /= len(Actions)
		}
		if got := Simulate(reqs, NewReplay("brute", plan, ActionExpire), cfg); got.TotalUSD < cheapest {
			cheapest, at = got.TotalUSD, code
		}
	}
	var winner []string
	n := at
	for range ids {
		winner = append(winner, string(Actions[n%len(Actions)]))
		n /= len(Actions)
	}
	return cheapest, strings.Join(winner, ";")
}

// NewOptimal must be the cheapest sequence that exists — checked by enumerating all of them.
//
// Every other assertion about it is comparative: TestOptimalIsALowerBoundOnEveryOtherArm checks
// it against the arms that happen to exist, which is ten sequences out of 6^n. The doc comment
// claims something stronger, and until this test that claim rested on the argument for the
// dynamic program rather than on evidence.
//
// THE MULTI-MODEL CASES ARE THE POINT. A single-model fixture passes this test with the
// model-switch defect fully present — the review said so before I wrote it, and it was right.
func TestOptimalIsExhaustivelyOptimal(t *testing.T) {
	const min5, hour = int64(300_000), int64(3_600_000)
	for _, tc := range []struct {
		name   string
		gaps   []int64
		models []string
	}{
		{"all short", []int64{20_000, 20_000, 20_000}, nil},
		{"all past an hour", []int64{hour + 1, hour + 1, hour + 1}, nil},
		{"straddling five minutes", []int64{min5 - 1, min5 + 1, 30_000}, nil},
		{"inside a keep-alive's reach, then past an hour", []int64{600_000, hour + 1, 40_000}, nil},
		{"one long gap between two short ones", []int64{15_000, 40 * 60_000, 15_000}, nil},
		// The four the old code failed. A conversation that switches model is 5.7% of this
		// deployment's corpus, and on it the ceiling was measured 65% above a plan the same
		// simulator priced lower.
		{"cheap then dear", []int64{600_000}, []string{"cheap", "dear"}},
		{"dear then cheap", []int64{600_000}, []string{"dear", "cheap"}},
		{"dear, cheap, dear", []int64{600_000, 600_000}, []string{"dear", "cheap", "dear"}},
		{"switching across an hour", []int64{hour + 1, 60_000}, []string{"cheap", "dear", "cheap"}},
		// A SHRINKING prefix, which is what a compaction produces — i.e. this repo's own
		// subject. The reusable = min(entry, prefix) clamp never fired on the old fixtures,
		// whose prefixes only ever grew, so deleting it changed nothing that was tested.
		{"a shrinking prefix", []int64{30_000, 30_000}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrink := tc.name == "a shrinking prefix"
			reqs, cfg := trajectory(t, tc.gaps, tc.models, shrink)
			best := Simulate(reqs, NewOptimal(reqs, cfg), cfg)
			cheapest, winner := bruteForce(t, reqs, cfg)
			if best.TotalUSD > cheapest+1e-9 {
				t.Errorf("NewOptimal chose $%.9f; exhaustive search over every plan found $%.9f "+
					"via %s.\nIt is not the cheapest sequence that exists, so every figure quoted "+
					"against this ceiling is wrong by at least the difference.",
					best.TotalUSD, cheapest, winner)
			}
			if best.TotalUSD < cheapest-1e-9 {
				t.Errorf("NewOptimal reports $%.9f, BELOW the exhaustive minimum $%.9f — the DP "+
					"and Simulate disagree about what a plan costs, so neither figure means "+
					"anything", best.TotalUSD, cheapest)
			}
		})
	}
}

// ── the hit that was not a hit ─────────────────────────────────────────────

// A request that writes no cache_control is NOT a hit, however warm the entry was.
//
// `hit` used to be computed from aliveness alone, before the action was known, so an expire turn
// landed in Hits, HitRate and AvoidedRecomputations while paying for a fully uncached prompt.
// The tell was two fields of one Result contradicting each other: AvoidedRecomputations of 1
// beside AvoidedTokens of 0. It also fed Compare.HitDelta, and so credited an avoided DELAY to a
// request that had taken the miss latency.
//
// The dollar total is unchanged by this — the money was always right — which is why nothing that
// checked costs could see it.
func TestExpireOverALiveEntryIsNotAHit(t *testing.T) {
	reqs, cfg := trajectory(t, []int64{60_000}, nil, false)
	plan := map[int64]Action{reqs[0].ID: ActionWrite5m, reqs[1].ID: ActionExpire}
	got := Simulate(reqs, NewReplay("write-then-expire", plan, ActionExpire), cfg)

	if got.Hits != 0 {
		t.Errorf("hits = %d; the second request wrote no cache_control, so the provider read "+
			"nothing from cache and billed its whole prefix as fresh input", got.Hits)
	}
	if got.Misses != 2 {
		t.Errorf("misses = %d, want 2", got.Misses)
	}
	if got.AvoidedRecomputations != 0 || got.AvoidedTokens != 0 {
		t.Errorf("avoided %d recomputations over %d tokens; a request that re-sent its whole "+
			"prefix as fresh input avoided nothing", got.AvoidedRecomputations, got.AvoidedTokens)
	}
	// The two fields must never contradict each other again, whatever the arm.
	if (got.AvoidedRecomputations == 0) != (got.AvoidedTokens == 0) {
		t.Errorf("AvoidedRecomputations=%d and AvoidedTokens=%d disagree about whether anything "+
			"was avoided", got.AvoidedRecomputations, got.AvoidedTokens)
	}
	// And the money is the same either way, which is the fact that kept this hidden.
	price := cfg.Prices.For(reqs[0].Model)
	want := price.RequestCost(reqs[0].InputTokens, 0, reqs[0].CachedContext, reqs[0].OutputTokens, TTL5m) +
		price.UncachedCost(reqs[1].InputTokens, reqs[1].CachedContext, reqs[1].OutputTokens)
	if math.Abs(got.TotalUSD-want) > 1e-9 {
		t.Errorf("total $%.9f, want $%.9f (one 5m write plus one fully uncached prompt)",
			got.TotalUSD, want)
	}
}

// ── determinism ───────────────────────────────────────────────────────────

// The optimum's PLAN, not just its cost, must be the same on every run.
//
// NewOptimal keeps the FIRST best on a tie (strict `<`), and its comment names that as the
// determinism guarantee. Nothing pinned it: switching to `<=` — keep the LAST best — survives
// the whole suite including the drift guard, because TotalUSD is identical either way and only
// Result.Decisions moves. A figure that flaps between runs while the total does not is the kind
// nobody can reproduce and nobody notices.
//
// The fixture gives every action exactly the same cost, so every transition is a perfect tie and
// the recorded pointer is the only thing that can keep two runs agreeing. With no prefix there is
// nothing to read, write or refresh, so all six actions cost input+output at every turn.
func TestTheOptimalPlanIsDeterministicUnderPerfectTies(t *testing.T) {
	reqs, cfg := trajectory(t, []int64{30_000, 30_000, 30_000}, nil, false)
	for _, r := range reqs {
		r.CachedContext = 0
	}
	planOf := func() string {
		o := NewOptimal(reqs, cfg)
		var parts []string
		for _, r := range reqs {
			parts = append(parts, string(o.Decide(Observation{RequestID: r.ID})))
		}
		return strings.Join(parts, ";")
	}
	first := planOf()
	for i := 0; i < 50; i++ {
		if got := planOf(); got != first {
			t.Fatalf("solve %d produced %q, the first produced %q — the plan is not "+
				"deterministic, so Result.Decisions flaps between runs", i, got, first)
		}
	}
	// Keeping the FIRST best means Actions[0] wins every tie, and Actions[0] is ActionExpire.
	want := strings.TrimSuffix(strings.Repeat(string(ActionExpire)+";", len(reqs)), ";")
	if first != want {
		t.Errorf("under a perfect tie the plan is %q, want %q — the first-best rule the "+
			"determinism guarantee rests on is not being applied", first, want)
	}
}

// ── the fixture ───────────────────────────────────────────────────────────

// trajectory builds one conversation. models gives a model per turn (nil = one model
// throughout); shrink makes the prefix SHRINK rather than grow, which is what a compaction does.
func trajectory(t *testing.T, gaps []int64, models []string, shrink bool) ([]*Request, Config) {
	t.Helper()
	const start = int64(1_786_967_311_185)
	ts := start
	reqs := make([]*Request, 0, len(gaps)+1)
	for i := 0; i <= len(gaps); i++ {
		reason := "hit"
		if i == 0 {
			reason = "cold_start"
		}
		prefix := int64(100_000 + i*25_000)
		if shrink {
			prefix = int64(250_000 - i*80_000)
		}
		reqs = append(reqs, &Request{
			ID: int64(i + 1), User: "acct", ConversationID: "conv", TS: ts,
			HourUTC: time.UnixMilli(ts).UTC().Hour(), Bucket: BucketAt(ts),
			Model: modelAt(models, i), InputTokens: 110, OutputTokens: 44,
			CachedContext: prefix, MissReason: reason,
			TTL: TTL5m, TTLSource: TTLSourceConfigured,
		})
		if i < len(gaps) {
			ts += gaps[i]
		}
	}
	Derive(reqs)
	// Rates an order of magnitude apart, so pricing a span at the wrong model is a visible
	// dollar difference rather than a rounding artefact.
	pin, pout := int64(1), int64(1)
	rate := func(in float64) Override {
		i, out, cr, w5, w1 := in, in*5, in*0.1, in*1.25, in*2.0
		return Override{Input: &i, Output: &out, CacheRead: &cr, Write5m: &w5, Write1h: &w1,
			PingInputTokens: &pin, PingOutputTokens: &pout}
	}
	prices := NewPriceList(context.Background(), []string{"m", "cheap", "dear"}, nil,
		Multipliers{}, map[string]Override{
			"m": rate(3.8e-6), "cheap": rate(0.4e-6), "dear": rate(4.0e-6)})
	return reqs, Config{Prices: prices, WindowEnd: ts}
}

func modelAt(models []string, i int) string {
	if i < len(models) {
		return models[i]
	}
	if len(models) > 0 {
		return models[len(models)-1]
	}
	return "m"
}

// A conversation is (user, session, MODEL). A cache entry does not transfer between models, so
// two requests differing only in model are two trajectories and neither is the other's successor.
//
// This deployment's own predictor stated that rule before the simulator implemented it, and
// without it Derive linked an opus request to a sonnet one and the replay granted a hit at 0.1x
// on an entry it could never have matched.
func TestTheModelIsPartOfTheConversationKey(t *testing.T) {
	base := int64(1_786_967_311_185)
	mk := func(id int64, model string, ts int64) *Request {
		return &Request{ID: id, User: "acct", ConversationID: "same-session", TS: ts,
			Model: model, CachedContext: 100_000, MissReason: "hit"}
	}
	opus, sonnet := mk(1, "dear", base), mk(2, "cheap", base+60_000)
	later := mk(3, "dear", base+120_000)
	rows := []*Request{opus, sonnet, later}
	Derive(rows)

	if opus.HasNext && opus.NextID == sonnet.ID {
		t.Error("an opus request was linked to a sonnet request as its successor; a cache entry " +
			"does not transfer between models, so the gap between them is not a reuse interval")
	}
	if !opus.HasNext || opus.NextID != later.ID {
		t.Errorf("the opus request's successor is %d (has_next=%v); it should be the next "+
			"request OF THE SAME MODEL, id %d", opus.NextID, opus.HasNext, later.ID)
	}
	if sonnet.HasNext {
		t.Errorf("the lone sonnet request has successor %d; it has none", sonnet.NextID)
	}
	if opus.Key() == sonnet.Key() {
		t.Errorf("two models share a conversation key: %+v", opus.Key())
	}
	// And the replay must not grant the sonnet request a hit on the opus entry.
	_, cfg := trajectory(t, []int64{1}, nil, false)
	got := Simulate(rows, Fixed5m(), cfg)
	if got.Conversations != 2 {
		t.Errorf("conversations = %d, want 2 (one per model)", got.Conversations)
	}
	if got.Hits != 1 {
		t.Errorf("hits = %d; only the second opus request can hit, and the sonnet request "+
			"cannot read what opus wrote", got.Hits)
	}
}

// A session that ALTERNATES models must still link each model's requests to each other.
//
// This guards the second half of putting Model in the key, and it is load-bearing rather than
// belt-and-braces. Derive sorts before it walks, so if the grouping sort does not order by model
// as well, an alternating session interleaves — opus, sonnet, opus, sonnet — and two requests of
// the same model are never ADJACENT in the slice. The walk only ever compares neighbours, so
// their successors are silently lost: HasNext goes false on requests that do have one.
//
// That is under-linking, the opposite direction from the cross-model hit it was fixing, and
// equally wrong: it suppresses idle gaps from History, suppresses hits, and inflates cost. Found
// by mutating the tiebreak away, which no existing test noticed.
func TestAlternatingModelsStillLinkWithinEachModel(t *testing.T) {
	base := int64(1_786_967_311_185)
	mk := func(id int64, model string, ts int64) *Request {
		return &Request{ID: id, User: "acct", ConversationID: "one-session", TS: ts,
			Model: model, CachedContext: 100_000, MissReason: "hit"}
	}
	// Interleaved in time, which is how the sort can scramble them.
	rows := []*Request{
		mk(1, "dear", base),
		mk(2, "cheap", base+10_000),
		mk(3, "dear", base+20_000),
		mk(4, "cheap", base+30_000),
	}
	Derive(rows)
	byID := map[int64]*Request{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, want := range []struct{ from, to int64 }{{1, 3}, {2, 4}} {
		r := byID[want.from]
		if !r.HasNext || r.NextID != want.to {
			t.Errorf("request %d (model %q) has successor %d (has_next=%v), want %d — the same "+
				"model's next request was not adjacent after the grouping sort, so its successor "+
				"was lost", want.from, r.Model, r.NextID, r.HasNext, want.to)
		}
	}
	for _, id := range []int64{3, 4} {
		if byID[id].HasNext {
			t.Errorf("request %d is the last of its model and has successor %d", id, byID[id].NextID)
		}
	}
	// And no cross-links: 1 must never point at 2, nor 2 at 3.
	if byID[1].NextID == 2 || byID[2].NextID == 3 {
		t.Error("a request was linked across models despite the model being in the key")
	}
}

// The invariant that makes the closed-span pricing fix unobservable, asserted directly.
//
// With Model in the conversation key, the previous request of a trajectory always has the SAME
// model as the current one — so pricing a closed idle span at either is identical, and no test
// can distinguish the two. That is a good position to be in and a bad one to leave unguarded: it
// means the pricing fix is correct-by-subsumption, and anyone who later removes Model from the
// key silently reintroduces the defect it was written for. So the invariant is checked here
// rather than trusted.
func TestAConversationNeverChangesModelMidStream(t *testing.T) {
	base := int64(1_786_967_311_185)
	var rows []*Request
	id := int64(0)
	for _, model := range []string{"dear", "cheap", "m"} {
		for turn := 0; turn < 3; turn++ {
			id++
			ts := base + int64(turn)*30_000
			rows = append(rows, &Request{ID: id, User: "acct", ConversationID: "shared",
				TS: ts, Model: model, CachedContext: 120_000, MissReason: "hit"})
		}
	}
	Derive(rows)
	// Walk each trajectory the way Simulate does and assert the model never moves inside one.
	seen := map[Conversation]string{}
	for _, r := range rows {
		k := r.Key()
		if prev, ok := seen[k]; ok && prev != r.Model {
			t.Errorf("conversation %+v contains both model %q and %q; a closed idle span would "+
				"then have two candidate rates and the ceiling and the simulator could price it "+
				"differently", k, prev, r.Model)
		}
		seen[k] = r.Model
		if k.Model != r.Model {
			t.Errorf("Key().Model = %q but the request's model is %q", k.Model, r.Model)
		}
	}
	if len(seen) != 3 {
		t.Errorf("one session with three models produced %d trajectories, want 3", len(seen))
	}
}

// Group.Valued must mean "any request here could be priced", not "all of them could".
func TestGroupValuedOnAMixedGroup(t *testing.T) {
	base := int64(1_786_967_311_185)
	rows := []*Request{
		{ID: 1, User: "acct", ConversationID: "c", TS: base, Model: "m",
			CachedContext: 100_000, MissReason: "cold_start"},
		{ID: 2, User: "acct", ConversationID: "c2", TS: base + 1000, Model: "no-rates",
			CachedContext: 100_000, MissReason: "cold_start"},
	}
	Derive(rows)
	_, cfg := trajectory(t, []int64{1}, nil, false)
	got := Simulate(rows, Fixed5m(), cfg)
	var user *Group
	for i := range got.ByUser {
		if got.ByUser[i].Key == "acct" {
			user = &got.ByUser[i]
		}
	}
	if user == nil {
		t.Fatal("no group for the only account in the window")
	}
	if user.Unpriced != 1 || user.Requests != 2 {
		t.Fatalf("group has %d unpriced of %d requests; want 1 of 2", user.Unpriced, user.Requests)
	}
	if !user.Valued {
		t.Error("a group with one priced request of two reports Valued=false; its dollar figure " +
			"is real for the half that could be priced, and Unpriced says how much is missing")
	}
}

// The n floor the Stats fallback rests on. minCell exists so a cell cannot speak for itself on
// too few observations — "a strategy switching tier on that is choosing by noise" — and nothing
// pinned the number.
func TestTheMinCellFloorHolds(t *testing.T) {
	h := NewHistory()
	for i := 0; i < minCell-1; i++ {
		h.Observe("u", "m", BucketNight, 30*time.Second)
	}
	if _, n, level := h.ReuseWithin("u", "m", BucketNight, Horizon5m); level == LevelUserBucket {
		t.Errorf("a cell with %d observations answered at its own level (n=%d); the floor is %d",
			minCell-1, n, minCell)
	}
	h.Observe("u", "m", BucketNight, 30*time.Second)
	if _, n, level := h.ReuseWithin("u", "m", BucketNight, Horizon5m); level != LevelUserBucket {
		t.Errorf("a cell with exactly %d observations answered at %s (n=%d); the floor is "+
			"inclusive", minCell, level, n)
	}
}

// MedianIdle must be the median, not an extreme. It is on the Stats interface and no shipped
// strategy calls it, so nothing else would notice it returning the maximum.
func TestMedianIdleIsTheMedian(t *testing.T) {
	h := NewHistory()
	for _, d := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second,
		4 * time.Second, 5 * time.Second, 100 * time.Second} {
		h.Observe("u", "m", BucketNight, d)
	}
	got, n, _ := h.MedianIdle("u", "m", BucketNight)
	if n != 6 {
		t.Fatalf("n = %d, want 6", n)
	}
	// Nearest-rank on an even count takes the lower of the two middles: 3s, not 3.5s and
	// certainly not the 100s maximum.
	if got != 3*time.Second {
		t.Errorf("median = %v, want 3s (the lower of the two middles of 1,2,3,4,5,100)", got)
	}
}
