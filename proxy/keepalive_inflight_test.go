package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
		req.Header.Set("x-context-guru-session", "inflight-1")
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
	for deadline := time.Now().Add(5 * time.Second); h.keeper.inflightCount("", "inflight-1") == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the slow request never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.keeper.sweep(clock.advance(90 * time.Second)) // due, but a request of the session is in flight
	if n := pings.Load(); n != 0 {
		t.Errorf("%d pings while the session's slow request was in flight", n)
	}
	close(release)
	<-done
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
