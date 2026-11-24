package campaign

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/tenant"
)

// The 1h-tier arms must carry a POSITIVE headTTLMinTokens, not just a non-negative one:
// 0 paired with HeadTTL1h=true silently disables the head-TTL upgrade downstream
// (apply.Opts' own gate is `HeadTTL1h && HeadTTLMinTokens > 0`) while the ping schedule
// still runs on the 1-hour cadence — paying for a tier that can never actually apply.
func TestCampaignArmForKeepAlive1hArmsHaveAPositiveHeadTTLMinTokens(t *testing.T) {
	for _, arm := range []string{kvcache.StrategyKeepAlive1h, kvcache.StrategyKeepAlive1hOnce} {
		a := ArmFor(arm)
		if !a.HeadTTL1h {
			t.Fatalf("%s: expected headTTL1h true", arm)
		}
		if a.HeadTTLMinTokens <= 0 {
			t.Errorf("%s: headTTLMinTokens = %d, want a positive value", arm, a.HeadTTLMinTokens)
		}
	}
}

// Baseline arms mean "change nothing" and create no strategy; unknown arms are refused
// with a reason, never silently treated as a baseline.
func TestCampaignArmForBaselineAndUnknown(t *testing.T) {
	for _, arm := range []string{kvcache.StrategyNoCache, kvcache.StrategyFixed5m, kvcache.StrategyFixed1h} {
		a := ArmFor(arm)
		if !a.Activatable || !a.Baseline {
			t.Errorf("%s: got %+v, want activatable baseline", arm, a)
		}
	}
	for _, arm := range []string{
		kvcache.StrategyHistorical, kvcache.StrategyStickySession1h, kvcache.StrategyObserved,
		kvcache.StrategyExtend1h, kvcache.StrategyOptimal, kvcache.StrategyReplay, "nonsense-arm",
	} {
		a := ArmFor(arm)
		if a.Activatable || a.Reason == "" {
			t.Errorf("%s: got %+v, want not activatable with a reason", arm, a)
		}
	}
}

func TestResolveCampaignCellUserExclusionIsTheCallersJob(t *testing.T) {
	// ResolveCell itself does not special-case an empty user — the create
	// handler filters those out before ever calling it. This test documents that the
	// function still resolves such a cell like any other, so a reviewer does not go
	// looking for a guard inside it that was deliberately placed one level up.
	cell := Cell{User: "", HourUTC: 9, BestStrategy: kvcache.StrategyKeepAlive5m}
	got := ResolveCell(cell, func(string) bool { return true })
	if got.TenantID != "" || !got.Activatable {
		t.Errorf("got %+v, want an activatable cell for the empty tenant (exclusion happens elsewhere)", got)
	}
}

func TestResolveCampaignCellNonActivatableArmRecordsAReason(t *testing.T) {
	cell := Cell{User: "t1", HourUTC: 9, BestStrategy: kvcache.StrategyHistorical}
	got := ResolveCell(cell, func(string) bool { return true })
	if got.Activatable || got.SkipReason == "" {
		t.Errorf("got %+v, want not activatable with a reason", got)
	}
}

func TestResolveCampaignCellGatesThe1hTierPerTenantModel(t *testing.T) {
	cell := Cell{User: "t1", HourUTC: 9, BestStrategy: kvcache.StrategyKeepAlive1h}
	honored := ResolveCell(cell, func(tenantID string) bool { return tenantID == "t1" })
	if !honored.Activatable {
		t.Errorf("got %+v, want activatable when the tenant's model honors the 1h tier", honored)
	}
	notHonored := ResolveCell(cell, func(tenantID string) bool { return false })
	if notHonored.Activatable || notHonored.SkipReason == "" {
		t.Errorf("got %+v, want not activatable with a reason when the model gate fails", notHonored)
	}
}

