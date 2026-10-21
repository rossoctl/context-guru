// Golden tests for scripts/cg.py, the /cg:* commands. A stub proxy answers from testdata/cg (and the
// cost bundle) with fixed numbers and a fixed clock, so every screen is byte-for-byte reproducible.
// UPDATE_GOLDEN=1 rewrites the goldens after an intended change; read the diff.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const fixedNow = 1791473400000 // 2026-10-08T15:30:00Z
const cgDay = 86400000

func cgRead(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cgProxyStub serves the endpoints cg.py reads.
func cgProxyStub(t *testing.T, cache string) string {
	t.Helper()
	bundle := map[string]json.RawMessage{}
	if err := json.Unmarshal(cgRead(t, "cost", "bundle.json"), &bundle); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	file := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.Write(cgRead(t, "cg", name+".json")) }
	}
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since := q.Get("since")
		midnight := cgNow() - cgNow()%cgDay
		switch {
		case q.Get("session") != "":
			w.Write(cgRead(t, "cg", "stats_session.json"))
		case since == fmt.Sprint(midnight):
			w.Write(cgRead(t, "cg", "stats_today.json"))
		case since == fmt.Sprint(midnight-cgDay):
			w.Write(cgRead(t, "cg", "stats_yesterday.json"))
		default:
			w.Write(cgRead(t, "cg", "stats_week.json"))
		}
	})
	mux.HandleFunc("/api/series", file("series"))
	mux.HandleFunc("/api/requests", file("requests"))
	mux.HandleFunc("/api/config", file("config"))
	mux.HandleFunc("/api/cachestate", file(cache))
	mux.HandleFunc("GET /api/adapt/cache", file("adapt_cache"))
	mux.HandleFunc("GET /api/adapt/tools", file("adapt_tools"))
	mux.HandleFunc("GET /api/adapt/compaction", file("adapt_compaction"))
	mux.HandleFunc("POST /api/adapt/compaction/apply", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Option, Basis string }
		json.NewDecoder(r.Body).Decode(&b) //nolint:errcheck
		if b.Option == "medium" && b.Basis == "a1b2c3d4" {
			w.Write([]byte(`{"ok":true,"kind":"compaction","applied":"medium","previous":"current","effective":"now","note":"preset set to medium","undo":{"option":"revert"}}`))
			return
		}
		if b.Option != "revert" || b.Basis != "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"want a bare revert"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"kind":"compaction","applied":"revert","previous":"c1","effective":"now","note":"Restored the pipeline from before your last change."}`))
	})
	mux.HandleFunc("POST /api/adapt/cache/apply", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Option, Basis string
			Force         bool
		}
		json.NewDecoder(r.Body).Decode(&b) //nolint:errcheck
		switch {
		case b.Option == "by-segment" && !b.Force:
			w.Write([]byte(`{"ok":false,"applied":"shadow","force_available":true,"note":"The go-live check did not pass, so it runs in shadow and changes nothing."}`))
		case b.Option == "by-segment":
			w.Write([]byte(`{"ok":true,"applied":"by-segment","effective":"now","note":"The plan is live although the go-live check did not pass","undo":{"option":"default","basis":"77b0c2e1"}}`))
		case b.Basis != "" && b.Basis != "3f9a1c":
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"stale"}`))
		case b.Option != "adaptive" && b.Option != "5-min-ping":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"unknown option \"` + b.Option + `\""}`))
		default:
			w.Write(cgRead(t, "cg", "adapt_apply.json"))
		}
	})
	// A long apply: 202 with a poll path, then the finished result.
	mux.HandleFunc("POST /api/adapt/tools/apply", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Option string }
		json.NewDecoder(r.Body).Decode(&b) //nolint:errcheck
		if b.Option == "revert" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"nothing to revert: the last change to this setting was not made by adapt"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"state":"running","poll":"/api/adapt/tools/apply/job1"}`))
	})
	mux.HandleFunc("GET /api/adapt/tools/apply/job1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"state":"done","ok":true,"applied":"remove-sufficient","previous":"keep","effective":"now","note":"6 unused items filtered (playwright, figma, brand-voice and 3 more)","undo":{"option":"restore","basis":"5d0e17aa"}}`))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	for path, key := range map[string]string{"/api/spend/explain": "spend", "/api/tools": "tools", "/api/tools/last-used": "last_used"} {
		body := bundle[key]
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("http://%s", ln.Addr())
}

