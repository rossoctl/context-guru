package dash

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// The seam between the dashboard and the strategy registry.
//
// These exist because of a failure that was invisible in every other way: package kvcache
// shipped ten arms and this package carried a closed list of six, so four working, tested arms —
// two keep-alive policies, the extend-to-1h policy, and the exact cost ceiling — could not be
// selected from the dashboard at all. Nothing was red. The code read correctly on both sides.

// Every arm the domain publishes is reachable through this API, or is explicitly marked as not
// selectable with a reason. There is no third state, and no arm may be silently absent.
func TestEveryRegistryArmIsOfferedOrExplicitlyMarkedUnselectable(t *testing.T) {
	arms := KVCacheArms()
	byName := map[string]KVCacheArm{}
	for _, a := range arms {
		byName[a.Name] = a
	}
	for _, spec := range kvcache.Registry() {
		a, ok := byName[spec.Name]
		if !ok {
			t.Errorf("arm %q exists in kvcache.Registry() and is not offered by the dashboard at "+
				"all. An arm nobody can select is an arm nobody compares against.", spec.Name)
			continue
		}
		if a.Unreachable != spec.Unreachable || a.Description != spec.Description {
			t.Errorf("arm %q was re-described by this layer: %+v vs %+v", spec.Name, a.StrategySpec,
				spec)
		}
		// Selectable arms must actually build. A spec that says NeedsDataset builds from the real
		// rows, so it is probed with one.
		rows := []*kvcache.Request{
			{ID: 1, User: "t", ConversationID: "s", TS: kvBase, Model: "m", CachedContext: 1000},
			{ID: 2, User: "t", ConversationID: "s", TS: kvBase + 10_000, Model: "m", CachedContext: 1000},
		}
		kvcache.Derive(rows)
		got := buildStrategy(spec.Name, rows, KVCacheSimConfig{}, kvcache.Config{Prices: testPriceList()})
		if a.Selectable && got == nil {
			t.Errorf("arm %q is offered as selectable and cannot be built", spec.Name)
		}
		if !a.Selectable && got != nil {
			t.Errorf("arm %q is marked unselectable but builds fine; the marking is wrong and the "+
				"page is hiding a working arm", spec.Name)
		}
	}
	// The custom arm is this layer's own, and it is the only name here that is NOT in the
	// registry: it carries the page's thresholds and the in-process Predictor seam.
	c, ok := byName[KVStrategyCustom]
	if !ok || !c.Selectable {
		t.Errorf("the custom arm is not offered: %+v", c)
	}
	if buildStrategy(KVStrategyCustom, nil, KVCacheSimConfig{}, kvcache.Config{}) == nil {
		t.Error("the custom arm does not build")
	}
	// And a name that is not an arm builds nothing, rather than falling back to one.
	if buildStrategy("wishful-thinking", nil, KVCacheSimConfig{}, kvcache.Config{}) != nil {
		t.Error("an unknown name resolved to a strategy; the page would then report one policy's " +
			"savings under another's label")
	}
}

// `replay` is in the registry and must NOT be a checkbox: its whole input is an action list
// supplied in the process, so no query string can ask for it. It is listed and marked, not
// dropped — a reader who has seen it named in the domain has to be able to find out why it is
// not on the picker.
func TestTheReplayArmIsListedButNotSelectable(t *testing.T) {
	var found bool
	for _, a := range KVCacheArms() {
		if a.Name != kvcache.StrategyReplay {
			continue
		}
		found = true
		if a.Selectable {
			t.Error("the replay arm is offered as selectable; nothing in a URL can supply its " +
				"action list")
		}
	}
	if !found {
		t.Error("the replay arm was dropped from the payload rather than marked")
	}
}

// The exact ceiling is marked unreachable all the way to the wire, and it is NOT the default
// denominator.
//
// It reads the true next-request time, so a percentage measured against it would be a share of a
// number no policy can reach. Two separate properties: the flag survives to the payload, and the
// default baseline is a reachable arm.
func TestTheCeilingArmIsMarkedAndIsNeverTheDefaultBaseline(t *testing.T) {
	var ceilings int
	for _, a := range KVCacheArms() {
		if a.Unreachable {
			ceilings++
		}
	}
	if ceilings == 0 {
		t.Fatal("no arm is marked unreachable; the exact ceiling has stopped being labelled as one")
	}
	base := kvCacheDefaultBaseline()
	for _, a := range KVCacheArms() {
		if a.Name == base && a.Unreachable {
			t.Errorf("the default baseline %q reads the future; every percentage on the page "+
				"would be a share of an unreachable number", base)
		}
	}
	// The default comes from the registry's own flag rather than from a constant here, so the
	// two cannot disagree about the one number every percentage is divided by.
	var flagged string
	for _, s := range kvcache.Registry() {
		if s.Baseline {
			flagged = s.Name
		}
	}
	if flagged == "" {
		t.Skip("the registry flags no baseline arm")
	}
	if base != flagged {
		t.Errorf("the default baseline is %q but the registry flags %q", base, flagged)
	}
}

