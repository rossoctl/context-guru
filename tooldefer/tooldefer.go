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
// Without 3 user sessions of history, nothing is deferred. A tool loaded through search counts as
// used, so the user's next sessions keep it loaded.
//
// # Cache discipline
//
// `tools` hashes first, so a tool that moves between loaded and deferred rewrites the whole cached
// prefix. So the set is decided ONCE per thread, at its first request, and never changes within the
// thread. It is stored under a metadata key (tool names only), which the disk store keeps across a
// restart. On a miss (a restart with no disk copy) the set is decided again with the same rule,
// from history that leaves out the current session, which gives the same answer unless another
// session of the user ended in between. Every tool a search in this transcript has loaded is then
// added, because the client echoes those loads (Rebuild).
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

// The thresholds of the rule. See the package comment.
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
	// `exclude`.
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
}

// Decide returns the tool names to defer, sorted. Empty when the rule defers nothing.
func Decide(h History, in Input) []string {
	if h == nil || in.Session == "" {
		return nil
	}
	tools := gjson.GetBytes(in.Body, "tools")
	if !tools.IsArray() {
		return nil
	}
	past := h.UserSessions(in.Tenant, in.Session, UserSessions)
	if len(past) < UserSessions {
		return nil // without 3 user sessions: defer nothing
	}
	used := usedInTranscript(in.Body)
	keep := map[string]bool{}
	for _, k := range in.Keep {
		keep[k] = true
	}
	forced := forcedTool(in.Body)
	var out []string
	tools.ForEach(func(_, t gjson.Result) bool {
		n := t.Get("name").String()
		switch {
		case n == "" || t.Get("type").Exists(): // a server tool, or no name
		case t.Get("defer_loading").Bool(): // the client deferred it already
		case NeverDefer(n), keep[n], n == forced, used[n]:
		case tokens.Count(t.Raw) < MinTokens:
		case !unusedIn(past, n):
		case in.Project != "" && h.ProjectUsed(in.Tenant, in.Project, n, in.Session, ProjectSessions):
		default:
			out = append(out, n)
		}
		return true
	})
	sort.Strings(out)
	return out
}

// unusedIn reports whether every one of the sessions declared name and none used it.
func unusedIn(past []Session, name string) bool {
	for _, s := range past {
		if !s.Declared[name] || s.Used[name] {
			return false
		}
	}
	return true
}

// usedInTranscript is every tool name the transcript has called, plus every tool a search result
// loaded (a load counts as a use).
func usedInTranscript(body []byte) map[string]bool {
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
	for n := range Loaded(body) {
		out[n] = true
	}
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

// Rebuild is the set after a store miss: the rule decided again (the history leaves out this
// session), plus every tool our search has loaded in this transcript. A tool our search loaded was
// deferred by definition, so adding it back cannot move a tool the provider cached as loaded.
//
// Known limit: a tool the model called directly while it was deferred, with no search (the API
// accepts that), reads as "used" and comes back loaded. So does a tool whose history moved because
// another session ended in between. Either rewrites the prefix once. The disk store, on with the
// dashboard, makes both cases rare: Rebuild runs only when the stored record is gone.
func Rebuild(h History, in Input) []string {
	set := map[string]bool{}
	for _, n := range Decide(h, in) {
		set[n] = true
	}
	if hasSearchTurn(in.Body) {
		for n := range Loaded(in.Body) {
			if !NeverDefer(n) {
				set[n] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
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