func cgNow() int64 { return fixedNow }

func runCG(t *testing.T, base string, env []string, args ...string) string {
	t.Helper()
	out, code := runCGCode(t, base, env, args...)
	if code != 0 {
		t.Fatalf("cg.py %v: exit %d\n%s", args, code, out)
	}
	return out
}

// runCGCode is runCG for a command whose exit code is part of what is tested.
func runCGCode(t *testing.T, base string, env []string, args ...string) (string, int) {
	t.Helper()
	requireTool(t, "python3")
	cmd := exec.Command("python3", append([]string{filepath.Join("scripts", "cg.py")}, append(args,
		"--now", "2026-10-08T15:30:00Z", "--tz-offset-min", "0")...)...)
	cfg := t.TempDir()
	os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"awaySummaryEnabled":false,"statusLine":{"type":"command","command":"python3 x/statusline.py","refreshInterval":5}}`), 0o644) //nolint:errcheck
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "COLUMNS=100", "CG_ASCII=", "CLAUDE_CONFIG_DIR="+cfg, "CLAUDE_PROJECT_DIR="+t.TempDir(), "CG_MANAGED_SETTINGS="+filepath.Join(cfg, "no-managed.json"), "CONTEXT_GURU_STATE="+t.TempDir(), "CLAUDE_CODE_SESSION_ID=session-A",
		"ANTHROPIC_BASE_URL="+base+"/anthropic", "TZ=UTC")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("cg.py %v: %v\n%s", args, err, out)
	}
	return string(out), 0
}

func cgGolden(t *testing.T, name, got string) {
	t.Helper()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("ANSI escapes in %s; /cg:* output is plain Unicode so it reads on any theme", name)
	}
	path := filepath.Join("testdata", "cg", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want := string(cgRead(t, "cg", name+".golden")); got != want {
		t.Errorf("%s differs from its golden.\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestCGGoldens(t *testing.T) {
	warm, cold := cgProxyStub(t, "cachestate_warm"), cgProxyStub(t, "cachestate_cold")
	for _, c := range []struct {
		name, base string
		args       []string
		rc         int
	}{
		{"home", warm, []string{"home"}, 0},
		{"savings", warm, []string{"insights", "savings"}, 0},
		{"savings-details", warm, []string{"insights", "savings", "details"}, 0},
		{"cache-warm", warm, []string{"insights", "cache"}, 0},
		{"cache-cold", cold, []string{"insights", "cache"}, 0},
		{"session", warm, []string{"session"}, 0},
		{"doctor", warm, []string{"doctor"}, 0},
		{"insights", warm, []string{"insights"}, 0},
		{"insights-savings", warm, []string{"insights", "savings"}, 0},
		{"insights-cache", warm, []string{"insights", "cache"}, 0},
		{"insights-tools", warm, []string{"insights", "tools"}, 0},
		{"insights-tools-details", warm, []string{"insights", "tools", "details"}, 0},
		{"insights-unknown", warm, []string{"insights", "nonsense"}, 0},
		{"apply-under-insights", warm, []string{"insights", "tools", "apply", "remove-sufficient@8c21e0d4"}, 1},
		{"save-more", warm, []string{"save-more"}, 0},
		{"save-more-config", warm, []string{"save-more", "config"}, 0},
		{"save-more-config-details", warm, []string{"save-more", "config", "details"}, 0},
		{"save-more-cache", warm, []string{"save-more", "cache"}, 0},
		{"save-more-cache-details", warm, []string{"save-more", "cache", "details"}, 0},
		{"remove-mcp-and-skills", warm, []string{"remove-mcp-and-skills"}, 0},
		{"remove-mcp-and-skills-details", warm, []string{"remove-mcp-and-skills", "details"}, 0},
		{"remove-mcp-and-skills-edge", warm, []string{"remove-mcp-and-skills", "--fixture", filepath.Join("testdata", "cg", "fx_tools_edge.json")}, 0},
		{"apply-under-remove", warm, []string{"remove-mcp-and-skills", "apply", "tools", "remove-sufficient@8c21e0d4"}, 1},
		{"apply-config", warm, []string{"apply", "config", "medium@a1b2c3d4"}, 0},
		{"apply-cache", warm, []string{"apply", "cache", "adaptive@3f9a1c"}, 0},
		{"apply-stale", warm, []string{"apply", "cache", "adaptive@000000"}, 1},
		{"apply-tools-async", warm, []string{"apply", "tools", "remove-sufficient@8c21e0d4"}, 0},
		{"apply-under-read", warm, []string{"save-more", "cache", "apply", "adaptive@3f9a1c"}, 1},
		{"revert-under-read", warm, []string{"save-more", "revert", "config"}, 1},
		{"revert-kind-first", warm, []string{"config", "revert"}, 1},
		{"revert-under-remove", warm, []string{"remove-mcp-and-skills", "revert"}, 1},
		{"revert-config", warm, []string{"revert", "config"}, 0},
		{"revert-old-name", warm, []string{"revert", "compaction"}, 0},
		{"revert-nothing", warm, []string{"revert", "tools"}, 1},
		{"apply-gate", warm, []string{"apply", "cache", "by-segment@3f9a1c"}, 1},
		{"apply-force", warm, []string{"apply", "cache", "by-segment@3f9a1c", "--force"}, 0},
		{"apply-any-order", warm, []string{"apply", "adaptive@3f9a1c", "cache"}, 0},
		{"apply-unknown", warm, []string{"apply", "cache", "nonsense"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code := runCGCode(t, c.base, nil, c.args...)
			if code != c.rc {
				t.Errorf("exit %d, want %d", code, c.rc)
			}
			cgGolden(t, c.name, out)
		})
	}
}

// The first line of every screen is the answer: one line, no raw token counts.
func TestCGBottomLineFirst(t *testing.T) {
	base := cgProxyStub(t, "cachestate_warm")
	for _, c := range []string{"home", "insights savings", "insights cache", "insights tools", "session"} {
		out := runCG(t, base, nil, strings.Fields(c)...)
		first := strings.SplitN(out, "\n", 2)[0]
		if len(first) < 20 || strings.Contains(first, "tokens") {
			t.Errorf("%s: first line %q is not a bottom line in dollars", c, first)
		}
	}
}

func TestCGNoProxy(t *testing.T) {
	requireTool(t, "python3")
	cmd := exec.Command("python3", filepath.Join("scripts", "cg.py"), "insights", "savings")
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=https://api.anthropic.com", "CG_URL=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "No Context Guru proxy") {
		t.Errorf("want the no-proxy message, got %v: %s", err, out)
	}
}

// The save-more and remove-mcp-and-skills skills pre-approve only their read. A change is a separate
// top-level command, so Claude Code asks the user for permission to run it: a second gate behind the skill's
// own yes/no question. No read command writes, whatever words follow it.
func TestAdaptSkillPreApprovesOnlyTheRead(t *testing.T) {
	for _, c := range []string{"save-more", "remove-mcp-and-skills"} {
		body := readFileString(t, filepath.Join("skills", c, "SKILL.md"))
		if !regexp.MustCompile(`(?m)^allowed-tools: Bash\(python3 "\$\{CLAUDE_PLUGIN_ROOT\}/scripts/cg\.py" ` + c + `:\*\)(, Bash\(python3 "\$\{CLAUDE_PLUGIN_ROOT\}/scripts/settings\.py" record-run ` + c + `\))?$`).MatchString(body) {
			t.Errorf("%s must pre-approve exactly its own read, `cg.py %s:*`", c, c)
		}
	}
	base := cgProxyStub(t, "cachestate_warm")
	for _, args := range [][]string{{"save-more", "apply", "cache", "adaptive@3f9a1c"},
		{"save-more", "revert", "config"}, {"remove-mcp-and-skills", "apply", "tools", "remove-sufficient@8c21e0d4"}} {
		if out, code := runCGCode(t, base, nil, args...); code == 0 || strings.Contains(out, "Applied") {
			t.Errorf("%v must not write (it is inside the pre-approved prefix): exit %d\n%s", args, code, out)
		}
	}
}

// No skill pre-approves a change. A pre-approved pattern names one READ command of cg.py (the first word picks
// the command, so `cg.py insights:*` can only ever run the insights screens); a bare `cg.py:*` would also cover
// `cg.py apply` and `cg.py revert` while that skill is active, and skip the harness's permission prompt.
// Codex skills pre-approve nothing, and every write they run asks for escalation.
func TestNoSkillPreApprovesAChange(t *testing.T) {
	reads := map[string]bool{"save-more": true, "remove-mcp-and-skills": true, "cache-campaign": true, "insights": true}
	for _, c := range []string{"home", "session", "doctor"} {
		reads[c] = true
	}
	allowed := regexp.MustCompile(`(?m)^allowed-tools:(.*)$`)
	pattern := regexp.MustCompile(`Bash\(([^)]*)\)`)
	cgPat := regexp.MustCompile(`scripts/cg\.py"?([^)]*)\)`)
	call := regexp.MustCompile(`(?m)^python3 .*scripts/(cg|tv)\.py.*$`)
	skills, _ := filepath.Glob(filepath.Join("skills", "*", "SKILL.md"))
	if len(skills) < 10 {
		t.Fatalf("found %d skills", len(skills))
	}
	for _, f := range skills {
		body := readFileString(t, f)
		m := allowed.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		for _, p := range cgPat.FindAllStringSubmatch(m[1], -1) {
			words := strings.Fields(strings.TrimSuffix(p[1], ":*"))
			if len(words) == 0 || !reads[words[0]] {
				t.Errorf("%s pre-approves %q: name one read command, never a cg.py wildcard", f, "cg.py"+p[1])
			}
			for _, w := range words {
				if w == "apply" || w == "revert" {
					t.Errorf("%s pre-approves a change: %q", f, "cg.py"+p[1])
				}
			}
		}
		// A pattern is a literal prefix of the command Claude Code is asked to run: the quotes count. Every read
		// the skill runs must start with one of its own patterns, or the pre-approval silently never applies.
		var prefixes []string
		for _, p := range pattern.FindAllStringSubmatch(m[1], -1) {
			prefixes = append(prefixes, strings.TrimSuffix(p[1], ":*"))
		}
		for _, c := range call.FindAllString(body, -1) {
			matched := false
			for _, p := range prefixes {
				matched = matched || strings.HasPrefix(c, p)
			}
			change := strings.Contains(c, ".py\" $ARGUMENTS") // the skill's apply/revert line: must prompt
			if matched == change {
				t.Errorf("%s runs %q, which %s its own allowed-tools %v", f, c,
					map[bool]string{true: "is a change but matches", false: "does not match"}[change], prefixes)
			}
		}
	}
	codex, _ := filepath.Glob(filepath.Join("..", "codex-marketplace", "plugins", "context-guru", "skills", "*", "SKILL.md"))
	for _, f := range codex {
		body := readFileString(t, f)
		if allowed.MatchString(body) {
			t.Errorf("%s: Codex skills pre-approve nothing", f)
		}
		if regexp.MustCompile(`cg\.py (apply|revert)`).MatchString(body) && !strings.Contains(body, "require_escalated") {
			t.Errorf("%s runs a change without asking for escalation", f)
		}
	}
}