// Every default arm resolves. A default set naming an arm that cannot be built would leave the
// page's first render reporting a strategy as "not a strategy".
func TestEveryDefaultArmResolves(t *testing.T) {
	for _, name := range KVCacheDefaultStrategies {
		rows := []*kvcache.Request{{ID: 1, User: "t", ConversationID: "s", TS: kvBase, Model: "m"}}
		kvcache.Derive(rows)
		if buildStrategy(name, rows, KVCacheSimConfig{}, kvcache.Config{Prices: testPriceList()}) == nil {
			t.Errorf("default arm %q does not resolve", name)
		}
	}
}

// The tier filter is observedTTL expressed as SQL, and this is the assertion that the two agree.
//
// They are one definition in two languages: the Go function decides what a row's tier IS, and
// the predicate decides which rows a filter KEEPS. A disagreement would be a page whose group
// table says 400 requests used the one-hour tier and whose detail table, filtered to that tier,
// shows a different number — with nothing to say which was right.
func TestTheTierFilterAgreesWithTheTierReconstruction(t *testing.T) {
	// Every combination that produces a distinct answer, plus an unrecognised tier.
	type row struct {
		recorded    string
		write1h     int64
		read, write int64
	}
	cases := []row{
		{"ephemeral_5m", 0, 1000, 0},
		{"ephemeral_5m", 0, 0, 0},
		{"ephemeral_1h", 0, 1000, 0},
		{"ephemeral_1h", 500, 0, 1000},
		{"", 0, 1000, 0},
		{"", 0, 0, 1000},
		{"", 500, 0, 1000},
		{"", 0, 0, 0},
		{"ephemeral_10m", 0, 1000, 0},
		{"ephemeral_10m", 0, 0, 0},
		{"ephemeral_10m", 700, 0, 1000},
	}
	var evs []*Event
	want := map[string]int{} // tier -> how many rows observedTTL puts in it
	for i, c := range cases {
		e := kvEvent("t", "s", "m", kvBase+int64(i)*1000, c.read, c.write)
		e.CacheTTL, e.CacheWrite1h = c.recorded, c.write1h
		evs = append(evs, e)
		tier, src := observedTTL(c.recorded, c.write1h, c.read+c.write)
		want[ttlGroupKey(&kvcache.Request{TTL: tier, TTLSource: src})]++
	}
	db := seedKV(t, evs...)

	for _, tier := range []string{string(kvcache.TTL5m), string(kvcache.TTL1h), "none",
		TTLUnrecorded} {
		rows, total, err := db.KVCacheDataset(allTenants(), KVCacheOptions{TTL: tier})
		if err != nil {
			t.Fatal(err)
		}
		if int(total) != want[tier] || len(rows) != want[tier] {
			t.Errorf("filter ttl=%q kept %d rows (total %d); observedTTL puts %d rows in that "+
				"tier — the SQL predicate and the Go reconstruction disagree",
				tier, len(rows), total, want[tier])
		}
		// And every row the filter kept really is in that group by the Go function's own reckoning.
		for _, r := range rows {
			if got := ttlGroupKey(r); got != tier {
				t.Errorf("filter ttl=%q returned a row whose reconstructed tier is %q", tier, got)
			}
		}
	}
	// The three tiers partition the dataset: no row is in two of them and none is in none.
	all, total, err := db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sum := want[string(kvcache.TTL5m)] + want[string(kvcache.TTL1h)] + want["none"] +
		want[TTLUnrecorded]
	if sum != len(all) {
		t.Errorf("the four groups hold %d of %d rows; a tier filter that does not partition the "+
			"dataset drops rows from the page silently", sum, total)
	}
	// The unrecorded group is a group of its OWN and is never folded into the five-minute one.
	// That absorption is what made the grouped table say "3,106 requests used the 5-minute tier"
	// about a window whose own coverage banner reported 295 of them as not recorded.
	if want[TTLUnrecorded] == 0 {
		t.Fatal("this fixture no longer contains a row that cached something at an unrecorded " +
			"tier, so it cannot check that such rows are kept apart")
	}
	for _, r := range all {
		if r.TTLSource == kvcache.TTLSourceUnknown && ttlGroupKey(r) == string(kvcache.TTL5m) {
			t.Error("a row whose tier was never recorded is grouped as a 5-minute one; the page " +
				"would report it as evidence of a policy nobody configured")
		}
	}
}

