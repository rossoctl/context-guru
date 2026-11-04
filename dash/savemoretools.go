package dash

import (
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/skills"
	"github.com/rossoctl/context-guru/tooldefer"
)

// save-more tools (#485 D3, #869 item 4): one row per tool, MCP server and skill, with its state,
// its use, the cost of carrying it and the reason for its state, plus the actions an account can
// take on tool deferral: keep a tool (never defer it), always defer a tool (#878), turn search on or
// off for the account, and revert the last change.
//
// The rows come from the same inventory /api/tools reads (ToolReportFor), so the use and cost
// figures are the dashboard's own. The deferral half (is it deferred by Context Guru, and why) is
// the host's: dash holds no configuration and no deferral history, so the host passes a
// SaveMoreToolsHost, as it passes the removal list through SetToolFilterState.
//
// Metadata only: tool names, counts and token weights.

// Tool states on a save-more tools row.
const (
	StateLoaded        = "loaded"
	StateDeferredCG    = "deferred_cg"     // Context Guru's history rule defers it
	StateDeferredCC    = "deferred_client" // Claude Code sends it in its deferred-tools list
	StateRemoved       = "removed"         // on the account's removal list
	SaveMoreToolsKind  = "save_more_tools"
	saveMoreToolsLimit = 200
)

// SaveMoreToolsHost is what the host knows about one account's tool deferral.
type SaveMoreToolsHost struct {
	// SearchOn is whether tool deferral is on for the account (toolfilter `defer: auto`).
	SearchOn bool
	// Keep is the account's keep list, AlwaysDefer its always-defer list (#878), Removed its
	// removal list.
	Keep, AlwaysDefer, Removed []string
	// Explain says whether Context Guru's history rule would defer name, and why. nil when the host
	// has no history (deferral never ran in this process).
	Explain func(name string) (deferred bool, reason string)
	// Writable is false when the account's configuration cannot be edited here; Reason says why.
	Writable bool
	Reason   string
}

// SaveMoreToolsRow is one tool, MCP server or skill.
type SaveMoreToolsRow struct {
	Kind   string `json:"kind"` // tool | mcp_server | skill
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	State  string `json:"state"`
	Reason string `json:"reason"`
	// Builtin marks one of Claude Code's own tools.
	Builtin bool `json:"builtin,omitempty"`
	// Kept is true when the row is on the account's keep list; AlwaysDeferred when it is on the
	// always-defer list (#878).
	Kept           bool `json:"kept,omitempty"`
	AlwaysDeferred bool `json:"always_deferred,omitempty"`
	// Tokens is the weight on one request; SessionsDeclared/SessionsUsed/Calls its use.
	Tokens           int   `json:"tokens"`
	SessionsDeclared int   `json:"sessions_declared"`
	SessionsUsed     int   `json:"sessions_used"`
	Calls            int   `json:"calls"`
	LastUsedTS       int64 `json:"last_used_ts,omitempty"`
	// CarryUSD is what carrying it cost over the window, used or not; CarryPriced false when some
	// request had no known rates.
	CarryUSD    float64 `json:"carry_usd"`
	CarryPriced bool    `json:"carry_priced"`
	// Actions are the option ids an apply may name for this row (see SaveMoreToolsDoc.Options).
	Actions []string `json:"actions"`
}

// SaveMoreToolsOption is one change the report offers. Apply names it by ID.
type SaveMoreToolsOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Action is keep | unkeep | defer | undefer | search_on | search_off; Name the row it acts on, ""
	// for the account.
	Action string `json:"action"`
	Name   string `json:"name,omitempty"`
}

// SaveMoreToolsNotYet is one action the design lists and this proxy does not offer yet.
type SaveMoreToolsNotYet struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Issue  int    `json:"issue"`
}

// SaveMoreToolsDoc is the GET /api/save-more/tools response.
type SaveMoreToolsDoc struct {
	Kind     string   `json:"kind"`
	SearchOn bool     `json:"search_on"`
	Keep     []string `json:"keep"`
	// AlwaysDefer is the account's always-defer list (#878).
	AlwaysDefer []string              `json:"always_defer"`
	Writable    bool                  `json:"writable"`
	Reason      string                `json:"reason,omitempty"`
	Window      AdaptToolsWindow      `json:"window"`
	Rows        []SaveMoreToolsRow    `json:"rows"`
	Options     []SaveMoreToolsOption `json:"options"`
	// NotYet names the actions #485 D3 lists that this proxy does not offer yet, each with the
	// issue that builds it, so a reader sees them as missing rather than absent.
	NotYet []SaveMoreToolsNotYet `json:"not_yet"`
	// Truncated is how many rows were left out past the limit (the cheapest ones).
	Truncated int `json:"truncated,omitempty"`
	// BasisID names this report; an apply sends it back with the option id.
	BasisID string `json:"basis_id,omitempty"`
}