// On the hosted service the adapt routes take the account's own proxy token; the other screens never send it.
func TestAdaptSendsTheTokenOnlyOnAdapt(t *testing.T) {
	requireTool(t, "python3")
	got := map[string]string{}
	mux := http.NewServeMux()
	for _, p := range []string{"/api/adapt/tools", "/api/stats"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			got[r.URL.Path] = r.Header.Get("x-context-guru-token")
			w.Write(cgRead(t, "cg", "adapt_tools.json"))
		})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	for _, args := range [][]string{{"remove-mcp-and-skills"}, {"savings"}} {
		cmd := exec.Command("python3", append([]string{filepath.Join("scripts", "cg.py")}, args...)...)
		cmd.Env = append(os.Environ(), "CG_URL=http://"+ln.Addr().String(), "ANTHROPIC_BASE_URL=", "CG_TOKEN=", "ANTHROPIC_AUTH_TOKEN=cg_live_test")
		cmd.CombinedOutput() //nolint:errcheck
	}
	if got["/api/adapt/tools"] != "cg_live_test" {
		t.Errorf("adapt sent token %q, want the agent's cg_live_ key", got["/api/adapt/tools"])
	}
	if v, ok := got["/api/stats"]; ok && v != "" {
		t.Errorf("a read screen sent the token %q", v)
	}
}

