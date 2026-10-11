package dash

import (
	"os/exec"
	"strings"
	"testing"
)

// Runs the pure bottom-line models under node (skips loudly when node is absent, like
// TestNavHashCompatibility). See bottomline.test.mjs.
func TestBottomLineModels(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH, so bottomline.test.mjs did not run; run `node --test dash/bottomline.test.mjs`")
	}
	out, err := exec.Command(node, "--test", "bottomline.test.mjs").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "# fail 0") || strings.Contains(string(out), "# pass 0") {
		t.Fatalf("node --test bottomline.test.mjs failed: %v\n%s", err, out)
	}
}
