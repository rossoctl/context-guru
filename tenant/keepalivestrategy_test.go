package tenant

import (
	"path/filepath"
	"testing"
	"time"
)

// jerusalem loads the zone once per test file; a failure here means the tzdata import in
// keepalivestrategy.go did not do its job.
func jerusalem(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Fatalf("Asia/Jerusalem did not load: %v", err)
	}
	return loc
}

// Israel's DST law: clocks spring forward at 02:00 on the Friday before the last Sunday of
// March. In 2026 that is Friday, March 27 — 02:00 IST jumps straight to 03:00 IDT, so the
// wall-clock hour 02:00-02:59 does not exist that day. A window whose matching is done in
// UTC, or with a fixed offset, gets this wrong on both sides of the jump; one done through
// time.LoadLocation does not, because it re-resolves the offset for the instant it is given
// rather than assuming yesterday's.
func TestWindowMatchesAcrossTheIsraeliDSTTransition(t *testing.T) {
	loc := jerusalem(t)
	// 01:30 local, still IST (UTC+2) — five minutes before the jump would begin.
	beforeJump := time.Date(2026, 3, 27, 1, 30, 0, 0, loc)
	// 03:30 local, now IDT (UTC+3) — thirty minutes after the jump. The clock never read
	// 02:xx at all on this day.
	afterJump := time.Date(2026, 3, 27, 3, 30, 0, 0, loc)

	early := Window{Days: []time.Weekday{time.Friday}, Start: "01:00", End: "02:00", TZ: "Asia/Jerusalem"}
	late := Window{Days: []time.Weekday{time.Friday}, Start: "03:00", End: "04:00", TZ: "Asia/Jerusalem"}

	if ok, err := early.contains(beforeJump); err != nil || !ok {
		t.Errorf("early.contains(beforeJump) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := early.contains(afterJump); err != nil || ok {
		t.Errorf("early.contains(afterJump) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := late.contains(beforeJump); err != nil || ok {
		t.Errorf("late.contains(beforeJump) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := late.contains(afterJump); err != nil || !ok {
		t.Errorf("late.contains(afterJump) = %v, %v; want true, nil", ok, err)
	}
}

// A window with no Days matches every day of the week.
func TestWindowWithNoDaysMatchesEveryDay(t *testing.T) {
	loc := jerusalem(t)
	w := Window{Start: "09:00", End: "18:00", TZ: "Asia/Jerusalem"}
	for _, day := range []int{24, 25, 26, 27, 28, 29, 30} { // a full week, March 2026
		now := time.Date(2026, 3, day, 12, 0, 0, 0, loc)
		if ok, err := w.contains(now); err != nil || !ok {
			t.Errorf("day %d: contains = %v, %v; want true, nil", day, ok, err)
		}
	}
}

// A window's End must be strictly after Start; an overnight window is out of scope.
func TestWindowValidateRejectsOvernightAndBadShapes(t *testing.T) {
	cases := []struct {
		name string
		w    Window
		ok   bool
	}{
		{"ordinary", Window{Start: "09:00", End: "18:00", TZ: "Asia/Jerusalem"}, true},
		{"overnight", Window{Start: "22:00", End: "06:00", TZ: "Asia/Jerusalem"}, false},
		{"equal", Window{Start: "09:00", End: "09:00", TZ: "Asia/Jerusalem"}, false},
		{"bad start", Window{Start: "9:00", End: "18:00", TZ: "Asia/Jerusalem"}, false},
		{"bad minute", Window{Start: "09:60", End: "18:00", TZ: "Asia/Jerusalem"}, false},
		{"bad tz", Window{Start: "09:00", End: "18:00", TZ: "Nowhere/Fake"}, false},
		{"default tz", Window{Start: "09:00", End: "18:00"}, true},
		{"bad day", Window{Start: "09:00", End: "18:00", Days: []time.Weekday{7}}, false},
	}
	for _, c := range cases {
		err := c.w.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestTargetValidate(t *testing.T) {
	if err := (Target{Mode: TargetAll}).Validate(); err != nil {
		t.Errorf("all-target refused: %v", err)
	}
	if err := (Target{Mode: TargetList, TenantIDs: []string{"t1"}}).Validate(); err != nil {
		t.Errorf("list-target with a tenant refused: %v", err)
	}
	if err := (Target{Mode: TargetList}).Validate(); err == nil {
		t.Error("an empty list-target was accepted; it matches nothing, which is never what was meant")
	}
	if err := (Target{Mode: "bogus"}).Validate(); err == nil {
		t.Error("an unknown target mode was accepted")
	}
}

func TestValidatePredictor(t *testing.T) {
	if err := (Strategy{}).ValidatePredictor(); err != nil {
		t.Errorf("no predictor named (the default) was refused: %v", err)
	}
	if err := (Strategy{PredictorID: "stop_reason", PredictorThreshold: 0}).ValidatePredictor(); err != nil {
		t.Errorf("a threshold of exactly 0 was refused: %v", err)
	}
	if err := (Strategy{PredictorID: "stop_reason", PredictorThreshold: 1}).ValidatePredictor(); err != nil {
		t.Errorf("a threshold of exactly 1 was refused: %v", err)
	}
	if err := (Strategy{PredictorID: "stop_reason", PredictorThreshold: -0.01}).ValidatePredictor(); err == nil {
		t.Error("a negative threshold was accepted")
	}
	if err := (Strategy{PredictorID: "stop_reason", PredictorThreshold: 1.01}).ValidatePredictor(); err == nil {
		t.Error("a threshold above 1 was accepted")
	}
	// A nonsense threshold left over from a previous edit is not an error when no
	// predictor is actually named -- it is unused either way.
	if err := (Strategy{PredictorThreshold: 99}).ValidatePredictor(); err != nil {
		t.Errorf("an unused threshold with no predictor named was refused: %v", err)
	}
}

func TestStrategyMatchesRequiresActiveTargetAndWindow(t *testing.T) {
	loc := jerusalem(t)
	inWindow := time.Date(2026, 6, 1, 12, 0, 0, 0, loc) // a Monday, no DST edge involved
	s := Strategy{
		Active: true,
		Windows: []Window{
			{Start: "09:00", End: "18:00", TZ: "Asia/Jerusalem"},
		},
		Target: Target{Mode: TargetList, TenantIDs: []string{"t1"}},
	}
	if !s.Matches(inWindow, "t1") {
		t.Error("an active, in-window, correctly-targeted strategy did not match")
	}
	if s.Matches(inWindow, "t2") {
		t.Error("a list-target strategy matched a tenant not on its list")
	}
	off := s
	off.Active = false
	if off.Matches(inWindow, "t1") {
		t.Error("an inactive strategy matched")
	}
	outside := time.Date(2026, 6, 1, 3, 0, 0, 0, loc)
	if s.Matches(outside, "t1") {
		t.Error("a strategy matched outside every one of its windows")
	}
}

// Persistence across a simulated restart: strategies survive a Close and a fresh Open of
// the same file, unlike a per-session override (which is deliberately memory-only).
func TestStrategyPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant.db")

	r1, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := r1.CreateStrategy("mgr-1", Strategy{
		Name: "business hours", IdleSeconds: 280, MaxPings: 2, MinPrefixTokens: 20000,
		MaxUSDPerPing: 0.25, Active: true,
		Windows: []Window{{Start: "09:00", End: "18:00", TZ: "Asia/Jerusalem"}},
		Target:  Target{Mode: TargetAll},
	})
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if err := r1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer r2.Close()
	list, err := r2.ListStrategies()
	if err != nil {
		t.Fatalf("ListStrategies after reload: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d strategies after reload, want 1", len(list))
	}
	got := list[0]
	if got.ID != created.ID || got.Name != "business hours" || got.IdleSeconds != 280 ||
		got.MaxPings != 2 || !got.Active || len(got.Windows) != 1 ||
		got.Windows[0].Start != "09:00" || got.Target.Mode != TargetAll {
		t.Errorf("reloaded strategy does not match what was created: %+v", got)
	}
}

// HeadTTL1h/HeadTTLMinTokens round-trip through create, a sparse patch, and a reload —
// the same three checks TestStrategyCRUD and TestStrategyPersistsAcrossReload already run
// for the older fields, extended to the two this migration added.
func TestStrategyHeadTTLFieldsPersistAndPatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant.db")
	r1, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := r1.CreateStrategy("mgr-1", Strategy{
		Name: "1h head", IdleSeconds: 280, MaxPings: 1, HeadTTL1h: true, HeadTTLMinTokens: 50000,
		Windows: []Window{{Start: "09:00", End: "18:00"}},
		Target:  Target{Mode: TargetAll},
	})
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if !created.HeadTTL1h || created.HeadTTLMinTokens != 50000 {
		t.Errorf("create did not round-trip head-TTL fields: %+v", created)
	}

	off := false
	updated, err := r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{HeadTTL1h: &off})
	if err != nil {
		t.Fatalf("UpdateStrategy: %v", err)
	}
	if updated.HeadTTL1h {
		t.Error("patch did not turn head_ttl_1h off")
	}
	if updated.HeadTTLMinTokens != 50000 {
		t.Errorf("patch touched a field it was not given: HeadTTLMinTokens = %d", updated.HeadTTLMinTokens)
	}

	newTokens := 12345
	updated, err = r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{HeadTTLMinTokens: &newTokens})
	if err != nil {
		t.Fatalf("UpdateStrategy: %v", err)
	}
	if updated.HeadTTLMinTokens != newTokens {
		t.Errorf("patch did not update HeadTTLMinTokens: got %d, want %d", updated.HeadTTLMinTokens, newTokens)
	}
	if err := r1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer r2.Close()
	reloaded, err := r2.StrategyByID(created.ID)
	if err != nil {
		t.Fatalf("StrategyByID after reload: %v", err)
	}
	if reloaded.HeadTTL1h || reloaded.HeadTTLMinTokens != newTokens {
		t.Errorf("reload lost the patched head-TTL fields: %+v", reloaded)
	}
}

// Create/Update/Delete round trip, including that a delete stops it matching and a
// not-found id is reported distinctly.
func TestStrategyCRUD(t *testing.T) {
	r := open(t, Options{})
	s, err := r.CreateStrategy("mgr-1", Strategy{
		Name: "s1", IdleSeconds: 280, MaxPings: 1,
		Windows: []Window{{Start: "09:00", End: "18:00"}},
		Target:  Target{Mode: TargetAll},
	})
	if err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	newName := "s1-renamed"
	newPings := 3
	updated, err := r.UpdateStrategy("mgr-2", s.ID, StrategyPatch{Name: &newName, MaxPings: &newPings})
	if err != nil {
		t.Fatalf("UpdateStrategy: %v", err)
	}
	if updated.Name != newName || updated.MaxPings != newPings || updated.UpdatedBy != "mgr-2" {
		t.Errorf("update did not apply: %+v", updated)
	}
	if updated.IdleSeconds != 280 {
		t.Errorf("update touched a field it was not given: IdleSeconds = %d", updated.IdleSeconds)
	}
	if err := r.DeleteStrategy(s.ID); err != nil {
		t.Fatalf("DeleteStrategy: %v", err)
	}
	if _, err := r.StrategyByID(s.ID); err != ErrNoStrategy {
		t.Errorf("StrategyByID after delete = %v, want ErrNoStrategy", err)
	}
	if err := r.DeleteStrategy(s.ID); err != ErrNoStrategy {
		t.Errorf("second delete = %v, want ErrNoStrategy", err)
	}
}

// OneHourOnWait (#1243) round-trips through create, a sparse patch and a reload.
func TestStrategyOneHourOnWaitPersistsAndPatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenant.db")
	r1, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := r1.CreateStrategy("mgr-1", Strategy{
		Name: "1h on wait", IdleSeconds: 240, MaxPings: 2, OneHourOnWait: true,
		Windows: []Window{{Start: "00:00", End: "24:00"}}, Target: Target{Mode: TargetAll},
	})
	if err != nil || !created.OneHourOnWait {
		t.Fatalf("create did not keep one_hour_on_wait: %+v %v", created, err)
	}
	off := false
	if u, err := r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{OneHourOnWait: &off}); err != nil || u.OneHourOnWait {
		t.Fatalf("patch did not turn it off: %+v %v", u, err)
	}
	on := true
	if _, err := r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{OneHourOnWait: &on}); err != nil {
		t.Fatal(err)
	}
	// A patch that names another field leaves it alone.
	name := "renamed"
	if u, _ := r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{Name: &name}); !u.OneHourOnWait {
		t.Error("a patch of the name reset one_hour_on_wait")
	}
	r1.Close()
	r2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	list, err := r2.ListStrategies()
	if err != nil || len(list) != 1 || !list[0].OneHourOnWait {
		t.Fatalf("after reload: %+v %v", list, err)
	}
}