// The save-more screens fit an 80-column terminal in ASCII, and colour only repeats what the glyphs say:
// with the escapes stripped, a coloured screen is the plain one. The lines meant to be run are never wrapped.
func TestSaveMoreWidthASCIIAndColour(t *testing.T) {
	base := cgProxyStub(t, "cachestate_warm")
	for _, c := range []string{"save-more", "remove-mcp-and-skills"} {
		out, _ := runCGCode(t, base, []string{"CG_ASCII=1", "COLUMNS=80"}, c)
		if c == "save-more" {
			cgGolden(t, "save-more-ascii-80", out)
		}
		for _, ln := range strings.Split(out, "\n") {
			run := strings.Contains(ln, "/context-guru:") || strings.HasPrefix(ln, "Question:") // copied as it is: never wrapped
			if !run && len([]rune(ln)) > 79 {
				t.Errorf("%s: line wider than 80 columns: %q", c, ln)
			}
			for _, r := range ln {
				if r > 127 {
					t.Errorf("%s: non-ASCII %q in ASCII mode: %q", c, r, ln)
					break
				}
			}
		}
		plain, _ := runCGCode(t, base, nil, c)
		colour, _ := runCGCode(t, base, []string{"NO_COLOR=", "FORCE_COLOR=1"}, c)
		if !strings.Contains(colour, "\x1b[") {
			t.Errorf("%s: FORCE_COLOR gave no colour", c)
		}
		if got := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(colour, ""); got != plain {
			t.Errorf("%s: colour changes the text, not only its colour", c)
		}
	}
}

