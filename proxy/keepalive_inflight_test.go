package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// A subagent's request is in flight for longer than the main thread's remaining cache lifetime: the
// main thread's ping still goes out on time (a session-wide pause would let its cache expire).
func TestASlowSubagentRequestDoesNotDelayTheMainThreadsPing(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var pings atomic.Int32
	slowArrived := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		b.ReadFrom(r.Body)
		switch {
		case strings.Contains(b.String(), "SUMMARIZE"):
			slowArrived <- struct{}{} // reached upstream, so the keeper has seen it arrive
			<-release                 // the slow call
		case gjsonMaxTokens(b.Bytes()) == 1:
			pings.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"aws/claude-sonnet-5","content":[{"type":"text","text":"ok"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":48576,"cache_creation_input_tokens":0}}`))
	}))
	defer up.Close()
	rec, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	cfg, err := config.LoadBytes([]byte("preset: off\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{AnthropicUpstream: up.URL, Dashboard: rec,
		Cache: CachePolicy{KeepAlive: true, Idle: 60 * time.Second, MaxPings: 2, MaxUSDPerPing: -1, MinPrefixTokens: 1}}) // no $ ceiling: other tests install price tables
	t.Cleanup(h.Close)
	clock := &testClock{at: time.Now()}
	rebuildKeeper(h, nil, clock) // the test clock and an inline runner; pings still go to `up`
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	defer unblock() // runs before the servers close: a failing run must not leave the upstream handler blocked

	sess := "inflight-" + strconv.FormatInt(time.Now().UnixNano(), 36) // process-wide thread state outlives one run
	post := func(agent string, texts ...string) {
		var msgs []any
		for i, s := range texts {
			msgs = append(msgs, map[string]any{"role": []string{"user", "assistant"}[i%2], "content": s})
		}
		m := map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64, "messages": msgs,
			"system": []any{map[string]any{"type": "text", "text": "you are claude code", "cache_control": map[string]any{"type": "ephemeral"}}}}
		m["tools"] = []any{map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}}}
		body, _ := json.Marshal(m)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", sess)
		req.Header.Set("x-api-key", "sk-test")
		req.Header.Set("anthropic-version", "2023-06-01")
		if agent != "" {
			req.Header.Set(agentIDHeader, agent)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}
	// record runs in serve's defer, AFTER the response reaches the client: wait for it, or the next
	// request's record can run first and take the turn the gate counts. The first turn is refused
	// by the gate (counted skipped), the second is held (live).
	settled := func(done func(KeepAliveStats) bool) {
		for deadline := time.Now().Add(5 * time.Second); !done(h.keeper.Stats()) && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
	}
	post("", "fix the bug")
	settled(func(st KeepAliveStats) bool { return st.Skipped >= 1 })
	post("", "fix the bug", "done", "thanks")
	settled(func(st KeepAliveStats) bool { return st.Live >= 1 })
	if h.keeper.Stats().Live != 1 {
		t.Fatalf("setup: %d live, want 1 (%+v)", h.keeper.Stats().Live, h.keeper.Stats())
	}
	done := make(chan struct{})
	go func() { post("sub1", "SUMMARIZE the module for the main agent"); close(done) }()
	select {
	case <-slowArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request never arrived")
	}
	h.keeper.Sweep(clock.advance(61 * time.Second)) // the main thread is due; the subagent's call is still open
	if n := pings.Load(); n != 1 {
		t.Errorf("%d main-thread pings while a subagent request was in flight, want 1 (on time)", n)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow request never finished")
	}
}

// Two requests of ONE thread overlap: the first one's fresh hold is not pinged while the second is
// still open, and is pinged once that one has finished.
func TestAnOpenRequestOnTheSameThreadPausesItsPing(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://up", path: "/v1/messages"}
	recordOne(t, k, pol, kaBody, clock.now(), up)  // an earlier turn: the gate needs turn >= 1
	k.arrive("t1", "sess-1", "")                   // request A
	k.arrive("t1", "sess-1", "")                   // request B, overlapping
	recordTurn(t, k, pol, kaBody, clock.now(), up) // A finishes and holds
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(30 * time.Millisecond)
	if fs.n() != 0 {
		t.Errorf("%d pings while another request of the same thread was open", fs.n())
	}
	k.depart("t1", "sess-1", "") // B ends without a hold (a failed request)
	k.Sweep(clock.advance(time.Second))
	time.Sleep(30 * time.Millisecond)
	if fs.n() != 1 {
		t.Errorf("%d pings after the thread's last request ended, want 1", fs.n())
	}
}

