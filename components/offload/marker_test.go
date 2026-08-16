package offload

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// A full (reversible) marker must degrade to an irreversible off-style drop when the store
// cannot persist the stash — otherwise it leaves an unresolvable <<cg:HASH>> marker in the
// request and silently loses the dropped content.
//
// Asserted against tryMark/commitMark, which is what every shipped offloader calls. This
// used to test the eager mark() helper instead; that helper's own doc said "retained only
// for tests", so the invariant was verified on a code path no request ever took.
func TestMarkerDegradesWhenStoreCannotPersist(t *testing.T) {
	// Long enough that the marker-inclusive never-worse check accepts the rewrite.
	original := strings.Repeat("original content worth stashing rather than re-sending. ", 8)
	assemble := func(tok string) string { return "[dropped] " + tok }

	rep := &components.Report{}
	c := &components.Ctx{Store: store.Nop{}}
	newText, key, eff, ok := tryMark(c, markerFull, original, " [hint]", assemble)
	if !ok {
		t.Fatal("marker-inclusive never-worse check should accept this rewrite")
	}
	if eff != markerOff || key != "" || strings.Contains(newText, "<<cg:") {
		t.Fatalf("non-persisting store: want a degraded markerless drop, got eff=%v key=%q text=%q",
			eff, key, newText)
	}
	commitMark(c, rep, eff, key, original)
	if !rep.Irreversible {
		t.Fatal("degraded drop must set Irreversible so the pipeline keeps it (not reverted)")
	}

	// With a persisting store, full mode emits a resolvable marker and stashes the original.
	rep2 := &components.Report{}
	c2 := &components.Ctx{Store: store.NewMemory(store.Options{})}
	newText2, key2, eff2, ok2 := tryMark(c2, markerFull, original, "", assemble)
	if !ok2 || eff2 != markerFull || key2 == "" || !strings.Contains(newText2, "<<cg:"+key2+">>") {
		t.Fatalf("persisting store: want marker+key, got eff=%v key=%q text=%q", eff2, key2, newText2)
	}
	commitMark(c2, rep2, eff2, key2, original)
	if rep2.Irreversible {
		t.Fatal("a reversible full-mode drop must not be marked Irreversible")
	}
	if got, ok := c2.Store.Get(key2); !ok || string(got) != original {
		t.Fatalf("persisting store must retain the original, got %q ok=%v", got, ok)
	}
}
