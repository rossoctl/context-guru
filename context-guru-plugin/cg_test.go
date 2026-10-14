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
	os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"python3 x/statusline.py","refreshInterval":5}}`), 0o644) //nolint:errcheck
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "COLUMNS=100", "CG_ASCII=", "CLAUDE_CONFIG_DIR="+cfg, "CLAUDE_CODE_SESSION_ID=session-A",
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
		{"savings", warm, []string{"savings"}, 0},
		{"savings-details", warm, []string{"savings", "details"}, 0},
		{"cache-warm", warm, []string{"cache"}, 0},
		{"cache-cold", cold, []string{"cache"}, 0},
		{"waste", warm, []string{"waste"}, 0},
		{"session", warm, []string{"session"}, 0},
		{"doctor", warm, []string{"doctor"}, 0},
		{"save-more", warm, []string{"save-more"}, 0},
		{"save-more-config", warm, []string{"save-more", "config"}, 0},
		{"save-more-config-details", warm, []string{"save-more", "config", "details"}, 0},
		{"save-more-cache", warm, []string{"save-more", "cache"}, 0},
		{"save-more-cache-details", warm, []string{"save-more", "cache", "details"}, 0},
		{"remove-mcp-and-skills", warm, []string{"remove-mcp-and-skills"}, 0},
		{"remove-mcp-and-skills-details", warm, []string{"remove-mcp-and-skills", "details"}, 0},
		{"adapt-redirect", warm, []string{"adapt", "tools"}, 0},
		{"apply-config", warm, []string{"apply", "config", "medium@a1b2c3d4"}, 0},
		{"apply-cache", warm, []string{"apply", "cache", "adaptive@3f9a1c"}, 0},
		{"apply-stale", warm, []string{"apply", "cache", "adaptive@000000"}, 1},
		{"apply-tools-async", warm, []string{"apply", "tools", "remove-sufficient@8c21e0d4"}, 0},
		{"apply-under-read", warm, []string{"save-more", "cache", "apply", "adaptive@3f9a1c"}, 1},
		{"revert-under-read", warm, []string{"save-more", "revert", "config"}, 1},
		{"revert-kind-first", warm, []string{"config", "revert"}, 1},
		{"revert-under-cache", warm, []string{"cache", "revert"}, 1},
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
	for _, c := range []string{"home", "savings", "cache", "waste", "session"} {
		out := runCG(t, base, nil, c)
		first := strings.SplitN(out, "\n", 2)[0]
		if len(first) < 20 || strings.Contains(first, "tokens") {
			t.Errorf("%s: first line %q is not a bottom line in dollars", c, first)
		}
	}
}

func TestCGNoProxy(t *testing.T) {
	requireTool(t, "python3")
	cmd := exec.Command("python3", filepath.Join("scripts", "cg.py"), "savings")
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=https://api.anthropic.com", "CG_URL=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "No Context Guru proxy") {
		t.Errorf("want the no-proxy message, got %v: %s", err, out)
	}
}

// The save-more, remove-mcp-and-skills and adapt skills pre-approve only their read. A change is a separate
// top-level command, so Claude Code asks the user for permission to run it: a second gate behind the skill's
// own yes/no question. No read command writes, whatever words follow it.
func TestAdaptSkillPreApprovesOnlyTheRead(t *testing.T) {
	for _, c := range []string{"save-more", "remove-mcp-and-skills", "adapt"} {
		body := readFileString(t, filepath.Join("skills", c, "SKILL.md"))
		if !regexp.MustCompile(`(?m)^allowed-tools: Bash\(python3 "\$\{CLAUDE_PLUGIN_ROOT\}/scripts/cg\.py" ` + c + `:\*\)(, Bash\(python3 "\$\{CLAUDE_PLUGIN_ROOT\}/scripts/settings\.py" record-run ` + c + `\))?$`).MatchString(body) {
			t.Errorf("%s must pre-approve exactly its own read, `cg.py %s:*`", c, c)
		}
	}
	base := cgProxyStub(t, "cachestate_warm")
	for _, args := range [][]string{{"adapt", "cache", "apply", "adaptive@3f9a1c"}, {"save-more", "apply", "cache", "adaptive@3f9a1c"},
		{"save-more", "revert", "config"}, {"remove-mcp-and-skills", "apply", "tools", "remove-sufficient@8c21e0d4"}} {
		if out, code := runCGCode(t, base, nil, args...); code == 0 || strings.Contains(out, "Applied") {
			t.Errorf("%v must not write (it is inside the pre-approved prefix): exit %d\n%s", args, code, out)
		}
	}
}

// No skill pre-approves a change. A pre-approved pattern names one READ command of cg.py (the first word picks
// the command, so `cg.py waste:*` can only ever run the waste screen); a bare `cg.py:*` would also cover
// `cg.py apply` and `cg.py revert` while that skill is active, and skip the harness's permission prompt.
// Codex skills pre-approve nothing, and every write they run asks for escalation.
func TestNoSkillPreApprovesAChange(t *testing.T) {
	reads := map[string]bool{"adapt": true, "save-more": true, "remove-mcp-and-skills": true, "cache-campaign": true}
	for _, c := range []string{"home", "savings", "cache", "waste", "session", "doctor"} {
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

// The Codex plugin ships the same two scripts, because plugin directories are installed apart and
// cannot import from each other. They are copies, and this keeps them identical.
func TestCGScriptsAreVendoredIntoCodex(t *testing.T) {
	for _, f := range []string{"cg.py", "cgcost.py"} {
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
		"waste": "waste", "session": "session", "doctor": "doctor",
		"save-more": "save-more", "save-more-ascii-80": "save-more", "apply-config": "save-more", "revert-config": "save-more",
		"apply-gate":            "save-more",
		"remove-mcp-and-skills": "remove-mcp-and-skills", "apply-tools-async": "remove-mcp-and-skills",
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