// A panic in the host's record BEFORE it hands off to keepalive.Keeper.Record must still depart
// the thread, or its open count leaks and the thread is never pinged again. A nil request panics
// at r.UserAgent() while the campaign's arguments are built, ahead of the hand-off. (The normal
// path departs exactly once — TestAnOpenRequestOnTheSameThreadPausesItsPing pins that.)
func TestAPanicBeforeTheHandOffStillDeparts(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://up", path: "/v1/messages"}
	recordOne(t, k, pol, kaBody, clock.now(), up)  // an earlier turn: the gate needs turn >= 1
	k.arrive("t1", "sess-1", "")                   // request A, which will panic in record
	k.arrive("t1", "sess-1", "")                   // request B, overlapping
	recordTurn(t, k, pol, kaBody, clock.now(), up) // B finishes and holds
	if k.Stats().Live != 1 {
		t.Fatalf("setup: %d live, want 1", k.Stats().Live)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("setup: record with a nil request did not panic")
			}
		}()
		k.record(&Tenancy{ID: "t1", Cache: pol}, "sess-1", "", clock.now(), []byte(kaBody), up, nil,
			bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576}, true)
	}()
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(30 * time.Millisecond)
	if fs.n() != 1 {
		t.Errorf("%d pings after A's record panicked before its hand-off, want 1 "+
			"(A's open count leaked and paused the thread)", fs.n())
	}
}

// routeFailLimit and routeTripFor mirror package keepalive's breaker constants (breaker.go), which
// these host-level tests drive through the proxy's own record and Dispatcher.
const (
	routeFailLimit = 3
	routeTripFor   = time.Hour
)

func gjsonMaxTokens(b []byte) int {
	var m struct {
		MaxTokens int `json:"max_tokens"`
	}
	json.Unmarshal(b, &m)
	return m.MaxTokens
}

// A ping that reads nothing (a 200 with no usage, or a relayed 5xx) is FAILED: counted, the session
// stops, and routeFailLimit of them in a row switch the route off; a working route is unaffected.
func TestPingThatRefreshesNothingFailsAndTripsTheRoute(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	var sent atomic.Int32
	var empty atomic.Bool
	empty.Store(true)
	fs.fn = func(*http.Request, []byte) (Usage, int, error) {
		sent.Add(1)
		if empty.Load() {
			return Usage{}, http.StatusOK, nil
		}
		return Usage{CacheRead: 48576, Output: 1}, http.StatusOK, nil
	}
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	bad := upstream{base: "http://litellm", path: "/v1/messages"}
	good := upstream{base: "http://api", path: "/v1/messages"}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-x")
	hold := func(sess string, up upstream, tenant ...string) {
		id := "t1"
		if len(tenant) > 0 {
			id = tenant[0]
		}
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: id, Cache: pol}, sess, "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	for i := 0; i < routeFailLimit; i++ {
		hold("s"+string(rune('a'+i)), bad)
	}
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	st := k.Stats()
	if st.Failed != routeFailLimit || st.RouteTrips != 1 {
		t.Fatalf("failed %d (want %d), trips %d (want 1)", st.Failed, routeFailLimit, st.RouteTrips)
	}
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if int(sent.Load()) != routeFailLimit {
		t.Errorf("%d pings, want %d: a failed session must not be pinged again", int(sent.Load()), routeFailLimit)
	}
	hold("s-new", bad)
	if k.Stats().Live != 0 {
		t.Errorf("a tripped route still holds a new session")
	}
	hold("s-other-tenant", bad, "t2")
	if k.Stats().Live != 1 {
		t.Errorf("one tenant's failures switched off another tenant's keep-alive")
	}
	k.retireSession("t2", "s-other-tenant")
	empty.Store(false)
	hold("s-good", good)
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if int(sent.Load()) != routeFailLimit+1 || k.Stats().Failed != routeFailLimit {
		t.Errorf("working route: %d pings, %d failed", int(sent.Load()), k.Stats().Failed)
	}
	clock.advance(routeTripFor)
	hold("s-later", bad)
	if k.Stats().Live == 0 {
		t.Errorf("the route stayed off past routeTripFor")
	}
}

