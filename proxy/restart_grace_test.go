package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// codesafePipeline builds the pipeline every test here serves through.
func codesafePipeline(t *testing.T) (*config.Config, *metrics.Aggregator, *components.Pipeline) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, agg, pipe
}

// send posts one Anthropic-dialect request under the given session header.
func send(t *testing.T, server *httptest.Server, session string) {
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

// restartedHandler plays the process AFTER a restart: a fresh recorder on the same database,
// seeded the way main.go seeds it (recency, then the grace clock), and a fresh handler around
// it. The handler's tracker start is set back to startedAt, so #759's start-time fallback
// alone would protect nothing on it — only the wired lookup can.
func restartedHandler(t *testing.T, up, dbPath string, seededAt, startedAt int64) (*Handler, *dash.Recorder) {
	t.Helper()
	cfg, agg, pipe := codesafePipeline(t)
	rec, err := dash.NewRecorder(dash.Options{DBPath: dbPath, BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	if _, err := rec.SeedSessions(seededAt); err != nil {
		t.Fatalf("SeedSessions: %v", err)
	}
	if _, err := rec.SeedGraceSessions(seededAt); err != nil {
		t.Fatalf("SeedGraceSessions: %v", err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up,
		OpenAIUpstream:    up,
		Preset:            cfg.Preset,
		Dashboard:         rec,
		Prices:            fixedPricer{},
	})
	h.tracker.SetStartedAt(startedAt)
	return h, rec
}

// row returns the dashboard row of one session.
func row(t *testing.T, rec *dash.Recorder, session string) *dash.Event {
	t.Helper()
	page, err := rec.DB().Requests(dash.Filter{}, 0, 10)
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

// Issue #766: the proxy wires the tracker's restart grace to the dashboard recorder (New, next
// to SetRestartGrace), so after a restart the grace decides by the thread's own last cache
// refresh — the time SeedGraceSessions reloads from the requests table — and not by the
// proxy's start time. The pre-restart rows are injected with a controlled timestamp, so the
// grace's answer does not depend on when the test happens to run.
func TestTheRestartGraceAsksTheRecorderForTheThreadsLastRequest(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "d.db")
	rec, err := dash.NewRecorder(dash.Options{DBPath: dbPath, BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	t0 := time.Now().UnixMilli()
	// The thread's last request: one minute before the restart, well inside the 5-minute
	// lifetime a plain Anthropic body carries.
	rec.Record(&dash.Event{TS: t0 - 60_000, SessionID: "sess-restart",
		Model: "aws/claude-sonnet-5", TokensBefore: 10})
	waitForRows(t, rec, 1)

	h2, rec2 := restartedHandler(t, up.URL, dbPath, t0, t0-2*60*60*1000)
	if at, ok := rec2.ThreadRefreshedAt("sess-restart"); !ok || at != t0-60_000 {
		t.Fatalf("the seeded grace clock = (%d, %v), want the row's time (%d, true)", at, ok, t0-60_000)
	}
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()

	send(t, srv2, "sess-restart")
	send(t, srv2, "sess-brand-new")
	waitForRows(t, rec2, 2)

	// Protection shows as the frozen prefix, not as "nothing changed": the deterministic
	// components outside the boundary's reach (format, linecap) still trim, so the row's
	// saving is not the signal. The ATTEMPTED figure is: a protected first request offers
	// the pipeline no mutable tail at all.
	live := row(t, rec2, "sess-restart")
	if live.AttemptedTokens != 0 || live.FrozenTokens == 0 {
		t.Fatalf("live thread after the restart: %d of %d tokens attempted; want the whole request frozen (the grace protects it)",
			live.AttemptedTokens, live.TokensBefore)
	}
	fresh := row(t, rec2, "sess-brand-new")
	if fresh.AttemptedTokens == 0 {
		t.Fatalf("brand-new thread after the restart: 0 of %d tokens attempted; want the request editable (no history, no grace)",
			fresh.TokensBefore)
	}
}

// Issue #766, round 2: a keep-alive ping that READ the cache keeps the provider's entry warm,
// so it counts on the grace clock. A thread idle two hours but pinged one minute before the
// restart, back one minute after it, is protected; the same thread whose ping read nothing
// stays expired and editable.
func TestTheRestartGraceCountsKeepAlivePingsThatRefreshTheCache(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "d.db")
	rec, err := dash.NewRecorder(dash.Options{DBPath: dbPath, BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	t0 := time.Now().UnixMilli()
	const twoH = int64(2 * 60 * 60 * 1000)
	inject := func(session string, at int64, keepAlive bool, cacheRead int64) {
		rec.Record(&dash.Event{TS: at, SessionID: session, Model: "aws/claude-sonnet-5",
			TokensBefore: 10, KeepAlive: keepAlive, CacheRead: cacheRead})
	}
	// The pinged thread: last real request two hours ago, last ping one minute before the
	// restart, and that ping read the cache.
	inject("sess-ping", t0-twoH, false, 0)
	inject("sess-ping", t0-60_000, true, 900)
	// The cold thread: the same real request, but its ping read nothing — the entry was
	// already gone, so nothing kept it warm.
	inject("sess-cold", t0-twoH, false, 0)
	inject("sess-cold", t0-60_000, true, 0)
	waitForRows(t, rec, 4)

	h2, rec2 := restartedHandler(t, up.URL, dbPath, t0, t0-twoH)
	if at, ok := rec2.ThreadRefreshedAt("sess-ping"); !ok || at != t0-60_000 {
		t.Fatalf("pinged thread's grace clock = (%d, %v), want the ping's time (%d, true)", at, ok, t0-60_000)
	}
	if at, ok := rec2.ThreadRefreshedAt("sess-cold"); !ok || at != t0-twoH {
		t.Fatalf("cold thread's grace clock = (%d, %v), want its real request's time (%d, true)", at, ok, t0-twoH)
	}
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()

	send(t, srv2, "sess-ping")
	send(t, srv2, "sess-cold")
	waitForRows(t, rec2, 2)

	pinged := row(t, rec2, "sess-ping")
	if pinged.AttemptedTokens != 0 || pinged.FrozenTokens == 0 {
		t.Fatalf("pinged thread after the restart: %d of %d tokens attempted; want the whole request frozen (the ping kept the entry warm)",
			pinged.AttemptedTokens, pinged.TokensBefore)
	}
	cold := row(t, rec2, "sess-cold")
	if cold.AttemptedTokens == 0 {
		t.Fatalf("cold thread after the restart: 0 of %d tokens attempted; want the request editable (the ping read nothing, so nothing was warm)",
			cold.TokensBefore)
	}
}

// Issue #766, round 2: in observe mode the dashboard row carries no session id (the enforced
// trace is zeroed), so the recorder cannot know any thread and a wired lookup would read
// every warm thread as new — the one answer worse than not asking. New wires the lookup only
// for an enforcing mode, so observe keeps #759's start-time fallback: an unknown thread
// inside one cache lifetime of the restart still reads as all-cached.
//
// The dashboard row cannot show this — an observe row's figures are zeroed with the trace —
// so the probe asks the tracker directly. That is the same tracker the off-path run
// consults, which is where observe mode's boundary decision happens.
func TestObserveModeKeepsTheStartTimeFallbackForTheRestartGrace(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "d.db")
	rec, err := dash.NewRecorder(dash.Options{DBPath: dbPath, BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	// The recorder knows a thread, the way a live one would: if observe mode had wired the
	// lookup, it would have something to answer with — and a thread it does not know would
	// read as new, past a fallback that no longer runs.
	rec.SeedGrace(map[string]int64{"sess-other": time.Now().UnixMilli() - 60_000})

	cfg, agg, pipe := codesafePipeline(t)
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL,
		OpenAIUpstream:    up.URL,
		Preset:            cfg.Preset,
		Dashboard:         rec,
		Prices:            fixedPricer{},
		Mode:              components.ModeObserve,
	})
	srv := httptest.NewServer(h.Mux())
	defer srv.Close()
	send(t, srv, "sess-observe")

	// The restart: this handler is the restarted process, and a thread the recorder has
	// never seen must still read as all-cached inside the start-time window. With the lookup
	// wired (the bug), the same probe reads 0: the lookup misses and calls the thread new.
	if n, _ := h.tracker.TurnAt("sess-observe-probe", 925, time.Now().UnixMilli(), 5*time.Minute); n != 925 {
		t.Fatalf("unknown thread inside the start-time window = %d, want 925 (the fallback protects it)", n)
	}
}
