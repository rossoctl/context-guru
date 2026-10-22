package dash

import (
	"os/exec"
	"strings"
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

// The Keep-alive (Cache strategy) ledger is shared between the Overview and its tab for a few seconds, and
// that share is keyed by the whole query, so a change of tenant, window or filter cannot get the old ledger.
func TestSharedKeepAliveLedgerIsKeyedByTheQuery(t *testing.T) {
	src, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{"const key = qs({});", "state.kaLedgerKey !== key", "state.kaLedgerKey = key;"} {
		if !strings.Contains(s, want) {
			t.Errorf("keepAliveLedger no longer contains %q; a change of scope could show the previous scope's ledger", want)
		}
	}
}