// SaveMoreTools builds the report for f, one account's scope.
func (d *DB) SaveMoreTools(f Filter, price func(string) (modelinfo.Price, bool), host SaveMoreToolsHost) (*SaveMoreToolsDoc, error) {
	rep, err := d.ToolReportFor(f, price)
	if err != nil {
		return nil, err
	}
	return buildSaveMoreTools(rep, host, f), nil
}

// buildSaveMoreTools is the arithmetic, apart from the query so it is testable on its own.
func buildSaveMoreTools(rep *ToolReport, host SaveMoreToolsHost, f Filter) *SaveMoreToolsDoc {
	keep, always, removed := set(host.Keep), set(host.AlwaysDefer), set(host.Removed)
	doc := &SaveMoreToolsDoc{Kind: SaveMoreToolsKind, SearchOn: host.SearchOn, Keep: append([]string{}, host.Keep...),
		AlwaysDefer: append([]string{}, host.AlwaysDefer...),
		Writable:    host.Writable, Reason: host.Reason, Rows: []SaveMoreToolsRow{}, Options: []SaveMoreToolsOption{},
		NotYet: []SaveMoreToolsNotYet{}}
	doc.Window = AdaptToolsWindow{Since: f.Since, Until: f.Until, Sessions: rep.Coverage.Captured, Requests: rep.Coverage.Requests}
	sort.Strings(doc.Keep)
	sort.Strings(doc.AlwaysDefer)

	state := func(name string, clientDeferred bool) (string, string) {
		// Client-deferred first: the removal filter removes only `tools` elements, so a name Claude
		// Code defers is still sent even when it is on the removal list.
		switch {
		case clientDeferred:
			return StateDeferredCC, "Claude Code defers it: only its name is sent until the model searches for it"
		case removed[name]:
			return StateRemoved, "on your removal list: the proxy does not send it"
		case !host.SearchOn:
			return StateLoaded, "tool search is off for your account"
		case always[name] && !keep[name] && !tooldefer.NeverDefer(name):
			// The always-defer list reads no history, so it holds without Explain too.
			return StateDeferredCG, "Context Guru defers it in new threads (on your always-defer list), " +
				"unless the thread uses it or it is under 200 tokens"
		case host.Explain == nil:
			return StateLoaded, "no tool history on this proxy yet"
		}
		// Explain checks the user rule only. The project veto and the 200-token minimum are decided
		// per request, so a thread may still send this tool in full; the reason says so.
		if on, why := host.Explain(name); on {
			return StateDeferredCG, "Context Guru defers it in new threads (" + why +
				"), unless this project used it or it is under 200 tokens"
		} else {
			return StateLoaded, why
		}
	}
	add := func(r SaveMoreToolsRow) {
		r.Kept = keep[r.Name] || (r.Kind == KindMCPServer && keep["mcp__"+r.Server])
		r.AlwaysDeferred = always[keepName(r)]
		// The name lists take a bare tool name (toolfilter's charset); '@' would also split the
		// plugin's option@basis token.
		if r.Kind != KindSkill && !adaptToolsOwn.MatchString(r.Name) && adaptToolsSafe.MatchString(r.Name) &&
			!strings.Contains(r.Name, "@") {
			if r.Kept {
				r.Actions = []string{"unkeep:" + keepName(r)}
			} else {
				r.Actions = []string{"keep:" + keepName(r)}
			}
			// Always defer: not for a never-defer tool or a kept one (the keep list wins), and not for
			// one the proxy does not send or Claude Code defers itself. Undefer is always offered.
			switch {
			case r.AlwaysDeferred:
				r.Actions = append(r.Actions, "undefer:"+keepName(r))
			case !r.Kept && !tooldefer.NeverDefer(r.Name) && r.State != StateRemoved && r.State != StateDeferredCC:
				r.Actions = append(r.Actions, "defer:"+keepName(r))
			}
		}
		if r.Actions == nil {
			r.Actions = []string{}
		}
		doc.Rows = append(doc.Rows, r)
	}

	// Tools: built-ins and other client tools (Aside), and MCP tools grouped per server.
	for _, t := range rep.Aside {
		if t.Kind != KindTool || adaptToolsOwn.MatchString(t.Name) {
			continue
		}
		st, why := state(t.Name, t.Deferred)
		add(SaveMoreToolsRow{Kind: KindTool, Name: t.Name, State: st, Reason: why, Builtin: t.Builtin,
			Tokens: t.Tokens, SessionsDeclared: t.SessionsDeclared, SessionsUsed: t.SessionsUsed, Calls: t.Calls,
			LastUsedTS: t.LastUsedTS, CarryUSD: t.CarryUSD, CarryPriced: t.CarryPriced})
	}
	// A server reads "deferred by Claude Code" only when every tool of it is: Claude Code defers
	// tools one by one, so one deferred tool does not make the whole server deferred.
	deferredTools, serverTools := map[string]int{}, map[string]int{}
	var lastUsed = map[string]int64{}
	for _, t := range rep.Tools {
		if t.Kind != KindMCPTool {
			continue
		}
		serverTools[t.Server]++
		if t.Deferred {
			deferredTools[t.Server]++
		}
		if t.LastUsedTS > lastUsed[t.Server] {
			lastUsed[t.Server] = t.LastUsedTS
		}
	}
	for _, s := range rep.Servers {
		if adaptToolsOwn.MatchString(s.Server) {
			continue
		}
		name := "mcp__" + s.Server
		st, why := state(name, serverTools[s.Server] > 0 && deferredTools[s.Server] == serverTools[s.Server])
		add(SaveMoreToolsRow{Kind: KindMCPServer, Name: name, Server: s.Server, State: st, Reason: why,
			Tokens: s.Tokens, SessionsDeclared: s.SessionsDeclared, SessionsUsed: s.SessionsUsed, Calls: s.Calls,
			LastUsedTS: lastUsed[s.Server], CarryUSD: s.CarryUSD, CarryPriced: s.CarryPriced})
	}
	// Skills: deferral does not touch them (#823 decides the skills listing), so only their state and
	// use are shown.
	for _, t := range rep.Skills.Skills {
		if adaptToolsOwn.MatchString(t.Name) {
			continue
		}
		st, why := StateLoaded, "skills are listed by name and description; tool deferral does not change them"
		if removed[skills.RemovePrefix+t.Name] {
			st, why = StateRemoved, "on your removal list: the proxy does not send it"
		}
		add(SaveMoreToolsRow{Kind: KindSkill, Name: t.Name, State: st, Reason: why,
			Tokens: t.Tokens, SessionsDeclared: t.SessionsDeclared, SessionsUsed: t.SessionsUsed, Calls: t.Calls,
			LastUsedTS: t.LastUsedTS, CarryUSD: t.CarryUSD, CarryPriced: t.CarryPriced})
	}
	sort.SliceStable(doc.Rows, func(i, j int) bool {
		if doc.Rows[i].CarryUSD != doc.Rows[j].CarryUSD {
			return doc.Rows[i].CarryUSD > doc.Rows[j].CarryUSD
		}
		return doc.Rows[i].Name < doc.Rows[j].Name
	})
	if len(doc.Rows) > saveMoreToolsLimit {
		doc.Truncated = len(doc.Rows) - saveMoreToolsLimit
		doc.Rows = doc.Rows[:saveMoreToolsLimit]
	}

	// The options: each keep, unkeep, defer or undefer a row offers, and the account switch.
	seen := map[string]bool{}
	for _, r := range doc.Rows {
		for _, id := range r.Actions {
			if seen[id] {
				continue
			}
			seen[id] = true
			act, name, _ := strings.Cut(id, ":")
			label := "never defer " + name
			switch act {
			case "unkeep":
				label = "take " + name + " off the keep list"
			case "defer":
				label = "always defer " + name + ", whatever your history says"
			case "undefer":
				label = "take " + name + " off the always-defer list"
			}
			doc.Options = append(doc.Options, SaveMoreToolsOption{ID: id, Label: label, Action: act, Name: name})
		}
	}
	if host.SearchOn {
		doc.Options = append(doc.Options, SaveMoreToolsOption{ID: "search_off", Action: "search_off",
			Label: "turn tool search off for your account: every tool is sent in full"})
	} else {
		doc.Options = append(doc.Options, SaveMoreToolsOption{ID: "search_on", Action: "search_on",
			Label: "turn tool search on for your account: tools your history shows unused are deferred"})
	}
	return doc
}

// keepName is the keep-list entry for a row: a tool's own name, or mcp__<server> for a server.
func keepName(r SaveMoreToolsRow) string {
	if r.Kind == KindMCPServer {
		return "mcp__" + r.Server
	}
	return r.Name
}

func set(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, n := range list {
		out[n] = true
	}
	return out
}

// SaveMoreToolsOffers is the option map an apply may name, for OfferAdapt.
func (doc *SaveMoreToolsDoc) SaveMoreToolsOffers() map[string]any {
	out := make(map[string]any, len(doc.Options))
	for _, o := range doc.Options {
		out[o.ID] = o
	}
	return out
}
