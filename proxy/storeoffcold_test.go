package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// ttlSpy records the cache lifetime the cold check handed to the pipeline for each request.
type ttlSpy struct {
	mu  *sync.Mutex
	ttl *[]int64
}

func (ttlSpy) Name() string                 { return "ttlspy" }
func (ttlSpy) Enabled(*components.Ctx) bool { return true }
func (s ttlSpy) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	s.mu.Lock()
	*s.ttl = append(*s.ttl, c.CacheTTLMs)
	s.mu.Unlock()
	return nil
}

// #1350 with the store off, through the real proxy. The handler has a Nop store (storage off), so apply's cold
// check has no stored clock. The keeper's real ping, a free 1-hour one, is the only record of it. The proxy must
// plug the keeper's record into apply (apply.Opts.FreePing): then the request that returns after the ping is judged
// against the 1-hour lifetime, not the 5-minute one, and a cold-only cut cannot fire.
//
// The lifetime the pipeline sees is the observable: it is EntryLifetime of the cold check's own facts.
func TestTheProxyGivesTheColdCheckTheKeepersFreePingWhenTheStoreIsOff(t *testing.T) {
	for _, tc := range []struct {
		name        string
		st          store.Store
		keeperKnows bool // the keeper holds the ping itself (only when the store keeps nothing)
	}{
		{"store off (Nop)", store.Nop{}, true},
		// A real store: the stored clock says the same hour, and the keeper holds no copy of it.
		{"a real store", store.NewMemory(store.Options{}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5",` +
					`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
					`"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":48576,"cache_creation_input_tokens":0}}`))
			}))
			defer up.Close()
			var mu sync.Mutex
			var ttls []int64
			pipe := components.NewPipeline([]components.Component{ttlSpy{&mu, &ttls}}, nil)
			agg := metrics.NewAggregator()
			// Retention needs a real audit sink, or the keeper holds nothing (see the wire test).
			sink, err := dash.NewRecorder(dash.Options{DBPath: ":memory:", BatchSize: 1, FlushInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer sink.Close()
			h := New(pipe, tc.st, agg, Options{AnthropicUpstream: up.URL, Dashboard: sink})
			t.Cleanup(h.Close)
			// The keeper runs on a clock 10 minutes in the past, so its ping starts about 5 minutes ago, and a
			// real request now reads it.
			clock := &testClock{at: time.Now().Add(-10 * time.Minute)}
			fs := &fakeSender{usage: Usage{CacheRead: 48576, Output: 1}}
			k := rebuildKeeper(h, fs, clock)
			tn := h.static.Load()
			tn.Cache = keepalive.Policy{KeepAlive: true, Idle: 240 * time.Second, MaxPings: 2, MaxUSDPerPing: 0.25,
				MinPrefixTokens: 20000, WaitPlan: true, OneHourOnWait: true, Idle1h: 3540 * time.Second, MaxPings1h: 1}
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			body := strings.Replace(kaBody, `"model":"aws/claude-sonnet-5"`, `"model":"claude-sonnet-5"`, 1)
			// The session id the proxy will resolve for this request, so the keeper's thread is the request's thread.
			sess := apply.BodyOpts(context.Background(), components.NewPipeline(nil, nil), store.Nop{}, apply.Opts{Provider: bschemas.Anthropic,
				Body: []byte(body), Tenant: tn.ID, Session: "storeoff-1"}).Session
			rq := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
			rq.Header.Set("Authorization", "Bearer sk-test")
			rq.Header.Set("Anthropic-Version", "2023-06-01")
			up2 := upstream{base: up.URL, path: "/v1/messages"}
			// A tool turn, then the turn that ends the person's wait (end_turn): the keeper pings that thread.
			for i, u := range []Usage{
				{CacheRead: 48576, CacheWrite: 1200, StopReason: "tool_use", ToolUseNames: []string{"Bash"}},
				{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"},
			} {
				k.record(tn, sess, "", clock.now().Add(time.Duration(i-1)*time.Second), []byte(body), up2, rq, bschemas.Anthropic,
					"/v1/messages", http.StatusOK, u, true)
			}
			if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
				t.Fatalf("the keeper fired %d pings, want 1", n)
			}
			alias := apply.BodyOpts(context.Background(), components.NewPipeline(nil, nil), store.Nop{}, apply.Opts{Provider: bschemas.Anthropic,
				Body: []byte(body), Tenant: tn.ID}).TouchAlias
			if _, ok := k.StoreOffPing(alias); ok != tc.keeperKnows {
				t.Fatalf("the keeper holds the free 1-hour ping = %v, want %v", ok, tc.keeperKnows)
			}

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			// The return carries NO session header: the same conversation under another session id (apply derives
			// one). The cold check asks by alias, so it still finds the ping.
			req.Header.Set("x-api-key", "sk-test")
			req.Header.Set("anthropic-version", "2023-06-01")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if len(ttls) != 1 {
				t.Fatalf("the pipeline ran %d times, want 1", len(ttls))
			}
			if got := time.Duration(ttls[0]) * time.Millisecond; got != time.Hour {
				t.Errorf("the cold check judged the request against a %v lifetime, want 1h", got)
			}
		})
	}
}
