package proxy

// The inventory hand-off has to carry the request's project.
//
// WHY A SOURCE-LEVEL ASSERTION. The call is one line, and what it passes decides whether the
// dashboard can scope capability usage to a folder at all. Passing "" compiles, runs, records
// every invocation with an unknown folder, and shows up only as "every project reports the same
// usage" on a page nobody looks at for weeks — which is precisely how the defect this fixes
// reached a release. Driving it through a real proxy would be mostly scaffolding and would assert
// the same single fact.

import (
	"os"
	"regexp"
	"testing"
)

func TestTheInventoryHandoffCarriesTheProject(t *testing.T) {
	b, err := os.ReadFile("dashcapture.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	call := regexp.MustCompile(`RecordInventory\([^)]*\)`).FindString(src)
	if call == "" {
		t.Fatal("no RecordInventory call found; this check needs rewriting against whatever " +
			"replaced it")
	}
	// The project argument must be the request's own, not a literal.
	if !regexp.MustCompile(`e\.Project\s*\)`).MatchString(call) {
		t.Errorf("RecordInventory is called as %s — it has to pass e.Project as its last "+
			"argument. An empty literal compiles and records every invocation with an unknown "+
			"folder, which is the defect that made every project report identical usage", call)
	}
}
