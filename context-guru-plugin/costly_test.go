// Tests for the "Settings that cost you money" card of save-more (scripts/costly.py, scripts/costly_settings.json).
// The Python unit tests run here too (scripts/test_costly.py). The goldens use a stub proxy and a temp HOME-like config dir:
// no real user file is read or written.
package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCostlySettingsPythonTests(t *testing.T) {
	requireTool(t, "python3")
	out, err := exec.Command("python3", "-I", "-W", "ignore", filepath.Join("scripts", "test_costly.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_costly.py: %v\n%s", err, out)
	}
}

// costlyStub is a proxy that answers /api/adapt/settings (or, with old=true, 404s on it like a proxy that predates it).
func costlyStub(t *testing.T, old bool) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/adapt/settings", func(w http.ResponseWriter, r *http.Request) {
		if old {
			http.NotFound(w, r)
			return
		}
		w.Write(cgRead(t, "cg", "adapt_settings.json")) //nolint:errcheck
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// runCGSettings runs cg.py with a settings file in the user config dir, and returns the output and exit code.
func runCGSettings(t *testing.T, base string, settings map[string]any, env []string, args ...string) (string, int) {
	t.Helper()
	home, state := t.TempDir(), t.TempDir()
	cfg := filepath.Join(home, ".claude") // under HOME, so the card says ~/.claude/settings.json whatever the temp dir is
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	env = append([]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + cfg, "CLAUDE_PROJECT_DIR=" + t.TempDir(),
		"CG_MANAGED_SETTINGS=" + filepath.Join(cfg, "none.json"), "CONTEXT_GURU_STATE=" + state}, env...)
	// runCGCode puts its own CLAUDE_CONFIG_DIR first; the later value wins in exec.Cmd.Env.
	return runCGCode(t, base, env, args...)
}

func TestCostlySettingsGoldens(t *testing.T) {
	betas := map[string]any{"env": map[string]any{"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"}}
	for _, c := range []struct {
		name string
		old  bool
		env  []string
		args []string
	}{
		{"save-more-settings", false, nil, []string{"save-more", "settings"}},
		{"save-more-settings-old-proxy", true, nil, []string{"save-more", "settings"}},
		{"save-more-settings-ascii-80", false, []string{"CG_ASCII=1", "COLUMNS=80"}, []string{"save-more", "settings"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, code := runCGSettings(t, costlyStub(t, c.old), betas, c.env, c.args...)
			if code != 0 {
				t.Errorf("exit %d\n%s", code, out)
			}
			cgGolden(t, c.name, out)
		})
	}
}

// The default save-more shows the settings card with config and cache, and the other two reports are unchanged.
func TestSaveMoreDefaultIncludesTheSettingsCard(t *testing.T) {
	warm := cgProxyStub(t, "cachestate_warm")
	out, code := runCGSettings(t, warm, map[string]any{"env": map[string]any{"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"}}, nil, "save-more")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"Save more: config", "Save more: cache", "Settings that cost you money", "Cost: not measured yet",
		"apply settings disable-experimental-betas"} {
		if !strings.Contains(out, want) {
			t.Errorf("default save-more lacks %q\n%s", want, out)
		}
	}
	if out2, _ := runCGSettings(t, warm, map[string]any{"awaySummaryEnabled": false}, nil, "save-more"); strings.Contains(out2, "Settings that cost you money") {
		t.Error("the card shows although no check fires")
	}
}

// A read never writes, and a change under a read command is refused.
func TestSaveMoreSettingsReadNeverWrites(t *testing.T) {
	cfg := t.TempDir()
	p := filepath.Join(cfg, "settings.json")
	body := `{"env":{"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":"1"}}`
	os.WriteFile(p, []byte(body), 0o600) //nolint:errcheck
	env := []string{"CLAUDE_CONFIG_DIR=" + cfg, "CG_MANAGED_SETTINGS=" + filepath.Join(cfg, "none.json"), "CONTEXT_GURU_STATE=" + t.TempDir()}
	base := costlyStub(t, false)
	runCGCode(t, base, env, "save-more", "settings")
	out, code := runCGCode(t, base, env, "save-more", "apply", "settings", "disable-experimental-betas")
	if code == 0 || !strings.Contains(out, "Run this instead") {
		t.Errorf("a change under save-more must be refused with the exact command: exit %d\n%s", code, out)
	}
	if got, _ := os.ReadFile(p); string(got) != body {
		t.Errorf("settings file changed by a read: %s", got)
	}
	entries, _ := os.ReadDir(cfg)
	if len(entries) != 1 {
		t.Errorf("a read left files in the config dir: %v", entries)
	}
}
