package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func sigPost(h *Handler, body string) int {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/signal", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	h.clientSignal(w, r)
	return w.Code
}

// A SessionEnd retires what is held for exactly that session, and the no-signal policy is unchanged
// when no event arrives.
func TestSessionEndRetiresTheHeldEntry(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	k, fs, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://up", path: "/v1/messages"}
	recordOne(t, k, pol, kaBody, clock.now(), up)
	if k.Stats().Live != 1 {
		t.Fatalf("setup: %d live, want 1", k.Stats().Live)
	}
	k.signal("t1", "sess-1", "SessionEnd", "prompt_input_exit", clock.now())
	if k.Stats().Live != 0 {
		t.Errorf("SessionEnd left %d entries held", k.Stats().Live)
	}
	clock.advance(290 * time.Second)
	k.sweep(clock.now())
	time.Sleep(50 * time.Millisecond)
	if fs.n() != 0 {
		t.Errorf("%d pings after SessionEnd", fs.n())
	}
}

// A request that was in flight when the user left must not re-arm the hold; one that starts after
// the mark (a resumed session) is tracked normally.
func TestInFlightRequestDoesNotRearmButResumeDoes(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	k, _, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://up", path: "/v1/messages"}
	recordOne(t, k, pol, kaBody, clock.now(), up)
	end := clock.now()
	k.signal("t1", "sess-1", "SessionEnd", "other", end)
	recordTurn(t, k, pol, kaBody, end.Add(-time.Second), up) // started before the end
	if k.Stats().Live != 0 {
		t.Errorf("a request in flight at SessionEnd re-armed the hold")
	}
	recordTurn(t, k, pol, kaBody, end.Add(time.Minute), up) // resumed
	if k.Stats().Live != 1 {
		t.Errorf("a resumed session was not tracked")
	}
}

// SessionStart compact retires the old prefix but leaves no end mark: the session goes on, and a
// post-compaction request is tracked even if it started before the event arrived. Presence events
// (resume, clear, startup, fork) change nothing.
func TestCompactRetiresWithoutEndingAndPresenceIsRecordedOnly(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	k, fs, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://up", path: "/v1/messages"}
	recordOne(t, k, pol, kaBody, clock.now(), up)
	for _, kind := range []string{"resume", "clear", "startup", "fork"} {
		if k.signal("t1", "sess-1", "SessionStart", kind, clock.now()) || k.Stats().Live != 1 {
			t.Fatalf("SessionStart %s acted (live %d)", kind, k.Stats().Live)
		}
	}
	clock.advance(10 * time.Second) // the compaction response takes a while; the hook fires at its end
	at := clock.now()
	if !k.signal("t1", "sess-1", "SessionStart", "compact", at) || k.Stats().Live != 0 {
		t.Fatalf("SessionStart compact left %d entries held", k.Stats().Live)
	}
	recordTurn(t, k, pol, kaBody, at.Add(-time.Second), up)
	if k.Stats().Live != 1 {
		t.Errorf("compact left an end mark: the next request was not tracked")
	}
	clock.advance(290 * time.Second)
	k.sweep(clock.now())
	time.Sleep(50 * time.Millisecond)
	if fs.n() != 1 {
		t.Errorf("%d pings, want 1 (the post-compaction entry only)", fs.n())
	}
}

// Only the main agent compacts (/compact, auto-compact; since the thread fix the compaction call stays
// on the main thread): a compact signal retires the main thread's entry, and an idle subagent's held
// cache keeps its keep-alive. SessionEnd still retires every thread.
func TestCompactRetiresOnlyTheMainThread(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://up", path: "/v1/messages"}
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-caller-secret")
	hold := func(threadID string) {
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: "t1", Cache: pol}, "sess-1", threadID, clock.now().Add(time.Duration(i-1)*time.Second), []byte(kaBody), up, r,
				bschemas.Anthropic, "/v1/messages", http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
	}
	held := func(threadID string) bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		_, ok := k.live[kaThreadKey("t1", "sess-1", threadID)]
		return ok
	}
	hold("")
	hold("a:sub1")
	clock.advance(30 * time.Second)
	k.signal("t1", "sess-1", "SessionStart", "compact", clock.now())
	if held("") || !held("a:sub1") {
		t.Errorf("compact: main held %v (want false), idle subagent held %v (want true)", held(""), held("a:sub1"))
	}
	k.signal("t1", "sess-1", "SessionEnd", "prompt_input_exit", clock.now())
	if held("a:sub1") {
		t.Errorf("SessionEnd left the subagent's entry held")
	}
}

