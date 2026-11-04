// Package tooldefer defers the tool declarations a user does not use (#485).
//
// A deferred tool stays in the request with `defer_loading: true`. The model sees only its
// name, in a names list, and loads its definition through the API's own search tool
// (tool_search_tool_bm25) when it needs it. Nothing is removed, so a wrong guess costs one search
// turn and never a cache rewrite.
//
// # The rule
//
// A tool is deferred when ALL of these hold (Decide):
//
//  1. it is not used in this thread so far;
//  2. the user's last 3 sessions, in any project, declared it and used it in none;
//  3. this project did not use it in its last 10 sessions (the project veto);
//  4. it is not on the never-defer list (Bash, Read, Edit, Write, Agent, Skill, ToolSearch,
//     AskUserQuestion, Context Guru's own tools, a tool tool_choice forces, the account keep list);
//  5. it weighs at least 200 tokens, weighed on the ORIGINAL request.
//
// The 200 tokens are o200k tokens (our own tokenizer, tokens.Count), not Claude tokens: about 340
// Claude tokens. The ledger scales its weights by tokens.BilledDeltaFactor; the rule does not, on
// purpose, so the set it defers stays the one #485 measured (#866, N6).
//
// An MCP server is one unit: all of mcp__<server>__* is deferred together, when the user's last 3
// sessions declared the server and used none of its tools, the project used none of them in its last
// 10 sessions, and the server's tools weigh at least 200 o200k tokens together. A load of any one of its
// tools counts as a use of the server.
//
// Without 3 user sessions of history, nothing is deferred. A tool loaded through search counts as
// used, so the user's next sessions keep it loaded.
//
// # The always-defer list
//
// A tool on the account's always-defer list (toolfilter `always_defer`, #878; a tool name, or
// mcp__<server> for a whole server) skips rules 2 and 3, and the 3-session minimum: it reads no
// history. It still obeys rule 1, rule 4 (the keep list wins when a name is on both lists) and
// rule 5 (for a server: its total). The list reads no history, so Rebuild gives the same set.
//
// # Cache discipline
//
// `tools` hashes first, so a tool that moves between loaded and deferred rewrites the whole cached
// prefix. So the set is decided ONCE per thread, at its first request, and never changes within the
// thread. It is stored under a metadata key (tool names only), which the disk store keeps across a
// restart. On a miss mid-thread (the record evicted at the pin cap) the transcript proves part of
// the set: every tool our search loaded was deferred. The rule is run again, as at the thread's
// first request; when its set leaves out a tool the transcript proves, the history drifted, so the
// original is sent and nothing is saved (Rebuild).
//
// The search tool goes into `tools` at the first request and stays. The names list goes into
// Claude Code's own deferred-tools block at messages[1] when it exists, else into a system message
// inserted at index 1, with the same bytes on every request.
//
// Fail open: any parse problem returns the input unchanged.
package tooldefer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// The server-side search tool. The version is pinned: an unknown date is a 400.
const (
	SearchType = "tool_search_tool_bm25_20251119"
	SearchName = "tool_search_tool_bm25"
)

// The thresholds of the rule. See the package comment. MinTokens is in o200k tokens.
const (
	UserSessions    = 3
	ProjectSessions = 10
	MinTokens       = 200
)

// claudeCodeSearch is Claude Code's own client-side search tool.
const claudeCodeSearch = "ToolSearch"

// placeholder is the element Claude Code adds to keep its own deferred loading on.
const placeholder = "DeferredToolPlaceholder"

// neverDefer is the fixed part of the never-defer list.
var neverDefer = map[string]bool{
	"Bash": true, "Read": true, "Edit": true, "Write": true, "Agent": true, "Skill": true,
	claudeCodeSearch: true, "AskUserQuestion": true,
}

// NeverDefer reports whether the fixed list, or Context Guru's own prefix, protects name.
func NeverDefer(name string) bool {
	return neverDefer[name] || strings.HasPrefix(name, "context_guru_") || name == SearchName || name == placeholder
}