// A real miss (the ping landed after expiry: read 0, the prefix written again) is not a failure: it
// is counted missed, does not trip the route, and pinging goes on. Two in a row are a prefix that
// does not match, and the session stops.
func TestARealMissIsNotAFailedPing(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	var sent atomic.Int32
	miss := Usage{CacheWrite: 48576, Output: 1}
	reply := []Usage{miss, {CacheRead: 48576, Output: 1}, miss, miss}
	fs.fn = func(*http.Request, []byte) (Usage, int, error) {
		n := sent.Add(1)
		return reply[n-1], http.StatusOK, nil
	}
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 4, MinPrefixTokens: 20000}
	up := upstream{base: "http://api", path: "/v1/messages"}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-x")
	for i := 0; i < 2; i++ {
		k.record(&Tenancy{ID: "t1", Cache: pol}, "s", "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
			bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	for i := 0; i < 5; i++ {
		k.Sweep(clock.advance(290 * time.Second))
		time.Sleep(30 * time.Millisecond)
	}
	st := k.Stats()
	if sent.Load() != 4 || st.Missed != 2 || st.Failed != 0 || st.RouteTrips != 0 || st.Wrote != 1 {
		t.Errorf("sent %d (want 4: miss, read, miss, miss->stop), missed %d (want 2), failed %d, trips %d, wrote %d (want 1)",
			sent.Load(), st.Missed, st.Failed, st.RouteTrips, st.Wrote)
	}
}

// A ping with input tokens but no cache read (OpenAI after expiry) is a miss; a 200 with no usage,
// or a relayed 5xx, is a failure.
func TestPingFailureNeedsNoUsageOrAnError(t *testing.T) {
	for _, c := range []struct {
		name   string
		u      Usage
		status int
		failed bool
	}{
		{"no usage", Usage{}, http.StatusOK, true},
		{"5xx is transient", Usage{}, http.StatusBadGateway, false},
		{"429 is transient", Usage{}, http.StatusTooManyRequests, false},
		{"401 is not", Usage{}, http.StatusUnauthorized, true},
		{"openai miss", Usage{FreshInput: 48576, Output: 1}, http.StatusOK, false},
		{"anthropic miss", Usage{CacheWrite: 48576, Output: 1}, http.StatusOK, false},
		{"read", Usage{CacheRead: 48576, Output: 1}, http.StatusOK, false},
	} {
		k, fs, clock := testKeeper(t, Limits{})
		fs.fn = func(*http.Request, []byte) (Usage, int, error) { return c.u, c.status, nil }
		pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-x")
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: "t1", Cache: pol}, "s", "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
				upstream{base: "http://api", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages", http.StatusOK,
				Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
		k.Sweep(clock.advance(290 * time.Second))
		time.Sleep(30 * time.Millisecond)
		if got := k.Stats().Failed == 1; got != c.failed {
			t.Errorf("%s: failed %v, want %v", c.name, got, c.failed)
		}
	}
}

// An overload burst (529, 429, any 5xx) is transient: skipped, the session goes on pinging, and the
// route is never tripped.
func TestOverloadedPingsDoNotTripTheRoute(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	var sent atomic.Int32
	fs.fn = func(*http.Request, []byte) (Usage, int, error) {
		sent.Add(1)
		return Usage{}, []int{529, http.StatusTooManyRequests}[sent.Load()%2], nil
	}
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://api", path: "/v1/messages"}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-x")
	for n := 0; n < routeFailLimit+1; n++ {
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: "t1", Cache: pol}, "s"+strconv.Itoa(n), "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	k.Sweep(clock.advance(290 * time.Second)) // the next slot retries
	time.Sleep(50 * time.Millisecond)
	if st := k.Stats(); st.Failed != 0 || st.RouteTrips != 0 || sent.Load() != 2*(routeFailLimit+1) {
		t.Errorf("529/429s: failed %d, trips %d, sent %d (want 0, 0, %d: every session retried)", st.Failed, st.RouteTrips, sent.Load(), 2*(routeFailLimit+1))
	}
}

// A non-retryable 4xx (not an auth refusal) counts toward the breaker; forget clears an account's trip; an OpenAI ping that
// misses twice running (fresh input, no read) stops the session.
func TestBreakerCountsA4xxForgetClearsItAndAnOpenAIDoubleMissStops(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	fs.fn = func(*http.Request, []byte) (Usage, int, error) { return Usage{}, http.StatusBadRequest, nil }
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 3, MinPrefixTokens: 20000}
	up := upstream{base: "http://litellm", path: "/v1/messages"}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-x")
	hold := func(sess string) {
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: "t1", Cache: pol}, sess, "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	for n := 0; n < routeFailLimit; n++ {
		hold("s" + strconv.Itoa(n))
	}
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if k.Stats().RouteTrips != 1 {
		t.Fatalf("%d trips after %d 400s, want 1", k.Stats().RouteTrips, routeFailLimit)
	}
	k.forget("t1")
	hold("after-forget")
	if k.Stats().Live != 1 {
		t.Errorf("forget left the account's trip in place")
	}
	k.retireSession("t1", "after-forget")

	var sent atomic.Int32
	fs.fn = func(*http.Request, []byte) (Usage, int, error) {
		sent.Add(1)
		return Usage{FreshInput: 48576, Output: 1}, http.StatusOK, nil
	}
	hold("openai-miss")
	for i := 0; i < 3; i++ {
		k.Sweep(clock.advance(290 * time.Second))
		time.Sleep(30 * time.Millisecond)
	}
	if sent.Load() != 2 {
		t.Errorf("%d pings, want 2 (one miss goes on, the second stops the session)", sent.Load())
	}
}

