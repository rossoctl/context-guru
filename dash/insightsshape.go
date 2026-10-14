package dash

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// The shaping layer: the step between "what is declared and what was used" and "what should you do
// about it". Those are different kinds of claim, and the second is far easier to get subtly wrong —
// a verdict stronger than its evidence, a removal naming a setting the mechanism ignores, a
// subtotal that double-counts, or this feature recommending its own removal. Each produces a report
// that still reads correctly, which is why every rule below is pinned by a test.
//
// WHY THIS EXISTS IN GO AS WELL AS IN PYTHON. The same rules live in the plugin's insights.py,
// which serves the static report. The browser cannot read SKILL.md off disk and cannot be given the
// honesty rules without putting them beyond the reach of a test harness, so a SERVED page needs its
// shaping done here. The duplication is the known hazard; it is the PRESET_PIPELINES situation
// again, and it is tolerable only while a test pins the two implementations against each other.

// The scope at which a capability can actually be removed. Not a grouping label: these are four
// different commands with four different blast radii, and picking the wrong one produces an
// instruction that silently does nothing.
const (
	// ScopeItem — removable on its own: a standalone skill, a client tool, one MCP tool by name.
	ScopeItem = "item"
	// ScopeServer — the MCP server is the unit. Denying one tool leaves the rest declared.
	ScopeServer = "server"
	// ScopePlugin — the PLUGIN is the unit. `skillOverrides` has no effect on a plugin's skills,
	// verified on real request traffic: the entry was still sent with the override set, while the
	// same override removed a standalone skill entirely. A per-skill instruction here is a line
	// the user pastes that changes nothing, which is worse than no instruction.
	ScopePlugin = "plugin"
	// ScopeNone — not offered at any scope. Claude Code's own built-ins: the weight is real and
	// removing one takes away a capability the model is expected to have.
	ScopeNone = "none"
)

// What the reader may be told to do about an item.
const (
	// VerdictRemove — unused, removable, and observed long enough to be sure.
	VerdictRemove = "remove"
	// VerdictConsider — unused and removable, but not yet observed long enough. Listed, because
	// the floor gates ADVICE and not disclosure; refusing to recommend is honest, refusing to say
	// what is there is merely unhelpful.
	VerdictConsider = "consider"
	// VerdictKeep — in use, or a built-in. Two different facts, worded apart.
	VerdictKeep = "keep"
	// VerdictStale — it WAS used, it is not any more, and it is still carried on every request.
	//
	// The state the report could not express. A user working three months in one project —
	// planning, then development, now research — has planning skills used heavily in month one
	// and dead since month two. `calls > 0` filed them under keep, alongside a tool invoked a
	// minute ago, so the report never mentioned them.
	//
	// Deliberately NOT solved with a session window. A session is not a unit of time: measured on
	// one real store, spans were 0.0h, 1.1h and 99.1h, with a single session covering four days,
	// so "the last N sessions" is anywhere from minutes to weeks and "the last N days" anywhere
	// from two sessions to sixty. The gap between last-used and last-carried is a property of the
	// item instead, independent of how anybody structures their work, and it is evidence a reader
	// can check against their own memory.
	VerdictStale = "stale"
)

// StaleAfter is how long a capability must go unused, while still being carried, before this
// report will call it abandoned.
//
// Fourteen days, and the number is a judgement rather than a measurement — so it is one constant
// with its reasoning attached rather than a threshold scattered through the code. Shorter would
// catch a fortnight's holiday or a two-week spell on one part of a codebase, both of which are
// normal and neither of which means a tool is finished with. Much longer and a project that
// changed phase a month ago still shows its old tooling as current, which is the complaint this
// exists to answer.
//
// It is a floor on the EVIDENCE, not a window on the data: every use is counted however old, and
// this only decides when a gap is long enough to report.
const StaleAfter = 14 * 24 * time.Hour

// selfPlugin is this plugin's own name. Held as a constant because it is the one string whose job
// is keeping the reporter out of its own report.
const selfPlugin = "context-guru"

// removalScope maps a Removal.Kind to the scope at which that mechanism actually operates.
var removalScope = map[string]string{
	"plugin_skill": ScopePlugin,
	"mcp_server":   ScopeServer,
	"mcp_tool":     ScopeServer,
	"skill":        ScopeItem,
	"client_tool":  ScopeItem,
	"builtin":      ScopeNone,
}

