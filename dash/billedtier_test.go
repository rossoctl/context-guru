package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
)

// One rule says "this response billed the 1-hour tier": keepalive.BilledOneHour. The cache-state
// panel and the live keep-alive panel read it through SQL. On a mixed write (part of the write at
// 1 hour) the keeper uses 5 minutes, so both panels must show 300 s. They showed 3600 s.
func TestBilledTierPanelsAgreeWithTheKeeperOnAMixedWrite(t *testing.T) {
	now := int64(1_700_000_000_000)
	for _, c := range []struct {
		name           string
		write, write1h int64
	}{
		{"all 5m", 100_000, 0},
		{"mixed", 100_000, 40_000},
		{"all 1h", 100_000, 100_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := int64(300)
			if keepalive.BilledOneHour(c.write, c.write1h, false) {
				want = 3600
			}
			e := kaAgent(now-100_000, "mix", 0.10)
			e.Provider, e.Model = "anthropic", "aws/claude-sonnet-5"
			e.CacheRead, e.CacheWrite, e.CacheWrite1h = 0, c.write, c.write1h
			fx := newKAFixture(t, e)

			ttls, err := fx.db.threadTTLs(Filter{TenantAll: true, Session: "mix"})
			if err != nil || len(ttls) != 1 {
				t.Fatalf("threadTTLs = %v, %v", ttls, err)
			}
			for _, got := range ttls {
				if got != want {
					t.Errorf("cache-state ttl = %d s, keeper says %d s", got, want)
				}
			}

			price := func(string) (modelinfo.Price, bool) {
				return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
			}
			live, err := fx.db.KeepAliveLive(Filter{TenantAll: true}, now, 280, 2, price)
			if err != nil || len(live.Rows) != 1 {
				t.Fatalf("KeepAliveLive = %+v, %v", live, err)
			}
			if got := live.Rows[0].TTLSeconds; got != want {
				t.Errorf("live keep-alive ttl = %d s, keeper says %d s", got, want)
			}
		})
	}
}

// Two writes in one millisecond: the later row (by id) decides the tier. A full-1h write and a
// mixed write after it are 5 minutes, not 1 hour.
func TestLiveTierOfTwoWritesInOneMillisecondFollowsTheLaterRow(t *testing.T) {
	now := int64(1_700_000_000_000)
	full := kaAgent(now-100_000, "tie", 0.10)
	full.Provider, full.Model = "anthropic", "aws/claude-sonnet-5"
	full.CacheRead, full.CacheWrite, full.CacheWrite1h = 0, 100_000, 100_000
	mixed := kaAgent(now-100_000, "tie", 0.10)
	mixed.Provider, mixed.Model = "anthropic", "aws/claude-sonnet-5"
	mixed.CacheRead, mixed.CacheWrite, mixed.CacheWrite1h = 0, 100_000, 40_000
	fx := newKAFixture(t, full, mixed)
	price := func(string) (modelinfo.Price, bool) {
		return modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}, true
	}
	live, err := fx.db.KeepAliveLive(Filter{TenantAll: true}, now, 280, 2, price)
	if err != nil || len(live.Rows) != 1 {
		t.Fatalf("KeepAliveLive = %+v, %v", live, err)
	}
	if got := live.Rows[0].TTLSeconds; got != 300 {
		t.Errorf("live ttl = %d s, want 300 (the later write is mixed)", got)
	}
}

// seedOneHourTiers and FactsOf read the same rule as the keeper, on a mixed write too.
func TestFactsOfOneHourIsTheKeepersRule(t *testing.T) {
	for _, c := range []struct{ write, write1h int64 }{{0, 0}, {100, 0}, {100, 40}, {100, 100}} {
		got := FactsOf(&Event{CacheWrite: c.write, CacheWrite1h: c.write1h}).OneHour
		if want := keepalive.BilledOneHour(c.write, c.write1h, false); got != want {
			t.Errorf("write %d / 1h %d: FactsOf.OneHour = %v, keeper %v", c.write, c.write1h, got, want)
		}
	}
}
