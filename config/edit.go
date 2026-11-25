package config

// Typed editors of the config document (#1304). Every host that applies a tool-filter switch,
// a save-more tools option or an adapt-compaction option writes the document through these, so
// each host writes the same text. A host decodes the request, calls an editor and stores the
// result. It does not touch the document itself.
//
// An editor returns ErrUnloadable when the stored document does not load as fields. Any other
// error names an invalid value (the host answers it as a bad request).

import (
	"errors"
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/internal/skills"
)

// ErrUnloadable is the refusal to edit a stored document that does not load as fields.
var ErrUnloadable = errors.New("your stored configuration does not load, so it cannot be edited " +
	"as fields; a manager must repair it on the account page")

// ToolfilterComponent is the pipeline name that carries toolfilter's lists.
const ToolfilterComponent = "toolfilter"

// NameList is one of toolfilter's name lists as a form value, sorted. A Form holds a stated key
// as `any`, and yaml.v3 decodes a sequence into []any, so both shapes are read.
func NameList(v any) []string {
	var out []string
	switch list := v.(type) {
	case []string:
		out = append(out, list...)
	case []any:
		for _, v := range list {
			if s, isStr := v.(string); isStr && s != "" {
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ToolfilterRemoved reads toolfilter's removal list out of a stored configuration document.
// ok is false, with a reason, when the document does not load.
func ToolfilterRemoved(doc string) (names []string, ok bool, reason string) {
	f, err := ParseForm(doc)
	if err != nil || f.ParseError != "" {
		return nil, false, ErrUnloadable.Error()
	}
	return NameList(f.Components[ToolfilterComponent]["remove"]), true, ""
}

// ToolfilterLists reads toolfilter's `keep` and `always_defer` lists. Whether deferral is on
// is the host's to answer, so it is not read here.
func ToolfilterLists(doc string) (keep, always []string, ok bool, reason string) {
	f, err := ParseForm(doc)
	if err != nil || f.ParseError != "" {
		return nil, nil, false, ErrUnloadable.Error()
	}
	blk := f.Components[ToolfilterComponent]
	return NameList(blk["keep"]), NameList(blk["always_defer"]), true, ""
}

// ToolfilterDefer is toolfilter's deferral settings: `defer`, `keep` and `always_defer`.
type ToolfilterDefer struct {
	On           bool
	Keep, Always []string
}

// EditToolfilterRemoved is doc with toolfilter's removal list rewritten by edit. It adds
// toolfilter to the pipeline when absent, and never takes it out: an emptied list would
// otherwise write an explicit pipeline that differs from the default, for no gain. An account
// whose stored pipeline predates the default needs the add, or its first exclusion saves
// nothing. Every other key stays as it is.
func EditToolfilterRemoved(current string, edit func([]string) []string) (string, error) {
	f, perr := ParseForm(current)
	if perr != nil || f.ParseError != "" {
		return "", ErrUnloadable
	}
	names, _, _ := ToolfilterRemoved(current)
	names = edit(names)
	blk := toolfilterBlock(&f)
	blk["remove"] = names
	return applyToolfilter(current, &f)
}

// EditToolfilterDefer is doc with toolfilter's `defer`, `keep` and `always_defer` rewritten by
// edit. on is whether the document defers now, which the host decides and passes in. Like
// EditToolfilterRemoved it adds toolfilter to the pipeline when absent and leaves every other
// key alone. An empty always_defer list leaves no key behind.
func EditToolfilterDefer(current string, on bool, edit func(ToolfilterDefer) ToolfilterDefer) (string, error) {
	f, perr := ParseForm(current)
	if perr != nil || f.ParseError != "" {
		return "", ErrUnloadable
	}
	keep, always, _, _ := ToolfilterLists(current)
	c := edit(ToolfilterDefer{On: on, Keep: keep, Always: always})
	blk := toolfilterBlock(&f)
	blk["defer"] = "off"
	if c.On {
		blk["defer"] = "auto"
	}
	blk["keep"] = c.Keep
	if len(c.Always) > 0 {
		blk["always_defer"] = c.Always
	} else {
		delete(blk, "always_defer") // an empty list is the default: leave no key behind
	}
	return applyToolfilter(current, &f)
}

// toolfilterBlock is the form's toolfilter settings, created when absent.
func toolfilterBlock(f *Form) map[string]any {
	if f.Components == nil {
		f.Components = map[string]map[string]any{}
	}
	if f.Components[ToolfilterComponent] == nil {
		f.Components[ToolfilterComponent] = map[string]any{}
	}
	return f.Components[ToolfilterComponent]
}

// applyToolfilter writes f back with toolfilter in the pipeline. The route models exactly one
// component, so it claims exactly one. Without the claim ApplyForm keeps anything the stored
// pipeline runs, and an emptied list would not take toolfilter out (that is the intent).
// Position does not matter: the rewrite lives in apply, not in the component.
func applyToolfilter(current string, f *Form) (string, error) {
	f.Pipeline = WithComponent(f.Pipeline, ToolfilterComponent, true)
	f.PipelineKnown = []string{ToolfilterComponent}
	return ApplyForm(current, *f)
}

// RemovalNote says what the removal list now does, read back from the configuration that was
// written, so it never claims a change that did not happen: a restore cannot take out a name
// the configuration still lists (an operator's config file, say), and the note names those.
func RemovalNote(names []string, doc string, restore bool) string {
	after, _, _ := ToolfilterRemoved(doc)
	still := map[string]bool{}
	for _, n := range after {
		still[n] = true
	}
	var done, kept []string
	for _, n := range names {
		if still[n] == restore {
			kept = append(kept, n)
		} else {
			done = append(done, n)
		}
	}
	if !restore {
		return "filtering " + strings.Join(done, ", ")
	}
	note := "stopped filtering " + strings.Join(done, ", ")
	if len(done) == 0 {
		note = "stopped filtering nothing"
	}
	if len(kept) > 0 {
		note += "; still filtered because your configuration lists them: " + strings.Join(kept, ", ")
	}
	return note
}

// The declaration kinds an inventory row can carry. They are stored as text and read back as
// labels, so dash's Kind* constants alias these.
const (
	KindTool         = "tool"
	KindMCPTool      = "mcp_tool"
	KindServerTool   = "server_tool"
	KindSkill        = "skill"
	KindSkillListing = "skill_listing"
)

// DeclConfigName maps an inventory row to the string the removal list holds, and refuses the
// kinds that are not removable.
func DeclConfigName(kind, name, server string) (string, error) {
	switch kind {
	case "mcp_server":
		if server == "" {
			return "", errors.New("an MCP server needs a server name")
		}
		return "mcp__" + server, nil
	case KindServerTool:
		return "", errors.New("a provider-side tool is resolved by the provider from its " +
			"type, not by a schema we send, so it cannot be removed here")
	case KindSkill:
		if name == "" {
			return "", errors.New("no skill name")
		}
		if !skills.ValidName(name) {
			// The listing is the authority for what may be cut out of a real request, so a name
			// that could not have come from one is refused rather than written into a config that
			// would match nothing forever.
			return "", errors.New("that is not a skill name")
		}
		return skills.RemovePrefix + name, nil
	case KindSkillListing:
		return "", errors.New("the skills listing is one indivisible block of prose — remove the " +
			"individual skills instead, and the listing shrinks with them")
	case KindTool, KindMCPTool, "":
		if name == "" {
			return "", errors.New("no declaration name")
		}
		return name, nil
	}
	return "", errors.New("unknown declaration kind " + kind)
}

// WithName adds or removes one name, keeping the list sorted and duplicate-free so the stored
// document does not churn on a no-op change.
func WithName(names []string, name string, add bool) []string {
	out := make([]string, 0, len(names)+1)
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	if add {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// WithComponent ensures a component is present (appended last) or absent from a pipeline.
// A present component keeps its position: pipeline order decides what the components produce
// and so what is cached, and adding a name to the list must not reorder anything.
func WithComponent(pipeline []string, name string, want bool) []string {
	if want {
		for _, n := range pipeline {
			if strings.TrimSpace(n) == name {
				return pipeline
			}
		}
	}
	out := make([]string, 0, len(pipeline)+1)
	for _, n := range pipeline {
		if strings.TrimSpace(n) != name {
			out = append(out, n)
		}
	}
	if want {
		out = append(out, name)
	}
	return out
}

// --- adapt compaction ---------------------------------------------------------

// adaptCandidates are the components the adapt-compaction search may add, drop, reorder or
// tune: the shipped deterministic compaction components, in the default pipeline's order.
// Everything else in an account's pipeline keeps its place.
var adaptCandidates = []string{"format", "dedup", "toon", "cmdfilter", "searchfold", "textclean", "extract"}

// adaptKnobs are the settings the search varies, and the only settings the apply route accepts
// from a client. Values are the grid tried.
var adaptKnobs = map[string]map[string][]int{
	"extract": {"min_tokens": {200, 300, 400, 800}},
	"dedup":   {"min_tokens": {50, 100, 200}},
}

// AdaptCandidates returns a copy of the components the search may change, in the default
// pipeline's order. A caller that edits the copy does not change the next answer.
func AdaptCandidates() []string { return append([]string(nil), adaptCandidates...) }

// AdaptKnobGrid returns a copy of the settings the search varies and the values tried.
func AdaptKnobGrid() map[string]map[string][]int {
	out := make(map[string]map[string][]int, len(adaptKnobs))
	for c, knobs := range adaptKnobs {
		out[c] = make(map[string][]int, len(knobs))
		for k, vals := range knobs {
			out[c][k] = append([]int(nil), vals...)
		}
	}
	return out
}

// IsAdaptCandidate reports whether name is one of the adapt candidates.
func IsAdaptCandidate(name string) bool {
	for _, c := range adaptCandidates {
		if c == name {
			return true
		}
	}
	return false
}

// AdaptKnobAllowed reports whether comp.key=v is a setting the apply route may store.
func AdaptKnobAllowed(comp, key string, v int) bool {
	for _, x := range adaptKnobs[comp][key] {
		if x == v {
			return true
		}
	}
	return false
}

// AdaptMerge returns the pipeline to store: cands in their new order, and every other
// component of cur kept right after the candidate it followed in cur (walking back past
// candidates the new order drops), so cachesplit stays after the offloaders and an LLM
// component stays where its owner put it relative to what remains.
func AdaptMerge(cur, cands []string) []string {
	keep := map[string]bool{}
	for _, c := range cands {
		keep[c] = true
	}
	after := map[string][]string{} // surviving candidate ("" = head) -> fixed comps after it
	anchor := ""
	for _, c := range cur {
		if IsAdaptCandidate(c) {
			if keep[c] {
				anchor = c
			}
			continue
		}
		after[anchor] = append(after[anchor], c)
	}
	out := append([]string(nil), after[""]...)
	for _, c := range cands {
		out = append(out, c)
		out = append(out, after[c]...)
	}
	return out
}

// ErrNotAdaptable is the refusal of a setting outside the searched grid.
type ErrNotAdaptable struct{ Component, Key string }

func (e ErrNotAdaptable) Error() string {
	return "not an adaptable setting: " + e.Component + "." + e.Key
}

// EditAdaptCompaction is doc with an adapt-compaction choice applied: the candidates replaced
// in place, the searched settings set, nothing else touched. It returns the stored pipeline too.
func EditAdaptCompaction(doc string, candidates []string, settings map[string]map[string]int) (string, []string, error) {
	f, err := ParseForm(doc)
	if err != nil || f.ParseError != "" {
		return "", nil, ErrUnloadable
	}
	for c, knobs := range settings {
		for k, v := range knobs {
			// The grid the report searched, or the value already stored.
			if stored, ok := f.Components[c][k].(int); !AdaptKnobAllowed(c, k, v) && (!ok || stored != v) {
				return "", nil, ErrNotAdaptable{c, k}
			}
		}
	}
	base := f.Pipeline
	f.Pipeline = AdaptMerge(f.Pipeline, candidates)
	// The candidates are the only names this may take out; ApplyForm keeps anything else.
	f.PipelineKnown, f.PipelineBase = AdaptCandidates(), base
	// Nor does it state the cache block: a parsed form carries one, and ApplyForm would write
	// `keepalive: false` into a document that never said it, pinning the account against a
	// default that later changes.
	f.Cache = nil
	if f.Components == nil {
		f.Components = map[string]map[string]any{}
	}
	for c, knobs := range settings {
		if f.Components[c] == nil {
			f.Components[c] = map[string]any{}
		}
		for k, v := range knobs {
			f.Components[c][k] = v
		}
	}
	next, err := ApplyForm(doc, f)
	if err != nil {
		return "", nil, err
	}
	return next, f.Pipeline, nil
}
