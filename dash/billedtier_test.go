package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
)

// A write can be mixed: a 1-hour head over a 5-minute tail. Two named rules answer two questions.
// The panels' clock asks "does the HEAD live 1 hour" (keepalive.HeadLifetimeOneHour); the keeper asks
// "does the WHOLE entry" (keepalive.BilledOneHour). Both panels read the head rule through real SQL;
// a read-only turn after the write inherits it.
func TestPanelsFollowTheHeadRuleAndTheKeeperKeepsTheWholeRule(t *testing.T) {
	now := int64(1_700_000_000_000)
	price := func(string) (modelinfo.Price, bool) {
		return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
	}
	for _, c := range []struct {
		name           string
		write, write1h int64
		readOnlyAfter  bool
		head, whole    bool
	}{
		{"all 5m", 100_000, 0, false, false, false},
		{"mixed", 100_000, 40_000, false, true, false},
		{"all 1h", 100_000, 100_000, false, true, true},
		{"mixed then read-only", 100_000, 40_000, true, true, false},
		{"5m then read-only", 100_000, 0, true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			secs := func(b bool) int64 {
				if b {
					return 3600
				}
				return 300
			}
			e := kaAgent(now-100_000, "mix", 0.10)
			e.Provider, e.Model = "anthropic", "aws/claude-sonnet-5"
			e.CacheRead, e.CacheWrite, e.CacheWrite1h = 0, c.write, c.write1h
			evs := []*Event{e}
			if c.readOnlyAfter {
				r := kaAgent(now-50_000, "mix", 0.10)
				r.Provider, r.Model = "anthropic", "aws/claude-sonnet-5"
				r.CacheRead, r.CacheWrite, r.CacheWrite1h = c.write, 0, 0
				evs = append(evs, r)
			}
			fx := newKAFixture(t, evs...)

			if got := keepalive.HeadLifetimeOneHour(c.write, c.write1h, false); got != c.head {
				t.Errorf("HeadLifetimeOneHour = %v, want %v", got, c.head)
			}
			if got := keepalive.BilledOneHour(c.write, c.write1h, false); got != c.whole {
				t.Errorf("BilledOneHour = %v, want %v", got, c.whole)
			}
			ttls, err := fx.db.threadTTLs(Filter{TenantAll: true, Session: "mix"})
			if err != nil || len(ttls) != 1 {
				t.Fatalf("threadTTLs = %v, %v", ttls, err)
			}
			for _, got := range ttls {
				if got != secs(c.head) {
					t.Errorf("cache-state ttl = %d s, want %d s", got, secs(c.head))
				}
			}
			live, err := fx.db.KeepAliveLive(Filter{TenantAll: true}, now, 280, 2, price)
			if err != nil || len(live.Rows) != 1 {
				t.Fatalf("KeepAliveLive = %+v, %v", live, err)
			}
			if got := live.Rows[0].TTLSeconds; got != secs(c.head) {
				t.Errorf("live keep-alive ttl = %d s, want %d s", got, secs(c.head))
			}
		})
	}
}

// A read-only turn inherits the last answer, in both rules.
func TestBothTierRulesInheritOnAReadOnlyTurn(t *testing.T) {
	for _, prev := range []bool{false, true} {
		if got := keepalive.BilledOneHour(0, 0, prev); got != prev {
			t.Errorf("BilledOneHour read-only prev %v = %v", prev, got)
		}
		if got := keepalive.HeadLifetimeOneHour(0, 0, prev); got != prev {
			t.Errorf("HeadLifetimeOneHour read-only prev %v = %v", prev, got)
		}
	}
}

// Two writes in one millisecond: the later row (by id) decides the tier. A full-1h write and a
// later write with no 1-hour token are 5 minutes, not 1 hour.
func TestLiveTierOfTwoWritesInOneMillisecondFollowsTheLaterRow(t *testing.T) {
	now := int64(1_700_000_000_000)
	full := kaAgent(now-100_000, "tie", 0.10)
	full.Provider, full.Model = "anthropic", "aws/claude-sonnet-5"
	full.CacheRead, full.CacheWrite, full.CacheWrite1h = 0, 100_000, 100_000
	mixed := kaAgent(now-100_000, "tie", 0.10)
	mixed.Provider, mixed.Model = "anthropic", "aws/claude-sonnet-5"
	mixed.CacheRead, mixed.CacheWrite, mixed.CacheWrite1h = 0, 100_000, 0
	fx := newKAFixture(t, full, mixed)
	price := func(string) (modelinfo.Price, bool) {
		return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
	}
	live, err := fx.db.KeepAliveLive(Filter{TenantAll: true}, now, 280, 2, price)
	if err != nil || len(live.Rows) != 1 {
		t.Fatalf("KeepAliveLive = %+v, %v", live, err)
	}
	if got := live.Rows[0].TTLSeconds; got != 300 {
		t.Errorf("live ttl = %d s, want 300 (the later write is all 5m)", got)
	}
}

// FactsOf (and seedOneHourTiers) read the head rule, on a mixed write too: the same rule as the accountant's BilledTTL.
func TestFactsOfOneHourIsTheHeadRule(t *testing.T) {
	for _, c := range []struct{ write, write1h int64 }{{0, 0}, {100, 0}, {100, 40}, {100, 100}} {
		got := FactsOf(&Event{CacheWrite: c.write, CacheWrite1h: c.write1h}).OneHour
		if want := keepalive.HeadLifetimeOneHour(c.write, c.write1h, false); got != want {
			t.Errorf("write %d / 1h %d: FactsOf.OneHour = %v, head rule %v", c.write, c.write1h, got, want)
		}
	}
}
