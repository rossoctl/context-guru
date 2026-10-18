package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// TestKeepAlivePlansReportsOnlyThreadsStillToBePinged: the status line's "held warm until" must
// come from entries the keeper will really ping, scoped to the session (and tenant) asked about.
// The entries are made through the host's own record and sweep: one that spent all its pings and
// one whose ping was refused (stopped) have no plan.
func TestKeepAlivePlansReportsOnlyThreadsStillToBePinged(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	h := k.h
	h.keeper = k
	fs.fn = func(_ *http.Request, body []byte) (Usage, int, error) {
		if strings.Contains(string(body), "refuse-me") {
			return Usage{}, http.StatusBadRequest, nil // a refused ping stops its entry
		}
		return Usage{CacheRead: 48576, Output: 1}, http.StatusOK, nil
	}
	t0 := clock.now()
	pol := CachePolicy{KeepAlive: true, Idle: 270 * time.Second, MaxPings: 3, MinPrefixTokens: 20000}
	hold := func(tenant, session, thread, text string, maxPings int, at time.Time) {
		p := pol
		p.MaxPings = maxPings
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		body := strings.Replace(kaBody, `"text":"hi"`, `"text":"`+text+`"`, 1)
		for i := 0; i < 2; i++ { // the gate skips a thread's first request
			k.record(&Tenancy{ID: tenant, Cache: p}, session, thread, at.Add(time.Duration(i-1)*time.Second), []byte(body),
				upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
				http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	hold("t1", "s", "a:x", "spend-all", 1, t0) // spends all its pings in the sweep below
	hold("t1", "s", "a:y", "refuse-me", 3, t0) // stopped by the sweep below
	k.Sweep(clock.advance(271 * time.Second))
	main := clock.now()
	hold("t1", "s", "", "main", 3, main)        // not yet pinged
	hold("t1", "other", "", "other", 3, main)   // another session
	hold("t2", "s", "a:z", "tenant-2", 3, main) // another tenant

	got := h.KeepAlivePlans("t1", false, "s")
	if len(got) != 1 || got[""].PingsLeft != 3 {
		t.Fatalf("plans = %+v, want only the main thread with 3 pings left", got)
	}
	if p := got[""]; p.NextPingMS != main.Add(270*time.Second).UnixMilli() || p.IdleMS != 270_000 ||
		p.RefreshedMS != main.UnixMilli() {
		t.Errorf("plan = %+v, want the next ping at start + idle and the refresh at the start", p)
	}
	if all := h.KeepAlivePlans("", true, "s"); len(all) != 2 || all["a:z"].PingsLeft != 3 {
		t.Errorf("all tenants = %+v, want the main thread and t2's a:z", all)
	}
	if h.KeepAlivePlans("t1", false, "") != nil {
		t.Error("no session asked about has no plans")
	}
	if (*Handler)(nil).KeepAlivePlans("t1", false, "s") != nil {
		t.Error("a nil handler has no plans")
	}
}

// TestKeepAlivePlanCountsOnlyPingsThatRefreshed: the sweep advances startedAt before a ping is
// sent, so a ping that fails must not count as a refresh. The plan's RefreshedMS stays on the last
// real refresh until a ping actually reads the cache.
func TestKeepAlivePlanCountsOnlyPingsThatRefreshed(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	k.h.keeper = k
	pol := kaPolicy()
	pol.MaxPings = 3
	t0 := clock.now()
	recordOne(t, k, pol, kaBody, t0, upstream{base: "http://up", path: "/v1/messages"})

	fs.mu.Lock()
	fs.err = errors.New("dial tcp: connection refused")
	fs.mu.Unlock()
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	p := k.h.KeepAlivePlans("t1", false, "sess-1")[""]
	if p.RefreshedMS != t0.UnixMilli() || p.PingsLeft != 2 {
		t.Fatalf("after a failed ping: plan %+v, want RefreshedMS at the real request (%d) and 2 pings left", p, t0.UnixMilli())
	}

	fs.mu.Lock()
	fs.err = nil
	fs.mu.Unlock()
	at := clock.advance(281 * time.Second)
	k.Sweep(at)
	waitPings(t, k, 2)
	if p := k.h.KeepAlivePlans("t1", false, "sess-1")[""]; p.RefreshedMS != at.UnixMilli() {
		t.Errorf("after a ping that read the cache: RefreshedMS = %d, want the ping's start %d", p.RefreshedMS, at.UnixMilli())
	}
}

// TestAPingThatReadNothingIsNotARefresh: a 200 ping that reports no cache read (the entry was
// gone, so the provider wrote it again) answered, but refreshed nothing that was there before.
// refreshedAt must stay on the real request.
func TestAPingThatReadNothingIsNotARefresh(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	t0 := clock.now()
	recordOne(t, k, kaPolicy(), kaBody, t0, upstream{base: "http://up", path: "/v1/messages"})
	fs.mu.Lock()
	fs.usage = Usage{FreshInput: 10, CacheWrite: 48576, Output: 1}
	fs.mu.Unlock()

	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	got := k.Plans("t1", false, "sess-1")[""].RefreshedAt
	if !got.Equal(t0) {
		t.Errorf("refreshedAt = %v after a ping that read nothing, want the real request's start %v", got, t0)
	}
}