// The time-of-day filter really narrows by the hour, in UTC.
//
// It is here because the first version of this predicate was assembled with a fmt.Sprintf that
// consumed the strftime('%H') as a format verb — so every bucket filter compared the hour against
// the literal "%!H", matched nothing, and rendered as an empty table. An empty table is exactly
// what a quiet afternoon looks like, which is why this asserts a POSITIVE match rather than only
// that the query runs.
func TestTheTimeOfDayFilterMatchesTheHourItNames(t *testing.T) {
	db := seedKV(t, kvEvent("t", "s", "m", kvBase, 1000, 0))
	in := string(kvcache.BucketAt(kvBase))
	rows, _, err := db.KVCacheDataset(allTenants(), KVCacheOptions{Bucket: in})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the row's own band %q kept %d rows, want 1", in, len(rows))
	}
	if got := string(rows[0].Bucket); got != in {
		t.Errorf("the row's bucket is %q but it matched the filter for %q", got, in)
	}
	for _, b := range kvcache.Buckets {
		if string(b) == in {
			continue
		}
		other, _, err := db.KVCacheDataset(allTenants(), KVCacheOptions{Bucket: string(b)})
		if err != nil {
			t.Fatal(err)
		}
		if len(other) != 0 {
			t.Errorf("band %q kept %d rows from a different band", b, len(other))
		}
	}
}

// testPriceList is a price list for the arms that need one at CONSTRUCTION time — the exact
// ceiling computes its plan from the rates, so building it against no prices makes every action
// free and the plan meaningless.
func testPriceList() *kvcache.PriceList {
	return kvcache.NewPriceList(context.Background(), []string{"m"}, staticPricer{ibmSonnet},
		kvcache.Multipliers{}, nil)
}

// The SQL partition IS kvcache.Conversation, and this is the assertion that they have not drifted.
//
// They are one definition in two languages: the Go type says what a trajectory is, and the window
// function in kvCacheCTE has to group by exactly that. When kvcache.Conversation gained `Model` —
// because a cache entry does not transfer between models, so an opus request cannot read a sonnet
// request's entry — the SQL kept partitioning on (tenant, session) and started linking two
// different models' requests as successor. Every arm then gets a hit at 0.1x on an entry it could
// never have matched, and the bias is one-directional: everything looks cheaper and hits more often
// than it can. Nothing failed on the derivation itself; a key lookup in an unrelated test failed
// with "tenant-a has 0 rows", which is how it surfaced.
//
// Two halves, because the structural one alone would not have caught it and the behavioural one
// alone would not say why.
func TestTheSQLPartitionIsExactlyTheConversationKey(t *testing.T) {
	// STRUCTURAL: a field added to the key is a field the partition needs. This fails on the ADD,
	// naming what to do, rather than leaving it to be discovered from a wrong number.
	want := []string{"User", "Conversation", "Model"}
	typ := reflect.TypeOf(kvcache.Conversation{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kvcache.Conversation is %v, and this package's window function partitions by "+
			"(tenant_id, session_id, model). They must be the same grouping. If a field was added, "+
			"add the matching column to kvCacheCTE's PARTITION BY in the same change; if one was "+
			"removed, take the column out.", got)
	}
	for _, col := range []string{"r.tenant_id", "r.session_id", "r.model"} {
		if !strings.Contains(kvCacheCTE, col) {
			t.Errorf("the CTE does not partition by %s", col)
		}
	}

	// BEHAVIOURAL: two requests in one session on DIFFERENT models are not each other's successor.
	db := seedKV(t,
		kvEvent("t", "s", "aws/claude-opus-5", kvBase, 1000, 0),
		kvEvent("t", "s", "aws/claude-sonnet-5", kvBase+30_000, 1000, 0),
		kvEvent("t", "s", "aws/claude-opus-5", kvBase+90_000, 1000, 0),
	)
	rows, _, err := db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]*kvcache.Request{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	var opusFirst, sonnet *kvcache.Request
	for _, r := range rows {
		switch {
		case r.Model == "aws/claude-sonnet-5":
			sonnet = r
		case opusFirst == nil || r.TS < opusFirst.TS:
			opusFirst = r
		}
	}
	if opusFirst == nil || sonnet == nil {
		t.Fatal("the fixture did not produce one request per model")
	}
	// The opus request's successor is the LATER OPUS request, 90 s away — not the sonnet request
	// 30 s away, whose entry it could not have read.
	idle, ok := opusFirst.Idle()
	if !ok {
		t.Fatal("the first opus request has no successor at all")
	}
	if idle != 90*time.Second {
		t.Errorf("the opus request's idle gap is %v, want 90s. A %v gap means it was linked to the "+
			"sonnet request, whose cache entry it could never have matched — which grants a hit at "+
			"the read rate on an entry that does not exist.", idle, idle)
	}
	// And the sonnet request is the only one of its model, so it has no successor.
	if sonnet.HasNext {
		t.Error("the sonnet request was given a successor; it is the only request on its model")
	}
}