// The backgrounded compact event can land after the first post-compaction request was recorded: that
// new entry stays, the old prefix's entry goes.
func TestLateCompactSignalKeepsThePostCompactionEntry(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	k, _, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	up := upstream{base: "http://up", path: "/v1/messages"}
	recordOne(t, k, pol, kaBody, clock.now(), up) // the old prefix (the compaction call)
	clock.advance(30 * time.Second)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer sk-caller-secret")
	for _, at := range []time.Time{clock.now().Add(-time.Second), clock.now()} { // the post-compaction requests, already recorded
		k.record(&Tenancy{ID: "t1", Cache: pol}, "sess-1", "post", at, []byte(kaBody), up, r, bschemas.Anthropic, "/v1/messages",
			http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	}
	if k.Stats().Live != 2 {
		t.Fatalf("setup: %d live, want 2", k.Stats().Live)
	}
	clock.advance(500 * time.Millisecond)
	k.signal("t1", "sess-1", "SessionStart", "compact", clock.now())
	k.mu.Lock()
	_, oldHeld := k.live[kaThreadKey("t1", "sess-1", "")]
	_, newHeld := k.live[kaThreadKey("t1", "sess-1", "post")]
	k.mu.Unlock()
	if oldHeld || !newHeld {
		t.Errorf("late compact event: old prefix held %v (want false), post-compaction held %v (want true)", oldHeld, newHeld)
	}
}

// With the flag off nothing changes: the route is absent and a stale mark is ignored.
func TestSignalsAreInertWhenDisabled(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	h := &Handler{keeper: k}
	if c := sigPost(h, `{"session":"s","event":"SessionEnd"}`); c != http.StatusNotFound {
		t.Errorf("disabled route answered %d, want 404", c)
	}
	k.ended = map[string]map[string]time.Time{"t1": {kaKey("t1", "sess-1"): clock.now().Add(time.Hour)}}
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	recordOne(t, k, pol, kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"})
	if k.Stats().Live != 1 {
		t.Errorf("a mark applied with the flag off")
	}
}

// Validation: closed event set, closed kind set, no unknown fields, an unusable id, an oversize body.
func TestSignalValidation(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	k, _, _ := testKeeper(t, Limits{})
	h := &Handler{keeper: k, limiter: NewLimiter(Limits{}), static: &Tenancy{ID: "t1"}}
	for body, want := range map[string]int{
		`{"session":"abc-1","event":"SessionEnd","kind":"logout"}`:             http.StatusNoContent,
		`{"session":"abc-1","event":"SessionStart","kind":"compact"}`:          http.StatusNoContent,
		`{"session":"abc-1","event":"SessionStart","kind":"resume"}`:           http.StatusNoContent,
		`{"session":"abc-1","event":"SessionStart","kind":"bogus"}`:            http.StatusBadRequest,
		`{"session":"abc-1","event":"PreToolUse"}`:                             http.StatusBadRequest,
		`{"session":"abc-1","event":"SessionEnd","kind":"rm -rf"}`:             http.StatusBadRequest,
		`{"session":"abc-1","event":"SessionEnd","prompt":"x"}`:                http.StatusBadRequest,
		`{"session":"../../x","event":"SessionEnd"}`:                           http.StatusBadRequest,
		`{"session":"","event":"SessionEnd"}`:                                  http.StatusBadRequest,
		`{"session":"` + strings.Repeat("a", 2000) + `","event":"SessionEnd"}`: http.StatusBadRequest,
	} {
		if got := sigPost(h, body); got != want {
			t.Errorf("%.60s -> %d, want %d", body, got, want)
		}
	}
}

// Hosted: /signal needs the tenant's own token, and a tenant naming another tenant's session id
// only addresses a key inside its own namespace.
func TestSignalNeedsTheTenantsTokenAndCannotReachAnotherTenant(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	srv, reg := offloadTenancy(t)
	_, tok, err := reg.Register("laptop", "a@ibm.com")
	if err != nil {
		t.Fatal(err)
	}
	post := func(auth string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/signal", strings.NewReader(`{"session":"s1","event":"SessionEnd"}`))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(""); c != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", c)
	}
	if c := post("Bearer cg_live_forged"); c != http.StatusUnauthorized {
		t.Errorf("forged token: %d, want 401", c)
	}
	if c := post("Bearer " + tok); c != http.StatusNoContent {
		t.Errorf("valid token: %d, want 204", c)
	}

	k, _, clock := testKeeper(t, Limits{})
	pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
	recordOne(t, k, pol, kaBody, clock.now(), upstream{base: "http://up", path: "/v1/messages"}) // tenant t1, sess-1
	k.signal("t2", "t2:sess-1", "SessionEnd", "other", clock.now())
	k.signal("t2", "t2:t1:sess-1", "SessionEnd", "other", clock.now())
	if k.Stats().Live != 1 {
		t.Errorf("another tenant's SessionEnd retired this tenant's entry")
	}
}

// One tenant filling its end-mark bucket evicts only its own oldest marks.
func TestEndMarkCapIsPerTenant(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	now := clock.now()
	k.mu.Lock()
	k.markEndedLocked("victim", kaKey("victim", "v"), now)
	for i := 0; i < maxEndedPerTenant+50; i++ {
		k.markEndedLocked("noisy", kaKey("noisy", "s"+strconv.Itoa(i)), now.Add(time.Duration(i)*time.Second))
	}
	nb, vb := len(k.ended["noisy"]), len(k.ended["victim"])
	k.mu.Unlock()
	if nb != maxEndedPerTenant || vb != 1 {
		t.Errorf("noisy bucket %d (want %d), victim bucket %d (want 1)", nb, maxEndedPerTenant, vb)
	}
}

// The limiter applies: a tenant over its request rate gets 429, not a retire.
func TestSignalIsMetered(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	k, _, _ := testKeeper(t, Limits{})
	h := &Handler{keeper: k, limiter: NewLimiter(Limits{RequestsPerMinute: 1}), static: &Tenancy{ID: "t1"}}
	body := `{"session":"abc-1","event":"SessionEnd"}`
	if c := sigPost(h, body); c != http.StatusNoContent {
		t.Fatalf("first = %d", c)
	}
	if c := sigPost(h, body); c != http.StatusTooManyRequests {
		t.Errorf("second = %d, want 429", c)
	}
}

// A cross-origin page can POST text/plain to a loopback proxy without a preflight: only JSON is taken.
func TestSignalTakesJSONOnly(t *testing.T) {
	t.Setenv("CONTEXT_GURU_CLIENT_SIGNALS", "on")
	k, _, _ := testKeeper(t, Limits{})
	h := &Handler{keeper: k, limiter: NewLimiter(Limits{}), static: &Tenancy{ID: "t1"}}
	for ct, want := range map[string]int{"application/json": http.StatusNoContent, "application/json; charset=utf-8": http.StatusNoContent,
		"text/plain": http.StatusUnsupportedMediaType, "": http.StatusUnsupportedMediaType, "application/x-www-form-urlencoded": http.StatusUnsupportedMediaType} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/signal", strings.NewReader(`{"session":"abc-1","event":"SessionEnd","kind":"other"}`))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		h.clientSignal(w, r)
		if w.Code != want {
			t.Errorf("Content-Type %q: %d, want %d", ct, w.Code, want)
		}
	}
}
