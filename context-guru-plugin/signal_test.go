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
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// runSignal pipes a hook payload into scripts/signal.sh the way Claude Code does.
func runSignal(t *testing.T, env []string, payload string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "signal.sh"))
	// A HOME of its own, so the hook never reads the developer's real settings files (#948).
	cmd.Env = append([]string{"PATH=/usr/bin:/bin:/usr/local/bin", "HOME=" + t.TempDir()}, env...)
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
	if len(ss) != 3 || !strings.Contains(ss[0].Hooks[0].Command, "start-proxy.sh") || ss[1].Matcher != "compact|resume|clear" ||
		!strings.Contains(ss[1].Hooks[0].Command, "signal.sh") || ss[2].Matcher != "startup|fork" ||
		!strings.Contains(ss[2].Hooks[0].Command, "signal.sh") {
		t.Errorf("SessionStart hooks = %+v", ss)
	}
}

// A plain session start sends nothing, unless the user set ENABLE_TOOL_SEARCH=false: then the hook
// sends the session id and the raw value to /tool-search, so the proxy turns tool deferral off for the
// session (#485). Nothing else of the payload goes.
func TestSignalHookSendsToolSearchFalseOnStartup(t *testing.T) {
	ch := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		got["path"] = r.URL.Path
		ch <- got
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	payload := `{"session_id":"abc-123","hook_event_name":"SessionStart","source":"startup"}`
	base := []string{"CONTEXT_GURU_CLIENT_SIGNALS=on", "ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_API_KEY=k"}
	for _, v := range []string{"", "ENABLE_TOOL_SEARCH=auto", "ENABLE_TOOL_SEARCH=true"} {
		env := base
		if v != "" {
			env = append(append([]string{}, base...), v)
		}
		runSignal(t, env, payload)
	}
	runSignal(t, append(append([]string{}, base...), "ENABLE_TOOL_SEARCH=FALSE"), payload)
	select {
	case got := <-ch:
		if len(got) != 3 || got["session"] != "abc-123" || got["enable_tool_search"] != "FALSE" || got["path"] != "/tool-search" {
			t.Errorf("body = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ENABLE_TOOL_SEARCH=false did not reach the server")
	}
	select {
	case got := <-ch:
		t.Errorf("a second event was sent: %v", got)
	case <-time.After(1500 * time.Millisecond):
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

// Silent when off, on a bad payload and (by default) toward a base URL that is not a context-guru
// proxy; never fails when the proxy is down.
func TestSignalHookIsInertOffAndFailsOpen(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	base := []string{"ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_AUTH_TOKEN=t"}
	end := `{"session_id":"s","hook_event_name":"SessionEnd","reason":"other"}`
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=off"}, base...), end)                                                            // flag off
	runSignal(t, append([]string{"CLAUDE_PLUGIN_OPTION_CLIENT_SIGNALS=off"}, base...), end)                                                    // option off
	runSignal(t, []string{"ANTHROPIC_BASE_URL=" + strings.Replace(srv.URL, "127.0.0.1", "gateway.invalid", 1), "ANTHROPIC_AUTH_TOKEN=t"}, end) // auto: not ours
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `not json`)                                                      // garbage
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `{"session_id":"../x","hook_event_name":"SessionEnd"}`)          // unusable id
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `{"session_id":"s","hook_event_name":"PreToolUse"}`)             // not an event we send
	time.Sleep(100 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests sent when none should be", n)
	}
	srv.Close()
	runSignal(t, append([]string{"CONTEXT_GURU_CLIENT_SIGNALS=on"}, base...), `{"session_id":"s","hook_event_name":"SessionEnd","reason":"other"}`) // proxy down: still exit 0
}