// The derived half of the dataset is computed TWICE, and only the copy that never runs is
// tested.
//
// scanKVCacheRequest fills HasNext, IdleMs, NextTS, NextID, Within5m and Within1h from the
// CTE's LEAD. kvcache.Derive fills the same six from a row's neighbour in the slice. The
// production path is the FORMER — Derive has no production caller at all, only test ones — and
// the boundary assertion lives on the latter: kvcache.TestFiveMinuteAndOneHourBoundariesAreExact
// pins Derive's `<=` and fires instantly when it is changed to `<`, while the same change to
// dash/kvcache.go's copy passes every test in both packages. A guard aimed at the dead twin of
// the live code is worse than no guard, because it reads as coverage.
//
// The four gaps below are the whole point: a `<` instead of a `<=` is invisible at any gap that
// is not EXACTLY a horizon, and both horizons are inclusive — a conversation that came back
// after exactly five minutes is within five minutes.
//
// It also pins the two implementations against each other, over an UNFILTERED read. They agree
// there, field for field. They legitimately DISAGREE over a filtered or capped read — measured
// at 3 of 4 rows with has_next=yes — because Derive reads a row's successor from the adjacent
// SLICE ELEMENT, so over a narrowed read it takes whichever row survived the filter as the
// successor and the real one as absent. That is why there are two implementations and why
// neither can be deleted in favour of the other: the window function saw the whole
// conversation, and Derive is the reference it is checked against.
func TestTheLiveDerivationIsExactAtBothHorizons(t *testing.T) {
	const m5, h1 = int64(300_000), int64(3_600_000)
	// One conversation per gap, so no row's successor can be another case's row.
	db := seedKV(t,
		kvEvent("t1", "exactly-5m", "m", kvBase, 0, 100_000),
		kvEvent("t1", "exactly-5m", "m", kvBase+m5, 100_000, 0),
		kvEvent("t1", "5m-plus-1ms", "m", kvBase, 0, 100_000),
		kvEvent("t1", "5m-plus-1ms", "m", kvBase+m5+1, 100_000, 0),
		kvEvent("t1", "exactly-1h", "m", kvBase, 0, 100_000),
		kvEvent("t1", "exactly-1h", "m", kvBase+h1, 100_000, 0),
		kvEvent("t1", "1h-plus-1ms", "m", kvBase, 0, 100_000),
		kvEvent("t1", "1h-plus-1ms", "m", kvBase+h1+1, 100_000, 0),
	)
	rows, _, err := db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The FIRST request of each conversation is the one with a gap; index by conversation.
	first := map[string]*kvcache.Request{}
	for _, r := range rows {
		if cur, ok := first[r.ConversationID]; !ok || r.TS < cur.TS {
			first[r.ConversationID] = r
		}
	}
	for _, tc := range []struct {
		conv               string
		idleMs             int64
		within5m, within1h bool
	}{
		{"exactly-5m", m5, true, true},
		{"5m-plus-1ms", m5 + 1, false, true},
		{"exactly-1h", h1, false, true},
		{"1h-plus-1ms", h1 + 1, false, false},
	} {
		r := first[tc.conv]
		if r == nil {
			t.Fatalf("%s: no row", tc.conv)
		}
		idle, ok := r.Idle()
		if !ok {
			t.Errorf("%s: the first request has no idle time", tc.conv)
			continue
		}
		if want := time.Duration(tc.idleMs) * time.Millisecond; idle != want {
			t.Errorf("%s: idle = %v, want %v", tc.conv, idle, want)
		}
		if r.Within5m != tc.within5m {
			t.Errorf("%s: a gap of %d ms has within_5m = %v, want %v. Both horizons are "+
				"INCLUSIVE: a conversation that came back after exactly five minutes came back "+
				"within five minutes.", tc.conv, tc.idleMs, r.Within5m, tc.within5m)
		}
		if r.Within1h != tc.within1h {
			t.Errorf("%s: a gap of %d ms has within_1h = %v, want %v", tc.conv, tc.idleMs,
				r.Within1h, tc.within1h)
		}
	}

	// The tie-break, which is a property of the CTE's ORDER BY rather than of the arithmetic:
	// 9 of the production corpus's consecutive pairs share a millisecond, and without `, r.id`
	// in the window's ORDER BY the successor of such a row is whichever the planner happened to
	// emit — so the same window derives two different datasets. The lower id comes first and the
	// gap is a real ZERO, not an absence.
	tied := seedKV(t,
		kvEvent("t1", "tied", "m", kvBase, 0, 100_000),
		kvEvent("t1", "tied", "m", kvBase, 100_000, 0),
	)
	tr, _, err := tied.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 2 {
		t.Fatalf("want 2 tied rows, got %d", len(tr))
	}
	sort.Slice(tr, func(i, j int) bool { return tr[i].ID < tr[j].ID })
	lo, hi := tr[0], tr[1]
	if !lo.HasNext || lo.NextID != hi.ID {
		t.Errorf("tied timestamps: the lower id (%d) must lead and point at %d, got next_id=%d "+
			"has_next=%v", lo.ID, hi.ID, lo.NextID, lo.HasNext)
	}
	if lo.NextTS != hi.TS {
		t.Errorf("tied timestamps: next_ts = %d, want %d", lo.NextTS, hi.TS)
	}
	if idle, ok := lo.Idle(); !ok || idle != 0 {
		t.Errorf("tied timestamps are a zero-length gap, not an absent one: idle=%v ok=%v",
			idle, ok)
	}
	if !lo.Within5m || !lo.Within1h {
		t.Error("a zero-length gap is within both horizons")
	}
	if hi.HasNext {
		t.Errorf("the higher id (%d) is last and has no successor", hi.ID)
	}

	// And the two implementations, field for field, over the UNFILTERED read.
	dead := make([]*kvcache.Request, len(rows))
	for i, r := range rows {
		c := *r
		c.HasNext, c.IdleMs, c.NextTS, c.NextID = false, nil, 0, 0
		c.Within5m, c.Within1h = false, false
		dead[i] = &c
	}
	kvcache.Derive(dead)
	byID := map[int64]*kvcache.Request{}
	for _, r := range dead {
		byID[r.ID] = r
	}
	for _, live := range rows {
		d := byID[live.ID]
		if d == nil {
			t.Fatalf("id %d vanished from Derive's output", live.ID)
		}
		li, lok := live.Idle()
		di, dok := d.Idle()
		if lok != dok || li != di || live.NextTS != d.NextTS || live.NextID != d.NextID ||
			live.Within5m != d.Within5m || live.Within1h != d.Within1h {
			t.Errorf("id %d: the SQL LEAD derived {idle=%v ok=%v next_ts=%d next_id=%d w5m=%v "+
				"w1h=%v}, kvcache.Derive derived {idle=%v ok=%v next_ts=%d next_id=%d w5m=%v "+
				"w1h=%v}. Over an unfiltered read the two must agree exactly.",
				live.ID, li, lok, live.NextTS, live.NextID, live.Within5m, live.Within1h,
				di, dok, d.NextTS, d.NextID, d.Within5m, d.Within1h)
		}
	}
}

