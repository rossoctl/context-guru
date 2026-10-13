package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// A slow request of the session (a long /compact call) is in flight: the held entry of its other
// thread is due, but no ping goes out until that request has finished.
func TestNoPingWhileARequestOfTheSessionIsInFlight(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var pings atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		b.ReadFrom(r.Body)
		switch {
		case strings.Contains(b.String(), "SUMMARIZE"):
			<-release // the slow call
		case gjsonMaxTokens(b.Bytes()) == 1:
			pings.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"aws/claude-sonnet-5","content":[{"type":"text","text":"ok"}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":48576,"cache_creation_input_tokens":0}}`))
	}))
	defer up.Close()
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
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
		Cache: CachePolicy{KeepAlive: true, Idle: 60 * time.Second, MaxPings: 2, MaxUSDPerPing: 1, MinPrefixTokens: 1}})
	t.Cleanup(h.Close)
	clock := &testClock{at: time.Now()}
	h.keeper.now = clock.now
	h.keeper.dispatch = h.keeper.fire
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	defer unblock() // runs before the servers close: a failing run must not leave the upstream handler blocked

	sess := "inflight-" + strconv.FormatInt(time.Now().UnixNano(), 36) // process-wide thread state outlives one run
	post := func(tools bool, texts ...string) {
		var msgs []any
		for i, s := range texts {
			msgs = append(msgs, map[string]any{"role": []string{"user", "assistant"}[i%2], "content": s})
		}
		m := map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64, "messages": msgs,
			"system": []any{map[string]any{"type": "text", "text": "you are claude code", "cache_control": map[string]any{"type": "ephemeral"}}}}
		if tools {
			m["tools"] = []any{map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}}}
		}
		body, _ := json.Marshal(m)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", sess)
		req.Header.Set("x-api-key", "sk-test")
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	}
	post(true, "fix the bug")
	post(true, "fix the bug", "done", "thanks")
	if h.keeper.Stats().Live != 1 {
		t.Fatalf("setup: %d live, want 1 (%+v)", h.keeper.Stats().Live, h.keeper.Stats())
	}
	done := make(chan struct{})
	go func() { post(false, "fix the bug", "done", "thanks", "ok", "SUMMARIZE the conversation"); close(done) }()
	for deadline := time.Now().Add(5 * time.Second); h.keeper.inflightCount("", sess) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the slow request never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.keeper.sweep(clock.advance(90 * time.Second)) // due, but a request of the session is in flight
	if n := pings.Load(); n != 0 {
		t.Errorf("%d pings while the session's slow request was in flight", n)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow request never finished")
	}
	h.keeper.sweep(clock.advance(time.Second))
	if n := pings.Load(); n != 1 {
		t.Errorf("%d pings after the slow request finished, want 1", n)
	}
}

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
	k, _, clock := testKeeper(t, Limits{})
	var sent atomic.Int32
	var empty atomic.Bool
	empty.Store(true)
	k.send = func(j pingJob, b []byte) (Usage, int, error) {
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
	k.sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	st := k.Stats()
	if st.Failed != routeFailLimit || st.RouteTrips != 1 {
		t.Fatalf("failed %d (want %d), trips %d (want 1)", st.Failed, routeFailLimit, st.RouteTrips)
	}
	k.sweep(clock.advance(290 * time.Second))
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
	k.sweep(clock.advance(290 * time.Second))
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

// Explicit cache control is a Claude feature: another family on an Anthropic-shaped route is not held.
func TestKeepAliveNeedsAClaudeModelOnTheAnthropicRoute(t *testing.T) {
	for model, want := range map[string]bool{"aws/claude-sonnet-5": true, "claude-haiku-4-5": true, "opus5-5": true,
		"vertex_ai/claude-opus-4-5@20251101": true, "us.anthropic.claude-sonnet-4-5-20250929-v1:0": true,
		"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-opus-4-5-v1:0": true,
		"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3":                false,
		"gpt-5-mini-2025-08-07": false, "gpt-5.6-sol": false} {
		if got := keepAliveEligible(bschemas.Anthropic, model, "/v1/messages"); got != want {
			t.Errorf("%s: eligible %v, want %v", model, got, want)
		}
	}
}

func (k *keeper) inflightCount(tenant, session string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.inflight[kaKey(tenant, session)]
}

// A real miss (the ping landed after expiry: read 0, the prefix written again) is not a failure: it
// is counted missed, does not trip the route, and pinging goes on. Two in a row are a prefix that
// does not match, and the session stops.
func TestARealMissIsNotAFailedPing(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	var sent atomic.Int32
	miss := Usage{CacheWrite: 48576, Output: 1}
	reply := []Usage{miss, {CacheRead: 48576, Output: 1}, miss, miss}
	k.send = func(j pingJob, b []byte) (Usage, int, error) {
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
		k.sweep(clock.advance(290 * time.Second))
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
		{"openai miss", Usage{FreshInput: 48576, Output: 1}, http.StatusOK, false},
		{"anthropic miss", Usage{CacheWrite: 48576, Output: 1}, http.StatusOK, false},
		{"read", Usage{CacheRead: 48576, Output: 1}, http.StatusOK, false},
	} {
		k, _, clock := testKeeper(t, Limits{})
		k.send = func(pingJob, []byte) (Usage, int, error) { return c.u, c.status, nil }
		pol := CachePolicy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2, MinPrefixTokens: 20000}
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-x")
		for i := 0; i < 2; i++ {
			k.record(&Tenancy{ID: "t1", Cache: pol}, "s", "", clock.now().Add(time.Duration(i)*time.Second), []byte(kaBody),
				upstream{base: "http://api", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages", http.StatusOK,
				Usage{CacheRead: 48576, CacheWrite: 1200}, true)
		}
		k.sweep(clock.advance(290 * time.Second))
		time.Sleep(30 * time.Millisecond)
		if got := k.Stats().Failed == 1; got != c.failed {
			t.Errorf("%s: failed %v, want %v", c.name, got, c.failed)
		}
	}
}

// CONTEXT_GURU_KEEPALIVE_MODELS adds names for aliases that do not say what they are.
func TestKeepAliveModelAliasesFromTheEnvironment(t *testing.T) {
	t.Setenv("CONTEXT_GURU_KEEPALIVE_MODELS", " application-inference-profile/a1b2c3 , team-model ")
	for model, want := range map[string]bool{"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3": true,
		"Team-Model-Large": true, "gpt-5-mini-2025-08-07": false} {
		if got := keepAliveEligible(bschemas.Anthropic, model, "/v1/messages"); got != want {
			t.Errorf("%s: eligible %v, want %v", model, got, want)
		}
	}
}

// An overload burst (529, any 5xx) is transient: it neither fails the session nor trips the route.
func TestOverloadedPingsDoNotTripTheRoute(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	k.send = func(pingJob, []byte) (Usage, int, error) { return Usage{}, 529, nil }
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
	k.sweep(clock.advance(290 * time.Second))
	time.Sleep(50 * time.Millisecond)
	if st := k.Stats(); st.Failed != 0 || st.RouteTrips != 0 {
		t.Errorf("529s: failed %d, trips %d, want 0 and 0", st.Failed, st.RouteTrips)
	}
}

// Bedrock and Vertex are Claude routes whatever the deployment is called (inference-profile ARNs).
func TestBedrockAndVertexNeedNoClaudeName(t *testing.T) {
	arn := "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3"
	for _, p := range []bschemas.ModelProvider{bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex} {
		if !keepAliveEligible(p, arn, "/v1/messages") {
			t.Errorf("%s: an inference-profile ARN was not eligible", p)
		}
	}
}
