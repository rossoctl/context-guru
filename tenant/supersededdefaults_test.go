package tenant

// Internal test on purpose: supersededDefaults is unexported and stays that way. It also
// needs no `config` import, so the rule that `tenant` does not depend on the pipeline
// holds for the test binary too (see defaultconfig_test.go).

import "testing"

// The live default must never appear in the superseded list. Everything the list is for
// depends on "matches an entry" meaning "is running an OLD default": if the current one
// were in there, every tenant tracking today's recommendation would be classified as
// running something stale, and any future sweep built on it would rewrite configurations
// that are already correct.
func TestSupersededDefaultsExcludesTheLiveDefault(t *testing.T) {
	for i, old := range supersededDefaults {
		if old == DefaultConfigYAML {
			t.Errorf("supersededDefaults[%d] is byte-identical to the CURRENT "+
				"DefaultConfigYAML; an entry means 'this value is no longer the default'", i)
		}
	}
}

// Duplicates are a symptom, not a bug in themselves: the same literal recorded twice means
// a default was reverted and re-superseded, or that a commit added an entry without
// checking. Either way the list stopped being a faithful history, which is its only job.
func TestSupersededDefaultsHasNoDuplicatesOrBlanks(t *testing.T) {
	seen := map[string]int{}
	for i, old := range supersededDefaults {
		if old == "" {
			t.Errorf("supersededDefaults[%d] is empty; an empty stored config already means "+
				"'tracks the server default' and must never be matched as a superseded one", i)
			continue
		}
		if prev, dup := seen[old]; dup {
			t.Errorf("supersededDefaults[%d] duplicates [%d]", i, prev)
			continue
		}
		seen[old] = i
	}
}
