package proxy

import (
	"testing"

	"github.com/rossoctl/accountant"
)

func TestCounterfactualImportable(t *testing.T) {
	if accountant.NewTrackers() == nil {
		t.Fatal("nil Trackers")
	}
}
