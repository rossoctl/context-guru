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
