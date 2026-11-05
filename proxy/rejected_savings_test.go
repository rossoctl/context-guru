package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
)

// A request the provider rejected (a 4xx or 5xx) removed no tokens from any bill. Its row must
// book no saved tokens: not on the event, not on its component rows. The row keeps the size
// the proxy received, so tokens_after equals tokens_before. A 2xx row keeps its saving (#914).
func TestFinishBooksNoSavedTokensOnARejectedRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		usage      Usage
		usageOK    bool
		wantSaved  int
		wantActed  bool
		wantUnique int
	}{
		{"2xx keeps the saving", 200, Usage{FreshInput: 10, CacheWrite: 1000}, true, 900, true, 400},
		{"400 books none", 400, Usage{}, false, 0, false, 0},
		{"500 books none", 500, Usage{}, false, 0, false, 0},
		// A 2xx with no usage was billed by the provider: its saving is real, only unpriced.
		{"2xx without usage keeps the saving", 200, Usage{}, false, 900, true, 400},
		// Status 0 is a request that never reached a provider (/compact): not a rejection.
		{"status 0 keeps the saving", 0, Usage{}, false, 900, true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := dash.NewRecorder(dash.Options{
				DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rec.Close() })
			c := &capture{rec: rec, tenant: "t1", provider: "anthropic", model: "m", route: "/v1/messages",
				start: time.Now(), status: tc.status,
				trace: apply.Trace{Session: "s", Messages: 4, AttemptedTokens: 900, Run: &components.RunReport{
					TokensBefore: 1000, TokensAfter: 100,
					Components: []components.Report{{Component: "mask", Kind: "offload", TokensBefore: 1000, TokensAfter: 100}},
				}},
				unique: map[string]int{"mask": 400}}
			c.finish(tc.usage, tc.usageOK, false, 0, 0)

			var got *dash.Event
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) && got == nil {
				if pg, err := rec.DB().Requests(dash.Filter{TenantAll: true}, 0, 10); err == nil && len(pg.Requests) == 1 {
					got, err = rec.DB().Request(pg.Requests[0].ID, false)
					if err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			if got == nil {
				t.Fatal("no row was stored")
			}
			if got.Saved() != tc.wantSaved || got.SavedUnique != tc.wantUnique {
				t.Errorf("event saved = %d unique = %d, want %d and %d (tokens %d -> %d)",
					got.Saved(), got.SavedUnique, tc.wantSaved, tc.wantUnique, got.TokensBefore, got.TokensAfter)
			}
			if wantAtt := map[bool]int{true: 900, false: 0}[tc.wantSaved > 0]; got.AttemptedTokens != wantAtt {
				t.Errorf("attempted_tokens = %d, want %d", got.AttemptedTokens, wantAtt)
			}
			if len(got.Components) != 1 {
				t.Fatalf("components = %d, want 1", len(got.Components))
			}
			if c := got.Components[0]; c.SavedGross != tc.wantSaved || c.SavedUnique != tc.wantUnique || c.Acted != tc.wantActed {
				t.Errorf("component gross = %d unique = %d acted = %v, want %d, %d, %v",
					c.SavedGross, c.SavedUnique, c.Acted, tc.wantSaved, tc.wantUnique, tc.wantActed)
			}
		})
	}
}

// Review 927 round 1, finding 3. A rejected request whose error body still carries a usage object
// must book no dollars beside its zero tokens. The 200 control shows the same two requests do book a
// saving, so the zero on the 429 and the 400 comes from the status and not from the fixture.
func TestFinishBooksNoDollarsOnARejectedRequestThatReportedUsage(t *testing.T) {
	decl := func(name string, n int) map[string]any {
		return map[string]any{"name": name, "description": strings.Repeat("Does a thing. ", n),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}
	}
	b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 64,
		"tools":    []any{decl("bash", 3), decl("big_one", 400), decl("big_two", 400)},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	run := func(status int) dash.Event {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			n := len(body) / 4
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
				`"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34,`+
				`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`, n*8/10, n-n*8/10-12)
		}))
		defer up.Close()
		h, rec, tap := accHandlerYAML(t, up.URL,
			"pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    remove: [big_one, big_two]\n")
		accPostBody(t, h, "sess-rej", b)
		accPostBody(t, h, "sess-rej", b)
		waitForRows(t, rec, 2)
		return tap.all()[1]
	}
	ctl := run(200)
	if ctl.BaselineCostUSD <= ctl.CostUSD {
		t.Fatalf("control (200) booked no saving: baseline %v cost %v", ctl.BaselineCostUSD, ctl.CostUSD)
	}
	for _, status := range []int{429, 400} {
		e := run(status)
		if e.Status != status {
			t.Fatalf("status = %d, want %d", e.Status, status)
		}
		if e.Saved() != 0 || e.SavedUnique != 0 || e.BaselineCostUSD != e.CostUSD {
			t.Errorf("status %d: saved tokens %d, unique %d, baseline-cost delta %v, want all 0",
				status, e.Saved(), e.SavedUnique, e.BaselineCostUSD-e.CostUSD)
		}
		for _, c := range e.Components {
			if c.SavedUSD != 0 || c.SavedGross != 0 {
				t.Errorf("status %d: component %s saved %d tokens / $%v, want 0", status, c.Component, c.SavedGross, c.SavedUSD)
			}
		}
		if e.AccountantFallback() != "no_result" {
			t.Errorf("status %d: fallback %q, want no_result", status, e.AccountantFallback())
		}
	}
}
