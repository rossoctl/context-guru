package store

import (
	"testing"
	"time"
)

// A tenant's own state_ttl_seconds wins; the host's value fills only an unset one.
func TestSetStateTTLKeepsTheStoresOwnValue(t *testing.T) {
	own := NewMemory(Options{StateTTLSeconds: 60})
	own.SetStateTTL(24 * time.Hour)
	if own.stateTTL != time.Minute {
		t.Errorf("stateTTL = %v, want 1m", own.stateTTL)
	}
	unset := NewMemory(Options{})
	unset.SetStateTTL(24 * time.Hour)
	if unset.stateTTL != 24*time.Hour {
		t.Errorf("stateTTL = %v, want 24h", unset.stateTTL)
	}
}
