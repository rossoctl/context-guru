package offload

import (
	"strings"
	"testing"
)

// An old config that still names max_ask_items gets a pointer to max_ask_batches, not a bare "field not found".
func TestAnOldMaxAskItemsKeyGetsAPointerToMaxAskBatches(t *testing.T) {
	_, err := newExtractSweep([]byte("min_tokens: 2000\nmax_ask_items: 40\n"))
	if err == nil {
		t.Fatal("max_ask_items: 40 was accepted: it is not a setting")
	}
	for _, want := range []string{"max_ask_items", "max_ask_batches"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "not found in type") {
		t.Errorf("error = %q: a bare yaml 'not found' instead of the pointer", err)
	}
	if _, err := newExtractSweep([]byte("min_tokens: 2000\nmax_ask_batches: 6\n")); err != nil {
		t.Errorf("max_ask_batches: 6 was refused: %v", err)
	}
}