// A cell whose winner was picked on a subset of its own traffic must not activate a live
// schedule. BestStrategy is an argmax over the PRICED requests only, so unpriced traffic in the
// cell means the arm about to be enforced won a comparison the rest of that cell never entered.
// This gate is the one dash.KVCacheSuggestion.UnpricedRequests exists to make possible: before
// that field, nothing on this path could see the condition at all.
func TestResolveCampaignCellRefusesACellWhoseArmWasPickedOnPricedRowsOnly(t *testing.T) {
	partial := Cell{User: "t1", HourUTC: 9, Requests: 36,
		UnpricedRequests: 20, BestStrategy: kvcache.StrategyStopReasonGated}
	got := ResolveCell(partial, func(string) bool { return true })
	if got.Activatable || got.SkipReason == "" {
		t.Errorf("got %+v, want not activatable with a Reason: 20 of 36 requests unpriced means "+
			"stop-reason-gated (which buys pings) won on 16 of them", got)
	}
	// Fully priced, everything else equal: still activatable, so the gate narrows rather than
	// closes the path.
	full := partial
	full.UnpricedRequests = 0
	if got := ResolveCell(full, func(string) bool { return true }); !got.Activatable {
		t.Errorf("got %+v, want activatable once every request in the cell is priced", got)
	}
	// Baseline arms are exempt for the same reason InsufficientData exempts them: enforcing
	// "change nothing" needs no evidence about which arm won.
	base := partial
	base.BestStrategy = kvcache.StrategyFixed5m
	if got := ResolveCell(base, func(string) bool { return true }); !got.Activatable {
		t.Errorf("got %+v, want a baseline arm to stay activatable regardless of coverage", got)
	}
}

func TestResolveCampaignCellBaselineArmIsActivatableWithNoConfig(t *testing.T) {
	cell := Cell{User: "t1", HourUTC: 9, BestStrategy: kvcache.StrategyFixed5m}
	got := ResolveCell(cell, func(string) bool { return true })
	if !got.Activatable || !got.Config.Baseline {
		t.Errorf("got %+v, want an activatable baseline cell", got)
	}
}

// An (tenant, hour) an active campaign already enforces must not be enforced a second
// time: only one of the two strategies can win the resolution chain, so the loser's
// campaign would report a prediction for a policy that never actually ran.
func TestMarkCampaignOverlapsSkipsHoursAnActiveCampaignAlreadyHolds(t *testing.T) {
	held := []tenant.CampaignCellOwner{
		{TenantID: "t1", HourUTC: 9, CampaignID: "c-old", CampaignName: "morning push"},
	}
	taken := &Resolved{TenantID: "t1", HourUTC: 9, Activatable: true,
		Config: ArmFor(kvcache.StrategyKeepAlive5m)}
	// Same hour, different tenant — no conflict: a strategy's Target names accounts, so
	// t2's hour 9 is not the hour t1's strategy holds.
	otherTenant := &Resolved{TenantID: "t2", HourUTC: 9, Activatable: true,
		Config: ArmFor(kvcache.StrategyKeepAlive5m)}
	// Same tenant, different hour — no conflict either.
	otherHour := &Resolved{TenantID: "t1", HourUTC: 10, Activatable: true,
		Config: ArmFor(kvcache.StrategyKeepAlive5m)}
	// A baseline cell creates no strategy, so it cannot collide with one.
	baseline := &Resolved{TenantID: "t1", HourUTC: 9, Activatable: true,
		Config: ArmFor(kvcache.StrategyFixed5m)}

	MarkOverlaps([]*Resolved{taken, otherTenant, otherHour, baseline}, held)

	if taken.Activatable || taken.SkipReason == "" {
		t.Errorf("held cell = %+v, want not activatable with a reason", taken)
	}
	if !strings.Contains(taken.SkipReason, "morning push") {
		t.Errorf("skip reason %q does not name the campaign holding the hour", taken.SkipReason)
	}
	for _, c := range []*Resolved{otherTenant, otherHour, baseline} {
		if !c.Activatable {
			t.Errorf("cell %+v was skipped, want still activatable", c)
		}
	}
}

// A DIFFERENT arm for an already-held hour is still a conflict, not a refinement — see
// MarkOverlaps' own doc comment. Pinned because "the arms differ, so let it
// through" is the plausible-looking relaxation that would silently restore two competing
// strategies for one hour.
func TestMarkCampaignOverlapsIsBlindToTheArm(t *testing.T) {
	held := []tenant.CampaignCellOwner{
		{TenantID: "t1", HourUTC: 9, CampaignID: "c-old", CampaignName: "old"},
	}
	differentArm := &Resolved{TenantID: "t1", HourUTC: 9, Activatable: true,
		Arm: kvcache.StrategyStopReasonGated, Config: ArmFor(kvcache.StrategyStopReasonGated)}
	MarkOverlaps([]*Resolved{differentArm}, held)
	if differentArm.Activatable {
		t.Errorf("got %+v, want not Activatable: a different arm for a held hour is still a conflict",
			differentArm)
	}
}