// History is the user's tool history, as the rule reads it. A nil History means "no history":
// nothing is deferred.
type History interface {
	// UserSessions returns the user's last n sessions (newest first, any project), leaving out
	// the session `exclude`, each as the tool names it declared and used. Leaving out the current
	// session is what makes a re-decision after a restart give the same set.
	UserSessions(tenant, exclude string, n int) []Session
	// ProjectUsed reports whether the project used tool in its last n sessions, leaving out
	// `exclude`. tool may be a server unit ("mcp__<server>__"): any tool of that server counts.
	ProjectUsed(tenant, project, tool, exclude string, n int) bool
}

// Session is one past session of the history: which tools it declared and which it used.
type Session struct {
	Declared map[string]bool
	Used     map[string]bool
}

// Input is everything Decide reads. The body is the ORIGINAL request, before any rewrite.
type Input struct {
	// Tenant is the user ("" on a single-tenant proxy, which has one user).
	Tenant  string
	Project string
	Body    []byte
	// Session is the current session: the history leaves it out.
	Session string
	// Keep is the account's keep list: never deferred.
	Keep []string
	// AlwaysDefer is the account's always-defer list: deferred without the history rule (#878).
	AlwaysDefer []string
}

// Decide returns the tool names to defer, sorted. Empty when the rule defers nothing.
func Decide(h History, in Input) []string {
	return decide(h, in, usedInTranscript(in.Body))
}

// decide is Decide with the transcript's uses given: a tool in used is never deferred.
func decide(h History, in Input, used map[string]bool) []string {
	if in.Session == "" || (h == nil && len(in.AlwaysDefer) == 0) {
		return nil
	}
	tools := gjson.GetBytes(in.Body, "tools")
	if !tools.IsArray() {
		return nil
	}
	// Without 3 user sessions the history rule defers nothing; the always-defer list still applies.
	var past []Session
	if h != nil {
		past = h.UserSessions(in.Tenant, in.Session, UserSessions)
	}
	history := len(past) >= UserSessions
	if !history && len(in.AlwaysDefer) == 0 {
		return nil
	}
	keep, always := map[string]bool{}, map[string]bool{}
	for _, k := range in.Keep {
		keep[k] = true
	}
	for _, k := range in.AlwaysDefer {
		always[k] = true
	}
	forced := forcedTool(in.Body)
	// MCP tools are decided per SERVER (mcp__<server>__*): the unit a user installs. A server is
	// deferred whole or not at all, from the history of all its tools together, and its 200-token
	// minimum is the sum of its tools (o200k tokens, see MinTokens). Built-in tools are decided one by one.
	type unit struct {
		names  []string
		tokens int
		veto   bool // some member is excluded on its own: the whole unit stays loaded
		listed int  // members on the always-defer list by their own name
		always bool // the unit is on the always-defer list: the history rule is skipped
	}
	units := map[string]*unit{}
	var order []string
	tools.ForEach(func(_, t gjson.Result) bool {
		n := t.Get("name").String()
		if n == "" || t.Get("type").Exists() || t.Get("defer_loading").Bool() {
			return true // a server tool, no name, or one the client deferred already
		}
		key := n
		if srv := mcpServer(n); srv != "" {
			key = mcpPrefix + srv + "__"
		}
		u := units[key]
		if u == nil {
			u = &unit{}
			units[key] = u
			order = append(order, key)
		}
		u.names = append(u.names, n)
		u.tokens += tokens.Count(t.Raw)
		if NeverDefer(n) || keep[n] || keep[strings.TrimSuffix(key, "__")] || n == forced || used[n] {
			u.veto = true
		}
		if always[n] {
			u.listed++
		}
		u.always = always[strings.TrimSuffix(key, "__")]
		return true
	})
	var out []string
	for _, key := range order {
		u := units[key]
		// A server is listed as mcp__<server>, or by every tool it declares: one listed tool does
		// not defer the rest of its server.
		u.always = u.always || u.listed == len(u.names)
		switch {
		case u.veto:
		case u.tokens < MinTokens:
		case u.always:
			out = append(out, u.names...)
		case !history:
		case !unusedIn(past, key):
		case in.Project != "" && h.ProjectUsed(in.Tenant, in.Project, key, in.Session, ProjectSessions):
		default:
			out = append(out, u.names...)
		}
	}
	sort.Strings(out)
	return out
}

