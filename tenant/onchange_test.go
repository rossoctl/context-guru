package tenant

import "testing"

// The dashboard drops cached reads that include account state when this fires, so it has to fire for
// every committed write to an account and for the strategy ledger, and not for a no-op.
func TestSetOnChangeFiresAfterCommittedWrites(t *testing.T) {
	r, err := Open("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	r.SetOnChange(func(kind string) { kinds = append(kinds, kind) })

	tn, _, err := r.Register("laptop", "a@example.test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := "pipeline: [format]\n"
	if err := r.Update(tn, tn.ID, Patch{ConfigYAML: &cfg}); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 || kinds[0] != ChangeAccount {
		t.Fatalf("after an account update the listener saw %v, want [account]", kinds)
	}
	// The same document again changes nothing, so nothing is dropped.
	if err := r.Update(tn, tn.ID, Patch{ConfigYAML: &cfg}); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 {
		t.Errorf("a no-op update fired the listener: %v", kinds)
	}
	// A refused write does not fire it either.
	other := *tn
	other.ID = "someone-else"
	if err := r.Update(&other, tn.ID, Patch{ConfigYAML: &cfg}); err == nil {
		t.Fatal("expected a forbidden update")
	}
	if len(kinds) != 1 {
		t.Errorf("a refused update fired the listener: %v", kinds)
	}
}
