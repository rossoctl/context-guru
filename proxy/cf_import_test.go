package proxy

import (
	"testing"

	cf "github.com/rossoctl/accountant"
)

func TestCounterfactualImportable(t *testing.T) {
	if cf.NewTrackers() == nil {
		t.Fatal("nil Trackers")
	}
}