// Every arm replays the SAME []*kvcache.Request, so an arm that wrote to one would score the
// arms after it against a dataset the arms before it never saw.
//
// kvcache.Simulate's doc says "It is not mutated". That is a comment, and the two arms that
// consume the whole slice at CONSTRUCTION time — observed-policy reads every row's tier,
// optimal groups every row by conversation and sorts each group — are exactly where an in-place
// sort or a written-back field would be easy to introduce and impossible to see: the arms are
// replayed in registry order, so the damage would land on whichever arm happens to come later
// and would move a cost, not raise an error.
//
// The reverse-order pass is the half that catches it. A snapshot comparison alone misses a
// mutation an arm makes and then undoes; running every arm again from last to first and
// requiring the same total is what makes contamination between arms observable. When it does
// fire, the FIRST arm named is the offender — the snapshot stays dirty, so every arm after it is
// named too.
func TestNoArmMutatesTheDatasetTheOtherArmsReplay(t *testing.T) {
	// A row that cached something at a tier NOBODY WROTE DOWN: observedTTL reports it as TTL5m
	// with TTLSourceUnknown, i.e. an ASSUMPTION the dataset carries. Without one of these in the
	// fixture an arm that stamped its assumption back onto the row — observed-policy is the arm
	// that reads the tier, so it is the one that would — mutates nothing here and this guard
	// passes over the exact write it exists to catch.
	unrecorded := kvEvent("t2", "s2", "aws/claude-opus-5", kvBase+250_000, 0, 70_000)
	unrecorded.CacheTTL = ""
	db := seedKV(t,
		kvEvent("t1", "s1", "aws/claude-opus-5", kvBase, 0, 150_000),
		kvEvent("t1", "s1", "aws/claude-opus-5", kvBase+60_000, 150_000, 0),
		// A model switch mid-conversation: two trajectories, not one, and the arm that groups by
		// conversation has to build its own slices to sort.
		kvEvent("t1", "s1", "aws/claude-sonnet-5", kvBase+120_000, 0, 90_000),
		kvEvent("t1", "s2", "aws/claude-opus-5", kvBase+200_000, 0, 40_000),
		kvEvent("t2", "s1", "aws/claude-opus-5", kvBase+300_000, 0, 220_000),
		unrecorded,
	)
	rows, _, err := db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The whole derived dataset as a comparable value, plus the slice's own order: an arm that
	// sorted it in place would leave every field intact and still change what the next arm
	// replays, because Simulate walks it as given.
	snapshot := func() (string, string) {
		var fields, order []byte
		for _, r := range rows {
			idle, ok := r.Idle()
			fields = fmt.Appendf(fields, "%d|%s|%s|%s|%d|%d|%d|%d|%v|%v|%d|%d|%v|%v|%s|%s|%v|%v ",
				r.ID, r.User, r.ConversationID, r.Model, r.TS, r.InputTokens, r.OutputTokens,
				r.CachedContext, idle, ok, r.NextTS, r.NextID, r.Within5m, r.Within1h, r.TTL,
				r.TTLSource, r.Hit, r.CostKnown)
			order = fmt.Appendf(order, "%d ", r.ID)
		}
		return string(fields), string(order)
	}
	wantFields, wantOrder := snapshot()

	prices := kvcache.NewPriceList(context.Background(), modelsOf(rows),
		staticPricer{ibmSonnet}, kvcache.Multipliers{}, nil)
	sim := kvcache.Config{Prices: prices, MaxPings: 2, WindowEnd: kvBase + 300_000}

	var names []string
	for _, arm := range KVCacheArms() {
		if arm.Selectable {
			names = append(names, arm.Name)
		}
	}
	if len(names) < 5 {
		t.Fatalf("only %d selectable arms; this guard is meant to cover all of them", len(names))
	}

	forward := map[string]float64{}
	for _, n := range names {
		s := buildStrategy(n, rows, KVCacheSimConfig{}, sim)
		if s == nil {
			t.Fatalf("%s did not build", n)
		}
		forward[n] = kvcache.Simulate(rows, s, sim).TotalUSD
		gotFields, gotOrder := snapshot()
		if gotFields != wantFields {
			t.Errorf("%s changed a field of the shared dataset", n)
		}
		if gotOrder != wantOrder {
			t.Errorf("%s reordered the shared slice: %q -> %q. Simulate walks it as given, so "+
				"every arm after this one replays a different history.", n, wantOrder, gotOrder)
		}
	}
	// Last to first. Any total that moves is one arm's run leaking into another's.
	for i := len(names) - 1; i >= 0; i-- {
		n := names[i]
		s := buildStrategy(n, rows, KVCacheSimConfig{}, sim)
		got := kvcache.Simulate(rows, s, sim).TotalUSD
		near2(t, n+" replayed last-to-first", got, forward[n])
	}
}