// An out-of-range HourUTC must never reach TileHours: the live source can never
// produce one (always time.Time.UTC().Hour()), but the upload source accepts a
// hand-edited payload verbatim, and TileHours would otherwise build an invalid Window
// string ("24:00", "-1:00") that gets persisted successfully but can never match
// anything — a strategy that silently pings and saves nothing forever with no error
// anywhere. Checked BEFORE the arm is even resolved, so it applies no matter how
// enforceable the recommended arm would otherwise be.
func TestResolveCampaignCellRejectsOutOfRangeHourUTC(t *testing.T) {
	for _, hour := range []int{-1, 24, 30, 100} {
		cell := Cell{User: "t1", HourUTC: hour, BestStrategy: kvcache.StrategyKeepAlive5m}
		got := ResolveCell(cell, func(string) bool { return true })
		if got.Activatable || got.SkipReason == "" {
			t.Errorf("hour %d: got %+v, want not activatable with a reason", hour, got)
		}
	}
	// The boundary values must still work.
	for _, hour := range []int{0, 23} {
		cell := Cell{User: "t1", HourUTC: hour, BestStrategy: kvcache.StrategyKeepAlive5m}
		got := ResolveCell(cell, func(string) bool { return true })
		if !got.Activatable {
			t.Errorf("hour %d: got %+v, want activatable", hour, got)
		}
	}
}

// A thin cell (too few requests to trust its winning arm as a pattern) must not become
// a real, live strategy for a non-baseline arm — the suggest engine's own
// InsufficientData flag exists specifically to warn against acting on it.
func TestResolveCampaignCellInsufficientDataBlocksActivationForNonBaselineArms(t *testing.T) {
	cell := Cell{
		User: "t1", HourUTC: 9, BestStrategy: kvcache.StrategyKeepAlive5m, InsufficientData: true,
	}
	got := ResolveCell(cell, func(string) bool { return true })
	if got.Activatable || got.SkipReason == "" {
		t.Errorf("got %+v, want not activatable with a reason", got)
	}
}

// A baseline arm ("change nothing") is exempt from the insufficient-data gate: doing
// nothing carries no risk regardless of sample size, unlike committing to a real
// ping/write-tier schedule off a thin backtest.
func TestResolveCampaignCellInsufficientDataDoesNotBlockBaselineArms(t *testing.T) {
	cell := Cell{
		User: "t1", HourUTC: 9, BestStrategy: kvcache.StrategyFixed5m, InsufficientData: true,
	}
	got := ResolveCell(cell, func(string) bool { return true })
	if !got.Activatable || !got.Config.Baseline {
		t.Errorf("got %+v, want an activatable baseline cell despite insufficient data", got)
	}
}

// WeekdaysFromNames must never silently produce an empty slice: tenant.Window treats
// an empty Days as "every day matches," not "no day restricted," and PredictedUSD is
// always backtested against Sunday-Thursday only — an omitted or fully-unparseable
// Weekdays list must fall back to that canonical set, never to "every day."
func TestWeekdaysFromNamesFallsBackToCanonicalWeekWhenNothingIsRecognized(t *testing.T) {
	for _, names := range [][]string{nil, {}, {"Someday", "Otherday"}} {
		got := WeekdaysFromNames(names)
		if len(got) != len(DefaultWeekdays) {
			t.Errorf("WeekdaysFromNames(%v) = %v, want the canonical %v", names, got, DefaultWeekdays)
			continue
		}
		for i, d := range DefaultWeekdays {
			if got[i] != d {
				t.Errorf("WeekdaysFromNames(%v) = %v, want the canonical %v", names, got, DefaultWeekdays)
				break
			}
		}
	}
}

