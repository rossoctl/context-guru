package proxy

import (
	"net/http"
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
	window := []tenant.Window{{Start: "00:00", End: "23:59", Days: nil}}
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
	window := []tenant.Window{{Start: "00:00", End: "23:59"}}
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
		Windows: []tenant.Window{{Start: "00:00", End: "23:59"}},
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

// The resolution chain's top rule: a strategy sits ABOVE account config and BELOW a
// session override. The override still wins on top for the fields it moves, and still
// cannot widen MaxUSDPerPing — which the strategy, unlike an override, is allowed to set.
func TestSessionOverrideStillWinsOverAMatchingStrategy(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	strategy := tenant.Strategy{
		ID: "biz-hours", Active: true, IdleSeconds: 150, MaxPings: 3, MinPrefixTokens: 5000,
		MaxUSDPerPing: 0.5,
		Windows:       []tenant.Window{{Start: "00:00", End: "23:59"}},
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
// tightly, plus its own check on MaxUSDPerPing (which an override does not expose).
func TestValidStrategyBounds(t *testing.T) {
	cases := []struct {
		name          string
		idle          time.Duration
		pings         int
		minPrefix     int
		maxUSDPerPing float64
		ok            bool
	}{
		{"in bounds", 280 * time.Second, 2, 20000, 0.25, true},
		{"idle too low", minOverrideIdle - time.Second, 2, 0, 0, false},
		{"idle too high", maxOverrideIdle + time.Second, 2, 0, 0, false},
		{"idle at floor", minOverrideIdle, 2, 0, 0, true},
		{"idle at ceiling", maxOverrideIdle, 2, 0, 0, true},
		{"pings too low", 280 * time.Second, minOverridePings - 1, 0, 0, false},
		{"pings too high", 280 * time.Second, maxOverridePings + 1, 0, 0, false},
		{"negative prefix", 280 * time.Second, 2, -1, 0, false},
		{"negative ceiling", 280 * time.Second, 2, 0, -0.01, false},
		{"zero ceiling means default, and is fine", 280 * time.Second, 2, 0, 0, true},
	}
	for _, c := range cases {
		err := validStrategyBounds(c.idle, c.pings, c.minPrefix, c.maxUSDPerPing)
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
		Windows:     []tenant.Window{{Start: "00:00", End: "23:59"}},
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
	body := `{"name":"biz hours","idle_seconds":280,"max_pings":2,"min_prefix_tokens":20000,
		"max_usd_per_ping":0.25,"active":true,
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