// Every group in the by-TTL table narrows to exactly the rows behind it, OVER HTTP.
//
// ttlGroupKey's doc promises this — "Doubles as the value that filters on that group, so
// clicking a row in the table narrows to exactly the rows behind it" — and it was false for two
// of the four groups. `ttl` was read into BOTH Filter.TTL (raw column) and KVCacheOptions.TTL
// (reconstructed tier), kvCacheQuery ANDed them, and the two vocabularies disagree precisely on
// the rows observedTTL exists to rescue: `unrecorded` became `cache_ttl = 'unrecorded'`, which
// nothing matches, so a group showing 295 requests drilled down to an empty table.
//
// TestTheTierFilterAgreesWithTheTierReconstruction already asserted the tier predicate is
// correct — at the DB layer, with Filter.TTL empty, which is the one place the defect could not
// appear. This one goes through the HANDLER, which is where the two readers met. The fixture
// covers all four groups, including a tier deduced from the provider's 1h write counter rather
// than recorded, because that is the row the raw-column predicate dropped.
func TestEveryTierGroupRoundTripsThroughItsOwnFilter(t *testing.T) {
	a, rec := newTestAPI(t, Options{})
	recorded5m := kvEvent("", "s1", "m", kvBase, 1000, 100)
	recorded1h := kvEvent("", "s2", "m", kvBase+1000, 1000, 100)
	recorded1h.CacheTTL = "ephemeral_1h"
	// No tier recorded, but the provider billed part of the write at the 1h tier — the ONLY
	// evidence a requested 1h was honoured. observedTTL calls this 1h; a raw-column predicate
	// cannot see it, which is the half of the defect that lost 37% of a real group.
	observed1h := kvEvent("", "s3", "m", kvBase+2000, 1000, 100)
	observed1h.CacheTTL, observed1h.CacheWrite1h = "", 100
	// Cached something at a tier nobody wrote down.
	unrecorded := kvEvent("", "s4", "m", kvBase+3000, 1000, 100)
	unrecorded.CacheTTL = ""
	// Carried no cache_control at all: nothing read, nothing written.
	uncached := kvEvent("", "s5", "m", kvBase+4000, 0, 0)
	uncached.CacheTTL = ""
	seed(t, rec, recorded5m, recorded1h, observed1h, unrecorded, uncached)

	_, body := get(t, a, "/api/kvcache", "")
	groups, _ := body["by_ttl"].([]any)
	if len(groups) < 4 {
		t.Fatalf("the fixture produced %d tier groups; it is built to produce four", len(groups))
	}
	var total float64
	for _, g := range groups {
		row, _ := g.(map[string]any)
		key, _ := row["key"].(string)
		want, _ := row["requests"].(float64)
		total += want
		// The group's own key, straight back through the handler as the filter value.
		_, page := get(t, a, "/api/kvcache/rows?limit=100&ttl="+key, "")
		got, _ := page["total"].(float64)
		if got != want {
			t.Errorf("the by-TTL table reports %.0f requests for tier %q and its own drill-down "+
				"returns %.0f. A group whose filter value does not select its own rows is a dead "+
				"link: on this page the reader clicks a count and gets an empty table.",
				want, key, got)
		}
		// And the analysis agrees with the table for the same filter.
		_, an := get(t, a, "/api/kvcache?ttl="+key, "")
		cards, _ := an["cards"].(map[string]any)
		if reqs, _ := cards["requests"].(float64); reqs != want {
			t.Errorf("tier %q: the analysis counts %.0f requests where the group says %.0f",
				key, reqs, want)
		}
	}
	if total != 5 {
		t.Errorf("the four tier groups hold %.0f of 5 rows; the tiers must partition the dataset",
			total)
	}
}

