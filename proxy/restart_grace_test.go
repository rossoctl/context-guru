package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// Issue #766: the proxy wires the tracker's restart grace to the dashboard recorder (New, next
// to SetRestartGrace), so after a restart the grace decides by the thread's own last request —
// the time SeedSessions reloads from the requests table — and not by the proxy's start time.
// One handler plays the process before the restart; a second handler, with a fresh recorder on
// the same database seeded the way main.go seeds it, plays the process after.
func TestTheRestartGraceAsksTheRecorderForTheThreadsLastRequest(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "d.db")
	h, rec := dashHandler(t, up.URL, dash.Options{DBPath: dbPath})
	// Model steady state: the pre-restart tracker is two hours old, so its grace does not
	// protect the thread's first request and the pipeline compacts. That request is the row
	// the restarted recorder seeds from.
	h.tracker.SetStartedAt(time.Now().Add(-2 * time.Hour).UnixMilli())
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()

	send := func(server *httptest.Server, session string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/anthropic/v1/messages",
			strings.NewReader(string(anthropicRequest("aws/claude-sonnet-5"))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
		req.Header.Set("x-context-guru-session", session)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("proxy returned %d for session %q", resp.StatusCode, session)
		}
	}
	send(srv, "sess-restart")
	waitForRows(t, rec, 1)

	// The restart: a fresh recorder on the same database, seeded as main.go seeds it, and a
	// fresh handler around it. The handler's tracker start is set two hours back, so #759's
	// start-time fallback alone would protect nothing on this handler: only the wired lookup
	// can. The thread's last request was moments ago, well inside the 5-minute lifetime a
	// plain Anthropic body carries.
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec2, err := dash.NewRecorder(dash.Options{DBPath: dbPath, BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec2.Close() })
	if n, err := rec2.SeedSessions(time.Now().UnixMilli()); err != nil || n < 1 {
		t.Fatalf("SeedSessions recovered %d sessions (err %v); want at least the live thread", n, err)
	}
	h2 := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL,
		OpenAIUpstream:    up.URL,
		Preset:            cfg.Preset,
		Dashboard:         rec2,
		Prices:            fixedPricer{},
	})
	h2.tracker.SetStartedAt(time.Now().Add(-2 * time.Hour).UnixMilli())
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()

	send(srv2, "sess-restart")
	send(srv2, "sess-brand-new")
	waitForRows(t, rec2, 2)

	row := func(session string) *dash.Event {
		page, err := rec2.DB().Requests(dash.Filter{}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Requests {
			if e.SessionID == session {
				return e
			}
		}
		t.Fatalf("no row for session %q (page total %d)", session, page.Total)
		return nil
	}
	// Protection shows as the frozen prefix, not as "nothing changed": the deterministic
	// components outside the boundary's reach (format, linecap) still trim, so the row's
	// saving is not the signal. The ATTEMPTED figure is: a protected first request offers
	// the pipeline no mutable tail at all.
	live := row("sess-restart")
	if live.AttemptedTokens != 0 || live.FrozenTokens == 0 {
		t.Fatalf("live thread after the restart: %d of %d tokens attempted; want the whole request frozen (the grace protects it)",
			live.AttemptedTokens, live.TokensBefore)
	}
	fresh := row("sess-brand-new")
	if fresh.AttemptedTokens == 0 {
		t.Fatalf("brand-new thread after the restart: 0 of %d tokens attempted; want the request editable (no history, no grace)",
			fresh.TokensBefore)
	}
}