// A 401/403 is about the key: it stops every session pinging with THAT credential and refuses new
// holds with it, while a teammate's sessions on the same account, route and model ping on, and the
// route breaker is untouched.
func TestAuthRefusalStopsOnlyThatCredential(t *testing.T) {
	k, fs, clock := testKeeper(t, Limits{})
	var sentA, sentB atomic.Int32
	fs.fn = func(r *http.Request, _ []byte) (Usage, int, error) {
		if r.Header.Get("x-api-key") == "sk-alice-revoked" { // every alice session pings with this key
			sentA.Add(1)
			return Usage{}, http.StatusUnauthorized, nil
		}
		sentB.Add(1)
		return Usage{CacheRead: 48576, Output: 1}, http.StatusOK, nil
	}
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 3, MinPrefixTokens: 20000}
	up := upstream{base: "http://litellm", path: "/v1/messages"}
	hold := func(sess, key string) {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("x-api-key", key)
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: "t1", Cache: pol}, sess, "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	hold("alice-0", "sk-alice-revoked")
	hold("bob-1", "sk-bob")
	clock.advance(100 * time.Second) // alice's other sessions come due one sweep later
	for i := 1; i < routeFailLimit+1; i++ {
		hold("alice-"+strconv.Itoa(i), "sk-alice-revoked")
	}
	for _, d := range []time.Duration{190, 100, 190} { // due: alice-0 + bob, then alice-1..3, then bob again
		k.Sweep(clock.advance(d * time.Second))
		time.Sleep(50 * time.Millisecond)
	}
	st := k.Stats()
	if st.RouteTrips != 0 {
		t.Errorf("an auth refusal tripped the route (%d trips)", st.RouteTrips)
	}
	if sentA.Load() != 1 {
		t.Errorf("%d pings with the revoked key, want 1: the first refusal stops every session using it", sentA.Load())
	}
	if sentB.Load() != 2 {
		t.Errorf("the teammate's session got %d pings, want 2", sentB.Load())
	}
	hold("alice-new", "sk-alice-revoked")
	hold("bob-2", "sk-bob")
	_, aliceHeld := k.Entry(keepalive.Key{Tenant: "t1", Session: "alice-new"})
	_, bobHeld := k.Entry(keepalive.Key{Tenant: "t1", Session: "bob-2"})
	if aliceHeld || !bobHeld {
		t.Errorf("new holds: revoked key held %v (want false), teammate held %v (want true)", aliceHeld, bobHeld)
	}
	hold("alice-rotated", "sk-alice-new-key")
	_, rotated := k.Entry(keepalive.Key{Tenant: "t1", Session: "alice-rotated"})
	if !rotated {
		t.Errorf("a new key for the same user was refused")
	}
}