// OneHourAlways (#1261, mode C) round-trips through create, a sparse patch and a reload, like OneHourOnWait.
func TestStrategyOneHourAlwaysPersistsAndPatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenant.db")
	r1, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := r1.CreateStrategy("mgr-1", Strategy{
		Name: "1h always", IdleSeconds: 240, MaxPings: 2, OneHourAlways: true,
		Windows: []Window{{Start: "00:00", End: "24:00"}}, Target: Target{Mode: TargetAll},
	})
	if err != nil || !created.OneHourAlways || created.OneHourOnWait {
		t.Fatalf("create did not keep one_hour_always alone: %+v %v", created, err)
	}
	off := false
	if u, err := r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{OneHourAlways: &off}); err != nil || u.OneHourAlways {
		t.Fatalf("patch did not turn it off: %+v %v", u, err)
	}
	on := true
	if _, err := r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{OneHourAlways: &on}); err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	if u, _ := r1.UpdateStrategy("mgr-2", created.ID, StrategyPatch{Name: &name}); !u.OneHourAlways {
		t.Error("a patch of the name reset one_hour_always")
	}
	r1.Close()
	r2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	list, err := r2.ListStrategies()
	if err != nil || len(list) != 1 || !list[0].OneHourAlways {
		t.Fatalf("after reload: %+v %v", list, err)
	}
}
