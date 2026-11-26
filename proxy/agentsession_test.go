package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// panicCanary is the panic message of panicky. It stands for a header value an agent's panic text
// could carry, and must never reach the log.
const panicCanary = "CANARY-agent-panic-7d1e-never-log"

// panicky is a test agent whose SessionID panics on the route it owns. It counts its calls, so a
// test can show the agent was asked: a passing fail-open test proves nothing if the host never
// calls the agent.
type panicky struct {
	agent.Base
	path  string
	calls *atomic.Int64
}

func (p panicky) Paths() []string { return []string{p.path} }
func (p panicky) SessionID(http.Header) string {
	if p.calls != nil {
		p.calls.Add(1)
	}
	panic(panicCanary)
}

// agentSessionHandler is bobSessionHandler with the agents taken from reg, not the default
// registry.
func agentSessionHandler(t *testing.T, reg *agent.Registry) (*httptest.Server, *dash.Recorder) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`))
	}))
	t.Cleanup(up.Close)
	cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dash.NewRecorder(dash.Options{DBPath: t.TempDir() + "/d.db", BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		OpenAIUpstream: up.URL,
		Preset:         cfg.Preset,
		Dashboard:      rec,
		Prices:         fixedPricer{},
	})
	h.agentFor = reg.ForPath
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv, rec
}

// An agent that panics must not fail the request. The session is then the one the route has
// with no agent: the panic costs the agent's own id, and nothing else.
func TestPanickingAgentSessionIDFailsOpenToTheSessionWithoutTheAgent(t *testing.T) {
	const route, upPath = "/openai/v1/chat/completions", "/v1/chat/completions"
	body := bobBody("fix it", "")
	hdr := map[string]string{"x-context-guru-session": "hdr-1"}
	plainSrv, plainRec := agentSessionHandler(t, agent.NewRegistry())
	want := sessionAt(t, plainSrv, plainRec, route, body, hdr)
	var calls atomic.Int64
	reg := agent.NewRegistry()
	if err := reg.Register(panicky{path: upPath, calls: &calls}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv, rec := agentSessionHandler(t, reg)
	logs := captureDefaultLog(t)

	// sessionAt fails the test on any status but 200.
	got := sessionAt(t, srv, rec, route, body, hdr)

	if calls.Load() == 0 {
		t.Fatal("the agent was never asked; the fail-open path was not run")
	}
	if got != want || got != "hdr-1" {
		t.Fatalf("session = %q, want %q (the session without the agent)", got, want)
	}
	out := logs.String()
	// Seen first, or the canary check below could pass on a log it never captured.
	if !strings.Contains(out, "recovered from panic in the agent's session id") {
		t.Fatalf("the recovery was not logged where this test reads:\n%s", out)
	}
	if strings.Contains(out, panicCanary) {
		t.Fatalf("the log holds the agent's panic message:\n%s", out)
	}
}

// captureDefaultLog sends the default logger to a buffer for this test.
func captureDefaultLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// The panicking agent's route without the session header: the fallback is the derived session,
// as on a route no agent owns.
func TestPanickingAgentSessionIDFailsOpenToTheDerivedSession(t *testing.T) {
	const route, upPath = "/openai/v1/chat/completions", "/v1/chat/completions"
	body := bobBody("fix it", "")
	plainSrv, plainRec := agentSessionHandler(t, agent.NewRegistry())
	want := sessionAt(t, plainSrv, plainRec, route, body, nil)
	reg := agent.NewRegistry()
	if err := reg.Register(panicky{path: upPath}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv, rec := agentSessionHandler(t, reg)

	got := sessionAt(t, srv, rec, route, body, nil)

	if got != want || got == "" {
		t.Fatalf("session = %q, want the derived session %q", got, want)
	}
}

// Bob Shell sends its task id in the body (metadata.taskId), with no X-Task-Id header. Bob's route
// must still take that id: the agent adds a header id, it does not decide whether the body ids
// are read. (Before the agent, Bob's route always read the body ids.)
func TestBobShellTaskIDWithoutTheHeaderIsStillTheSession(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	const shellTask = "b2f1c0de-4a51-4d0e-9f30-77a1c9d5e412"
	got := sessionAt(t, srv, rec, bobRoute, bobBody("fix it", `{"taskId":"`+shellTask+`"}`), nil)
	if got != shellTask {
		t.Fatalf("session = %q, want Bob Shell's body metadata.taskId %q", got, shellTask)
	}
}

// pathsOnly claims a route and overrides nothing else: no SessionID of its own.
type pathsOnly struct {
	agent.Base
	path string
}

func (p pathsOnly) Paths() []string { return []string{p.path} }

// An agent that does not override SessionID changes nothing on its route: the session is the one
// the route has with no agent, also when the body carries an id. It guards the result, not the
// path to it: the body's own ids are read downstream (sessionIDFrom) whichever way the agent gate
// goes, so the gate on the agent's id and an older gate on the agent's identity give the same
// session here.
func TestAnAgentWithoutItsOwnSessionIDChangesNothingOnItsRoute(t *testing.T) {
	const route = "/openai/v1/chat/completions"
	body := bobBody("fix it", `{"taskId":"t-123"}`)
	hdr := map[string]string{"x-context-guru-session": "hdr-1"}

	noAgent, recA := agentSessionHandler(t, agent.NewRegistry())
	want := sessionAt(t, noAgent, recA, route, body, hdr)

	reg := agent.NewRegistry()
	if err := reg.Register(pathsOnly{path: "/v1/chat/completions"}); err != nil {
		t.Fatal(err)
	}
	withAgent, recB := agentSessionHandler(t, reg)
	if got := sessionAt(t, withAgent, recB, route, body, hdr); got != want {
		t.Fatalf("session with a do-nothing agent = %q, want the no-agent session %q", got, want)
	}
}
