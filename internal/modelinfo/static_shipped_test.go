package modelinfo

import (
	"context"
	"testing"
)

// EVERY SHIPPED MODEL ID IS PINNED TO ITS PUBLISHED WINDOW, so the next model release fails this test
// instead of silently inheriting the `{"claude", 200000}` catch-all.
//
// That inheritance is what issue #233 was: Opus 5, Opus 4.6 and newer, Fable 5.1 and Sonnet 4.6 all
// resolved to 200,000 against a published 1,000,000 — five times low, with ok=true, so no caller ever
// saw "unknown". They saw a confident wrong number, and the live consumer (ExtractLLM.inputLimit, on a
// pinned model) silently under-extracted on exactly the long-context models it exists for.
//
// The point of pinning rather than spot-checking: a table nobody tests is a table that goes stale
// again, and the failure mode is invisible because the wrong answer is plausible.
func TestEveryShippedModelIDResolvesToItsPublishedWindow(t *testing.T) {
	s := DefaultStatic()
	ctx := context.Background()
	for _, tc := range []struct {
		id   string
		want int
	}{
		// The 1M families.
		{"claude-opus-5", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-fable-5-1", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4-7", 1_000_000},
		{"claude-opus-4-6", 1_000_000},
		{"claude-sonnet-4-6", 1_000_000},
		// The 200K families. haiku was already correct via the catch-all; it is stated explicitly so
		// a future haiku with a different window fails here rather than inheriting this one.
		{"claude-haiku-4-5", 200_000},
		{"claude-opus-4-1", 200_000},
		{"claude-sonnet-4-5", 200_000},
		// AND THE SAME IDS AS A GATEWAY ROUTES THEM. A provider prefix and a bracketed context-length
		// variant must not defeat the match — the variant suffix going unstripped was its own defect.
		{"aws/claude-opus-5", 1_000_000},
		{"aws/claude-sonnet-5", 1_000_000},
		{"aws/claude-haiku-4-5", 200_000},
		{"us.anthropic.claude-opus-5-v1:0", 1_000_000},
		{"claude-opus-5[1m]", 1_000_000},
		{"aws/claude-sonnet-5[1m]", 1_000_000},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got, ok := s.Window(ctx, tc.id)
			if !ok {
				t.Fatalf("%s resolved to nothing; every shipped id must at least hit its family floor",
					tc.id)
			}
			if got != tc.want {
				t.Errorf("%s => %d, want %d. A wrong window here is not a conservative floor: it is a "+
					"confident wrong number with ok=true, and the caller cannot tell",
					tc.id, got, tc.want)
			}
		})
	}
}

// LONGEST MATCH WINS, so the table's declaration order is cosmetic.
//
// It used to be first-match-wins with a comment asking the reader to keep the table "most-specific
// first" — an invariant held by a comment, which is an invariant that gets broken. Adding
// `{"claude-opus-5", 1000000}` after the `{"claude", 200000}` catch-all would have been a one-line
// change that silently did nothing at all, which is precisely how #233 would have recurred.
func TestTheStaticTablesOrderIsNotLoadBearing(t *testing.T) {
	ctx := context.Background()
	// The catch-all deliberately FIRST, and the specific entry last: the worst possible order.
	s := Static{table: []staticEntry{
		{"claude", 200_000},
		{"claude-opus-4", 200_000},
		{"claude-opus-5", 1_000_000},
	}}
	if got, ok := s.Window(ctx, "claude-opus-5"); !ok || got != 1_000_000 {
		t.Errorf("claude-opus-5 => %d,%v want 1000000: a more specific entry must win wherever it "+
			"sits in the slice, or the table's correctness depends on append order", got, ok)
	}
	// And the shorter entries still answer what they should.
	if got, _ := s.Window(ctx, "claude-opus-4-1"); got != 200_000 {
		t.Errorf("claude-opus-4-1 => %d, want 200000", got)
	}
	if got, _ := s.Window(ctx, "claude-haiku-4-5"); got != 200_000 {
		t.Errorf("claude-haiku-4-5 => %d, want the family catch-all 200000", got)
	}
}

// STILL NEVER EXACT, however right the entries are. Being correct about today's model list is not the
// same as being authoritative about tomorrow's, and that difference is the whole reason
// Trigger.FracResolvable refuses a figure from here.
func TestCorrectStaticEntriesAreStillNotExact(t *testing.T) {
	s := DefaultStatic()
	for _, id := range []string{"claude-opus-5", "claude-haiku-4-5", "aws/claude-sonnet-5"} {
		w, exact, ok := s.WindowExact(context.Background(), id)
		if !ok || w == 0 {
			t.Fatalf("%s resolved to nothing", id)
		}
		if exact {
			t.Errorf("%s reported exact=true from the static table. A fill percentage computed against "+
				"this would act on a guess, which is the defect #234 fixed by provenance rather than "+
				"by correcting values", id)
		}
	}
}
