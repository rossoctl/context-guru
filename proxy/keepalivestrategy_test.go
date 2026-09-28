package proxy

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/tenant"
)

// The resolution chain applyStrategy generalizes overrideFor into: account config, then
// the highest-priority matching ACTIVE strategy, then (elsewhere, in record) a session
// override on top. These tests are about the middle link and its precedence rule.

func TestApplyStrategyNoMatchLeavesPolicyUnchanged(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	account := CachePolicy{KeepAlive: false, Idle: 280 * time.Second, MaxPings: 2}
	got, applied := k.applyStrategy("t1", account, clock.now())
	if applied != "" {
		t.Errorf("applied = %q, want \"\" (no strategies at all)", applied)
	}
	if got != account {
		t.Errorf("policy changed with no strategy loaded: %+v", got)
	}
}

// A list-target strategy beats an all-target one, even when the all-target one is
// "more recent" — specificity is checked first and breaks the tie outright.
func TestApplyStrategyListBeatsAll(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	// "00:00".."23:59" is all day, and it now genuinely is: Window.contains treats both ends as
	// inclusive, so this covers 23:59 too. It did not before, which made this test fail for one
	// minute in 1440 — whenever CI ran at 23:59 in DefaultStrategyTZ. The boundary itself is
	// pinned at a fixed instant in tenant/keepalivewindow_test.go; this test still uses the real
	// clock because what it is about is the live control plane, not the calendar.
	window := []tenant.Window{{Start: "00:00", End: "24:00", Days: nil}}
	allTarget := tenant.Strategy{
		ID: "all", Active: true, Windows: window, Target: tenant.Target{Mode: tenant.TargetAll},
		IdleSeconds: 100, MaxPings: 1, UpdatedAt: now, // newer
	}
	listTarget := tenant.Strategy{
		ID: "list", Active: true, Windows: window,
		Target:      tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"t1"}},
		IdleSeconds: 200, MaxPings: 5, UpdatedAt: now.Add(-time.Hour), // older
	}
	k.setStrategies([]tenant.Strategy{allTarget, listTarget})

	account := CachePolicy{}
	got, applied := k.applyStrategy("t1", account, now)
	if applied != "list" {
		t.Fatalf("applied = %q, want %q (list-target beats all-target regardless of recency)", applied, "list")
	}
	if got.Idle != 200*time.Second || got.MaxPings != 5 {
		t.Errorf("policy did not come from the list-target strategy: %+v", got)
	}
	if !got.KeepAlive {
		t.Error("a matching strategy did not force KeepAlive on")
	}
}

// Among two equally-specific matches, the most recently UPDATED one wins — not the one
// that happens to sort first, and not the one that was created first.
func TestApplyStrategyRecencyBreaksTiesAmongEquallySpecificMatches(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	window := []tenant.Window{{Start: "00:00", End: "24:00"}}
	older := tenant.Strategy{
		ID: "older", Active: true, Windows: window, Target: tenant.Target{Mode: tenant.TargetAll},
		IdleSeconds: 100, MaxPings: 1, UpdatedAt: now.Add(-time.Hour),
	}
	newer := tenant.Strategy{
		ID: "newer", Active: true, Windows: window, Target: tenant.Target{Mode: tenant.TargetAll},
		IdleSeconds: 200, MaxPings: 2, UpdatedAt: now,
	}
	// Order in the slice must not matter: put the newer one first here and the older one
	// first in the sibling case below.
	k.setStrategies([]tenant.Strategy{newer, older})
	_, applied := k.applyStrategy("t1", CachePolicy{}, now)
	if applied != "newer" {
		t.Errorf("applied = %q, want %q", applied, "newer")
	}

	k.setStrategies([]tenant.Strategy{older, newer})
	_, applied = k.applyStrategy("t1", CachePolicy{}, now)
	if applied != "newer" {
		t.Errorf("applied = %q, want %q (order in the slice must not change the winner)", applied, "newer")
	}
}

