package dash

import "testing"

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
		// The entry was written whole at the asked tier: the billed tier is what sets the window (#957).
		w := kaAgent(1_000_000, "s1", 0.05)
		w.CacheWrite, w.CacheTTL = 1000, tc.ttl
		if tc.ttl == "ephemeral_1h" {
			w.CacheWrite1h = 1000
		}
		f := newKAFixture(t, w, ping, rescued)
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

// The window of kaSaved follows the BILLED tier of the entry the rescued request read, as
// keepalive.ResolveTier does, and not the tier the request asked for (#957). A 1h ask billed at 5m
// (Sonnet on Bedrock) or a mixed 1h-head/5m-tail write is a 5-minute entry.
func TestKaSavedWindowFollowsTheBilledTier(t *testing.T) {
	const min = 60 * 1000
	const base = 1_000_000
	rescueAt := int64(base + 80*min)
	for _, tc := range []struct {
		name       string
		ask        string
		write      int64 // cache_write of the earlier writer row
		write1h    int64
		rOwn       int64 // cache_write of the rescued row itself (0 = it only read)
		rOwn1h     int64
		pingAgo    int64 // minutes before the rescued request
		wantCredit bool
	}{
		{"1h ask billed 5m, ping 6 min before: removed", "ephemeral_1h", 1000, 0, 0, 0, 6, false},
		{"whole 1h write, ping 30 min before: kept", "ephemeral_1h", 1000, 1000, 0, 0, 30, true},
		{"mixed write, ping 6 min before: removed", "ephemeral_1h", 1000, 400, 0, 0, 6, false},
		{"mixed write, ping 3 min before: kept", "ephemeral_1h", 1000, 400, 0, 0, 3, true},
		{"5m thread, ping 3 min before: kept", "ephemeral_5m", 1000, 0, 0, 0, 3, true},
		{"5m thread, ping 6 min before: removed", "ephemeral_5m", 1000, 0, 0, 0, 6, false},
		{"the rescued row wrote 5m itself, ping 6 min before: removed", "ephemeral_1h", 1000, 1000, 500, 0, 6, false},
		{"the rescued row wrote 1h itself, ping 30 min before: kept", "ephemeral_1h", 1000, 0, 500, 500, 30, true},
		{"no earlier write at all, ping 6 min before: removed (unknown is 5m)", "ephemeral_1h", 0, 0, 0, 0, 6, false},
	} {
		var evs []*Event
		if tc.write > 0 {
			w := kaAgent(base, "s1", 0.05)
			w.CacheWrite, w.CacheWrite1h, w.CacheTTL = tc.write, tc.write1h, tc.ask
			evs = append(evs, w)
		}
		ping := kaPing(rescueAt-tc.pingAgo*min, "s1", 0.01, 50_000, 0)
		rescued := kaCredit(rescueAt, "s1", 0.40)
		rescued.CacheTTL, rescued.CacheWrite, rescued.CacheWrite1h = tc.ask, tc.rOwn, tc.rOwn1h
		evs = append(evs, ping, rescued)
		f := newKAFixture(t, evs...)
		o, err := f.db.Overview(Filter{TenantAll: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := o.KeepAliveSavedUSD > 0; got != tc.wantCredit {
			t.Errorf("%s: keepalive_saved_usd=%.4f, credited=%v, want %v", tc.name, o.KeepAliveSavedUSD, got, tc.wantCredit)
		}
	}
}

// The inherit lookup of kaSavedGate is scoped to the rescued row's THREAD and takes the LATEST
// earlier write (#957). Each case has a control that must be credited, so a broken fixture cannot
// pass for the right reason.
func TestKaSavedInheritsFromTheLatestWriteOfTheSameThread(t *testing.T) {
	const min = 60 * 1000
	const base = 1_000_000
	rescueAt := int64(base + 80*min)
	writer := func(at int64, thread string, w1h int64) *Event {
		e := kaAgent(at, "s1", 0.05)
		e.ThreadID, e.CacheTTL, e.CacheWrite, e.CacheWrite1h = thread, "ephemeral_1h", 1000, w1h
		return e
	}
	for _, tc := range []struct {
		name       string
		writers    []*Event
		thread     string // thread of the read-only rescued row
		wantCredit bool
	}{
		{"1h writer on another thread of the session: no inherit", []*Event{writer(base, "a:H", 1000)}, "", false},
		{"control: 1h writer on the same thread", []*Event{writer(base, "a:H", 1000)}, "a:H", true},
		{"1h write then a later 5m write, same thread: latest wins", []*Event{writer(base, "", 1000), writer(base+10*min, "", 0)}, "", false},
		{"control: 5m write then a later 1h write, same thread", []*Event{writer(base, "", 0), writer(base+10*min, "", 1000)}, "", true},
	} {
		ping := kaPing(rescueAt-30*min, "s1", 0.01, 50_000, 0)
		rescued := kaCredit(rescueAt, "s1", 0.40)
		rescued.ThreadID, rescued.CacheTTL = tc.thread, "ephemeral_1h"
		f := newKAFixture(t, append(tc.writers, ping, rescued)...)
		o, err := f.db.Overview(Filter{TenantAll: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := o.KeepAliveSavedUSD > 0; got != tc.wantCredit {
			t.Errorf("%s: keepalive_saved_usd=%.4f, credited=%v, want %v", tc.name, o.KeepAliveSavedUSD, got, tc.wantCredit)
		}
	}
}
