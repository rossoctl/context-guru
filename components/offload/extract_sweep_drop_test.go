package offload

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// sweepFixture is a spent-looking tool output whose every word is distinctive, so invariant 6 can
// assert that NONE of them reached the descriptor. Long enough that a drop is a real reduction.
const sweepFixture = `KUBERNETES_NAMESPACE=quarantine-zebra
deployment/ingress-flamingo   READY 3/3   RESTARTS 0
deployment/ledger-armadillo   READY 1/1   RESTARTS 17
SECRET_TOKEN_kalamazoo_74119 rotated at 2026-08-14T09:31:02Z
warning: pod ledger-armadillo evicted, reason=MemoryPressure
` + `filler-line-that-is-here-only-to-clear-the-never-worse-check
`

// INVARIANT 5. A dropped output stays recoverable: the marker is written, the original is stashed,
// and expand resolves it BYTE-FOR-BYTE. A drop advertised as reversible that is not would be a worse
// defect than no drop at all.
func TestDroppedOutputStaysRecoverable(t *testing.T) {
	st := store.NewMemory(store.Options{})
	rep := &components.Report{}
	c := &components.Ctx{Session: "s", Store: st}
	msg := tool(sweepFixture)

	key, ok := applySweepDrop(c, rep, markerFull, &msg, sweepFixture)
	// PRECONDITION: the drop actually happened. Without this the assertions below are vacuous —
	// a helper that returned early and changed nothing would leave the original in place, and
	// "recoverable" is trivially true of content that was never removed.
	if !ok {
		t.Fatal("the drop was refused, so nothing under test ran")
	}
	got := schema.MessageText(msg)
	if got == sweepFixture {
		t.Fatal("the message was not rewritten, so no recovery path was exercised")
	}
	if key == "" {
		t.Fatal("no store key: the original was never stashed")
	}
	if rep.Irreversible {
		t.Fatal("a full-marker drop must not be recorded as irreversible")
	}

	keys := expand.ParseMarkers(got)
	if len(keys) != 1 {
		t.Fatalf("expected exactly one resolvable marker, got %d in %q", len(keys), got)
	}
	orig, resolved := expand.Resolve(st, keys[0])
	if !resolved {
		t.Fatal("the marker did not resolve — the drop would be unrecoverable")
	}
	if orig != sweepFixture {
		t.Fatalf("round-trip is not byte-for-byte:\n want %q\n  got %q", sweepFixture, orig)
	}
	// And it must be strictly smaller, marker included, or the drop cost more than it saved.
	if schema.TextTokens(got) >= schema.TextTokens(sweepFixture) {
		t.Errorf("drop did not shrink the message: %d tokens from %d",
			schema.TextTokens(got), schema.TextTokens(sweepFixture))
	}
}

// A store that cannot persist must not leave an unresolvable marker behind: the drop degrades to a
// markerless one and records the deliberate lossy removal, so the pipeline keeps it rather than
// reverting it.
func TestDroppedOutputWithoutAPersistingStoreLeavesNoDanglingMarker(t *testing.T) {
	rep := &components.Report{}
	c := &components.Ctx{Session: "s", Store: store.Nop{}}
	msg := tool(sweepFixture)
	key, ok := applySweepDrop(c, rep, markerFull, &msg, sweepFixture)
	if !ok {
		t.Fatal("the drop was refused, so nothing under test ran")
	}
	got := schema.MessageText(msg)
	if key != "" || strings.Contains(got, "<<cg:") {
		t.Fatalf("non-persisting store left a marker nothing can resolve: key=%q text=%q", key, got)
	}
	if !rep.Irreversible {
		t.Fatal("an unrecoverable drop must set Irreversible or the pipeline will revert it")
	}
}

// INVARIANT 6. The descriptor left in place transports nothing. It is generated from the output's
// shape by OUR code, never by the model, and it carries no byte of the output.
func TestSweepDescriptorTransportsNothing(t *testing.T) {
	bodies := []string{
		sweepFixture,
		`[{"id":"acct_wolverine","balance":4711},{"id":"acct_pangolin","balance":88}]`,
		"Traceback (most recent call last):\n  File \"parser_marmoset.py\", line 44\nValueError: nope\n",
		strings.Repeat("plainprose sentence about the artichoke harvest.\n", 40),
	}
	for _, body := range bodies {
		desc := sweepDescriptor(body)
		// PRECONDITION: there IS a descriptor, and it reports the shape. A descriptor that came
		// back empty or generic would pass the leak assertion below while telling the agent
		// nothing, so the two must be checked together.
		if desc == "" {
			t.Fatalf("no descriptor for %.40q", body)
		}
		if !strings.Contains(desc, " lines,") || !strings.Contains(desc, " tokens") {
			t.Fatalf("descriptor does not report the shape: %q", desc)
		}
		// No word of the body may appear in it. Words rather than lines, because a leak worth
		// catching is a leaked identifier, not a leaked whole line.
		for _, w := range strings.FieldsFunc(body, func(r rune) bool {
			return r == ' ' || r == '\n' || r == '\t' || r == '"' || r == ',' || r == '{' || r == '}'
		}) {
			if len(w) < 5 {
				continue // too short to be an identifier, and collides with the shape numbers
			}
			if strings.Contains(desc, w) {
				t.Errorf("descriptor transports content from the output: %q appears in %q", w, desc)
			}
		}
	}
}

// The one figure a line count cannot supply. A multi-megabyte JSON API result is routinely a SINGLE
// line, and "1 line" is a useless thing to tell an agent about 200 records — which is also why the
// collapse component skips such payloads entirely.
func TestSweepDescriptorCountsRecordsOnASingleLineArray(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 200; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"n":777}`)
	}
	b.WriteString("]")
	desc := sweepDescriptor(b.String())
	if !strings.Contains(desc, "200 records") {
		t.Fatalf("a 200-record single-line array must report its record count, got %q", desc)
	}
}