// TileHours must cover exactly the given hours — no gap inside a merged run, no
// overlap or false coverage between separate runs, and hour 23 must never merge past
// midnight into hour 0 (tenant.Window has no overnight span).
func TestTileHoursCoversExactlyItsHoursNoGapNoOverlap(t *testing.T) {
	days := []time.Weekday{time.Sunday}
	windows := TileHours([]int{9, 10, 14, 23}, days)
	if len(windows) != 3 {
		t.Fatalf("got %d windows, want 3 (9-10 merged, 14 alone, 23 alone): %+v", len(windows), windows)
	}
	byStart := map[string]struct{ start, end string }{}
	for _, w := range windows {
		byStart[w.Start] = struct{ start, end string }{w.Start, w.End}
	}
	if w, ok := byStart["09:00"]; !ok || w.end != "11:00" {
		t.Errorf("hours 9-10 = %+v, want [09:00,11:00)", byStart["09:00"])
	}
	if w, ok := byStart["14:00"]; !ok || w.end != "15:00" {
		t.Errorf("hour 14 = %+v, want [14:00,15:00)", byStart["14:00"])
	}
	// Hour 23 ends at the END-OF-DAY sentinel, so the hour it tiles is covered in full. It used
	// to end at "23:59", which as an exclusive end left 23:59:00-23:59:59 covered by nothing —
	// this test asserted that gap as though it were a requirement, and it was only a limitation.
	w23, ok := byStart["23:00"]
	if !ok || w23.end != "24:00" {
		t.Fatalf("hour 23 = %+v, want [23:00,24:00) so the hour is covered in full", w23)
	}

	// Every window must itself be valid, and coverage must match exactly: every
	// minute inside a tiled hour is covered, the hole at 11:00-13:59 is covered by
	// NEITHER window, and no window reaches past its own hour into the next.
	loc, _ := time.LoadLocation("UTC")
	for _, w := range windows {
		w.TZ = "UTC"
		if err := w.Validate(); err != nil {
			t.Errorf("generated window %+v is invalid: %v", w, err)
		}
	}
	// Window.contains is unexported; go through Strategy.InWindow (exported) instead,
	// wrapping each generated window on its own in an always-active, all-target
	// strategy so only the window's OWN time span is under test.
	check := func(hour, minute int, wantCovered bool) {
		now := time.Date(2026, 6, 7, hour, minute, 0, 0, loc) // a Sunday
		covered := false
		for _, w := range windows {
			w.TZ, w.Days = "UTC", nil // isolate the time check from the day check
			s := tenant.Strategy{Active: true, Windows: []tenant.Window{w},
				Target: tenant.Target{Mode: tenant.TargetAll}}
			if s.InWindow(now) {
				covered = true
			}
		}
		if covered != wantCovered {
			t.Errorf("%02d:%02d covered=%v, want %v", hour, minute, covered, wantCovered)
		}
	}
	check(9, 0, true)
	check(10, 59, true)
	check(11, 0, false) // the hole between the merged run and hour 14
	check(13, 59, false)
	check(14, 0, true)
	check(14, 59, true)
	check(15, 0, false)
	check(23, 0, true)
	check(23, 59, true) // hour 23 means all of hour 23, 23:59 included
	check(0, 0, false)  // hour 23 must not wrap into hour 0
}

func TestTileHoursSingleHourEndingTheDay(t *testing.T) {
	windows := TileHours([]int{23}, nil)
	if len(windows) != 1 || windows[0].Start != "23:00" || windows[0].End != "24:00" {
		t.Errorf("got %+v, want a single [23:00,24:00) window — the whole of hour 23", windows)
	}
	// And it must actually cover the final minute, which is the point of the sentinel.
	s := tenant.Strategy{Active: true, Windows: []tenant.Window{{
		Start: windows[0].Start, End: windows[0].End, TZ: "UTC"}},
		Target: tenant.Target{Mode: tenant.TargetAll}}
	if !s.InWindow(time.Date(2026, 6, 7, 23, 59, 30, 0, time.UTC)) {
		t.Error("a [23:00,24:00) window does not cover 23:59; the end-of-day sentinel is broken")
	}
}

func TestWeekdaysFromNamesDropsUnrecognizedNamesRatherThanErroring(t *testing.T) {
	got := WeekdaysFromNames([]string{"Sunday", "Monday", "Someday"})
	if len(got) != 2 || got[0] != time.Sunday || got[1] != time.Monday {
		t.Errorf("got %v, want [Sunday, Monday]", got)
	}
}