// An inactive strategy, or one whose target excludes this tenant, or one whose window
// does not cover `now`, must not match — each checked independently.
func TestApplyStrategyRequiresActiveTargetAndWindow(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	base := tenant.Strategy{
		ID: "s", Active: true, IdleSeconds: 100, MaxPings: 1,
		Windows: []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
	}

	inactive := base
	inactive.Active = false
	k.setStrategies([]tenant.Strategy{inactive})
	if _, applied := k.applyStrategy("t1", CachePolicy{}, now); applied != "" {
		t.Errorf("an inactive strategy matched: %q", applied)
	}

	wrongTenant := base
	wrongTenant.Target = tenant.Target{Mode: tenant.TargetList, TenantIDs: []string{"other"}}
	k.setStrategies([]tenant.Strategy{wrongTenant})
	if _, applied := k.applyStrategy("t1", CachePolicy{}, now); applied != "" {
		t.Errorf("a strategy targeting another tenant matched: %q", applied)
	}

	outsideWindow := base
	outsideWindow.Windows = []tenant.Window{{Start: "00:00", End: "00:01"}}
	k.setStrategies([]tenant.Strategy{outsideWindow})
	if _, applied := k.applyStrategy("t1", CachePolicy{}, now); applied != "" {
		t.Errorf("a strategy outside its window matched: %q", applied)
	}
}

// A shadow-mode strategy matches (nothing about Matches changes) but never enforces: it
// must never turn KeepAlive on or touch any field of the policy.
func TestApplyStrategyShadowModeNeverEnforces(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	shadow := tenant.Strategy{
		ID: "s", Active: true, IdleSeconds: 100, MaxPings: 5, Mode: tenant.ModeShadow,
		Windows: []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
	}
	k.setStrategies([]tenant.Strategy{shadow})
	account := CachePolicy{KeepAlive: false, Idle: 7 * time.Second, MaxPings: 1}
	got, applied := k.applyStrategy("t1", account, now)
	if applied != "" {
		t.Errorf("a shadow strategy applied: %q", applied)
	}
	if got != account {
		t.Errorf("a shadow strategy changed the policy: %+v", got)
	}
	// An enforce-mode strategy matching the SAME tenant/window must win instead, proving
	// this is a mode check and not an accidental "never resolves at all" regression.
	enforce := shadow
	enforce.ID, enforce.Mode = "e", tenant.ModeEnforce
	k.setStrategies([]tenant.Strategy{shadow, enforce})
	if _, applied := k.applyStrategy("t1", account, now); applied != "e" {
		t.Errorf("applied = %q, want the enforce-mode sibling", applied)
	}
}

// A blank Mode (every strategy stored before this field existed, and every one built
// directly like the tests above) reads as enforcing — the DB column default and
// Strategy.Enforcing() must agree that "" means "enforce", never "shadow".
func TestApplyStrategyBlankModeStillEnforces(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	s := tenant.Strategy{
		ID: "s", Active: true, IdleSeconds: 100, MaxPings: 5,
		Windows: []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
	}
	k.setStrategies([]tenant.Strategy{s})
	if _, applied := k.applyStrategy("t1", CachePolicy{}, now); applied != "s" {
		t.Errorf("applied = %q, want %q (a blank Mode must enforce)", applied, "s")
	}
}

// The per-tenant off-switch: a tenant named in TenantCaps with Off excludes that ONE
// tenant from an otherwise-matching all-target strategy, without touching any other
// tenant it still covers.
func TestApplyStrategyPerTenantOffSwitch(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	s := tenant.Strategy{
		ID: "s", Active: true, IdleSeconds: 100, MaxPings: 5, Mode: tenant.ModeEnforce,
		Windows: []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
		TenantCaps: map[string]tenant.TenantCap{"loser": {Off: true}},
	}
	k.setStrategies([]tenant.Strategy{s})
	if _, applied := k.applyStrategy("loser", CachePolicy{}, now); applied != "" {
		t.Errorf("applyStrategy for the off-switched tenant = %q, want \"\"", applied)
	}
	if _, applied := k.applyStrategy("winner", CachePolicy{}, now); applied != "s" {
		t.Errorf("applyStrategy for an unmentioned tenant = %q, want %q — the off-switch must "+
			"not leak onto a tenant it does not name", applied, "s")
	}
}