// On by default toward the plugin's own (loopback) proxy, with no credential needed; toward a hosted
// service, with its x-context-guru-token header rather than the Anthropic key.
func TestSignalHookDefaultsOnForLocalAndHosted(t *testing.T) {
	ch := make(chan http.Header, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	end := `{"session_id":"abc-123","hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`
	runSignal(t, []string{"ANTHROPIC_BASE_URL=" + srv.URL + "/anthropic"}, end) // subscription login: no token in the env
	runSignal(t, []string{"ANTHROPIC_BASE_URL=" + srv.URL + "/anthropic", "ANTHROPIC_API_KEY=sk-ant-x",
		"ANTHROPIC_CUSTOM_HEADERS=x-context-guru-token: cg_live_abc"}, end)
	for i, want := range []string{"", "cg_live_abc"} {
		select {
		case h := <-ch:
			if got := h.Get("x-context-guru-token"); got != want || h.Get("x-api-key") != "" {
				t.Errorf("send %d: token header %q (want %q), x-api-key %q", i, got, want, h.Get("x-api-key"))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("send %d: no event reached the proxy", i)
		}
	}
}

// A non-loopback base URL is a context-guru proxy only when it carries the hosted token header.
func TestSignalHookAutoSendsToARemoteURLOnlyWithTheHostedToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0") // not matched as loopback by the hook's URL test
	if err != nil {
		t.Skipf("no second loopback address: %v", err)
	}
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-context-guru-token") == "cg_live_abc" {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	base := "ANTHROPIC_BASE_URL=http://" + ln.Addr().String() + "/anthropic"
	end := `{"session_id":"abc-123","hook_event_name":"SessionEnd","reason":"other"}`
	runSignal(t, []string{base, "ANTHROPIC_API_KEY=sk-ant-x"}, end)
	runSignal(t, []string{base, "ANTHROPIC_API_KEY=sk-ant-x", "ANTHROPIC_CUSTOM_HEADERS=x-context-guru-token: cg_live_abc"}, end)
	if n := hits.Load(); n != 1 {
		t.Errorf("%d hosted sends, want 1 (only the one with the token header)", n)
	}
}

// Every SessionStart source that can start a new session id carries the opt-out to /tool-search, so
// a resume, fork or clear does not lose it. resume and clear still send their keep-alive event to
// /signal too; startup and fork send only the opt-out. Without the opt-out, startup and fork send
// nothing.
func TestSignalHookSendsToolSearchOffOnEverySessionStart(t *testing.T) {
	for _, tc := range []struct {
		source   string
		wantPath []string // sorted
	}{
		{"startup", []string{"/tool-search"}},
		{"fork", []string{"/tool-search"}},
		// resume and clear also send the compaction settings report (#948), empty when nothing is set,
		// so a setting the user removed clears the proxy's record.
		{"resume", []string{"/compaction-config", "/signal", "/tool-search"}},
		{"clear", []string{"/compaction-config", "/signal", "/tool-search"}},
		{"compact", []string{"/signal"}}, // compaction keeps the session id
	} {
		t.Run(tc.source, func(t *testing.T) {
			ch := make(chan map[string]any, 8)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got map[string]any
				json.NewDecoder(r.Body).Decode(&got)
				got["path"] = r.URL.Path
				ch <- got
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			payload := `{"session_id":"sid-` + tc.source + `","hook_event_name":"SessionStart","source":"` + tc.source + `"}`
			env := []string{"CONTEXT_GURU_CLIENT_SIGNALS=on", "ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_API_KEY=k", "ENABLE_TOOL_SEARCH=off"}
			runSignal(t, env, payload)
			var paths []string
			for range tc.wantPath {
				select {
				case got := <-ch:
					p := got["path"].(string)
					paths = append(paths, p)
					if p == "/tool-search" && (got["session"] != "sid-"+tc.source || got["enable_tool_search"] != "off" || len(got) != 3) {
						t.Errorf("/tool-search body = %v", got)
					}
					if p == "/signal" && got["kind"] != tc.source {
						t.Errorf("/signal body = %v", got)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("got %v, want %v", paths, tc.wantPath)
				}
			}
			select {
			case got := <-ch:
				t.Errorf("an extra request: %v", got)
			case <-time.After(1200 * time.Millisecond):
			}
			sort.Strings(paths)
			if strings.Join(paths, ",") != strings.Join(tc.wantPath, ",") {
				t.Errorf("paths = %v, want %v", paths, tc.wantPath)
			}
			if tc.source == "startup" || tc.source == "fork" {
				runSignal(t, env[:3], payload) // no opt-out: nothing at all
				select {
				case got := <-ch:
					t.Errorf("%s without the opt-out sent %v", tc.source, got)
				case <-time.After(1200 * time.Millisecond):
				}
			}
		})
	}
}

// The session-start hook reports the settings that move Claude Code's own auto-compact point
// (#948) to /compaction-config: only the closed keys, from the env and from the user's settings
// files, and nothing else of those files or of the hook payload.
func TestSignalHookSendsCompactionConfig(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{
		"autoCompactWindow": 300000, "autoCompactEnabled": true, "apiKeyHelper": "secret-helper",
		"modelSettings": {"claude-opus-5": {"autoCompactWindow": "250k", "effort": "high"}}}`), 0o644)
	ch := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		got["path"] = r.URL.Path
		ch <- got
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	env := []string{"HOME=" + home, "CONTEXT_GURU_CLIENT_SIGNALS=on", "ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_API_KEY=k",
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=60", "DISABLE_AUTO_COMPACT=1"}
	runSignal(t, env, `{"session_id":"abc-123","hook_event_name":"SessionStart","source":"startup","cwd":"/nowhere","prompt":"secret prompt"}`)
	select {
	case got := <-ch:
		mw, _ := got["model_windows"].(map[string]any)
		if got["path"] != "/compaction-config" || got["session"] != "abc-123" || got["harness"] != "claude-code" ||
			got["env_pct"] != "60" || got["env_disable_auto"] != "1" || got["settings_window"] != "300000" ||
			got["settings_enabled"] != "true" || mw["claude-opus-5"] != "250k" || len(mw) != 1 {
			t.Errorf("body = %v", got)
		}
		for k := range got {
			switch k {
			case "path", "session", "harness", "env_pct", "env_disable_auto", "settings_window", "settings_enabled", "model_windows":
			default:
				t.Errorf("unexpected field %q", k)
			}
		}
		if b, _ := json.Marshal(got); strings.Contains(string(b), "secret") {
			t.Errorf("the body carries content: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the server")
	}
}

// With no setting changed, a startup sends nothing; a resume sends an empty report, so a setting
// the user removed clears the proxy's record.
func TestSignalHookCompactionConfigSendsNothingWhenNothingIsSet(t *testing.T) {
	ch := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		got["path"] = r.URL.Path
		ch <- got
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	env := []string{"HOME=" + t.TempDir(), "CONTEXT_GURU_CLIENT_SIGNALS=on", "ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_API_KEY=k"}
	runSignal(t, env, `{"session_id":"abc-123","hook_event_name":"SessionStart","source":"startup"}`)
	select {
	case got := <-ch:
		t.Fatalf("startup with nothing set sent %v", got)
	case <-time.After(1500 * time.Millisecond):
	}
	runSignal(t, env, `{"session_id":"abc-123","hook_event_name":"SessionStart","source":"resume"}`)
	var paths []string
	for i := 0; i < 2; i++ {
		select {
		case got := <-ch:
			paths = append(paths, got["path"].(string))
		case <-time.After(5 * time.Second):
			t.Fatalf("resume sent %v", paths)
		}
	}
	sort.Strings(paths)
	if paths[0] != "/compaction-config" || paths[1] != "/signal" {
		t.Errorf("resume sent %v", paths)
	}
}
