package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/tenant"
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
	sendBody(t, server, session, anthropicRequest("aws/claude-sonnet-5"))
}

// sendBody posts one Anthropic-dialect request with the given body; an empty session sends
// the request headerless, so its session id is the content-derived one.
func sendBody(t *testing.T, server *httptest.Server, session string, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/anthropic/v1/messages",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
	if session != "" {
		req.Header.Set("x-context-guru-session", session)
	}
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
	// lifetime a plain Anthropic body carries. Status 200 because the seed counts only rows
	// the upstream accepted — a failed request wrote no cache entry.
	rec.Record(&dash.Event{TS: t0 - 60_000, SessionID: "sess-restart", Status: 200,
		Model: "aws/claude-sonnet-5", TokensBefore: 10})
	waitForRows(t, rec, 1)

	h2, rec2 := restartedHandler(t, up.URL, dbPath, t0, t0-2*60*60*1000)
	if at, ok := rec2.ThreadRefreshedAt("sess-restart"); !ok || at != t0-60_000 {
		t.Fatalf("the seeded grace clock = (%d, %v), want the row's time (%d, true)", at, ok, t0-60_000)
	}
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()

	// The brand-new control is a DIFFERENT conversation: the alias widening records a
	// refresh under the content-derived alias key too, so a second session header over
	// the SAME body reads the entry the live session just refreshed — protected, and
	// correctly so. Brand-new means its own content, nothing warm anywhere.
	other := []byte(strings.Replace(string(anthropicRequest("aws/claude-sonnet-5")),
		"please fix the failing test", "please fix the other failing test", 1))
	send(t, srv2, "sess-restart")
	sendBody(t, srv2, "sess-brand-new", other)
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
		rec.Record(&dash.Event{TS: at, SessionID: session, Status: 200,
			Model: "aws/claude-sonnet-5", TokensBefore: 10, KeepAlive: keepAlive, CacheRead: cacheRead})
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

	// The cold control is a DIFFERENT conversation: the alias widening now records a
	// refresh under the content-derived alias key too, so a second session header over
	// the SAME body reads the entry the pinged session just refreshed — protected, and
	// correctly so. Cold means its own entry: another body, nothing warm anywhere.
	other := []byte(strings.Replace(string(anthropicRequest("aws/claude-sonnet-5")),
		"please fix the failing test", "please fix the other failing test", 1))
	send(t, srv2, "sess-ping")
	sendBody(t, srv2, "sess-cold", other)
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

// Issue #766, rounds 2 and 3: in observe mode the dashboard row carries no session id (the
// enforced trace is zeroed), so the recorder cannot know any thread and a lookup that
// answered would read every warm thread as new — the one answer worse than not asking. The
// lookup is wired unconditionally now, but it DECLINES every key while the whole proxy runs
// observe (tenantObserves), so #759's start-time fallback answers: an unknown thread inside
// one cache lifetime of the restart still reads as all-cached.
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
	// The recorder knows a thread, the way a live one would: it is there to make the failure
	// mode loud — if the lookup answered instead of declining (the bug), a thread it does
	// not know would read as new, past a fallback that no longer runs.
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
	// answering instead of declining (the bug), the same probe reads 0: the lookup misses and
	// calls the thread new.
	if n, _ := h.tracker.TurnAt("sess-observe-probe", 925, time.Now().UnixMilli(), 5*time.Minute); n != 925 {
		t.Fatalf("unknown thread inside the start-time window = %d, want 925 (the fallback protects it)", n)
	}
}

// derivedSession computes the session id a HEADERLESS request with this body resolves to —
// the same normalization, the same head, the same Scoped call the request path uses — so the
// test can name the row and the key the tracker will ask for.
func derivedSession(t *testing.T, body []byte) string {
	t.Helper()
	sys, firstUser := schema.SessionHead(apply.NormalizeMessages(bschemas.Anthropic, body))
	return session.Scoped("", "", sys, firstUser)
}