// The per-tenant MaxPings override: a lower (or higher) ceiling for one tenant than the
// strategy's own MaxPings, without an Off entry.
func TestApplyStrategyPerTenantMaxPingsOverride(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	one := 1
	s := tenant.Strategy{
		ID: "s", Active: true, IdleSeconds: 100, MaxPings: 5, Mode: tenant.ModeEnforce,
		Windows: []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:  tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
		TenantCaps: map[string]tenant.TenantCap{"capped": {MaxPings: &one}},
	}
	k.setStrategies([]tenant.Strategy{s})
	got, applied := k.applyStrategy("capped", CachePolicy{}, now)
	if applied != "s" || got.MaxPings != 1 {
		t.Errorf("applyStrategy(capped) = %+v, %q; want MaxPings=1, %q", got, applied, "s")
	}
	got, applied = k.applyStrategy("uncapped", CachePolicy{}, now)
	if applied != "s" || got.MaxPings != 5 {
		t.Errorf("applyStrategy(uncapped) = %+v, %q; want the strategy's own MaxPings=5 unchanged", got, applied)
	}
}

// tenant.Strategy.MaxUSDPerTenant and .Models are ADVISORY — checked by dash's strategy
// preview against history, never by the live ping path (see both fields' own doc
// comments): enforcing either here would need the request's own model, or a running
// per-tenant dollar counter, threaded into proxy/keepalive.go's CachePolicy/kaEntry, which
// this file does not own.
//
// dash/spend_test.go's TestPerTenantRowQuotaIsEnforced exists because this exact failure
// mode already shipped once, on a different field: "a per-tenant quota ... was
// audit-logged, rendered in the UI, and read by nothing". This test is the same guard
// pointed the other way — it pins TODAY'S advisory status so a future half-wiring (someone
// starts reading one of these fields in one place but not proving it binds) goes red here
// and gets a decision, rather than shipping as a silent surprise.
func TestMaxUSDPerTenantAndModelsAreAdvisoryNotEnforced(t *testing.T) {
	// Structural: CachePolicy is the ONLY thing applyStrategy hands back to the ping path.
	// If it grows a field by either name, this fails immediately and by name — the
	// opposite of the silent drift a label in a JS string cannot prevent.
	cpType := reflect.TypeOf(CachePolicy{})
	for _, bad := range []string{"MaxUSDPerTenant", "Models"} {
		if _, ok := cpType.FieldByName(bad); ok {
			t.Errorf("CachePolicy now has a %q field. MaxUSDPerTenant/Models are advisory-only "+
				"by design (see tenant.Strategy's doc comments) — if this is intentional new "+
				"wiring, add a companion test proving it actually binds (the same way "+
				"dash/spend_test.go's TestPerTenantRowQuotaIsEnforced does) and update this test "+
				"and every doc comment/UI label that still calls the field advisory", bad)
		}
	}

	// Behavioural: two otherwise-identical strategies, one with a budget/model list that
	// would refuse a live tenant outright if either were actually consulted, resolve
	// IDENTICALLY through applyStrategy.
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	window := []tenant.Window{{Start: "00:00", End: "24:00"}}
	permissive := tenant.Strategy{
		ID: "permissive", Active: true, Windows: window, Target: tenant.Target{Mode: tenant.TargetAll},
		IdleSeconds: 100, MaxPings: 3, Mode: tenant.ModeEnforce, UpdatedAt: now,
	}
	restrictive := permissive
	restrictive.ID = "restrictive"
	// A budget far below what any real ping could cost, and a model this tenant could
	// never be on: if either were live-enforced, this strategy could not resolve the same
	// way permissive does.
	restrictive.MaxUSDPerTenant = 0.0000001
	restrictive.Models = []string{"some/model-this-tenant-never-uses"}

	for _, s := range []tenant.Strategy{permissive, restrictive} {
		k.setStrategies([]tenant.Strategy{s})
		got, applied := k.applyStrategy("t1", CachePolicy{}, now)
		if applied != s.ID {
			t.Fatalf("%s: applied = %q, want %q — MaxUSDPerTenant/Models must not affect "+
				"whether a strategy matches", s.ID, applied, s.ID)
		}
		if !got.KeepAlive || got.MaxPings != 3 {
			t.Errorf("%s: policy = %+v — a restrictive MaxUSDPerTenant/Models value changed the "+
				"resolved policy; if intentional, these fields are no longer advisory and every "+
				"doc comment and UI label calling them so needs updating alongside this test",
				s.ID, got)
		}
	}
}

func TestValidMode(t *testing.T) {
	if err := validMode(tenant.ModeShadow); err != nil {
		t.Errorf("ModeShadow refused: %v", err)
	}
	if err := validMode(tenant.ModeEnforce); err != nil {
		t.Errorf("ModeEnforce refused: %v", err)
	}
	for _, bad := range []string{"", "off", "ENFORCE"} {
		if err := validMode(bad); err == nil {
			t.Errorf("validMode(%q) = nil, want an error", bad)
		}
	}
}

