package metrics

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// TestDiscardedRewriteIsNotReportedAsSaved reproduces the writeback discard that /stats
// used to publish as a real saving: cmdfilter shrinks an OpenAI role=tool message
// 500 -> 50, bifrost cannot round-trip it, so apply keeps the ORIGINAL bytes and the
// wire is byte-identical to the input. The Discarded counter existed to make that
// visible (#32) but nothing decremented the saving it invalidated.
func TestDiscardedRewriteIsNotReportedAsSaved(t *testing.T) {
	a := NewAggregator()
	a.Run(components.RunReport{TokensBefore: 500, TokensAfter: 50})
	a.Component(components.Report{Component: "cmdfilter", Kind: "offload",
		TokensBefore: 500, TokensAfter: 50, ChangedIdx: []int{0}})
	// ... and the writeback layer threw that one change away.
	a.Component(components.Report{Component: "cmdfilter", Kind: "offload", Discarded: 1})

	s := a.Snapshot()
	cs := s.Components["cmdfilter"]
	if cs.Discarded != 1 {
		t.Fatalf("discarded=%d want 1", cs.Discarded)
	}
	if cs.Saved != 0 {
		t.Errorf("saved_tokens=%d want 0: the change never reached the wire", cs.Saved)
	}
	if cs.SavedUnique != 0 {
		t.Errorf("saved_tokens_unique=%d want 0: the change never reached the wire", cs.SavedUnique)
	}
	if s.SavedTokens != 0 {
		t.Errorf("/stats saved_tokens=%d want 0", s.SavedTokens)
	}
}

// TestDiscardedSavingIsReversedOnce: two messages discarded in ONE request must not
// subtract the component's saving twice (it would report a negative saving), and a
// partial discard reverses only its share.
func TestDiscardedSavingIsReversedOnce(t *testing.T) {
	a := NewAggregator()
	a.Component(components.Report{Component: "dedup", TokensBefore: 200, TokensAfter: 100,
		ChangedIdx: []int{0, 1}})
	a.Component(components.Report{Component: "dedup", Discarded: 2})
	if got := a.Snapshot().Components["dedup"].Saved; got != 0 {
		t.Fatalf("saved=%d want 0 (not double-subtracted)", got)
	}

	b := NewAggregator()
	b.Component(components.Report{Component: "dedup", TokensBefore: 200, TokensAfter: 100,
		ChangedIdx: []int{0, 1}})
	b.Component(components.Report{Component: "dedup", Discarded: 1}) // only one of the two
	if got := b.Snapshot().Components["dedup"].Saved; got != 50 {
		t.Fatalf("saved=%d want 50 (half of 100 discarded)", got)
	}
}