// The status line's weekly tip reads when each command last ran. Each skill records a run with settings.py
// record-run under its own name, pre-approved as exactly that command, and only after a read that printed a
// report: never for a failed or refused run, never for an apply or revert.
func TestSaveMoreSkillsRecordTheirRuns(t *testing.T) {
	for _, c := range []string{"save-more", "remove-mcp-and-skills"} {
		body := readFileString(t, filepath.Join("skills", c, "SKILL.md"))
		call := "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py\" record-run " + c
		// One command, so a model cannot skip it or run it after a failure: && runs it only on exit 0.
		if !strings.Contains(body, "python3 \"${CLAUDE_PLUGIN_ROOT}/scripts/cg.py\" "+c+" $ARGUMENTS && "+call+"\n") {
			t.Errorf("%s: the read must be chained `... && %s`", c, call)
		}
		if !strings.Contains(body, "Bash("+call+")") {
			t.Errorf("%s: the run record must be pre-approved as exactly %q", c, call)
		}
		if strings.Count(body, "record-run "+c) != 2 { // the chain and its pre-approval; never on the apply/revert line
			t.Errorf("%s: record-run appears %d times, want the read chain and its pre-approval only", c, strings.Count(body, "record-run "+c))
		}
	}
	// The chain is only as good as the exit code: every failure or refusal of a read exits non-zero.
	for _, args := range [][]string{{"save-more", "revert", "config"}, {"remove-mcp-and-skills", "nonsense"}} {
		if _, code := runCGCode(t, cgProxyStub(t, "cachestate_warm"), nil, args...); code == 0 {
			t.Errorf("%v exited 0, so the run would be recorded", args)
		}
	}
	cmd := exec.Command("python3", filepath.Join("scripts", "cg.py"), "save-more")
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=https://api.anthropic.com", "CG_URL=", "ANTHROPIC_AUTH_TOKEN=", "CG_TOKEN=")
	if err := cmd.Run(); err == nil {
		t.Error("save-more with no proxy exited 0, so the run would be recorded")
	}
}

// CONTRACT section 1/6: /api/stats' manual_pings is a savings category of its own, "Manual pings",
// shown in savings and insights only once there is one. Existing goldens (no manual pings) stay as they are.
func TestCGSavingsShowsManualPings(t *testing.T) {
	stats := func(extra string) map[string]any {
		var st map[string]any
		doc := `{"total_saved_usd":41.2,"cost_usd":856.85,"baseline_cost_usd":898.05,"waterfall":[{"key":"compaction","delta_usd":-33.9}]` + extra + `}`
		if err := json.Unmarshal([]byte(doc), &st); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"/api/stats": st}
	}
	write := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		f := filepath.Join(t.TempDir(), "fx.json")
		if err := os.WriteFile(f, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}
	with := runCG(t, "http://127.0.0.1:1", nil, "insights", "savings", "--fixture", write(stats(`,"manual_pings":{"count":3,"ping_usd":0.06,"saved_usd":0.84}`)))
	if !strings.Contains(with, "Manual pings") || !strings.Contains(with, "$0.84") {
		t.Errorf("want a Manual pings row with $0.84 saved, got\n%s", with)
	}
	if !strings.Contains(with, "3 pings") || !strings.Contains(with, "$0.06") {
		t.Errorf("want the count and the ping cost beside it, got\n%s", with)
	}
	for name, extra := range map[string]string{"absent": "", "zero": `,"manual_pings":{"count":0,"ping_usd":0,"saved_usd":0}`} {
		if out := runCG(t, "http://127.0.0.1:1", nil, "insights", "savings", "--fixture", write(stats(extra))); strings.Contains(out, "Manual pings") {
			t.Errorf("%s: no Manual pings row expected, got\n%s", name, out)
		}
	}
}