// An abandoned request stops reading.
//
// The analysis reads up to kvCacheMaxRows rows and builds a Request per row — measured at ~135 MB
// and several seconds at the ceiling. Without a context on the read, that work ran to completion
// after the client had gone: a request killed 1.5 s in still burned 7.1 s of CPU and allocated to
// the end, so a reader holding the refresh key committed the process's memory several times over
// and got none of it back. On a single-tenant deployment that needs no credential.
func TestAnAbandonedAnalysisStopsReading(t *testing.T) {
	db := seedKV(t,
		kvEvent("t", "s", "m", kvBase, 1000, 0),
		kvEvent("t", "s", "m", kvBase+10_000, 1000, 0),
	)
	// A handle with no context still works, which is what every in-process caller relies on.
	if _, _, err := db.KVCacheDataset(allTenants(), KVCacheOptions{}); err != nil {
		t.Fatalf("a handle with no context must read normally: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		run  func(*DB) error
	}{
		{"KVCacheDataset", func(d *DB) error { _, _, err := d.KVCacheDataset(allTenants(), KVCacheOptions{}); return err }},
		{"KVCacheRows", func(d *DB) error { _, err := d.KVCacheRows(allTenants(), KVCacheOptions{Limit: 10}); return err }},
		{"KVCacheModels", func(d *DB) error { _, err := d.KVCacheModels(allTenants()); return err }},
		{"KVCacheMedianPrefix", func(d *DB) error { _, err := d.KVCacheMedianPrefix(allTenants(), KVCacheOptions{}); return err }},
	} {
		if err := tc.run(db.WithContext(ctx)); err == nil {
			t.Errorf("%s completed against a cancelled context; the read is not cancellable, so "+
				"a request the caller has abandoned keeps costing the process", tc.name)
		}
	}
}

// Concurrent analyses are bounded, and a caller who has gone does not take a slot.
//
// kvCacheMaxRows bounds ONE request. Nothing bounded the number of them, and the store's pool has
// no SetMaxOpenConns, so under WAL every reader runs in parallel: eight concurrent analyses
// measured 1.65 GB resident, which OOMs a 2 GB container.
func TestConcurrentAnalysesAreBounded(t *testing.T) {
	if kvCacheMaxConcurrent < 1 {
		t.Fatalf("the bound is %d, which admits nothing", kvCacheMaxConcurrent)
	}
	// Fill it.
	for i := 0; i < kvCacheMaxConcurrent; i++ {
		if err := acquireKVCache(context.Background()); err != nil {
			t.Fatalf("could not take slot %d: %v", i, err)
		}
	}
	defer func() {
		for i := 0; i < kvCacheMaxConcurrent; i++ {
			releaseKVCache()
		}
	}()
	// A caller who has already gone must not wait for one, and must not take one.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := acquireKVCache(gone); err == nil {
		releaseKVCache()
		t.Error("a cancelled caller was given a slot; the queue would hand capacity to requests " +
			"whose answer nobody is waiting for")
	}
	// And a live caller gets one as soon as a slot frees.
	releaseKVCache()
	if err := acquireKVCache(context.Background()); err != nil {
		t.Errorf("a freed slot was not reusable: %v", err)
	}
}

