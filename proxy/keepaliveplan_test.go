package proxy

import (
	"testing"
	"time"
)

// TestKeepAlivePlansReportsOnlyThreadsStillToBePinged: the status line's "held warm until" must
// come from entries the keeper will really ping, scoped to the session (and tenant) asked about.
func TestKeepAlivePlansReportsOnlyThreadsStillToBePinged(t *testing.T) {
	h := &Handler{}
	h.keeper = newKeeper(h)
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	pol := CachePolicy{KeepAlive: true, Idle: 270 * time.Second, MaxPings: 3}
	entry := func(tenant, session, thread string, pings int, stopped bool) *kaEntry {
		return &kaEntry{tenant: tenant, session: session, thread: thread, startedAt: at, pol: pol,
			pings: pings, stopped: stopped, turn: 1}
	}
	h.keeper.live = map[string]*kaEntry{
		"1": entry("t1", "s", "", 1, false),
		"2": entry("t1", "s", "a:x", 3, false),  // spent all its pings
		"3": entry("t1", "s", "a:y", 0, true),   // stopped
		"4": entry("t1", "other", "", 0, false), // another session
		"5": entry("t2", "s", "a:z", 0, false),  // another tenant
	}

	got := h.KeepAlivePlans("t1", false, "s")
	if len(got) != 1 || got[""].PingsLeft != 2 {
		t.Fatalf("plans = %+v, want only the main thread with 2 pings left", got)
	}
	if p := got[""]; p.NextPingMS != at.Add(270*time.Second).UnixMilli() || p.IdleMS != 270_000 {
		t.Errorf("plan = %+v, want the next ping at start + idle", p)
	}
	if all := h.KeepAlivePlans("", true, "s"); len(all) != 2 || all["a:z"].PingsLeft != 3 {
		t.Errorf("all tenants = %+v, want the main thread and t2's a:z", all)
	}
	if (*Handler)(nil).KeepAlivePlans("t1", false, "s") != nil {
		t.Error("a nil handler has no plans")
	}
}