// Two tenants sharing the same arm and the SAME hour set coalesce into one group; two
// tenants sharing the same arm but a DIFFERENT hour set must NOT be merged — a
// strategy's Target and Windows both apply to the whole strategy, so merging them
// would activate an hour for a tenant that was never recommended for it.
func TestCoalesceCampaignCellsGroupsOnlyIdenticalHourSets(t *testing.T) {
	arm5m := ArmFor(kvcache.StrategyKeepAlive5m)
	mk := func(tenantID string, hour int) *Resolved {
		return &Resolved{TenantID: tenantID, HourUTC: hour, Arm: kvcache.StrategyKeepAlive5m,
			Activatable: true, Config: arm5m}
	}
	cells := []*Resolved{
		mk("t1", 9), mk("t2", 9), // t1 and t2 share the exact same hour set {9}
		mk("t3", 9), mk("t3", 14), // t3 has a DIFFERENT hour set {9,14}
	}
	groups := Coalesce(cells)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2 (t1+t2 together, t3 alone): %+v", len(groups), groups)
	}
	var sharedGroup, soloGroup *Group
	for _, g := range groups {
		if len(g.TenantIDs) == 2 {
			sharedGroup = g
		} else {
			soloGroup = g
		}
	}
	if sharedGroup == nil || sharedGroup.TenantIDs[0] != "t1" || sharedGroup.TenantIDs[1] != "t2" {
		t.Errorf("shared group = %+v, want t1 and t2", sharedGroup)
	}
	if soloGroup == nil || len(soloGroup.TenantIDs) != 1 || soloGroup.TenantIDs[0] != "t3" ||
		len(soloGroup.HourSet) != 2 {
		t.Errorf("solo group = %+v, want t3 alone with 2 hours", soloGroup)
	}
}

func TestCoalesceCampaignCellsExcludesBaselineAndNonActivatable(t *testing.T) {
	cells := []*Resolved{
		{TenantID: "t1", HourUTC: 9, Arm: kvcache.StrategyFixed5m, Activatable: true,
			Config: Arm{Baseline: true}},
		{TenantID: "t1", HourUTC: 10, Arm: kvcache.StrategyHistorical, Activatable: false},
	}
	groups := Coalesce(cells)
	if len(groups) != 0 {
		t.Errorf("got %d groups, want 0 (nothing here is activatable-and-non-baseline)", len(groups))
	}
}

// keepalive-5m carries the recommended policy (240 s, K=2, 80k floor); the K=1 and gated arms
// stay at 280 because the K>=2 evidence covers K>=2 only.
func TestCampaignKeepAlive5mIsTheRecommendedPolicy(t *testing.T) {
	a := ArmFor(kvcache.StrategyKeepAlive5m)
	if a.IdleSeconds != 240 || a.MaxPings != 2 || config.DefaultKeepAliveMinPrefix != 80000 {
		t.Errorf("keepalive-5m = idle %d, K %d, floor %d; want 240, 2, 80000",
			a.IdleSeconds, a.MaxPings, config.DefaultKeepAliveMinPrefix)
	}
	if got := ArmFor(kvcache.StrategyKeepAlive5mOnce).IdleSeconds; got != 280 {
		t.Errorf("keepalive-5m-once idle = %d, want 280", got)
	}
}

// once, stop-reason-gated and the 1h arms keep 280 s / the old 20k floor: the 80k
// evidence is K=2 ping-everyone only.
func TestCampaignNonRecommendedArmsKeepTheirOldFloor(t *testing.T) {
	for _, arm := range []string{kvcache.StrategyKeepAlive5mOnce, kvcache.StrategyStopReasonGated} {
		a := ArmFor(arm)
		if a.IdleSeconds != 280 || a.MinPrefixTokens != 20000 {
			t.Errorf("%s = idle %d floor %d, want 280 / 20000", arm, a.IdleSeconds, a.MinPrefixTokens)
		}
	}
	if a := ArmFor(kvcache.StrategyKeepAlive5m); a.MinPrefixTokens != 80000 {
		t.Errorf("keepalive-5m floor = %d, want 80000", a.MinPrefixTokens)
	}
}

// The suggester's cells are UTC hours on UTC weekdays, so the generated windows must say so:
// left unset, Window.TZ resolves to DefaultStrategyTZ (Asia/Jerusalem) and the campaign
// enforces every cell 2-3 hours early. This is the generated windows UNMODIFIED, unlike the
// tests above that overwrite TZ before checking.
func TestTileHoursWindowsAreUTC(t *testing.T) {
	days := []time.Weekday{time.Sunday}
	s := tenant.Strategy{Active: true, Windows: TileHours([]int{8}, days),
		Target: tenant.Target{Mode: tenant.TargetAll}}
	at := func(h int) bool { return s.InWindow(time.Date(2026, 6, 7, h, 30, 0, 0, time.UTC)) } // a Sunday
	if !at(8) {
		t.Error("08:30 UTC is inside cell hour 8 but not covered")
	}
	if at(5) {
		t.Error("05:30 UTC (08:30 Asia/Jerusalem in summer) must not be covered by cell hour 8")
	}
	// Late-evening cell: UTC Sunday 22h must stay on UTC Sunday, not roll to Jerusalem Monday.
	s.Windows = TileHours([]int{22}, days)
	if !s.InWindow(time.Date(2026, 6, 7, 22, 30, 0, 0, time.UTC)) ||
		s.InWindow(time.Date(2026, 6, 8, 22, 30, 0, 0, time.UTC)) {
		t.Error("cell hour 22 on UTC Sunday must cover Sunday 22:30Z and not Monday 22:30Z")
	}
}