// InsightsItem is one declared capability, with what it cost and what removing it would mean.
type InsightsItem struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	Plugin string `json:"plugin,omitempty"`
	Tokens int    `json:"tokens"`
	// Reads is billed tokens spent carrying it in sessions that never invoked it.
	Reads    int64   `json:"reads"`
	USD      float64 `json:"usd"`
	Priced   bool    `json:"priced"`
	Invoked  bool    `json:"invoked"`
	Calls    int     `json:"calls,omitempty"`
	Scope    string  `json:"scope"`
	Verdict  string  `json:"verdict"`
	Why      string  `json:"why"`
	Command  string  `json:"command,omitempty"`
	Settings string  `json:"settings,omitempty"`
	// LastUsedTS / DeadDays: when it was last reached for, and how long it has been carried since.
	// The evidence behind a stale verdict, and a figure a reader can check against their memory.
	LastUsedTS int64   `json:"last_used_ts,omitempty"`
	DeadDays   float64 `json:"dead_days,omitempty"`
	Desc       string  `json:"desc,omitempty"`
	DescFull   string  `json:"desc_full,omitempty"`
	DescKnown  bool    `json:"desc_known"`
}

// InsightsOwner is the thing that removes a group of items, with its own subtotal.
//
// A VIEW, NOT AN ADDEND. These subtotals re-count tokens already counted per item, one level up.
// Summing them double-counts; adding them to RemovableUSD double-counts differently. RemovableUSD
// is computed over items, each counted once, and nothing here contributes to it.
type InsightsOwner struct {
	// Kind is "plugin" or "server". Kept apart rather than merged on the name, because
	// `mcp__giveinfo__info` and `giveinfo:blah` can share a word with no relationship at all.
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	Items    int     `json:"items"`
	Skills   int     `json:"skills,omitempty"`
	MCPTools int     `json:"mcp_tools,omitempty"`
	Tokens   int     `json:"tokens"`
	USD      float64 `json:"usd"`
	// UnpricedItems counts children whose models have no known rate, so a subtotal short by an
	// unknown amount says so rather than being presented as complete.
	UnpricedItems int    `json:"unpriced_items,omitempty"`
	Command       string `json:"command,omitempty"`
}

// SelfExclusion is this feature's own weight, held out of the tables and reported separately.
type SelfExclusion struct {
	Plugin  string  `json:"plugin,omitempty"`
	Items   int     `json:"items"`
	Tokens  int     `json:"tokens"`
	USD     float64 `json:"usd"`
	Command string  `json:"command,omitempty"`
}

// InsightsDoc is the shaped document a page renders.
type InsightsDoc struct {
	Items  []InsightsItem  `json:"items"`
	Owners []InsightsOwner `json:"owners"`
	// RemovableTokens / RemovableUSD are the per-request weight and the measured cost of the
	// items that can actually be removed. Deliberately NOT the inventory's headline figure, which
	// excludes built-ins entirely: this one excludes them from the OFFER and still lists them.
	RemovableTokens int     `json:"removable_tokens"`
	RemovableUSD    float64 `json:"removable_usd"`
	Basis           string  `json:"basis"`
	// Project is the folder this document describes, or "" for every project pooled. Echoed back
	// rather than left to the caller to remember: every figure below means something different
	// depending on it, and a narrowed document that looks identical to a pooled one is the
	// misreading this surface must not invite.
	Project string `json:"project,omitempty"`
	// Timeline is the per-item picture, present only when the caller asked for it. Attached rather
	// than merged into Items because the two series are aligned to ONE shared bucket list: a copy
	// of the axis on every row would be the same array sixty-eight times.
	Timeline     *ToolTimeline `json:"timeline,omitempty"`
	SelfExcluded SelfExclusion `json:"self_excluded"`
}

// InsightsOptions carries what shaping cannot derive from the inventory itself.
type InsightsOptions struct {
	// Qualified names the items the SERVER's own floor has cleared for removal, keyed by item
	// name. Supplied rather than recomputed because an inventory row carries no dates at all: a
	// local check could only test the session count, which passes for five sessions in one
	// afternoon — the exact case the seven-day span exists to reject.
	Qualified map[string]bool
	// Descriptions maps an item name to what it does, read from its own SKILL.md. Absent is
	// absent: a description this layer invented would be indistinguishable from one it read.
	Descriptions map[string]string
	// Project is the folder being reported on, or "" for every project pooled. It changes the
	// BASIS rather than any arithmetic — the rows are already scoped by the caller's filter — so
	// that a narrowed document says so instead of looking like an all-projects one.
	Project string
	// ProjectNone says the rows are the UNATTRIBUTED ones — requests that stated no directory,
	// plus every row written before the column existed. A separate flag for the same reason the
	// Filter needs one: the project being reported on is "", which as a value is indistinguishable
	// from "every project pooled", and the two need different wording.
	ProjectNone bool
}

