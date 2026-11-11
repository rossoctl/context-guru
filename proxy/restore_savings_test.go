package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// #481: with expand.fixed_restore the dashboard credited the full compaction of an output the
// model asked back for on every later turn, although each of those turns forwards a restored copy
// of it. The proxy knew the copy's size (Result.RestoredTokens) and only logged it.

// restoreSavingsRun drives one in-band expand (turn N) and three later turns that re-send the same
// history, against a real recorder, and returns the stored rows (oldest first) and the overview.
// The upstream bills usage as len(body)/5, so a turn that forwards the copy is billed for it.
func restoreSavingsRun(t *testing.T, fixedRestore bool) ([]*dash.Event, *dash.Overview) {
	t.Helper()
	big := strings.Repeat("the original tool output line that had to come back\n", 200)
	markerRe := regexp.MustCompile(`<<cg:([A-Za-z0-9_-]{1,64})>>`)
	var mu sync.Mutex
	expandNext := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		do := expandNext
		expandNext = false
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		raw := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(string(b))
		id := ""
		if m := markerRe.FindStringSubmatch(raw); m != nil {
			id = m[1]
		}
		usage := `"usage":{"input_tokens":` + strconv.Itoa(len(b)/5) + `,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
		if do {
			w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"tu_x","name":"context_guru_expand","input":{"id":"` + id + `"}}],"stop_reason":"tool_use",` + usage + `}`))
			return
		}
		w.Write([]byte(`{"id":"m2","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn",` + usage + `}`))
	}))
	t.Cleanup(up.Close)

	yaml := "pipeline: [extract]\ncomponents:\n  extract: {min_tokens: 5}\nexpand:\n  fixed_restore: " + strconv.FormatBool(fixedRestore) + "\n"
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"), BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL, OpenAIUpstream: up.URL, Dashboard: rec, Prices: fixedPricer{}})
	h.tracker.SetStartedAt(time.Now().Add(-2 * time.Hour).UnixMilli())
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)

	transcript := func(replies ...string) []any {
		out := []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": big}}},
		}
		for _, r := range replies {
			out = append(out,
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": r}}},
				map[string]any{"role": "user", "content": "next after " + r})
		}
		return out
	}
	post := func(msgs []any, expand bool) {
		mu.Lock()
		expandNext = expand
		mu.Unlock()
		b, _ := json.Marshal(map[string]any{"model": "aws/claude-sonnet-5", "max_tokens": 100, "messages": msgs,
			"tools": []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}}})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-context-guru-session", "sess-481")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post(transcript(), true)                        // turn N: the model expands in-band
	post(transcript("done"), false)                 // N+1
	post(transcript("done", "done"), false)         // N+2
	post(transcript("done", "done", "done"), false) // N+3
	waitForRows(t, rec, 4)

	page, err := rec.DB().Requests(dash.Filter{}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 4 {
		t.Fatalf("captured %d rows, want 4", len(page.Requests))
	}
	rows := make([]*dash.Event, 4)
	for i, e := range page.Requests { // newest first -> oldest first
		rows[3-i] = e
	}
	o, err := rec.DB().Overview(dash.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Expands != 1 {
		t.Fatalf("precondition: turn N must expand once, got %d", rows[0].Expands)
	}
	return rows, o
}

// CONTROL: with fixed_restore off the original goes back in place, so a later turn forwards the
// whole output again and credits nothing. This is the figure the fixed-restore run must not beat.
func TestRestoreSavingsControlOff(t *testing.T) {
	rows, o := restoreSavingsRun(t, false)
	for i, e := range rows[1:] {
		if e.TokensBefore-e.TokensAfter > 0 {
			t.Fatalf("control: turn N+%d credits %d tokens although the original is back in place", i+1, e.TokensBefore-e.TokensAfter)
		}
	}
	if o.SavedGross != int64(rows[0].TokensBefore-rows[0].TokensAfter) {
		t.Fatalf("control: saved_gross = %d, want turn N's %d alone", o.SavedGross, rows[0].TokensBefore-rows[0].TokensAfter)
	}
}

// A later turn forwards the compacted original AND a full restored copy, so it forwards at least
// what the client sent. It must not be credited a saving.
func TestFixedRestoreLaterTurnsCreditNoCompaction(t *testing.T) {
	rows, o := restoreSavingsRun(t, true)
	for i, e := range rows[1:] {
		if got := e.TokensBefore - e.TokensAfter; got > 0 {
			t.Errorf("turn N+%d: credited %d tokens saved (before=%d after=%d), but it forwards a restored copy of the output",
				i+1, got, e.TokensBefore, e.TokensAfter)
		}
	}
	if want := int64(rows[0].TokensBefore - rows[0].TokensAfter); o.SavedGross > want {
		t.Errorf("overview saved_gross = %d; want at most turn N's %d (the control's figure)", o.SavedGross, want)
	}
}

// The restored copy every later turn carries must reach the dashboard, not only the log:
// SafetyCost.RestoredTokens is what the turns carried, not the expand turn's own tokens.
func TestFixedRestoreCopySizeReachesTheDashboard(t *testing.T) {
	rows, o := restoreSavingsRun(t, true)
	if o.SafetyCost.RestoredTokens <= int64(rows[0].ExpandTokens) {
		t.Errorf("safety_cost.restored_tokens = %d; want more than the expand turn's %d, because 3 later turns each carried the copy",
			o.SafetyCost.RestoredTokens, rows[0].ExpandTokens)
	}
}