// Issue #766, round 3: the alias widening must hold in BOTH directions. The conversation's
// last request before the restart carried the session header, so its warmth landed under the
// explicit id A only; the first request after the restart is headerless, and its own derived
// id IS the alias id B — one key, asked for alone. The record path must therefore touch the
// grace clock under B too (the capture path derives the alias off the body), and the row must
// carry the alias so the restarted process's seed can recover B's warmth from the database.
// Within the lifetime: protected. A different conversation, also headerless: editable.
func TestTheRestartGraceProtectsAHeaderlessTurnAfterHeaderedOnes(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "d.db")
	rec, err := dash.NewRecorder(dash.Options{DBPath: dbPath, BatchSize: 1,
		FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	body := anthropicRequest("aws/claude-sonnet-5")
	derived := derivedSession(t, body)
	// A different conversation for the editable control: a different head hashes to a
	// different derived id.
	other := []byte(strings.Replace(string(anthropicRequest("aws/claude-sonnet-5")),
		"please fix the failing test", "please fix the other failing test", 1))
	otherDerived := derivedSession(t, other)
	if otherDerived == derived {
		t.Fatal("the control body derived the same session id as the seesaw conversation")
	}

	cfg, agg, pipe := codesafePipeline(t)
	h1 := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL,
		OpenAIUpstream:    up.URL,
		Preset:            cfg.Preset,
		Dashboard:         rec,
		Prices:            fixedPricer{},
	})
	srv1 := httptest.NewServer(h1.Mux())
	defer srv1.Close()
	// The conversation's last pre-restart request: header present, one minute before the
	// restart, well inside the 5-minute lifetime.
	sendBody(t, srv1, "sess-header", body)
	waitForRows(t, rec, 1)

	t0 := time.Now().UnixMilli()
	h2, rec2 := restartedHandler(t, up.URL, dbPath, t0, t0-2*60*60*1000)
	srv2 := httptest.NewServer(h2.Mux())
	defer srv2.Close()
	// The alias key the header request's row must have seeded: the derived id, this thread.
	if at, ok := rec2.ThreadRefreshedAt(derived); !ok || at < t0-60_000 {
		t.Fatalf("the alias id's grace clock = (%d, %v), want the header request's time", at, ok)
	}
	// The first request after the restart: headerless, so its own id IS the alias id.
	sendBody(t, srv2, "", body)
	sendBody(t, srv2, "", other)
	waitForRows(t, rec2, 2)

	seesaw := row(t, rec2, derived)
	if seesaw.AttemptedTokens != 0 || seesaw.FrozenTokens == 0 {
		t.Fatalf("headerless turn after headered ones: %d of %d tokens attempted; want the whole request frozen (the alias id's clock protects it)",
			seesaw.AttemptedTokens, seesaw.TokensBefore)
	}
	fresh := row(t, rec2, otherDerived)
	if fresh.AttemptedTokens == 0 {
		t.Fatalf("unknown headerless conversation: 0 of %d tokens attempted; want the request editable (no history, no grace)",
			fresh.TokensBefore)
	}
}

// Issue #766, round 3: mode is PER TENANT in hosted mode, so the grace lookup must decline
// an observe tenant's keys even on a proxy whose own mode is sync. One tenant's configuration
// document sets mode: observe. After a restart, that tenant's live thread — unknown to the
// recorder, because observe rows carry no session id — keeps #759's start-time fallback and
// reads as protected inside the lifetime, while a sync tenant's unknown thread still reads as
// new: the lookup answers for it, and a miss is a really new thread.
//
// The dashboard row cannot show the observe tenant's boundary (its figures are zeroed with
// the trace), so the probe asks the tracker directly — the same tracker the off-path
// observation consults.
func TestARestartProtectsAnObserveTenantsLiveThreadsOnAHostedProxy(t *testing.T) {
	var rec *dash.Recorder
	f := newHostedFixtureOpts(t, "up", "anthropic", "TEST_UPSTREAM_KEY", func(o *Options) {
		r, err := dash.NewRecorder(dash.Options{DBPath: filepath.Join(t.TempDir(), "d.db"),
			BatchSize: 1, FlushInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		rec = r
		o.Dashboard = rec
	})
	t.Cleanup(func() { rec.Close() })
	// A builder that reads the tenant's mode out of its configuration document, the way the
	// real one does (main.go's buildTenantConfig).
	f.h.opts.Tenants = NewTenantSource(f.reg, nil, func(doc []byte, e components.Emitter) (BuiltConfig, error) {
		bc := BuiltConfig{Pipe: components.NewPipeline(nil, e),
			Store: store.NewMemory(store.Options{}), Preset: "test"}
		if strings.Contains(string(doc), "mode: observe") {
			bc.Mode = components.ModeObserve
		}
		return bc, nil
	}, 0)

	obsTn, _ := f.register(t, "observe@ibm.com")
	syncTn, _ := f.register(t, "sync@ibm.com")
	doc := "mode: observe\n"
	if err := f.reg.Update(obsTn, obsTn.ID, tenant.Patch{ConfigYAML: &doc}); err != nil {
		t.Fatal(err)
	}
	// Re-read the tenant: the struct Register returned predates the document, and
	// Registry.Config reads the field off the struct it is handed.
	if refreshed, err := f.reg.Get(obsTn.ID); err != nil {
		t.Fatal(err)
	} else {
		obsTn = refreshed
	}
	// Load both tenancies the way a request would (Resolve → tenancy → build), so the mode
	// each tenant runs is the one the wired lookup will see.
	for _, tn := range []*tenant.Tenant{obsTn, syncTn} {
		if _, err := f.h.opts.Tenants.ForTenant(tn); err != nil {
			t.Fatal(err)
		}
	}

	// This handler is the restarted process, one minute after its start. The observe
	// tenant's thread is unknown to the recorder, so only the start-time fallback can
	// protect it — which is exactly what declining the key must achieve.
	f.h.tracker.SetStartedAt(time.Now().UnixMilli() - 60_000)
	if n, _ := f.h.tracker.TurnAt(obsTn.ID+":sess-live", 925, time.Now().UnixMilli(), 5*time.Minute); n != 925 {
		t.Fatalf("observe tenant's live thread = %d, want 925 (the start-time fallback protects it)", n)
	}
	// The sync tenant is served by the lookup: an unknown thread there is really new.
	if n, _ := f.h.tracker.TurnAt(syncTn.ID+":sess-new", 925, time.Now().UnixMilli(), 5*time.Minute); n != 0 {
		t.Fatalf("sync tenant's unknown thread = %d, want 0 (the lookup answers: really new)", n)
	}
}
