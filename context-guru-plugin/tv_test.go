// Runs the Python golden tests for the terminal views (scripts/test_tv.py): every view at three widths, in
// colour (dark and light), without colour and ASCII-only, against a synthetic history and a fixed clock.
// UPDATE_GOLDEN=1 rewrites testdata/tv/*.golden after an intended change; read the diff.
package plugin

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestTerminalViewGoldens(t *testing.T) {
	requireTool(t, "python3")
	out, err := exec.Command("python3", "-W", "ignore", filepath.Join("scripts", "test_tv.py")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_tv.py: %v\n%s", err, out)
	}
}
