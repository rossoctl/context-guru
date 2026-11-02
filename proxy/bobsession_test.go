package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// bobIDETask is the shape of the Bob IDE's X-Task-Id header on live traffic: 32 lowercase hex.
const bobIDETask = "009431a32bc317f3dae92fe48c7cdc96"

// bobSessionHandler is dashHandler with the Bob gateway on, so the dashboard row records the
// session the pipeline resolved for each route.
func bobSessionHandler(t *testing.T) (*httptest.Server, *dash.Recorder) {
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
	opts := dash.Options{DBPath: t.TempDir() + "/d.db", BatchSize: 1, FlushInterval: time.Millisecond}
	rec, err := dash.NewRecorder(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
		AnthropicUpstream: up.URL,
		OpenAIUpstream:    up.URL,
		BobUpstream:       up.URL,
		Preset:            cfg.Preset,
		Dashboard:         rec,
		Prices:            fixedPricer{},
	})
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv, rec
}

// sessionAt sends one request and returns the session id its dashboard row carries — the id
// the pipeline itself resolved (Trace.Session), not a private helper's answer.
func sessionAt(t *testing.T, srv *httptest.Server, rec *dash.Recorder, path, body string, hdr map[string]string) string {
	t.Helper()
	before := rec.Stats().Written
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned %d", path, resp.StatusCode)
	}
	waitForRows(t, rec, before+1)
	page, err := rec.DB().Requests(dash.Filter{}, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("want 1 newest row, got %d", len(page.Requests))
	}
	return page.Requests[0].SessionID
}

const bobRoute = "/inference/v1/chat/completions"

// bobBody is an OpenAI chat body as the Bob IDE sends it: no `metadata`. metaRaw, when set,
// adds one, as Bob Shell sends it.
func bobBody(firstUser, metaRaw string) string {
	b := `{"model":"gpt-x","messages":[{"role":"system","content":"You are Bob."},{"role":"user","content":"` + firstUser + `"}]`
	if metaRaw != "" {
		b += `,"metadata":` + metaRaw
	}
	return b + `}`
}

func TestBobModelCallTakesSessionFromXTaskID(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	got := sessionAt(t, srv, rec, bobRoute, bobBody("fix it", ""), map[string]string{"X-Task-Id": bobIDETask})
	if got != bobIDETask {
		t.Fatalf("session = %q, want the X-Task-Id %q", got, bobIDETask)
	}
}

func TestBobModelCallSplitsAndJoinsSessionsByXTaskID(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	other := "1f2e3d4c5b6a79881f2e3d4c5b6a7988"
	// The same head on all three calls, so only the header can tell them apart.
	a1 := sessionAt(t, srv, rec, bobRoute, bobBody("same", ""), map[string]string{"X-Task-Id": bobIDETask})
	b := sessionAt(t, srv, rec, bobRoute, bobBody("same", ""), map[string]string{"X-Task-Id": other})
	a2 := sessionAt(t, srv, rec, bobRoute, bobBody("same", ""), map[string]string{"X-Task-Id": bobIDETask})
	if a1 == b {
		t.Errorf("two task ids merged into one session %q", a1)
	}
	if a1 != a2 || a1 != bobIDETask || b != other {
		t.Errorf("sessions = %q, %q, %q; want %q, %q, %q", a1, b, a2, bobIDETask, other, bobIDETask)
	}
}

func TestBobBodyTaskIDOutranksXTaskID(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	const shellTask = "b2f1c0de-4a51-4d0e-9f30-77a1c9d5e412"
	got := sessionAt(t, srv, rec, bobRoute, bobBody("fix it", `{"taskId":"`+shellTask+`"}`),
		map[string]string{"X-Task-Id": bobIDETask})
	if got != shellTask {
		t.Fatalf("session = %q, want body metadata.taskId %q", got, shellTask)
	}
}

func TestBobBodyUserIDOutranksXTaskID(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	const ccSession = "2e168312-56a5-423e-92d1-816863d16a7d"
	userID := `{\"device_id\":\"d\",\"account_uuid\":\"\",\"session_id\":\"` + ccSession + `\"}`
	got := sessionAt(t, srv, rec, bobRoute, bobBody("fix it", `{"user_id":"`+userID+`"}`),
		map[string]string{"X-Task-Id": bobIDETask})
	if got != ccSession {
		t.Fatalf("session = %q, want body metadata.user_id's session %q", got, ccSession)
	}
}

func TestSessionHeaderOutranksXTaskID(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	got := sessionAt(t, srv, rec, bobRoute, bobBody("fix it", ""),
		map[string]string{"X-Task-Id": bobIDETask, "x-context-guru-session": "explicit-hdr"})
	if got != "explicit-hdr" {
		t.Fatalf("session = %q, want the x-context-guru-session header", got)
	}
}

func TestBobUnsafeXTaskIDFallsBackToDerivedSession(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	derived := sessionAt(t, srv, rec, bobRoute, bobBody("fix it", ""), nil)
	for name, v := range map[string]string{
		"traversal": "../../x",
		"too long":  strings.Repeat("a", 200),
		"empty":     "",
		"spaces":    "   ",
	} {
		if got := sessionAt(t, srv, rec, bobRoute, bobBody("fix it", ""), map[string]string{"X-Task-Id": v}); got != derived {
			t.Errorf("%s: session = %q, want the derived fallback %q", name, got, derived)
		}
	}
}

// X-Task-Id has no vendor prefix, so any client may send it. Only the Bob model route reads
// it: Claude Code and Codex sessions must not move.
func TestXTaskIDIgnoredOnNonBobRoutes(t *testing.T) {
	srv, rec := bobSessionHandler(t)
	for _, tc := range []struct{ path, body string }{
		{"/anthropic/v1/messages", `{"model":"claude-x","max_tokens":8,"system":"sys","messages":[{"role":"user","content":"fix it"}]}`},
		{"/openai/v1/chat/completions", bobBody("fix it", "")},
	} {
		without := sessionAt(t, srv, rec, tc.path, tc.body, nil)
		with := sessionAt(t, srv, rec, tc.path, tc.body, map[string]string{"X-Task-Id": bobIDETask})
		if with != without || with == bobIDETask {
			t.Errorf("%s: X-Task-Id moved the session from %q to %q", tc.path, without, with)
		}
	}
}

// Observe mode runs no pipeline on the request path, so serve derives the session itself to
// record billed input. It must read X-Task-Id there too, or the shadow run reads its state under
// one id and the record is written under another.
func TestBobObserveModeRecordsBilledInputUnderXTaskID(t *testing.T) {
	srv, h := bobObserveHandler(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+bobRoute, strings.NewReader(bobBody("fix it", "")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Task-Id", bobIDETask)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy returned %d", resp.StatusCode)
	}
	// The record is written in a defer on the response path, so poll for it.
	for i := 0; i < 200; i++ {
		if _, ok := h.shadow.Get(store.BilledPrefix + bobIDETask); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no cg:bin: record in the shadow store under the X-Task-Id %q", bobIDETask)
}

func bobObserveHandler(t *testing.T) (*httptest.Server, *Handler) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`))
	}))
	t.Cleanup(up.Close)
	cfg, err := config.LoadBytes([]byte("pipeline: [dedup]\n"))
	if err != nil {
		t.Fatal(err)
	}
	agg := metrics.NewAggregator()
	pipe, err := cfg.Build(agg)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pipe, store.NewMemory(store.Options{}), agg, Options{BobUpstream: up.URL, Mode: components.ModeObserve})
	t.Cleanup(h.Close)
	if h.shadow == nil {
		t.Fatal("observe mode built no shadow store")
	}
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return srv, h
}
