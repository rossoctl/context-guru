package all_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// docs/data-dependence.md says, for every registered component, whether it depends on stored
// customer data and what it does at each storage level (off, metadata only, everything). Many users do turn it off, and a
// component that quietly starts to need data stops working for them with nothing reported (#428).
//
// This guard makes the table hard to skip: a component registered without a row fails, a row for a
// component that does not exist fails, and a row without a plain-words reason fails. It is the same
// set-in-both-directions check as config/docdrift_test.go, for the same reason — iterating only the
// rows that happen to exist cannot notice the row nobody wrote.
const dataDepDoc = "../../docs/data-dependence.md"

// dataDepSection is the heading the component table lives under. Only rows in this section are
// read, so the feature and switch tables further down the page cannot satisfy the check by accident.
const dataDepSection = "## Pipeline components"

// dataDepRow matches a table row whose first cell is a backticked component name. The name class
// is wider than the names that exist today, so a misspelt row is REPORTED, not skipped.
var dataDepRow = regexp.MustCompile("^\\|\\s*`([a-z0-9_-]+)`\\s*\\|(.*)\\|\\s*$")

// dataDepKinds are the only allowed values of the "Kind" cell.
var dataDepKinds = map[string]bool{"none": true, "metadata": true, "content": true}

// codeRef matches a reason that is a pointer into the source rather than an explanation. The
// reason column is read by people deciding whether to turn storage off; "see marker.go:120" tells
// them nothing, and a file:line goes stale on the next refactor.
var codeRef = regexp.MustCompile(`\.go(:\d+)?\b|:\d+\b`)

// buildTagged are components that register only under a build tag, so a default `go test` does
// not see them. Their rows are allowed without a registration; a run with the tag checks them the
// same way as every other component.
var buildTagged = map[string]string{"skeleton": "cg_skeleton"}

// The column order of the component table. Cells are addressed by these indexes below. The three
// level columns say what the component does with storage off, with metadata only, and with
// everything stored.
const (
	colKind       = 0 // after the name cell
	colWhere      = 1
	colOff        = 2
	colMetaOnly   = 3
	colEverything = 4
	colReason     = 5
	colDecidedBy  = 6
	numCols       = 7
)

func TestDataDependenceDocCoversEveryComponent(t *testing.T) {
	raw, err := os.ReadFile(dataDepDoc)
	if err != nil {
		t.Fatalf("read %s: %v", dataDepDoc, err)
	}
	rows := map[string][]string{}
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.TrimSpace(line) == dataDepSection
			continue
		}
		if !in {
			continue
		}
		m := dataDepRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if _, dup := rows[name]; dup {
			t.Errorf("%s: component %q has two rows; keep one", dataDepDoc, name)
		}
		cells := strings.Split(m[2], "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows[name] = cells
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no component rows under %q — the table or its heading moved", dataDepDoc, dataDepSection)
	}

	registered := map[string]bool{}
	for _, n := range components.Names() {
		registered[n] = true
		if _, ok := rows[n]; !ok {
			t.Errorf("component %q is registered but has no row in %s under %q. "+
				"Add one: say whether it depends on stored data, what it does with storage off, and why.",
				n, dataDepDoc, dataDepSection)
		}
	}

	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cells := rows[n]
		if !registered[n] && buildTagged[n] == "" {
			t.Errorf("%s documents component %q, which is not registered", dataDepDoc, n)
			continue
		}
		if len(cells) != numCols {
			t.Errorf("%s: row %q has %d cells after the name, want %d", dataDepDoc, n, len(cells), numCols)
			continue
		}
		kind := strings.Trim(cells[colKind], "*` ")
		if !dataDepKinds[kind] {
			t.Errorf("%s: row %q has Kind %q, want one of none, metadata, content", dataDepDoc, n, cells[colKind])
		}
		for _, c := range []struct {
			col  int
			what string
		}{
			{colWhere, "Where"}, {colOff, "Off"}, {colMetaOnly, "Metadata only"}, {colEverything, "Everything"},
			{colReason, "Reason"}, {colDecidedBy, "Decided by"},
		} {
			if cells[c.col] == "" {
				t.Errorf("%s: row %q has an empty %q cell", dataDepDoc, n, c.what)
			}
		}
		reason := cells[colReason]
		if codeRef.MatchString(reason) {
			t.Errorf("%s: row %q gives a code reference as its reason (%q). "+
				"Say in plain words why the component needs (or does not need) the data.", dataDepDoc, n, reason)
		}
		if len(strings.Fields(reason)) < 8 {
			t.Errorf("%s: row %q has a reason of under 8 words (%q); explain why, in a sentence", dataDepDoc, n, reason)
		}
	}
}