// CONTRACT section 7: the tools part of insights lists skills, MCP servers and MCP tools with their
// cost, per-tool rows under each server, and never Claude Code's built-in tools. The cost is the
// whole carry (carry_usd) when the proxy reports it, else what was carried for nothing (unused_usd).
func TestCGInsightsToolsListsServersSkillsAndToolsWithoutBuiltins(t *testing.T) {
	tools := `{"totals":{"priced":true,"unused_usd":9},
	 "tools":[
	  {"kind":"mcp_tool","name":"mcp__github__create_issue","server":"github","tokens":900,"calls":5,"carry_usd":4.0,"unused_usd":0,"priced":true},
	  {"kind":"mcp_tool","name":"mcp__github__list_prs","server":"github","tokens":700,"calls":0,"carry_usd":3.0,"unused_usd":3.0,"priced":true},
	  {"kind":"mcp_tool","name":"mcp__sentry__search","server":"sentry","tokens":400,"calls":0,"carry_usd":2.0,"unused_usd":2.0,"priced":true},
	  {"kind":"tool","name":"Read","tokens":300,"calls":90,"carry_usd":7.0,"unused_usd":0,"builtin":true,"priced":true},
	  {"kind":"tool","name":"Bash","tokens":800,"calls":40,"carry_usd":8.0,"unused_usd":0,"builtin":true,"priced":true}],
	 "servers":[
	  {"server":"github","tools":2,"tools_used":1,"tokens":1600,"calls":5,"carry_usd":7.0,"unused_usd":3.0,"priced":true},
	  {"server":"sentry","tools":1,"tools_used":0,"tokens":400,"calls":0,"carry_usd":2.0,"unused_usd":2.0,"priced":true}],
	 "skills":{"state":"ok","skills":[
	  {"kind":"skill","name":"pdf","tokens":310,"calls":3,"carry_usd":1.5,"unused_usd":0,"priced":true},
	  {"kind":"skill","name":"brand-voice","tokens":420,"calls":0,"carry_usd":1.6,"unused_usd":1.6,"priced":true}]},
	 "aside":[]}`
	fx := map[string]any{"/api/tools": json.RawMessage(tools), "/api/tools/last-used": json.RawMessage(`{"tools":[]}`)}
	b, _ := json.Marshal(fx)
	f := filepath.Join(t.TempDir(), "fx.json")
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	out := runCG(t, "http://127.0.0.1:1", nil, "insights", "tools", "--fixture", f)
	for _, want := range []string{"github", "sentry", "pdf", "brand-voice", "create_issue", "list_prs", "$7.00", "$1.50"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, banned := range []string{"Read", "Bash"} {
		if strings.Contains(out, banned) {
			t.Errorf("built-in tool %q must not be listed:\n%s", banned, out)
		}
	}
	// per-tool rows sit under their own server, in order
	gh, ci, lp, se := strings.Index(out, "github"), strings.Index(out, "create_issue"), strings.Index(out, "list_prs"), strings.Index(out, "sentry")
	if !(gh < ci && ci < se && gh < lp && lp < se) {
		t.Errorf("github's tool rows must sit between github and sentry:\n%s", out)
	}
	// carry_priced:false means carry_usd is short (a carrying request had no known rates): never shown as a cost.
	short := strings.Replace(tools, `"name":"mcp__sentry__search","server":"sentry","tokens":400,"calls":0,"carry_usd":2.0,`, `"name":"mcp__sentry__search","server":"sentry","tokens":400,"calls":0,"carry_usd":0,"carry_priced":false,`, 1)
	short = strings.Replace(short, `{"server":"sentry","tools":1,"tools_used":0,"tokens":400,"calls":0,"carry_usd":2.0,`, `{"server":"sentry","tools":1,"tools_used":0,"tokens":400,"calls":0,"carry_usd":0,"carry_priced":false,`, 1)
	b, _ = json.Marshal(map[string]any{"/api/tools": json.RawMessage(short), "/api/tools/last-used": json.RawMessage(`{"tools":[]}`)})
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if o := runCG(t, "http://127.0.0.1:1", nil, "insights", "tools", "--fixture", f); strings.Contains(strings.SplitN(o, "\n\n", 2)[0], "sentry") || !strings.Contains(o, "github") {
		t.Errorf("an unpriced-carry server must not appear as a $0 cost:\n%s", o)
	}
	// An older proxy without carry_usd: the unused cost is what there is, and the output says so.
	old := strings.ReplaceAll(tools, `"carry_usd"`, `"x_carry"`)
	b, _ = json.Marshal(map[string]any{"/api/tools": json.RawMessage(old), "/api/tools/last-used": json.RawMessage(`{"tools":[]}`)})
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	out = runCG(t, "http://127.0.0.1:1", nil, "insights", "tools", "--fixture", f)
	if !strings.Contains(out, "github") || !strings.Contains(out, "never used") {
		t.Errorf("an older proxy must still list servers with the unused cost:\n%s", out)
	}
}

// `cg.py cachestate --json` is what the mods call: it prints the cachestate from the status line's own
// on-disk cache (CONTRACT section 4), so the first call fetches once and the next ones, from either
// script, make no request.
func TestCGCachestateJSONSharesTheStatuslineCache(t *testing.T) {
	const now = 1_800_000_000_000
	cache := fmt.Sprintf(`{"session":"session-A","model":"m","last_ts":%d,"ttl_s":300,"now_ms":%d,"prefix_tokens":52000,"priced":true,`+
		`"rewrite_usd":0.3,"threads":[{"id":"","main":true,"last_ts":%d,"end_ts":%d,"ttl_s":300,"prefix_tokens":52000}]}`, now-60_000, now, now-60_000, now-60_000)
	port, hits := statuslineCounting(t, cache, nil)
	xdg := t.TempDir()
	env := []string{"XDG_CACHE_HOME=" + xdg}
	base := "http://127.0.0.1:" + port
	var doc map[string]any
	for i := 0; i < 3; i++ {
		out := runCG(t, base, env, "cachestate", "--json")
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
	}
	if n := hitCount(hits, "cachestate"); n != 1 {
		t.Errorf("3 calls fetched cachestate %d times; the shared 10 s cache must make it 1", n)
	}
	if doc["session"] != "session-A" || doc["threads"] == nil {
		t.Errorf("want the cachestate body back, got %v", doc)
	}
	if _, ok := doc["age_s"]; !ok {
		t.Errorf("want age_s so a mod can tell stale from fresh, got %v", doc)
	}
	// The status line, given the same cache directory, must reuse that very file.
	stdin := `{"session_id":"session-A","context_window":{"total_input_tokens":52000,"context_window_size":200000}}`
	senv := routedEnv(port)
	senv["XDG_CACHE_HOME"] = xdg
	runStatusline(t, senv, stdin, "--layout=balanced")
	if n := hitCount(hits, "cachestate"); n != 1 {
		t.Errorf("the status line refetched cachestate (%d); it must read the file cg.py wrote", n)
	}
}

// With nothing routed and no session there is nothing to read: a clear JSON error, not a traceback.
func TestCGCachestateJSONNoSession(t *testing.T) {
	out, code := runCGCode(t, "http://127.0.0.1:1", []string{"CLAUDE_CODE_SESSION_ID="}, "cachestate", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &doc); err != nil || doc["error"] == nil || code == 0 {
		t.Errorf("want a JSON {error} and a non-zero exit, got code %d: %s", code, out)
	}
}

// The Codex plugin ships the same two scripts, because plugin directories are installed apart and
// cannot import from each other. They are copies, and this keeps them identical.
func TestCGScriptsAreVendoredIntoCodex(t *testing.T) {
	for _, f := range []string{"cg.py", "cgcost.py", "cgcache.py", "costly.py", "costly_settings.json"} {
		a, err := os.ReadFile(filepath.Join("scripts", f))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join("..", "codex-marketplace", "plugins", "context-guru", "scripts", f))
		if err != nil || string(a) != string(b) {
			t.Errorf("%s in the Codex plugin differs from context-guru-plugin/scripts; copy it over (err=%v)", f, err)
		}
	}
}

