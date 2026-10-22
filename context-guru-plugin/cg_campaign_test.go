package plugin

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// campaignStub serves only the campaign routes.
func campaignStub(t *testing.T) string {
	t.Helper()
	view := `{"id":"cmp-test","state":"done","mode":"off","live":false,"recommendation":"ship",` +
		`"window":{"since":0,"until":2592000000},"scope":{"type":"tenant","id":"t"},"summary":{` +
		`"model":{"strategy":"plan","net_usd":90,"ci95":[80,100]},"best_baseline":{"strategy":"rule","net_usd":70,"ci95":[60,80]},` +
		`"vs_best_baseline":{"vs":"rule","diff_usd":20,"ci95":[10,30]}},` +
		`"spec":{"evidence":{"tuner":{"tenant_plan":"K1"},"shipped":{"plan":"K2","fleet_plan":"K3","kappa":300,"half_life_days":3}}}}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cache/campaigns/build", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(202)
		w.Write([]byte(`{"campaign_id":"cmp-test","state":"running"}`))
	})
	mux.HandleFunc("GET /api/cache/campaigns", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"campaigns":[{"id":"cmp-test"}]}`)) })
	mux.HandleFunc("GET /api/cache/campaigns/cmp-test", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(view)) })
	mux.HandleFunc("POST /api/cache/campaigns/cmp-test/mode", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"cmp-test","mode":"shadow"}`))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("http://%s", ln.Addr())
}

func runCampaign(t *testing.T, base string, args ...string) string {
	t.Helper()
	cmd := exec.Command("python3", append([]string{"scripts/cg.py", "cache-campaign"}, args...)...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "ANTHROPIC_BASE_URL="+base+"/anthropic")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestCampaignUsesEndpoint(t *testing.T) {
	base := campaignStub(t)
	for _, args := range [][]string{{"build"}, {}, {"show", "cmp-test"}} {
		if out := runCampaign(t, base, args...); !strings.Contains(out, "beats the best fixed rule by $20.00") || strings.Contains(out, "EXAMPLE") {
			t.Errorf("%v:\n%s", args, out)
		}
	}
	if out := runCampaign(t, base, "mode", "cmp-test", "shadow"); !strings.Contains(out, "now shadow") {
		t.Errorf("mode:\n%s", out)
	}
	// details names the plan that runs (the auto builder's refit), not the one its held-out table evaluated
	if out := runCampaign(t, base, "show", "cmp-test", "details"); !strings.Contains(out, "Running plan K2, fleet plan K3") || strings.Contains(out, "K1") {
		t.Errorf("show details:\n%s", out)
	}
	if out := runCampaign(t, base, "--mock"); !strings.Contains(out, "EXAMPLE, not your data") {
		t.Errorf("mock:\n%s", out)
	}
}

// errorStub answers the way proxy/campaignbuild.go does when something is wrong: 401 without the login cookie, 404 for an
// unknown campaign, and a finished build with too little history.
func errorStub(t *testing.T, built *string) string {
	t.Helper()
	mux := http.NewServeMux()
	ctlErr := func(w http.ResponseWriter, code int, msg string) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"error":%q}`, msg)
	}
	mux.HandleFunc("GET /api/cache/campaigns", func(w http.ResponseWriter, _ *http.Request) { ctlErr(w, 401, "sign in first") })
	mux.HandleFunc("POST /api/cache/campaigns/build", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*built = string(b)
		ctlErr(w, 401, "sign in first")
	})
	mux.HandleFunc("GET /api/cache/campaigns/cmp-typo", func(w http.ResponseWriter, _ *http.Request) { ctlErr(w, 404, "no such campaign") })
	mux.HandleFunc("POST /api/cache/campaigns/cmp-typo/mode", func(w http.ResponseWriter, _ *http.Request) { ctlErr(w, 404, "no such campaign") })
	mux.HandleFunc("POST /api/cache/campaigns/cmp-thin/mode", func(w http.ResponseWriter, _ *http.Request) {
		ctlErr(w, 409, "this scope has too little history to run a campaign")
	})
	mux.HandleFunc("GET /api/cache/campaigns/cmp-thin", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"cmp-thin","state":"done","mode":"off","recommendation":"insufficient_data","window":{"since":null,"until":null},` +
			`"summary":{"recommendation":"insufficient_data","reason":"held-out rows 12 (need 500), sessions 2 (need 20)"}}`))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("http://%s", ln.Addr())
}

func TestCampaignErrorsSayWhatTheServerSaid(t *testing.T) {
	var built string
	base := errorStub(t, &built)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := fmt.Sprintf("http://%s", ln.Addr())
	ln.Close()
	for _, c := range []struct {
		base, want, not string
		args            []string
	}{
		{base, "CG_DASH_COOKIE", "HTTP 401", []string{"build", "--builder", "auto", "--session-end"}}, // 401 = no login cookie
		{base, "CG_DASH_COOKIE", "HTTP 401", nil},                                                     // the list behind "show latest"
		{base, "no such campaign", "No traffic", []string{"show", "cmp-typo"}},
		{base, "no such campaign", "No traffic", []string{"mode", "cmp-typo", "shadow"}},
		{base, "too little history", "", []string{"mode", "cmp-thin", "shadow"}},
		{base, "held-out rows 12 (need 500)", "has no result", []string{"show", "cmp-thin"}}, // insufficient_data: verdict and reason
		{dead, "Cannot reach the proxy", "not found", []string{"show", "cmp-x"}},
		{base, "Usage:", "", []string{"biuld"}},
	} {
		out := runCampaign(t, c.base, c.args...)
		if !strings.Contains(out, c.want) || (c.not != "" && strings.Contains(out, c.not)) {
			t.Errorf("%v: want %q (and not %q), got:\n%s", c.args, c.want, c.not, out)
		}
	}
	if !strings.Contains(built, `"builder": "auto"`) || !strings.Contains(built, `"session_end": true`) {
		t.Errorf("build --builder auto --session-end sent %s", built)
	}
}

// Only the reads are pre-approved: build and mode change the proxy, so the harness must ask.
func TestCacheCampaignSkillPreApprovesOnlyReads(t *testing.T) {
	body := readFileString(t, filepath.Join("skills", "cache-campaign", "SKILL.md"))
	// The path is quoted as the skill's own command line quotes it: a pattern is matched literally, so an unquoted one never matches.
	want := `allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign), Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign details), Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign show:*)` + "\n"
	if !strings.Contains(body, `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign $ARGUMENTS`) {
		t.Error("the skill's command line no longer has the form the patterns match")
	}
	if !strings.Contains(body, want) {
		t.Errorf("allowed-tools must name only the reads, want %q", want)
	}
	for _, bad := range []string{"cg.py:*", "cache-campaign:*", "build", "mode:*"} {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "allowed-tools:") && strings.Contains(line, bad) {
				t.Errorf("allowed-tools pre-approves %q", bad)
			}
		}
	}
}

// Claude Code matches an allowed-tools pattern literally: `Bash(x:*)` is a prefix of the command line, `Bash(x)` the whole line. Every read the
// skill can run must be covered, and no build or mode line may be.
func TestCacheCampaignPatternsCoverTheReadsAndNoWrite(t *testing.T) {
	body := readFileString(t, filepath.Join("skills", "cache-campaign", "SKILL.md"))
	var pats []string
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "allowed-tools: "); ok {
			for _, p := range strings.Split(rest, ", ") {
				pats = append(pats, strings.TrimSuffix(strings.TrimPrefix(p, "Bash("), ")"))
			}
		}
	}
	matches := func(cmd string) bool {
		for _, p := range pats {
			if pre, ok := strings.CutSuffix(p, ":*"); ok && strings.HasPrefix(cmd, pre) || p == cmd {
				return true
			}
		}
		return false
	}
	run := `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache-campaign`
	if !strings.Contains(body, run+" $ARGUMENTS") {
		t.Fatalf("the skill no longer runs %q", run+" $ARGUMENTS")
	}
	for _, a := range []string{"", " details", " show cmp-1", " show cmp-1 details"} {
		if !matches(run + a) {
			t.Errorf("read %q is not pre-approved by %q", run+a, pats)
		}
	}
	for _, a := range []string{" build", " build --builder auto --session-end", " mode cmp-1 shadow", " mode cmp-1 active --force"} {
		if matches(run + a) {
			t.Errorf("write %q is pre-approved", run+a)
		}
	}
}
