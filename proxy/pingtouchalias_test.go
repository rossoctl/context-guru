package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tooldefer"
)

// #902, end to end: an ultrahigh-shaped request, a ping on the DEFERRED body the keeper holds, then
// the next real request. The ping must keep the request's cold check warm.
const pingAliasBody = `{"model":"claude-sonnet-5","max_tokens":64,"tools":[` +
	`{"name":"Read","description":"r","input_schema":{"type":"object"}},` +
	`{"name":"Bash","description":"b","input_schema":{"type":"object"}},` +
	`{"name":"mcp__x__one","description":"o","input_schema":{"type":"object"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"fix the failing test",` +
	`"cache_control":{"type":"ephemeral"}}]},` +
	`{"role":"system","content":"the environment block"},{"role":"user","content":"go on"}]}`

type coldProbe struct{ seen *[]bool }

func (coldProbe) Name() string                 { return "coldprobe" }
func (coldProbe) Enabled(*components.Ctx) bool { return true }
func (s coldProbe) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	*s.seen = append(*s.seen, c.ColdCache)
	return nil
}

func TestAPingOnADeferredBodyKeepsTheNextRequestWarm(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deferTools bool
		viaTrace   bool // the fix: the keeper is handed the arriving request's alias
		wantCold   bool
	}{
		{"ultrahigh, alias from the arriving request", true, true, false},
		{"ultrahigh, alias re-derived from the deferred body (the bug)", true, false, true},
		{"xhigh, no deferral", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, clock := testKeeper(t, Limits{})
			st := store.NewMemory(store.Options{})
			var seen []bool
			pipe := components.NewPipeline([]components.Component{coldProbe{seen: &seen}}, nil)
			tracker := modes.NewTracker(0)
			start := clock.now()
			run := func(at time.Time) apply.Result {
				return apply.BodyOpts(context.Background(), pipe, st, apply.Opts{
					Tenant: "t1", Provider: bschemas.Anthropic, Body: []byte(pingAliasBody), Tracker: tracker, Now: at})
			}
			first := run(start)
			sent := []byte(pingAliasBody)
			if tc.deferTools {
				out, _, ok := tooldefer.Rewrite(sent, map[string]bool{"mcp__x__one": true})
				if !ok {
					t.Fatal("fixture: nothing deferred")
				}
				sent = out
			}
			alias := ""
			if tc.viaTrace {
				alias = first.Trace.TouchAlias
			}
			tn := &Tenancy{ID: "t1", Cache: kaPolicy(), Store: st}
			for _, d := range []time.Duration{-time.Second, 0} {
				r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
				r.Header.Set("Authorization", "Bearer sk-caller-secret")
				k.recordTouch(alias, tn, "sess-1", "", start.Add(d), sent,
					upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
					http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 100}, true)
			}
			clock.advance(281 * time.Second)
			if n := k.Sweep(clock.now()); n != 1 {
				t.Fatalf("%d pings sent, want 1", n)
			}
			run(start.Add(8 * time.Minute))
			if len(seen) != 2 {
				t.Fatalf("probe saw %d runs, want 2", len(seen))
			}
			if seen[1] != tc.wantCold {
				t.Fatalf("ColdCache = %v 8m after the request and 4m after the ping, want %v", seen[1], tc.wantCold)
			}
		})
	}
}
