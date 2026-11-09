// Runs the Python golden tests for the terminal views (scripts/test_tv.py): every view at three widths, in
// colour (dark and light), without colour and ASCII-only, against a synthetic history and a fixed clock.
// UPDATE_GOLDEN=1 rewrites testdata/tv/*.golden after an intended change; read the diff.
package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTerminalViewGoldens(t *testing.T) {
	requireTool(t, "python3")
	out, err := exec.Command("python3", "-W", "ignore", filepath.Join("scripts", "test_tv.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_tv.py: %v\n%s", err, out)
	}
}

// The Codex plugin ships copies of the view scripts (plugin directories are installed apart); this keeps them identical.
func TestTVScriptsAreVendoredIntoCodex(t *testing.T) {
	for _, f := range []string{"tv.py", "tvlib.py"} {
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

// The docs show real output: each view's page example is its 80-column golden, byte for byte.
func TestTVDocsExamplesAreTheGoldens(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "docs", "plugin", "terminal-views.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"watch", "timeline", "chart", "heatmap", "fill"} {
		g, err := os.ReadFile(filepath.Join("testdata", "tv", v+"-w80.golden"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "```text\n"+strings.TrimRight(string(g), "\n")+"\n```") {
			t.Errorf("docs/plugin/terminal-views.md does not contain the %s golden verbatim", v)
		}
	}
}