func TestValidTenantCaps(t *testing.T) {
	if err := validTenantCaps(nil); err != nil {
		t.Errorf("a nil map was refused: %v", err)
	}
	zero := 0
	if err := validTenantCaps(map[string]tenant.TenantCap{"t1": {MaxPings: &zero}}); err != nil {
		t.Errorf("MaxPings=0 (meaning off) was refused: %v", err)
	}
	neg := -1
	if err := validTenantCaps(map[string]tenant.TenantCap{"t1": {MaxPings: &neg}}); err == nil {
		t.Error("a negative override was accepted")
	}
}

// The no-data-fallback predictor variant: an empty stop_reason proceeds (unlike the plain
// stop-reason-gated entry, which refuses it), and a real, unfavourable stop_reason still
// refuses exactly as the plain entry does.
func TestPredictorForProceedOnNoData(t *testing.T) {
	p, ok := predictorFor("stop-reason-gated-proceed-on-no-data")
	if !ok {
		t.Fatal("stop-reason-gated-proceed-on-no-data is not a known predictor")
	}
	if got := p(""); got != 1.0 {
		t.Errorf("p(\"\") = %v, want 1.0 (proceed on no data)", got)
	}
	if got := p("end_turn"); got != 1.0 {
		t.Errorf("p(%q) = %v, want 1.0", "end_turn", got)
	}
	if got := p("tool_use"); got != 0.0 {
		t.Errorf("p(%q) = %v, want 0.0 (a real, unfavourable stop_reason still refuses)", "tool_use", got)
	}
}

// The resolution chain's top rule: a strategy sits ABOVE account config and BELOW a
// session override. The override still wins on top for the fields it moves, and still
// cannot widen MaxUSDPerPing — which the strategy, unlike an override, is allowed to set.
func TestSessionOverrideStillWinsOverAMatchingStrategy(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	strategy := tenant.Strategy{
		ID: "biz-hours", Active: true, IdleSeconds: 150, MaxPings: 3, MinPrefixTokens: 5000,
		MaxUSDPerPing: 0.5,
		Windows:       []tenant.Window{{Start: "00:00", End: "24:00"}},
		Target:        tenant.Target{Mode: tenant.TargetAll}, UpdatedAt: now,
	}
	k.setStrategies([]tenant.Strategy{strategy})

	account := CachePolicy{KeepAlive: false, Idle: 999 * time.Second, MaxPings: 99}
	afterStrategy, applied := k.applyStrategy("t1", account, now)
	if applied != "biz-hours" {
		t.Fatalf("applied = %q, want %q", applied, "biz-hours")
	}
	if afterStrategy.Idle != 150*time.Second || afterStrategy.MaxPings != 3 ||
		afterStrategy.MinPrefixTokens != 5000 || afterStrategy.MaxUSDPerPing != 0.5 {
		t.Fatalf("strategy did not resolve onto the policy: %+v", afterStrategy)
	}

	armOn(t, k, "sess-1", 60*time.Second, 7, now.Add(time.Hour))
	final := k.overrideFor("t1", "sess-1", afterStrategy)
	if final.Idle != 60*time.Second || final.MaxPings != 7 {
		t.Errorf("the session override did not win on top of the strategy: %+v", final)
	}
	if final.MaxUSDPerPing != 0.5 {
		t.Errorf("the override moved MaxUSDPerPing to %v; it may not widen a strategy's ceiling "+
			"any more than it may widen the account's own", final.MaxUSDPerPing)
	}
	if !final.KeepAlive {
		t.Error("the resolved policy is not on")
	}
}