const mcpPrefix = "mcp__"

// mcpServer returns the server half of an MCP tool name (mcp__<server>__<tool>), or "".
func mcpServer(n string) string {
	if !strings.HasPrefix(n, mcpPrefix) {
		return ""
	}
	rest := n[len(mcpPrefix):]
	i := strings.Index(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return ""
	}
	return rest[:i]
}

// inSet reports whether set holds name, where name may be a server unit ("mcp__<server>__"): then
// any tool of that server counts.
func inSet(set map[string]bool, name string) bool {
	if !strings.HasSuffix(name, "__") {
		return set[name]
	}
	for n := range set {
		if strings.HasPrefix(n, name) {
			return true
		}
	}
	return false
}

// unusedIn reports whether every one of the sessions declared name and none used it. name may be a
// server unit ("mcp__<server>__"): declared means any tool of the server declared, used means any
// tool of it used or loaded.
func unusedIn(past []Session, name string) bool {
	for _, s := range past {
		if !inSet(s.Declared, name) || inSet(s.Used, name) {
			return false
		}
	}
	return true
}

// usedInTranscript is every tool name the transcript has called, plus every tool a search result
// loaded (a load counts as a use).
func usedInTranscript(body []byte) map[string]bool {
	out := calledIn(body)
	for n := range Loaded(body) {
		out[n] = true
	}
	return out
}

// calledIn is every tool name the transcript has called.
func calledIn(body []byte) map[string]bool {
	out := map[string]bool{}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			switch b.Get("type").String() {
			case "tool_use":
				out[b.Get("name").String()] = true
			}
			return true
		})
		return true
	})
	return out
}

// Loaded is every tool name a search result in the transcript has loaded: the server-side search's
// tool_search_tool_result blocks, and the tool_reference blocks of Claude Code's own search.
func Loaded(body []byte) map[string]bool {
	out := map[string]bool{}
	if !strings.Contains(string(body), "tool_reference") {
		return out
	}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			switch b.Get("type").String() {
			case "tool_search_tool_result":
				b.Get("content.tool_references").ForEach(func(_, r gjson.Result) bool {
					out[r.Get("tool_name").String()] = true
					return true
				})
			case "tool_result":
				b.Get("content").ForEach(func(_, r gjson.Result) bool {
					if r.Get("type").String() == "tool_reference" {
						out[r.Get("tool_name").String()] = true
					}
					return true
				})
			}
			return true
		})
		return true
	})
	delete(out, "")
	return out
}

// forcedTool is the tool tool_choice forces, or "".
func forcedTool(body []byte) string {
	tc := gjson.GetBytes(body, "tool_choice")
	if tc.Get("type").String() == "tool" {
		return tc.Get("name").String()
	}
	return ""
}

// ── the thread-fixed record ─────────────────────────────────────────────────

// Record is one thread's decision. Tool names only: metadata, so the disk store keeps it.
type Record struct {
	// Deferred is the set decided at the thread's first request. Empty means "decided: nothing",
	// which is a decision too and is stored so the thread is never re-decided.
	Deferred []string `json:"d"`
	// Client is true when the thread runs in client-search mode (#898): the thread had Claude Code's
	// own ToolSearch at its first request, so it has no server-side search tool, and the proxy answers
	// the client's search results (AugmentSearches). Decided once, with the set.
	Client bool `json:"c,omitempty"`
	// Answers is the names each ToolSearch call was answered with, by tool_use id: nil or empty means
	// "the client's own result stands". Written the first time a call is seen and replayed from here,
	// so the bytes of every later request are the same (also after a restart). Bounded by maxAnswers.
	Answers map[string][]string `json:"a,omitempty"`
}

// Key is the store key of one thread's record.
func Key(session, thread string) string {
	if thread == "" {
		return store.ToolDeferPrefix + session
	}
	h := sha256.Sum256([]byte(thread))
	return store.ToolDeferPrefix + session + ":" + hex.EncodeToString(h[:8])
}

// Load reads a thread's record. ok=false on a miss.
func Load(st store.Store, session, thread string) (Record, bool) {
	if st == nil || session == "" {
		return Record{}, false
	}
	b, ok := st.Get(Key(session, thread))
	if !ok {
		return Record{}, false
	}
	var r Record
	if json.Unmarshal(b, &r) != nil {
		return Record{}, false
	}
	return r, true
}

