package plugin

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runSignal pipes a hook payload into scripts/signal.sh the way Claude Code does.
func runSignal(t *testing.T, env []string, payload string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "signal.sh"))
	cmd.Env = append([]string{"PATH=/usr/bin:/bin:/usr/local/bin"}, env...)
	cmd.Stdin = strings.NewReader(payload)
	if err := cmd.Run(); err != nil {
		t.Fatalf("signal.sh must always exit 0: %v", err)
	}
}

// The event is content-free: of the whole hook payload only the session id, the event name and the
// reason reach the proxy, and the credential rides in a header.
func TestSignalHookSendsOnlyTheClosedFields(t *testing.T) {
	type seen struct {
		body map[string]any
		auth string
	}
	ch := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var got map[string]any
		json.Unmarshal(b, &got)
		ch <- seen{got, r.Header.Get("Authorization")}
		if r.URL.Path != "/signal" {
			t.Errorf("posted to %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	env := []string{"CONTEXT_GURU_CLIENT_SIGNALS=on", "ANTHROPIC_BASE_URL=" + srv.URL + "/anthropic", "ANTHROPIC_AUTH_TOKEN=tok-test"}
	runSignal(t, env, `{"session_id":"abc-123","hook_event_name":"SessionEnd","reason":"prompt_input_exit","cwd":"/home/u/secret","transcript_path":"/home/u/t.jsonl","prompt":"my password is hunter2"}`)
	var sv seen
	select {
	case sv = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the server")
	}
	got, auth := sv.body, sv.auth
	if len(got) != 3 || got["session"] != "abc-123" || got["event"] != "SessionEnd" || got["kind"] != "prompt_input_exit" {
		t.Errorf("body = %v, want exactly session/event/kind", got)
	}
	if auth != "Bearer tok-test" {
		t.Errorf("authorization = %q", auth)
	}
}

// SessionStart sends its source as the kind (compact retires the old prefix on the proxy), and
// hooks.json fires it only for the sources the proxy uses, as its own group next to the proxy-starting
// hook (groups run in parallel, so the send retries a proxy that is not listening yet).
func TestSignalHookSendsSessionStartSource(t *testing.T) {
	ch := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		ch <- got
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	env := []string{"CONTEXT_GURU_CLIENT_SIGNALS=on", "ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_API_KEY=k"}
	runSignal(t, env, `{"session_id":"abc-123","hook_event_name":"SessionStart","source":"compact","model":"m","transcript_path":"/t.jsonl"}`)
	select {
	case got := <-ch:
		if len(got) != 3 || got["event"] != "SessionStart" || got["kind"] != "compact" {
			t.Errorf("body = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached the server")
	}
	b, _ := os.ReadFile(filepath.Join("hooks", "hooks.json"))
	var cfg struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	ss := cfg.Hooks["SessionStart"]
	if len(ss) != 2 || !strings.Contains(ss[0].Hooks[0].Command, "start-proxy.sh") || ss[1].Matcher != "compact|resume|clear" ||
		!strings.Contains(ss[1].Hooks[0].Command, "signal.sh") {
		t.Errorf("SessionStart hooks = %+v", ss)
	}
}

// A background event survives a proxy that starts listening a moment after the hook ran.
func TestSignalHookRetriesAProxyThatIsNotUpYet(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens until the hook has already run
	env := []string{"CONTEXT_GURU_CLIENT_SIGNALS=on", "ANTHROPIC_BASE_URL=http://" + addr, "ANTHROPIC_API_KEY=k"}
	runSignal(t, env, `{"session_id":"abc-123","hook_event_name":"SessionStart","source":"compact"}`)
	time.Sleep(300 * time.Millisecond)
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("port taken meanwhile: %v", err)
	}
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(http.StatusNoContent) })}
	go srv.Serve(ln)
	defer srv.Close()
	for deadline := time.Now().Add(4 * time.Second); hits.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Errorf("%d events reached the late proxy, want 1", hits.Load())
	}
}

// Off by default, silent on a bad payload, and never fails when the proxy is down.
func TestSignalHookIsInertOffAndFailsOpen(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	base := []string{"ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_AUTH_TOKEN=t"}
	runSignal(t, base, `{"session_id":"s","hook_event_name":"SessionEnd","reason":"other"}`)                                          // flag off
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `not json`)                                             // garbage
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `{"session_id":"../x","hook_event_name":"SessionEnd"}`) // unusable id
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `{"session_id":"s","hook_event_name":"PreToolUse"}`)    // not an event we send
	time.Sleep(100 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests sent when none should be", n)
	}
	srv.Close()
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `{"session_id":"s","hook_event_name":"SessionEnd","reason":"other"}`) // proxy down: still exit 0
}