// ShapeInsights turns an inventory into the document a page renders.
func ShapeInsights(rep *ToolReport, opt InsightsOptions) *InsightsDoc {
	now := time.Now().UnixMilli()
	doc := &InsightsDoc{Items: []InsightsItem{}, Owners: []InsightsOwner{},
		Basis: "measured over the window", Project: opt.Project}
	// SAID OUT LOUD, because per-project evidence is thinner than pooled evidence and the reader
	// has to know which they are reading. A verdict in one project rests on that folder's sessions
	// alone, so an item used daily elsewhere can legitimately appear unused here — true, and
	// dangerously misleading if the scope is not stated.
	switch {
	case opt.ProjectNone:
		doc.Basis = "measured over the window, across requests that named no project"
	case opt.Project != "":
		doc.Basis = "measured over the window, in this project only"
	}
	if rep == nil {
		return doc
	}

	rows := make([]ToolStat, 0, len(rep.Tools)+len(rep.Skills.Skills)+len(rep.Aside))
	rows = append(rows, rep.Tools...)
	rows = append(rows, rep.Skills.Skills...)
	// Aside is Claude Code's own tools, held apart by the inventory because they are the heaviest
	// group and removing one breaks the agent. They belong in a listing — hiding them understates
	// what the prompt costs — but never in an offer.
	rows = append(rows, rep.Aside...)

	plugins := map[string]*InsightsOwner{}
	servers := map[string]*InsightsOwner{}

	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		// This plugin's own skills never reach the tables.
		if strings.HasPrefix(r.Name, selfPlugin+":") {
			if r.SessionsUsed > 0 || r.Calls > 0 {
				continue // an invoked one was never in the inventory to exclude
			}
			doc.SelfExcluded.Plugin = selfPlugin
			doc.SelfExcluded.Items++
			doc.SelfExcluded.Tokens += r.Tokens
			if r.Priced {
				doc.SelfExcluded.USD += r.UnusedUSD
			}
			if doc.SelfExcluded.Command == "" {
				doc.SelfExcluded.Command = r.Removal.Command
			}
			continue
		}

		invoked := r.SessionsUsed > 0 || r.Calls > 0
		// STALE SITS BETWEEN THE TWO EXISTING STATES. It was invoked, so it is not "never used";
		// it has not been invoked for long enough that carrying it is now waste, so it is not
		// "in use" either. It is reported with its cost columns BECAUSE it is removable — which
		// is the whole difference from an item still earning its weight.
		stale, deadDays := staleness(r, now)
		scope := ScopeNone
		if !r.Builtin && !r.Removal.Danger {
			if s, ok := removalScope[r.Removal.Kind]; ok {
				scope = s
			} else {
				// An unknown mechanism is not a licence to guess at a command.
				scope = ScopeItem
			}
		}

		it := InsightsItem{
			Kind: r.Kind, Name: r.Name, Server: r.Server, Invoked: invoked, Calls: r.Calls,
		}
		if invoked && !stale {
			// AN INVOKED ITEM CARRIES NO COST COLUMNS, and that is not a gap. Every cost field
			// here means "spent carrying this for nothing": unused reads, the dollars they cost,
			// the weight that bought nothing. None of those figures exists for a capability that
			// was used, and filling them with its declared weight would put a real-looking price
			// on something that earned its place — the inventory would then be wrong about the
			// one thing it claims to measure.
			//
			// It carries a verdict and a reason, because "why is this not on the list" is a fair
			// question, and no scope, because removal was never on offer. It DOES carry
			// last_used: "in use" is a claim, and the date behind it is what makes the claim
			// checkable — the same reason a stale verdict names one.
			it.LastUsedTS = r.LastUsedTS
			it.Verdict, it.Why = verdictFor(r, ScopeNone, true, false)
			doc.Items = append(doc.Items, it)
			continue
		}
		it.Tokens, it.Reads, it.Priced = r.Tokens, r.UnusedReads, r.Priced
		it.Scope, it.Command, it.Settings = scope, r.Removal.Command, r.Removal.Settings
		if r.Priced {
			it.USD = r.UnusedUSD
		}
		if scope == ScopePlugin {
			if p, _, ok := splitPluginSkill(r.Name); ok {
				it.Plugin = p
			}
		}
		if d, ok := opt.Descriptions[r.Name]; ok && d != "" {
			it.DescFull, it.Desc, it.DescKnown = d, summarizeDesc(d), true
		}
		it.LastUsedTS, it.DeadDays = r.LastUsedTS, deadDays
		if stale {
			it.Verdict, it.Why = VerdictStale, staleWhy(r, deadDays)
		} else {
			it.Verdict, it.Why = verdictFor(r, scope, false, opt.Qualified[r.Name])
		}
		doc.Items = append(doc.Items, it)

		if scope == ScopeNone {
			continue
		}
		doc.RemovableTokens += r.Tokens
		if r.Priced {
			doc.RemovableUSD += r.UnusedUSD
		}

		// Owners. An item belongs to one only where its own mechanism says so: deriving a plugin
		// from a colon in the name would mint a phantom owner for any skill containing one, and
		// hand the reader a disable command for something that does not exist.
		switch scope {
		case ScopePlugin:
			if it.Plugin == "" {
				break
			}
			o := plugins[it.Plugin]
			if o == nil {
				o = &InsightsOwner{Kind: "plugin", Name: it.Plugin}
				plugins[it.Plugin] = o
			}
			if r.Kind == KindSkill {
				o.Skills++
			} else {
				o.MCPTools++
			}
			addChild(o, r)
		case ScopeServer:
			if r.Server == "" {
				break
			}
			o := servers[r.Server]
			if o == nil {
				o = &InsightsOwner{Kind: "server", Name: r.Server}
				servers[r.Server] = o
			}
			addChild(o, r)
		}
	}

	for _, m := range []map[string]*InsightsOwner{plugins, servers} {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			doc.Owners = append(doc.Owners, *m[n])
		}
	}
	return doc
}

