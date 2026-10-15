package dash

import (
	"os/exec"
	"testing"
)

// TestSavingsPartsAddUp runs savings.test.mjs: the Overview's three rows must sum to total_saved_usd.
func TestSavingsPartsAddUp(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH, so savings.test.mjs did not run; run `node --test dash/savings.test.mjs`")
	}
	if out, err := exec.Command(node, "--test", "savings.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("node --test savings.test.mjs failed: %v\n%s", err, out)
	}
}
