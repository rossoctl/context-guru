package dash

import (
	"os/exec"
	"testing"
)

// TestV2Models runs the node test for the five-page dashboard's pure models, the same way
// TestBottomLineModels runs bottomline.test.mjs.
func TestV2Models(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH, so v2.test.mjs did not run; run `node --test dash/v2.test.mjs`")
	}
	cmd := exec.Command(node, "--test", "v2.test.mjs")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test v2.test.mjs failed: %v\n%s", err, out)
	}
}
