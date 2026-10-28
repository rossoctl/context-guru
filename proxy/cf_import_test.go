package proxy

import (
	"testing"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

func TestCounterfactualImportable(t *testing.T) {
	if cf.NewTrackers() == nil {
		t.Fatal("nil Trackers")
	}
}
