package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// #1365. A real request writes its clock before the forward, over a standing free 1-hour ping, as a carry
// ("<ms>^<pingMs>#<seq>"). When the response is accepted, the keeper's Record ends THAT carry, and only if the proxy
// hands it the clock its own request wrote (apply.Trace.TouchAtMs and TouchSeq, through serve and recordTouch).
// This runs the real handler, the real apply and the real keeper over one store, so it fails when the proxy
// stops passing the clock: the carry then never ends, and an accepted request leaves the old ping's hour in place.
func TestTheProxyHandsTheRequestsOwnClockToTheKeepersConfirm(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantCarry bool // the ping's hour still stands after the request
	}{{"accepted", http.StatusOK, false}, {"failed", http.StatusInternalServerError, true}} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5",` +
					`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
					`"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":48576,"cache_creation_input_tokens":0}}`))
			}))
			defer up.Close()
			cfg, err := config.LoadBytes([]byte("pipeline: [textclean]\n"))
			if err != nil {
				t.Fatal(err)
			}
			agg := metrics.NewAggregator()
			pipe, err := cfg.Build(agg)
			if err != nil {
				t.Fatal(err)
			}
			st := store.NewMemory(store.Options{})
			h := New(pipe, st, agg, Options{AnthropicUpstream: up.URL})
			t.Cleanup(h.Close)
			srv := httptest.NewServer(h.Mux())
			defer srv.Close()

			body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "max_tokens": 64,
				"system": "you are claude code", "messages": []any{map[string]any{"role": "user", "content": "fix the test"}}})
			alias := apply.TouchAliasFor(h.static.Load().ID, body, bschemas.Anthropic)
			key := store.SeenPrefix + alias
			// A free 1-hour ping, 4 minutes ago, stands under the alias.
			ping := time.Now().Add(-4 * time.Minute).UnixMilli()
			apply.RecordCacheTouchAliasRoute(st, alias, ping, true, apply.RouteTag("r"))

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-context-guru-session", "touchconfirm-"+tc.name)
			req.Header.Set("x-api-key", "sk-test")
			req.Header.Set("anthropic-version", "2023-06-01")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			// The keeper records inside the handler: closing the server waits for it, so no polling is needed.
			srv.Close()
			v, _ := st.Get(key)
			has := strings.Contains(string(v), "^"+strconv.FormatInt(ping, 10))
			if !strings.Contains(string(v), "^") && tc.wantCarry {
				t.Fatalf("fixture: apply wrote no carry over the ping: %q", v)
			}
			if has != tc.wantCarry {
				t.Errorf("after a %d response the record is %q: the ping's hour standing = %v, want %v",
					tc.status, string(v), has, tc.wantCarry)
			}
		})
	}
}
