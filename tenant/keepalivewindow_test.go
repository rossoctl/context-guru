package tenant

import (
	"testing"
	"time"
)

// WINDOW BOUNDARIES, PINNED AT A FIXED INSTANT. Every case here names the exact time it tests, so
// none of it depends on when the suite runs.
//
// That matters more than the fix itself. The bug these cover made
// TestKeepAliveStrategyControlRoutesResolveLiveWithNoRestart fail for ONE MINUTE IN 1440 — it calls
// time.Now(), so it passed 1439 times out of 1440 and failed when CI happened to run at 23:59 in
// DefaultStrategyTZ. A test that can only fail at one wall-clock minute is a test that reports a
// real defect as flake.
func TestWindowBoundariesAndEndOfDay(t *testing.T) {
	jer, err := time.LoadLocation(DefaultStrategyTZ)
	if err != nil {
		t.Skipf("no tzdata for %s: %v", DefaultStrategyTZ, err)
	}

	at := func(h, m int) time.Time { return time.Date(2026, 9, 15, h, m, 30, 0, jer) }

	for _, tc := range []struct {
		name       string
		start, end string
		when       time.Time
		want       bool
	}{
		// END OF DAY. "24:00" is the only way to cover the final minute, because the end is
		// exclusive — and covering it is the regression this pins.
		{"24:00 covers 23:59", "00:00", "24:00", at(23, 59), true},
		{"24:00 covers midnight", "00:00", "24:00", at(0, 0), true},
		{"24:00 covers midday", "00:00", "24:00", at(12, 30), true},

		// "23:59" as an exclusive end genuinely stops before 23:59, and that is now the
		// documented meaning rather than a surprise.
		{"23:59 as an exclusive end excludes 23:59", "00:00", "23:59", at(23, 59), false},
		{"23:59 as an exclusive end includes 23:58", "00:00", "23:59", at(23, 58), true},

		// THE EXCLUSIVITY THE HOUR TILER DEPENDS ON: abutting windows must not both cover the
		// shared minute, or a campaign would double-count it.
		{"a tile's start is inside", "09:00", "11:00", at(9, 0), true},
		{"a tile's last minute is inside", "09:00", "11:00", at(10, 59), true},
		{"a tile's end belongs to the NEXT tile", "09:00", "11:00", at(11, 0), false},
		{"the next tile owns that minute", "11:00", "12:00", at(11, 0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Window{Start: tc.start, End: tc.end}
			got, err := w.contains(tc.when)
			if err != nil {
				t.Fatalf("contains: %v", err)
			}
			if got != tc.want {
				t.Errorf("window %s-%s at %s = %v, want %v",
					tc.start, tc.end, tc.when.Format("15:04"), got, tc.want)
			}
		})
	}
}

// THE EXACT INSTANT CI FAILED AT, named so a reader can tie the test to the run. 20:59:11Z is
// 23:59:11 in Asia/Jerusalem, which is what made an all-day window report itself closed.
func TestTheInstantThatBrokeCIIsInsideAnAllDayWindow(t *testing.T) {
	if _, err := time.LoadLocation(DefaultStrategyTZ); err != nil {
		t.Skipf("no tzdata for %s: %v", DefaultStrategyTZ, err)
	}
	ci := time.Date(2026, 9, 15, 20, 59, 11, 0, time.UTC)

	s := Strategy{
		Active:  true,
		Target:  Target{Mode: TargetAll},
		Windows: []Window{{Start: "00:00", End: "24:00"}},
	}
	if !s.InWindow(ci) {
		t.Errorf("an all-day window reports itself closed at %s UTC (23:59 in %s) — the boundary "+
			"regression is back", ci.Format(time.RFC3339), DefaultStrategyTZ)
	}
	if !s.Matches(ci, "any-tenant") {
		t.Error("Matches is false for an active all-target strategy inside its window")
	}
}

// A WINDOW IN ANOTHER ZONE still resolves against its own zone, not the host's — otherwise the
// same strategy would fire at different times on different machines.
func TestWindowHonoursItsOwnTimezone(t *testing.T) {
	if _, err := time.LoadLocation("America/New_York"); err != nil {
		t.Skip("no tzdata")
	}
	// 04:30 UTC is 00:30 in New York (EDT) and 07:30 in Jerusalem.
	when := time.Date(2026, 9, 15, 4, 30, 0, 0, time.UTC)

	ny := Window{Start: "00:00", End: "01:00", TZ: "America/New_York"}
	if ok, err := ny.contains(when); err != nil || !ok {
		t.Errorf("New York window 00:00-01:00 at 00:30 local = %v (err %v), want true", ok, err)
	}
	jer := Window{Start: "00:00", End: "01:00", TZ: DefaultStrategyTZ}
	if ok, err := jer.contains(when); err != nil || ok {
		t.Errorf("Jerusalem window 00:00-01:00 at 07:30 local = %v (err %v), want false", ok, err)
	}
}