// The zone survives the JSON round trip the strategy takes through storage, validates, and a
// UTC cell hour is covered at the same UTC instant on both sides of Asia/Jerusalem's DST changes.
func TestTileHoursWindowsUTCSurviveStorageAndDST(t *testing.T) {
	days := []time.Weekday{time.Friday, time.Saturday, time.Sunday, time.Monday, time.Tuesday}
	raw, err := json.Marshal(TileHours([]int{8}, days))
	if err != nil {
		t.Fatal(err)
	}
	var ws []tenant.Window
	if err := json.Unmarshal(raw, &ws); err != nil {
		t.Fatal(err)
	}
	s := tenant.Strategy{Active: true, Windows: ws, Target: tenant.Target{Mode: tenant.TargetAll}}
	for _, w := range ws {
		if w.TZ != "UTC" || w.Validate() != nil {
			t.Fatalf("window after round trip = %+v (err %v), want TZ UTC and valid", w, w.Validate())
		}
	}
	// Asia/Jerusalem DST: 2026-03-27 (spring forward) and 2026-10-25 (fall back); dates are Fri/Sun.
	for _, d := range []time.Time{
		time.Date(2026, 3, 27, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC),
	} {
		if !s.InWindow(d.Add(8*time.Hour+30*time.Minute)) || s.InWindow(d.Add(7*time.Hour+30*time.Minute)) ||
			s.InWindow(d.Add(9*time.Hour+30*time.Minute)) {
			t.Errorf("%s: cell hour 8 must cover exactly 08:00-09:00 UTC", d.Format("2006-01-02"))
		}
	}
}

// A tenant honors the 1h tier only when it ran models and every one of them is measured to honor it. No models is "not confirmed".
func TestHonorsHeadTTL1hFailsClosed(t *testing.T) {
	for _, c := range []struct {
		models []string
		want   bool
	}{{nil, false}, {[]string{}, false}, {[]string{"aws/claude-haiku-4-5"}, true}, {[]string{"aws/claude-haiku-4-5", "aws/claude-opus-4"}, false}, {[]string{"aws/claude-opus-4"}, false}} {
		if got := HonorsHeadTTL1h(c.models); got != c.want {
			t.Errorf("HonorsHeadTTL1h(%v) = %v, want %v", c.models, got, c.want)
		}
	}
}

// The outcome a campaign gives a cell, and the cell it records, carry the same fields the create route used to copy by hand.
func TestCellOutcomeAndTenantCell(t *testing.T) {
	cell := Cell{User: "t1", HourUTC: 9, Requests: 500, BestStrategy: kvcache.StrategyKeepAlive5m, SavingUSD: 12, BaselineUSD: 100, OptimalSavingUSD: 20}
	if ok, why := CellOutcome(cell, func(string) bool { return false }); !ok || why != "" {
		t.Fatalf("a keepalive-5m cell: ok %v reason %q", ok, why)
	}
	cell.UnpricedRequests = 3
	if ok, why := CellOutcome(cell, nil); ok || why == "" {
		t.Errorf("a cell with unpriced rows: ok %v reason %q, want a skip with a reason", ok, why)
	}
	cell.UnpricedRequests = 0
	r := ResolveCell(cell, nil)
	r.StrategyID = "s1"
	want := tenant.CampaignCell{TenantID: "t1", HourUTC: 9, Requests: 500, Arm: kvcache.StrategyKeepAlive5m, PredictedUSD: 12, BaselineUSD: 100,
		OptimalSavingUSD: 20, Activatable: true, StrategyID: "s1"}
	if got := r.TenantCell(); got != want {
		t.Errorf("TenantCell = %+v, want %+v", got, want)
	}
	if !ArmEnforceable(kvcache.StrategyKeepAlive5m) || ArmEnforceable("nonsense-arm") {
		t.Error("ArmEnforceable disagrees with ArmFor")
	}
}
