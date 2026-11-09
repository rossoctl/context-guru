package proxy

import (
	"testing"

	"github.com/rossoctl/accountant"
)

func TestAccountantImportable(t *testing.T) {
	if accountant.NewTrackers() == nil {
		t.Fatal("nil Trackers")
	}
}
