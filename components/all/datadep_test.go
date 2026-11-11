package all_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/datadep"
)

// docs/data-dependence/components/<name>.md says, for every registered component, whether it depends on stored
// customer data and what it does at each storage level (off, metadata only, everything). Many users do turn it off, and a
// component that quietly starts to need data stops working for them with nothing reported (#428).
//
// This guard makes the table hard to skip: a component registered without a row fails, a row for a
// component that does not exist fails, and a row without a plain-words reason fails. It is the same
// set-in-both-directions check as config/docdrift_test.go, for the same reason — iterating only the
// rows that happen to exist cannot notice the row nobody wrote.
//
// There is one file per component, so two PRs never edit the same file (#1088). The generator
// builds docs/data-dependence.md from them; internal/datadep tests that the page is current.
const dataDepDir = "docs/data-dependence/components"

// dataDepRoot is the repo root, seen from this package.
const dataDepRoot = "../.."

// dataDepName is the allowed shape of a component name. It is wider than the names that exist
// today, so a misspelt row file is REPORTED, not skipped.
var dataDepName = regexp.MustCompile("^[a-z0-9_-]+$")

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

func TestDataDependenceDocCoversEveryComponent(t *testing.T) {
	loaded, err := datadep.Load(dataDepRoot, datadep.Components)
	if err != nil {
		t.Fatalf("read %s: %v", dataDepDir, err)
	}
	if len(loaded) == 0 {
		t.Fatalf("%s: no component rows", dataDepDir)
	}
	rows := map[string]datadep.Row{}
	for _, r := range loaded {
		name := r.Fields["name"]
		if !dataDepName.MatchString(name) {
			t.Errorf("%s: component name %q is not a-z, 0-9, _ or -", r.File, name)
		}
		if name != r.Stem {
			t.Errorf("%s: name %q must equal the file name %q", r.File, name, r.Stem)
		}
		if _, dup := rows[name]; dup {
			t.Errorf("%s: component %q has two rows; keep one", dataDepDir, name)
		}
		rows[name] = r
	}

	registered := map[string]bool{}
	for _, n := range components.Names() {
		registered[n] = true
		if _, ok := rows[n]; !ok {
			t.Errorf("component %q is registered but has no row file %s/%s.md. "+
				"Add one: say whether it depends on stored data, what it does with storage off, and why. "+
				"Then run: %s", n, dataDepDir, n, datadep.RegenCommand)
		}
	}

	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := rows[n]
		if !registered[n] && buildTagged[n] == "" {
			t.Errorf("%s documents component %q, which is not registered", r.File, n)
			continue
		}
		kind := strings.Trim(r.Fields["kind"], "*` ")
		if !dataDepKinds[kind] {
			t.Errorf("%s: Kind %q, want one of none, metadata, content", r.File, r.Fields["kind"])
		}
		for _, c := range []struct{ key, what string }{
			{"where", "Where"}, {"off", "Off"}, {"metadata_only", "Metadata only"}, {"everything", "Everything"},
			{"reason", "Reason (the text after the front matter)"}, {"decided_by", "Decided by"},
		} {
			if r.Fields[c.key] == "" {
				t.Errorf("%s: empty %q cell", r.File, c.what)
			}
		}
		reason := r.Fields["reason"]
		if codeRef.MatchString(reason) {
			t.Errorf("%s: the reason is a code reference (%q). "+
				"Say in plain words why the component needs (or does not need) the data.", r.File, reason)
		}
		if len(strings.Fields(reason)) < 8 {
			t.Errorf("%s: the reason has under 8 words (%q); explain why, in a sentence", r.File, reason)
		}
	}
}