// validStrategyBounds enforces the SAME numeric bounds validOverride does, at least as
// tightly, plus its own check on MaxUSDPerPing (which an override does not expose) and,
// when headTTL1h is set, the wider 1h-tier idle/ping band instead of the 5m one.
func TestValidStrategyBounds(t *testing.T) {
	cases := []struct {
		name             string
		idle             time.Duration
		pings            int
		minPrefix        int
		maxUSDPerPing    float64
		headTTL1h        bool
		headTTLMinTokens int
		ok               bool
	}{
		{"in bounds", 280 * time.Second, 2, 20000, 0.25, false, 0, true},
		{"idle too low", minOverrideIdle - time.Second, 2, 0, 0, false, 0, false},
		{"idle too high", maxOverrideIdle + time.Second, 2, 0, 0, false, 0, false},
		{"idle at floor", minOverrideIdle, 2, 0, 0, false, 0, true},
		{"idle at ceiling", maxOverrideIdle, 2, 0, 0, false, 0, true},
		{"pings too low", 280 * time.Second, minOverridePings - 1, 0, 0, false, 0, false},
		{"pings too high", 280 * time.Second, maxOverridePings + 1, 0, 0, false, 0, false},
		{"negative prefix", 280 * time.Second, 2, -1, 0, false, 0, false},
		{"negative ceiling", 280 * time.Second, 2, 0, -0.01, false, 0, false},
		{"zero ceiling means default, and is fine", 280 * time.Second, 2, 0, 0, false, 0, true},
		{"negative head-ttl-min-tokens", 280 * time.Second, 2, 0, 0, false, -1, false},

		// The 1h-tier band, only reachable while headTTL1h is set. A positive
		// headTTLMinTokens (50000, mirroring config.DefaultHeadTTLMinTokens) is used
		// throughout so each case isolates the ONE bound it's actually testing —
		// idle/pings — rather than also tripping the separate zero-token-floor refusal
		// below.
		{"1h idle beyond the 5m ceiling is fine under the 1h band",
			maxOverrideIdle + time.Second, 2, 0, 0, true, 50000, true},
		{"1h idle at its own ceiling", maxOverrideIdle1h, 2, 0, 0, true, 50000, true},
		{"1h idle past its own ceiling", maxOverrideIdle1h + time.Second, 2, 0, 0, true, 50000, false},
		{"1h idle below the shared floor", minOverrideIdle - time.Second, 2, 0, 0, true, 50000, false},
		{"1h pings at its own ceiling", 3360 * time.Second, maxOverridePings1h, 0, 0, true, 50000, true},
		{"1h pings beyond its own ceiling but under the 5m one",
			3360 * time.Second, maxOverridePings1h + 1, 0, 0, true, 50000, false},
		{"1h negative head-ttl-min-tokens", 3360 * time.Second, 2, 0, 0, true, -1, false},
		// The new refusal this test data exists to exercise: headTTL1h=true paired
		// with a zero (not merely non-negative) token floor is invalid on its own,
		// independent of every other bound — see validStrategyBounds' own doc comment
		// on why 0 there is worse than merely "unset."
		{"1h zero head-ttl-min-tokens is invalid on its own", 3360 * time.Second, 2, 0, 0, true, 0, false},
		{"5m zero head-ttl-min-tokens is fine (headTTL1h is off)",
			280 * time.Second, 2, 0, 0, false, 0, true},
	}
	for _, c := range cases {
		err := validStrategyBounds(c.idle, c.pings, c.minPrefix, c.maxUSDPerPing,
			c.headTTL1h, c.headTTLMinTokens)
		if (err == nil) != c.ok {
			t.Errorf("%s: validStrategyBounds() = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// knownPredictorIDs and predictorFor are two separate lists by construction (a map a
// strategy is validated against, a switch pingable() actually evaluates), and nothing
// forces them to agree except this test — the exact drift a Strategy naming a "known"
// predictor pingable() cannot actually run would produce.
func TestKnownPredictorIDsMatchPredictorFor(t *testing.T) {
	for id := range knownPredictorIDs {
		if _, ok := predictorFor(id); !ok {
			t.Errorf("%q is in knownPredictorIDs but predictorFor does not recognise it", id)
		}
	}
	for _, id := range []string{"stop-reason-gated", "not-a-real-one"} {
		_, wantOK := knownPredictorIDs[id]
		_, gotOK := predictorFor(id)
		if gotOK != wantOK {
			t.Errorf("predictorFor(%q) ok=%v, knownPredictorIDs says %v", id, gotOK, wantOK)
		}
	}
}

// applyStrategy must carry PredictorID/PredictorThreshold from the matched strategy onto
// the resolved policy — the one thing that makes the whole opt-in gate reachable at all.
func TestApplyStrategyCopiesPredictorFields(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	s := tenant.Strategy{
		ID: "s1", Active: true, Target: tenant.Target{Mode: tenant.TargetAll},
		Windows:     []tenant.Window{{Start: "00:00", End: "24:00"}},
		IdleSeconds: 280, MaxPings: 1,
		PredictorID:        "stop-reason-gated",
		PredictorThreshold: 0.5,
	}
	k.setStrategies([]tenant.Strategy{s})
	got, applied := k.applyStrategy("t1", CachePolicy{}, now)
	if applied != "s1" {
		t.Fatalf("applied = %q, want %q", applied, "s1")
	}
	if got.PredictorID != "stop-reason-gated" || got.PredictorThreshold != 0.5 {
		t.Errorf("predictor fields did not carry through: %+v", got)
	}
}

// resolveHeadTTL is the request-path counterpart to applyStrategy: same matching walk,
// but for the write-tier half of the policy rather than the ping-scheduling half.

func TestResolveHeadTTLNoMatchReturnsTheAccountFallbackUnchanged(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	on, tokens := k.resolveHeadTTL("t1", clock.now(), true, 12345)
	if !on || tokens != 12345 {
		t.Errorf("resolveHeadTTL with no strategies = (%v, %d), want the account's own (true, 12345)",
			on, tokens)
	}
	on, tokens = k.resolveHeadTTL("t1", clock.now(), false, 0)
	if on || tokens != 0 {
		t.Errorf("resolveHeadTTL with no strategies = (%v, %d), want the account's own (false, 0)",
			on, tokens)
	}
}

func TestResolveHeadTTLMatchingStrategyOverridesTheAccount(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	s := tenant.Strategy{
		ID: "s1", Active: true, Target: tenant.Target{Mode: tenant.TargetAll},
		Windows:     []tenant.Window{{Start: "00:00", End: "24:00"}},
		IdleSeconds: 280, MaxPings: 1,
		HeadTTL1h:        true,
		HeadTTLMinTokens: 50000,
	}
	k.setStrategies([]tenant.Strategy{s})
	on, tokens := k.resolveHeadTTL("t1", now, false, 0)
	if !on || tokens != 50000 {
		t.Errorf("resolveHeadTTL = (%v, %d), want the matched strategy's (true, 50000)", on, tokens)
	}
}

// A strategy can only turn HeadTTL1h ON, never off — matching applyStrategy's own
// pol.KeepAlive = true. A matching strategy with HeadTTL1h false must not suppress an
// account that already opted itself in.
func TestResolveHeadTTLCannotTurnTheAccountsOwnSettingOff(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	s := tenant.Strategy{
		ID: "s1", Active: true, Target: tenant.Target{Mode: tenant.TargetAll},
		Windows:     []tenant.Window{{Start: "00:00", End: "24:00"}},
		IdleSeconds: 280, MaxPings: 1, HeadTTL1h: false,
	}
	k.setStrategies([]tenant.Strategy{s})
	on, tokens := k.resolveHeadTTL("t1", now, true, 50000)
	if !on || tokens != 50000 {
		t.Errorf("resolveHeadTTL = (%v, %d), want the account's own (true, 50000) left alone", on, tokens)
	}
}

// A nil keeper (keepalive not built into this deployment at all) must behave exactly like
// "no strategy matched" — the account's own setting, unchanged.
func TestResolveHeadTTLNilKeeperFallsBackToTheAccount(t *testing.T) {
	var k *keeper
	on, tokens := k.resolveHeadTTL("t1", time.Now(), true, 777)
	if !on || tokens != 777 {
		t.Errorf("resolveHeadTTL on a nil keeper = (%v, %d), want the account's own (true, 777)",
			on, tokens)
	}
}

func TestValidPredictorRef(t *testing.T) {
	if err := validPredictorRef("", 0); err != nil {
		t.Errorf("no predictor named (the default) was refused: %v", err)
	}
	if err := validPredictorRef("", 99); err != nil {
		t.Errorf("an unused threshold with no predictor named was refused: %v", err)
	}
	if err := validPredictorRef("stop_reason", 0.5); err == nil {
		t.Error("an unregistered predictor id was accepted; knownPredictorIDs is " +
			"deliberately empty until the runtime gating hook exists")
	}
	if err := validPredictorRef("", -0.5); err != nil {
		t.Errorf("an out-of-range threshold with no predictor named was refused: %v", err)
	}
	if orig := knownPredictorIDs["stop_reason"]; !orig {
		knownPredictorIDs["stop_reason"] = true
		defer delete(knownPredictorIDs, "stop_reason")
	}
	if err := validPredictorRef("stop_reason", 1.5); err == nil {
		t.Error("a registered predictor with an out-of-range threshold was accepted")
	}
	if err := validPredictorRef("stop_reason", 0.5); err != nil {
		t.Errorf("a registered predictor with a valid threshold was refused: %v", err)
	}
	// A named predictor with a threshold <= 0 makes predictorFor's gate a no-op — every
	// possible prediction (predictorFor only ever returns values in [0,1]) satisfies
	// ">= 0", so the strategy would ping unconditionally while still being labeled and
	// billed as predictor-gated.
	if err := validPredictorRef("stop_reason", 0); err == nil {
		t.Error("a registered predictor with a zero threshold was accepted; the gate would be a no-op")
	}
	if err := validPredictorRef("stop_reason", -0.1); err == nil {
		t.Error("a registered predictor with a negative threshold was accepted")
	}
}

// A strategy created through the control route is visible to the keeper's own
// resolution on the very next call, with no restart — "no re-push needed, since matching
// is live" — and a manager-only gate refuses a plain user. Update and delete take
// effect just as immediately.
func TestKeepAliveStrategyControlRoutesResolveLiveWithNoRestart(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	userJar, userID := f.signUpJar(t, "user@ibm.com")

	// A plain user may not create one.
	//
	// mode:"enforce" is explicit here: a strategy created with no mode at all now defaults
	// to shadow (opt-in enforcement, see tenant.ModeShadow), and this test is about the
	// live resolution chain, not about that default.
	body := `{"name":"biz hours","idle_seconds":280,"max_pings":2,"min_prefix_tokens":20000,
		"max_usd_per_ping":0.25,"active":true,"mode":"enforce",
		"windows":[{"start":"00:00","end":"23:59"}],"target":{"mode":"all"}}`
	w, _ := f.do(t, http.MethodPost, "/api/keepalive/strategies", body, userJar)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a plain user's create = %d, want 403: %s", w.Code, w.Body)
	}

	w, out := f.do(t, http.MethodPost, "/api/keepalive/strategies", body, mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %v", out)
	}

	// Live in the keeper right now, with no restart: an all-target strategy that is
	// active and covers this instant matches any tenant, including the one that just
	// signed up.
	if _, applied := f.h.keeper.applyStrategy(userID, CachePolicy{}, time.Now()); applied != id {
		t.Errorf("applyStrategy after create = %q, want %q", applied, id)
	}

	// The audit trail names the strategy by its id in the field column, actor and target
	// both the manager's own id.
	entries, err := f.reg.Audit("", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Field == id && e.After == "created: biz hours" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit row named the new strategy by id: %+v", entries)
	}

	// PATCH turns it off; the very next resolution stops matching.
	w, _ = f.do(t, http.MethodPatch, "/api/keepalive/strategies/"+id, `{"active":false}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200: %s", w.Code, w.Body)
	}
	if _, applied := f.h.keeper.applyStrategy(userID, CachePolicy{}, time.Now()); applied != "" {
		t.Errorf("a deactivated strategy still applied: %q", applied)
	}

	// PATCH turns it back on; the list route reports it as such.
	w, _ = f.do(t, http.MethodPatch, "/api/keepalive/strategies/"+id, `{"active":true}`, mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("re-activating patch = %d, want 200: %s", w.Code, w.Body)
	}
	w, out = f.do(t, http.MethodGet, "/api/keepalive/strategies", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %s", w.Code, w.Body)
	}
	list, _ := out["strategies"].([]any)
	if len(list) != 1 {
		t.Fatalf("list has %d strategies, want 1: %v", len(list), out)
	}
	row, _ := list[0].(map[string]any)
	if inWindow, _ := row["in_window"].(bool); !inWindow {
		t.Errorf("a strategy whose window covers all day is not reported in_window: %v", row)
	}

	// DELETE stops it matching immediately, and it disappears from the list.
	w, _ = f.do(t, http.MethodDelete, "/api/keepalive/strategies/"+id, "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200: %s", w.Code, w.Body)
	}
	if _, applied := f.h.keeper.applyStrategy(userID, CachePolicy{}, time.Now()); applied != "" {
		t.Errorf("a deleted strategy still applied: %q", applied)
	}
	w, out = f.do(t, http.MethodGet, "/api/keepalive/strategies", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("list after delete = %d, want 200: %s", w.Code, w.Body)
	}
	if list, _ := out["strategies"].([]any); len(list) != 0 {
		t.Errorf("deleted strategy still listed: %v", list)
	}
}

// Validation runs at the control-route level too: an out-of-bounds idle interval and a
// window with no schedule are both refused with a 400, and nothing is created.
func TestKeepAliveStrategyCreateValidatesAtTheRoute(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")

	tooFast := `{"name":"x","idle_seconds":1,"max_pings":2,"windows":[{"start":"00:00","end":"23:59"}],
		"target":{"mode":"all"}}`
	w, _ := f.do(t, http.MethodPost, "/api/keepalive/strategies", tooFast, mgrJar)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an idle interval below the floor = %d, want 400: %s", w.Code, w.Body)
	}

	noWindows := `{"name":"x","idle_seconds":280,"max_pings":2,"windows":[],"target":{"mode":"all"}}`
	w, _ = f.do(t, http.MethodPost, "/api/keepalive/strategies", noWindows, mgrJar)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a strategy with no windows = %d, want 400: %s", w.Code, w.Body)
	}

	list, err := f.reg.ListStrategies()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("a refused create nonetheless persisted %d strategies", len(list))
	}
}

// A create request that names no mode at all comes back shadow — opt-in enforcement for
// anything new — and does NOT resolve live, unlike
// TestKeepAliveStrategyControlRoutesResolveLiveWithNoRestart's explicit mode:"enforce".
func TestKeepAliveStrategyCreateDefaultsToShadow(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	_, userID := f.signUpJar(t, "user@ibm.com")

	body := `{"name":"biz hours","idle_seconds":280,"max_pings":2,
		"windows":[{"start":"00:00","end":"23:59"}],"target":{"mode":"all"}}`
	w, out := f.do(t, http.MethodPost, "/api/keepalive/strategies", body, mgrJar)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	if mode, _ := out["mode"].(string); mode != tenant.ModeShadow {
		t.Errorf("mode in the create response = %q, want %q", mode, tenant.ModeShadow)
	}
	if enforcing, _ := out["enforcing"].(bool); enforcing {
		t.Error("a strategy created with no mode reports enforcing:true")
	}
	if _, applied := f.h.keeper.applyStrategy(userID, CachePolicy{}, time.Now()); applied != "" {
		t.Errorf("a shadow-by-default strategy applied live: %q", applied)
	}
}

// The predictor catalog lists the baseline and both validated rules as selectable, and (when
// kvcache ships a compiled-in reuse model) the trained one as visible but not selectable.
func TestCtlListKeepAlivePredictors(t *testing.T) {
	f := newMgrFixture(t)
	mgrJar, _ := f.signUpJar(t, "boss@ibm.com")
	userJar, _ := f.signUpJar(t, "user@ibm.com")

	if w, _ := f.do(t, http.MethodGet, "/api/keepalive/predictors", "", userJar); w.Code != http.StatusForbidden {
		t.Fatalf("a plain user's list = %d, want 403", w.Code)
	}

	w, out := f.do(t, http.MethodGet, "/api/keepalive/predictors", "", mgrJar)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %s", w.Code, w.Body)
	}
	entries, _ := out["predictors"].([]any)
	byID := map[string]map[string]any{}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		id, _ := m["id"].(string)
		byID[id] = m
	}
	for _, id := range []string{"", "stop-reason-gated", "stop-reason-gated-proceed-on-no-data"} {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("no catalog entry for %q: %v", id, entries)
		}
		if sel, _ := e["selectable_for_enforcement"].(bool); !sel {
			t.Errorf("%q is not selectable_for_enforcement: %v", id, e)
		}
		if val, _ := e["validated"].(bool); !val {
			t.Errorf("%q is not validated: %v", id, e)
		}
	}
	for _, e := range entries {
		if kind, _ := e.(map[string]any)["kind"].(string); kind == "trained" {
			if sel, _ := e.(map[string]any)["selectable_for_enforcement"].(bool); sel {
				t.Errorf("a trained/research entry is selectable_for_enforcement: %v", e)
			}
		}
	}
}

func TestValidWindowsRequiresAtLeastOne(t *testing.T) {
	if err := validWindows(nil); err == nil {
		t.Error("a strategy with no windows was accepted; it can never fire")
	}
	if err := validWindows([]tenant.Window{{Start: "09:00", End: "18:00"}}); err != nil {
		t.Errorf("a single valid window was refused: %v", err)
	}
	if err := validWindows([]tenant.Window{{Start: "18:00", End: "09:00"}}); err == nil {
		t.Error("an overnight window was accepted")
	}
}
