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
	"github.com/rossoctl/context-guru/schema"
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

// idleProbe records Ctx.IdleMs: OpenAI's cache lifetime is a minimum, so ColdCache never fires there,
// and the idle time is what the same cg:seen clock decides.
type idleProbe struct{ seen *[]int64 }

func (idleProbe) Name() string                 { return "idleprobe" }
func (idleProbe) Enabled(*components.Ctx) bool { return true }
func (s idleProbe) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	*s.seen = append(*s.seen, c.IdleMs)
	return nil
}

// #902 on OpenAI. Our own tool search (tooldefer.RewriteSearch) INSERTS the names list as the first
// system message on Chat Completions (no leading system message) and as a developer message at
// input[0] on Responses (no `instructions`). The head the alias hashes then changes, as on Anthropic.
func TestAPingOnASearchRewrittenOpenAIBodyKeepsTheNextRequestWarm(t *testing.T) {
	tools := `"tools":[{"type":"function","name":"keep","parameters":{"type":"object"}},` +
		`{"type":"function","name":"mcp_one","parameters":{"type":"object"}}]`
	chatTools := `"tools":[{"type":"function","function":{"name":"keep","parameters":{"type":"object"}}},` +
		`{"type":"function","function":{"name":"mcp_one","parameters":{"type":"object"}}}]`
	for _, tc := range []struct{ name, wire, api, path, body string }{
		{"chat completions, no leading system message", schema.WireOpenAI, "", "/openai/v1/chat/completions",
			`{"model":"gpt-5.6",` + chatTools + `,"messages":[{"role":"user","content":"fix the failing test"},` +
				`{"role":"assistant","content":"ok"},{"role":"user","content":"go on"}]}`},
		{"responses, no instructions", schema.WireResponses, "responses", "/openai/v1/responses",
			`{"model":"gpt-5.6",` + tools + `,"input":[{"type":"message","role":"user","content":"fix the failing test"},` +
				`{"type":"message","role":"assistant","content":"ok"},{"type":"message","role":"user","content":"go on"}]}`},
	} {
		for _, viaTrace := range []bool{true, false} {
			name := tc.name + map[bool]string{true: ", alias from the arriving request", false: ", alias re-derived (the bug)"}[viaTrace]
			t.Run(name, func(t *testing.T) {
				st := store.NewMemory(store.Options{})
				var seen []int64
				pipe := components.NewPipeline([]components.Component{idleProbe{seen: &seen}}, nil)
				tracker := modes.NewTracker(0)
				start := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
				run := func(at time.Time) apply.Result {
					return apply.BodyOpts(context.Background(), pipe, st, apply.Opts{
						Tenant: "t1", Provider: bschemas.OpenAI, API: tc.api, CacheMode: "on",
						Body: []byte(tc.body), Tracker: tracker, Now: at})
				}
				first := run(start)
				sent, _, ok := tooldefer.RewriteSearch(tc.wire, []byte(tc.body), tooldefer.SearchRecord{Deferred: []string{"mcp_one"}})
				if !ok {
					t.Fatal("fixture: nothing deferred")
				}
				if apply.SessionIDFor("t1", "", bschemas.OpenAI, sent) == apply.SessionIDFor("t1", "", bschemas.OpenAI, []byte(tc.body)) {
					t.Fatal("the alias is the same before and after the rewrite: this shape is not the #902 shape")
				}
				// The ping's touch at +281 s, as keepalive's pingJob.touch makes it: under the alias the
				// host handed in when there is one, else derived from the body that went upstream.
				at := start.Add(281 * time.Second).UnixMilli()
				if viaTrace {
					if first.Trace.TouchAlias == "" {
						t.Fatal("the Trace carries no TouchAlias")
					}
					apply.RecordCacheTouchAlias(st, first.Trace.TouchAlias, at)
				} else {
					apply.RecordCacheTouch(st, "t1", sent, bschemas.OpenAI, at)
				}
				run(start.Add(8 * time.Minute))
				if len(seen) != 2 {
					t.Fatalf("probe saw %d runs, want 2", len(seen))
				}
				want := (8 * time.Minute).Milliseconds() // the bug: the ping is invisible, idle runs from the request
				if viaTrace {
					want = (8*time.Minute - 281*time.Second).Milliseconds() // idle runs from the ping's start
				}
				if seen[1] != want {
					t.Fatalf("IdleMs = %d, want %d", seen[1], want)
				}
			})
		}
	}
}
