package extract

import "strings"

// ONE QUESTION, MANY LISTS. The sweep asks its model one question over the cached conversation, at
// the keep-alive ping. Today the question carries one list: tool outputs that may be spent. #490
// (repeated tool calls) and #485 (unused tool groups) each want to put a second list in the SAME
// question, because the cache read is the expensive part and a second question would pay it twice.
//
// This file is the seam they plug into. A new list is its own type with its own Render; nothing here
// knows what it asks. The rules that hold for every list are the only code in this file:
//
//   - LABELS ARE ONE SEQUENCE across all lists, so a verdict's `i` names exactly one item. The
//     answer stays one JSON array of objects, each carrying `i`; Owner says which list a label
//     belongs to, and the list reads the rest of the object itself.
//   - AN OUTPUT IN TWO LISTS IS JUDGED ONCE. The first list to add an output keeps it; a later list
//     loses it and the loss is counted. Judging one output twice would pay for two answers that can
//     disagree.
//
// The spent-outputs list is the first and only list built here. Do not add another list in this
// package: it belongs with the code that decides what its items are.

// AskList is one list of items in a prefix-ask question.
type AskList interface {
	// Key names the list in logs and in held answers. Stable, lower-case, no spaces.
	Key() string
	// Candidates are the items the list wants judged, with Label unset. Ask assigns labels.
	Candidates() []AdjudicationItem
	// Render writes the list's section of the question for the items that survived dedupe, labels
	// already assigned.
	Render(items []AdjudicationItem) string
}

// SpentOutputsKey is the key of the list that asks whether a tool output is spent.
const SpentOutputsKey = "spent_outputs"

// SpentOutputs is the list the sweep has always asked: could the work go on without this output?
type SpentOutputs struct{ Items []AdjudicationItem }

func (SpentOutputs) Key() string                         { return SpentOutputsKey }
func (s SpentOutputs) Candidates() []AdjudicationItem    { return s.Items }
func (SpentOutputs) Render(it []AdjudicationItem) string { return BuildPrefixAsk(it) }

// multiListReply is added once the question carries more than one list. Without it each list's
// section would be read as its own "reply with ONLY a JSON array" and the model would answer twice.
const multiListReply = `

ONE REPLY FOR ALL THE LISTS ABOVE. Answer in a single JSON array. Each answer is one object that
carries the label "i" shown in front of its item. Do not write a second array.`

type askEntry struct {
	list  AskList
	items []AdjudicationItem
}

// Ask is one question being composed. The zero value is ready to use.
type Ask struct {
	entries []askEntry
	next    int
	seen    map[string]struct{}
	owner   map[int]string
}

// NewAsk returns an empty question.
func NewAsk() *Ask { return &Ask{} }

// Add puts a list in the question and returns how many of its items it lost because an earlier list
// already carries the same output (matched by tool_use id; an item with no id is never matched).
// A list whose items all survive is added whole. A list left with no items is not added at all, so
// the question never carries an empty section.
func (a *Ask) Add(l AskList) (deduped int) {
	if a.seen == nil {
		a.seen = map[string]struct{}{}
		a.owner = map[int]string{}
	}
	var keep []AdjudicationItem
	var ids []string
	for _, it := range l.Candidates() {
		// Against EARLIER lists only. Two items of one list are that list's own business; dropping
		// one here would shift the labels the list maps back to its candidates by position.
		if it.ID != "" {
			if _, dup := a.seen[it.ID]; dup {
				deduped++
				continue
			}
			ids = append(ids, it.ID)
		}
		it.Label = a.next
		a.next++
		a.owner[it.Label] = l.Key()
		keep = append(keep, it)
	}
	for _, id := range ids {
		a.seen[id] = struct{}{}
	}
	if len(keep) > 0 {
		a.entries = append(a.entries, askEntry{list: l, items: keep})
	}
	return deduped
}

// Empty reports whether no list has any item.
func (a *Ask) Empty() bool { return len(a.entries) == 0 }

// Items returns the items of one list with their labels, or nil.
func (a *Ask) Items(key string) []AdjudicationItem {
	for _, e := range a.entries {
		if e.list.Key() == key {
			return e.items
		}
	}
	return nil
}

// Owner names the list a label belongs to.
func (a *Ask) Owner(label int) (key string, ok bool) {
	key, ok = a.owner[label]
	return key, ok
}

// Prompt renders the question. With one list it is that list's own text, byte for byte.
func (a *Ask) Prompt() string {
	parts := make([]string, 0, len(a.entries))
	for _, e := range a.entries {
		parts = append(parts, e.list.Render(e.items))
	}
	out := strings.Join(parts, "\n\n")
	if len(a.entries) > 1 {
		out += multiListReply
	}
	return out
}

// Split groups the model's answers by the list that owns each label. An answer whose label no list
// owns is returned under the empty key, so the caller can count it rather than lose it.
func (a *Ask) Split(vs []Verdict) map[string][]Verdict {
	out := map[string][]Verdict{}
	for _, v := range vs {
		key, _ := a.Owner(v.Label)
		out[key] = append(out[key], v)
	}
	return out
}