// The docs show real output. Each example block on a docs page must be the golden, byte for byte,
// so a change to a screen fails here until the page is regenerated.
func TestDocsExamplesAreTheGoldens(t *testing.T) {
	for golden, page := range map[string]string{
		"home": "status", "savings": "savings", "cache-warm": "cache", "cache-cold": "cache",
		"session": "session", "doctor": "doctor",
		"save-more": "save-more", "save-more-ascii-80": "save-more", "apply-config": "save-more", "revert-config": "save-more",
		"apply-gate":            "save-more", "save-more-settings": "save-more",
		"remove-mcp-and-skills": "remove-mcp-and-skills", "apply-tools-async": "remove-mcp-and-skills", "insights": "insights",
	} {
		g := strings.TrimRight(string(cgRead(t, "cg", golden+".golden")), "\n")
		b, err := os.ReadFile(filepath.Join("..", "docs", "plugin", page+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "```text\n"+g+"\n```") {
			t.Errorf("docs/plugin/%s.md does not contain the %s golden verbatim", page, golden)
		}
	}
}

// levers runs cg.levers() on a stats document and returns the rows by name.
func cgLevers(t *testing.T, st map[string]any) map[string]float64 {
	t.Helper()
	py := requireTool(t, "python3")
	in, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	code := "import sys, json; sys.path.insert(0, sys.argv[1]); import cg; " +
		"print(json.dumps(dict(cg.levers(json.loads(sys.argv[2])))))"
	out, err := exec.Command(py, "-c", code, scriptsDir(t), string(in)).CombinedOutput()
	if err != nil {
		t.Fatalf("cg.levers: %v\n%s", err, out)
	}
	rows := map[string]float64{}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("cg.levers output %q: %v", out, err)
	}
	return rows
}