// Two keys that share every character a display mask would keep (prefix and last four) are two
// credentials: refusing one leaves the other pinging.
func TestCredentialIDsDoNotCollideOnAMask(t *testing.T) {
	a, b := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAwxyz", "sk-ant-api03-BBBBBBBBBBBBBBBBBBBBwxyz"
	id := func(v string) string {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("x-api-key", v)
		return credKey(upstream{base: "http://litellm", path: "/v1/messages"}, r.Header)
	}
	if id(a) == id(b) || id(a) != id(a) || id(a) == "" {
		t.Fatalf("credential ids: a=%q b=%q", id(a), id(b))
	}
	if strings.Contains(id(a), "wxyz") || strings.Contains(id(a), "AAAA") {
		t.Errorf("the id carries part of the key: %q", id(a))
	}
	k, fs, clock := testKeeper(t, Limits{})
	var sentB atomic.Int32
	fs.fn = func(r *http.Request, _ []byte) (Usage, int, error) {
		if r.Header.Get("x-api-key") == a { // session "a"'s key
			return Usage{}, http.StatusUnauthorized, nil
		}
		sentB.Add(1)
		return Usage{CacheRead: 48576, Output: 1}, http.StatusOK, nil
	}
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	hold := func(sess, key string) {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("x-api-key", key)
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: "t1", Cache: pol}, sess, "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
				upstream{base: "http://litellm", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages", http.StatusOK,
				Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	hold("a", a)
	k.Sweep(clock.advance(290 * time.Second)) // a's key is refused
	time.Sleep(50 * time.Millisecond)
	hold("b", b)
	k.Sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if sentB.Load() != 1 {
		t.Errorf("the look-alike key got %d pings, want 1: it is a different credential", sentB.Load())
	}
}

// A 403 blocks the key on that model only (a per-model permission or a gateway policy); a 401
// blocks the key on every model. forget clears the marks.
func TestA403BlocksTheKeyOnThatModelOnly(t *testing.T) {
	for _, c := range []struct {
		status       int
		otherPings   int32
		otherHeldNew bool
	}{{http.StatusForbidden, 1, true}, {http.StatusUnauthorized, 0, false}} {
		k, fs, clock := testKeeper(t, Limits{})
		var sentB atomic.Int32
		fs.fn = func(_ *http.Request, body []byte) (Usage, int, error) {
			if gjson.GetBytes(body, "model").String() == "aws/claude-sonnet-5" {
				return Usage{}, c.status, nil
			}
			sentB.Add(1)
			return Usage{CacheRead: 48576, Output: 1}, http.StatusOK, nil
		}
		pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
		bodyB := strings.Replace(kaBody, "aws/claude-sonnet-5", "aws/claude-opus-5", 1)
		hold := func(sess, body string) {
			r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
			r.Header.Set("x-api-key", "sk-shared-key")
			for i := 0; i < 2; i++ {
				k.record(&Tenancy{ID: "t1", Cache: pol}, sess, "", clock.now().Add(time.Duration(i)*time.Second), []byte(body),
					upstream{base: "http://litellm", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages", http.StatusOK,
					Usage{CacheRead: 48576, CacheWrite: 1200}, true)
			}
		}
		hold("a", kaBody)
		k.Sweep(clock.advance(290 * time.Second)) // model A refused
		time.Sleep(50 * time.Millisecond)
		hold("b", bodyB)
		k.Sweep(clock.advance(290 * time.Second))
		time.Sleep(50 * time.Millisecond)
		if sentB.Load() != c.otherPings {
			t.Errorf("%d on model A: the same key on model B got %d pings, want %d", c.status, sentB.Load(), c.otherPings)
		}
		hold("b2", bodyB)
		hold("a2", kaBody)
		_, bHeld := k.Entry(keepalive.Key{Tenant: "t1", Session: "b2"})
		_, aHeld := k.Entry(keepalive.Key{Tenant: "t1", Session: "a2"})
		if bHeld != c.otherHeldNew || aHeld {
			t.Errorf("%d: new hold on model B %v (want %v), on model A %v (want false)", c.status, bHeld, c.otherHeldNew, aHeld)
		}
		k.forget("t1")
		hold("a3", kaBody)
		if k.Stats().Live != 1 {
			t.Errorf("%d: forget left the credential blocked", c.status)
		}
	}
}