// Save writes a thread's record.
func Save(st store.Store, session, thread string, r Record) {
	if st == nil || session == "" {
		return
	}
	if r.Deferred == nil {
		r.Deferred = []string{}
	}
	if b, err := json.Marshal(r); err == nil {
		st.Put(Key(session, thread), b)
	}
}

// searchOffKey is the store key of a session's tool search opt-out.
func searchOffKey(session string) string { return store.ToolSearchOffPrefix + session }

// SearchOff reports whether st holds the tool search opt-out for session.
func SearchOff(st store.Store, session string) bool {
	if st == nil || session == "" {
		return false
	}
	v, ok := st.Get(searchOffKey(session))
	return ok && string(v) == "1"
}

// SetSearchOff stores the opt-out for session (on), or removes it.
func SetSearchOff(st store.Store, session string, on bool) {
	if st == nil || session == "" {
		return
	}
	v := []byte("0") // the Store has no delete: "0" is "not off"
	if on {
		v = []byte("1")
	}
	st.Put(searchOffKey(session), v)
}

// Rebuild is the set after a store miss mid-thread. The transcript proves part of the set: a tool
// our search loaded was deferred by definition. The rule is decided again as at the thread's first
// request: the history leaves out this session, and the loads (and the calls that followed them) do
// not count as uses. ok is false when that set leaves out a tool the transcript proves: the history
// drifted, the set is unknown, and the host sends the original request and saves nothing (#866, N4).
//
// The history can drift: Observe adds a session at its first request, so a session that only
// STARTED in between changes UserSessions. A drifted set moves a tool inside the thread's cached
// prefix, and saved, it would stay wrong for the rest of the thread.
//
// Known limits: a drift on a tool no search loaded leaves no trace in the transcript, so the check
// cannot see it. And a tool the model called directly while it was deferred, with no search (the API
// accepts that), reads as "used" and comes back loaded. Also, a tool Claude Code's own search loaded,
// with no call after it, is not a use here, so the rebuilt set can defer it where the first
// decision kept it loaded. Each rewrites the prefix once. The disk
// store, on with the dashboard, makes both rare: Rebuild runs only when the stored record is gone.
func Rebuild(h History, in Input) (set []string, ok bool) {
	proven := provenDeferred(in.Body)
	used := calledIn(in.Body)
	for n := range proven {
		delete(used, n)
	}
	set = decide(h, in, used)
	got := make(map[string]bool, len(set))
	for _, n := range set {
		got[n] = true
	}
	for n := range proven {
		if !got[n] {
			return nil, false
		}
	}
	return set, true
}

// provenDeferred is every tool our search loaded in this transcript that the request still
// declares, loaded, and the rule may defer: the part of the thread's set the transcript proves.
// Loads by Claude Code's own search are left out: Claude Code deferred those, not us.
func provenDeferred(body []byte) map[string]bool {
	out := map[string]bool{}
	if !hasSearchTurn(body) {
		return out
	}
	loaded := map[string]bool{}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_search_tool_result" {
				b.Get("content.tool_references").ForEach(func(_, r gjson.Result) bool {
					loaded[r.Get("tool_name").String()] = true
					return true
				})
			}
			return true
		})
		return true
	})
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		n := t.Get("name").String()
		if loaded[n] && !t.Get("type").Exists() && !t.Get("defer_loading").Bool() && !NeverDefer(n) {
			out[n] = true
		}
		return true
	})
	return out
}

// hasSearchTurn reports whether the transcript holds a call to OUR search tool.
func hasSearchTurn(body []byte) bool {
	if !strings.Contains(string(body), SearchName) {
		return false
	}
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			found = b.Get("type").String() == "server_tool_use" && b.Get("name").String() == SearchName
			return !found
		})
		return !found
	})
	return found
}

// HasAssistantTurn reports whether this request continues a thread (it is not the first one).
func HasAssistantTurn(body []byte) bool {
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		found = m.Get("role").String() == "assistant"
		return !found
	})
	return found
}