func cgWaterfall(total float64, deltas map[string]float64) map[string]any {
	var steps []map[string]any
	for k, v := range deltas {
		steps = append(steps, map[string]any{"key": k, "delta_usd": v})
	}
	return map[string]any{"total_saved_usd": total, "waterfall": steps}
}

// Sign convention of /api/stats (dash/overview.go, waterfall()): a saving is a NEGATIVE delta_usd, a
// cost is positive. #582: keepalive_saved was read as if positive, so a saving showed as a loss.
// The lever rows must add up to total_saved_usd, which is compaction + cache split + the keep-alive's
// net + tool filtering - own spend, and does NOT include self_removed (the account's own doing).
func TestInsightsLeversAddUpToTotalSaved(t *testing.T) {
	st := cgWaterfall(80+20+(120-60)+15-6, map[string]float64{
		"baseline": 1000, "compaction": -80, "cachesplit_saved": -20, "keepalive_saved": -120,
		"keepalive_ping": 60, "decl_filter_saved": -15, "self_removed": -30, "cg_llm": 6,
	})
	rows := cgLevers(t, st)
	var sum float64
	for _, v := range rows {
		sum += v
	}
	if want := st["total_saved_usd"].(float64); sum < want-0.005 || sum > want+0.005 {
		t.Errorf("rows add up to %.2f, total_saved is %.2f: %v", sum, want, rows)
	}
	for name, want := range map[string]float64{"Compaction": 80, "Cache split": 20, "Keep-alive pings": 60,
		"Tool filtering": 15, "Context Guru's own model spend": -6} {
		if got := rows[name]; got < want-0.005 || got > want+0.005 {
			t.Errorf("%s = %.2f, want %.2f", name, got, want)
		}
	}
}

// A keep-alive whose pings cost more than they saved is a real loss and must stay one.
func TestInsightsKeepAliveLossStaysNegative(t *testing.T) {
	rows := cgLevers(t, cgWaterfall(0, map[string]float64{"keepalive_saved": -10, "keepalive_ping": 40}))
	if got := rows["Keep-alive pings"]; got < -30.005 || got > -29.995 {
		t.Errorf("Keep-alive pings = %.2f, want -30.00 (saved 10, spent 40)", got)
	}
}
