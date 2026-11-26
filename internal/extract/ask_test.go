package extract

import (
	"strings"
	"testing"
)

// fakeList stands in for the lists #490 and #485 will add. It lives only in this test: no second
// list is built in this package.
type fakeList struct {
	key   string
	items []AdjudicationItem
}

func (f fakeList) Key() string                    { return f.key }
func (f fakeList) Candidates() []AdjudicationItem { return f.items }
func (f fakeList) Render(it []AdjudicationItem) string {
	var b strings.Builder
	b.WriteString("FAKE LIST " + f.key + ":")
	for _, i := range it {
		b.WriteString(" [")
		b.WriteString(string(rune('0' + i.Label)))
		b.WriteString("]")
	}
	return b.String()
}

func spentItems() []AdjudicationItem {
	return []AdjudicationItem{
		{ID: "toolu_a", SizeTokens: 1200, Head: "a"},
		{ID: "toolu_b", SizeTokens: 3000, Head: "b"},
	}
}

func TestOneListPromptIsBuildPrefixAskByteForByte(t *testing.T) {
	a := NewAsk()
	a.Add(SpentOutputs{Items: spentItems()})
	labelled := []AdjudicationItem{{Label: 0, ID: "toolu_a", SizeTokens: 1200, Head: "a"},
		{Label: 1, ID: "toolu_b", SizeTokens: 3000, Head: "b"}}
	if got, want := a.Prompt(), BuildPrefixAsk(labelled); got != want {
		t.Fatalf("a one-list question must equal the single-list prompt\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(a.Prompt(), "ONE REPLY FOR ALL THE LISTS") {
		t.Fatal("the one-reply rule belongs to a question with several lists only")
	}
}

func TestAnOutputInTwoListsIsJudgedOnce(t *testing.T) {
	a := NewAsk()
	a.Add(SpentOutputs{Items: spentItems()})
	deduped := a.Add(fakeList{key: "repeated_calls", items: []AdjudicationItem{
		{ID: "toolu_b"}, // already in the first list
		{ID: "toolu_c"},
	}})
	if deduped != 1 {
		t.Fatalf("the second list must lose exactly the shared output, lost %d", deduped)
	}
	if got := len(a.Items("repeated_calls")); got != 1 {
		t.Fatalf("second list keeps %d items, want 1", got)
	}
	if got := len(a.Items(SpentOutputsKey)); got != 2 {
		t.Fatalf("first list keeps %d items, want 2: the first list to add an output keeps it", got)
	}
}

func TestLabelsAreOneSequenceAndOwnersAreRecorded(t *testing.T) {
	a := NewAsk()
	a.Add(SpentOutputs{Items: spentItems()})
	a.Add(fakeList{key: "repeated_calls", items: []AdjudicationItem{{ID: "toolu_c"}}})
	for label, want := range map[int]string{0: SpentOutputsKey, 1: SpentOutputsKey, 2: "repeated_calls"} {
		if got, ok := a.Owner(label); !ok || got != want {
			t.Fatalf("label %d owned by %q (ok=%v), want %q", label, got, ok, want)
		}
	}
	if _, ok := a.Owner(9); ok {
		t.Fatal("a label no list carries must have no owner")
	}
	split := a.Split([]Verdict{{Label: 0, Verdict: "drop"}, {Label: 2, Verdict: "keep"}, {Label: 9}})
	if len(split[SpentOutputsKey]) != 1 || len(split["repeated_calls"]) != 1 || len(split[""]) != 1 {
		t.Fatalf("answers must route to the owning list, unknown labels under the empty key: %+v", split)
	}
	if !strings.Contains(a.Prompt(), "ONE REPLY FOR ALL THE LISTS") ||
		!strings.Contains(a.Prompt(), "FAKE LIST repeated_calls: [2]") {
		t.Fatalf("a several-list question carries every section and the one-reply rule:\n%s", a.Prompt())
	}
}

func TestAnItemWithNoIDIsNeverDeduped(t *testing.T) {
	a := NewAsk()
	a.Add(fakeList{key: "x", items: []AdjudicationItem{{}, {}}})
	if got := a.Add(fakeList{key: "y", items: []AdjudicationItem{{}}}); got != 0 {
		t.Fatalf("an item with no id cannot be matched, deduped %d", got)
	}
}

func TestAListLeftWithNothingIsNotAdded(t *testing.T) {
	a := NewAsk()
	a.Add(fakeList{key: "x", items: []AdjudicationItem{{ID: "t1"}}})
	a.Add(fakeList{key: "y", items: []AdjudicationItem{{ID: "t1"}}})
	if a.Items("y") != nil {
		t.Fatal("a list with every item deduped must not leave an empty section")
	}
	if strings.Contains(a.Prompt(), "ONE REPLY FOR ALL THE LISTS") {
		t.Fatal("one surviving list is a one-list question")
	}
}