func addChild(o *InsightsOwner, r ToolStat) {
	o.Items++
	o.Tokens += r.Tokens
	if r.Priced {
		o.USD += r.UnusedUSD
	} else {
		o.UnpricedItems++
	}
	if o.Command == "" {
		o.Command = r.Removal.Command
	}
}

// verdictFor decides what the reader may be told, and says why. The `why` is not decoration: a
// verdict with no stated evidence is an assertion, and this report's whole claim is that every
// line traces to a measurement.
func verdictFor(r ToolStat, scope string, invoked, qualified bool) (string, string) {
	switch {
	case invoked:
		// Kept because it EARNED its place, which is a different fact from a built-in's.
		return VerdictKeep, "invoked in " + plural(r.SessionsUsed, "session") +
			" — it earns the weight it carries"
	case scope == ScopeNone:
		return VerdictKeep, "one of Claude Code's own tools: the weight is real, and removing it " +
			"takes away a capability the model is expected to have"
	case qualified:
		return VerdictRemove, "declared in " + plural(r.SessionsDeclared, "session") +
			" and invoked in none, across enough sessions and days to be sure"
	default:
		return VerdictConsider, "never invoked, but only " +
			plural(r.SessionsDeclared, "session") + " of evidence — below the floor, and " +
			"sessions on one task say nothing about the next one"
	}
}

func plural(n int, word string) string {
	s := strconv.Itoa(n) + " " + word
	if n != 1 {
		s += "s"
	}
	return s
}

// summarizeDesc keeps the leading sentences of a description, up to a length a reader will scan.
// A skill's description is a TRIGGER written for a model — "use this when the user says X" — and
// its informative part is almost always the front.
func summarizeDesc(d string) string {
	const budget = 200
	text := strings.Join(strings.Fields(d), " ")
	if text == "" {
		return ""
	}
	for _, marker := range []string{"Triggers on:", "TRIGGER —", "Trigger:", "Use when"} {
		if cut := strings.Index(text, marker); cut > 40 {
			text = strings.TrimSpace(text[:cut])
			break
		}
	}
	if len(text) <= budget {
		return text
	}
	// Cut at a word, and mark it: an invisible truncation reads as the author's own full stop.
	if i := strings.LastIndex(text[:budget], " "); i > 0 {
		return text[:i] + "…"
	}
	return text[:budget] + "…"
}

// staleness decides whether a capability has been abandoned, and for how long.
//
// Both timestamps are required. Without LastUsedTS the item was never invoked, which is a
// different verdict entirely; without LastCarriedTS there is nothing to measure the gap against.
// A built-in is never stale: the question is academic for something that cannot be removed.
func staleness(r ToolStat, now int64) (bool, float64) {
	if r.LastUsedTS == 0 || r.LastCarriedTS == 0 || r.Builtin || r.Removal.Danger {
		return false, 0
	}
	// The gap is measured from the last USE to the last time it was CARRIED, not to now: a report
	// read a week after the traffic stopped would otherwise accuse every capability of going
	// stale during the gap in which nothing was happening at all.
	gap := r.LastCarriedTS - r.LastUsedTS
	if gap < int64(StaleAfter/time.Millisecond) {
		return false, 0
	}
	return true, float64(gap) / float64(24*time.Hour/time.Millisecond)
}

// staleWhy states the evidence as a DATE and a duration, which is the point of this verdict over a
// session window: "last used 12 September, carried on every request since" is something a reader
// can check against their own memory, and "unused in the last 10 sessions" is not.
func staleWhy(r ToolStat, deadDays float64) string {
	when := time.UnixMilli(r.LastUsedTS).Format("2 Jan 2006")
	return "last used " + when + " — " + strconv.Itoa(int(deadDays+0.5)) +
		" days carried on every request since, invoked in none of them"
}