// The cap trims to exactly kvCacheMaxRows, reports the TRUE total, and says it truncated.
//
// The count is now a second window-function pass that runs only when the read came back full — it
// was costing 0.76 s of a 7 s request to learn something the read already knew. That made the
// truncation branch the one that decides what `total` means, and it is unreachable in a test at
// the shipped ceiling, which is why kvCacheMaxRows is a var.
func TestTheCapTrimsAndStillReportsTheTrueTotal(t *testing.T) {
	var evs []*Event
	for i := 0; i < 9; i++ {
		evs = append(evs, kvEvent("t", "s", "m", kvBase+int64(i)*10_000, 1000, 0))
	}
	db := seedKV(t, evs...)
	restore := kvCacheMaxRows
	t.Cleanup(func() { kvCacheMaxRows = restore })

	kvCacheMaxRows = 4
	rows, total, err := db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Errorf("the read returned %d rows for a cap of 4; the row past the cap must be trimmed, "+
			"not returned", len(rows))
	}
	if total != 9 {
		t.Errorf("total = %d, want 9. Truncated, the total is the count of MATCHING rows, not of "+
			"returned ones — the pager and the truncation banner both divide by it.", total)
	}
	// Newest kept, oldest dropped: an analysis of the recent past is useful, one that stops in the
	// middle of the window is not.
	for _, r := range rows {
		if r.TS < kvBase+5*10_000 {
			t.Errorf("kept a row at ts=%d; the cap keeps the NEWEST rows", r.TS)
		}
	}
	an, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !an.Truncated || an.Scanned != 4 || an.Total != 9 {
		t.Errorf("truncated=%v scanned=%d total=%d; want true/4/9", an.Truncated, an.Scanned,
			an.Total)
	}
	// And exactly at the cap it is NOT truncated, which is the off-by-one the extra row exists to
	// get right.
	kvCacheMaxRows = 9
	rows, total, err = db.KVCacheDataset(allTenants(), KVCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 9 || total != 9 {
		t.Errorf("at a cap of exactly 9 rows: %d rows, total %d; want 9/9", len(rows), total)
	}
	if an2, err := db.KVCacheAnalyze(allTenants(), KVCacheOptions{}, nil, KVCacheSimConfig{}); err != nil {
		t.Fatal(err)
	} else if an2.Truncated {
		t.Error("a window of exactly the cap reported itself truncated")
	}
}
