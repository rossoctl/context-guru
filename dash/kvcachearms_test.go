package dash

import (
	"context"
	"reflect"
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
