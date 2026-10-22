package dash

import "testing"

// A 1-hour thread is pinged at 59 minutes, so the credit floor is an hour, not five minutes.
func TestKeepAliveCreditFloorFollowsTheRequestsTier(t *testing.T) {
	const min = 60 * 1000
	for _, tc := range []struct {
		name string
		ttl  string
		gap  int64
		want bool
	}{
		{"5m tier, 20 min gap: the entry had expired", "ephemeral_5m", 20 * min, true},
		{"1h tier, 20 min gap: the entry survived alone", "ephemeral_1h", 20 * min, false},
		{"1h tier, 59 min gap: still inside the hour", "ephemeral_1h", 59 * min, false},
		{"1h tier, 80 min gap: the 59-minute ping rescued it", "ephemeral_1h", 80 * min, true},
	} {
		e := keepaliveEvent(tc.gap, 1, 48_576, 48_576, 0)
		e.CacheTTL = tc.ttl
		e.Price(ibmSonnet, true)
		if got := e.KeepAliveSavedUSD > 0; got != tc.want {
			t.Errorf("%s: credited=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// kaSaved is the read-side reachability check. With the 5-minute window every rescue of a 1-hour
// thread was zeroed (ping at 59 min, return at 80 min: 21 min > 5 min) while the ping's cost still
// counted, so the ledger went negative and insights told the user to switch keep-alive off.
func TestKaSavedCreditsAOneHourRescue(t *testing.T) {
	const min = 60 * 1000
	for _, tc := range []struct {
		ttl  string
		want bool
	}{{"ephemeral_1h", true}, {"ephemeral_5m", false}} {
		ping := kaPing(1_000_000+59*min, "s1", 0.01, 50_000, 0)
		rescued := kaCredit(1_000_000+80*min, "s1", 0.40)
		rescued.CacheTTL = tc.ttl
		f := newKAFixture(t, kaAgent(1_000_000, "s1", 0.05), ping, rescued)
		o, err := f.db.Overview(Filter{TenantAll: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := o.KeepAliveSavedUSD > 0; got != tc.want {
			t.Errorf("cache_ttl %s: keepalive_saved_usd=%.4f, credited=%v, want %v",
				tc.ttl, o.KeepAliveSavedUSD, got, tc.want)
		}
	}
}

// held_until follows the thread's own TTL: a 1-hour thread with one ping at 59 min is held to the
// ping plus an hour, and a ping that would come after the entry expired extends nothing.
func TestHeldUntilFollowsAOneHourTTL(t *testing.T) {
	const ttl = 3600
	for _, c := range []struct {
		name string
		p    KeepAlivePlan
		want int64
	}{
		{"the 59-minute ping lands in time", KeepAlivePlan{RefreshedMS: 0, NextPingMS: 3_540_000, PingsLeft: 1, IdleMS: 3_540_000}, 3_540_000 + 3_600_000},
		{"a ping that comes too late", KeepAlivePlan{RefreshedMS: 0, NextPingMS: 3_700_000, PingsLeft: 1, IdleMS: 3_540_000}, 3_600_000},
		{"two pings chain", KeepAlivePlan{RefreshedMS: 0, NextPingMS: 3_540_000, PingsLeft: 2, IdleMS: 3_540_000}, 2*3_540_000 + 3_600_000},
	} {
		if got := c.p.heldUntil(ttl); got != c.want {
			t.Errorf("%s: heldUntil = %d, want %d", c.name, got, c.want)
		}
	}
}

// The next-request cost and the manual ping's credit use the tier's write rate: 2.0x input on the
// 1-hour tier, the 5-minute write rate otherwise.
func TestNextRequestUSDUsesTheTiersWriteRate(t *testing.T) {
	_, cold5 := NextRequestUSD(ibmSonnet, 1000, 300)
	_, cold1 := NextRequestUSD(ibmSonnet, 1000, 3600)
	if want := 1000 * ibmSonnet.CacheWrite; cold5 != want {
		t.Errorf("5m cold = %v, want %v", cold5, want)
	}
	if want := 1000 * ibmSonnet.Input * 2.0; cold1 != want {
		t.Errorf("1h cold = %v, want %v", cold1, want)
	}
}
