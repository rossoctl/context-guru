package apply

// Declaration filter: drop tool / MCP-server declarations the account has explicitly
// opted to stop carrying.
//
// # Why this is the largest single lever measured in this effort
//
// A coding agent re-sends its whole tool catalogue on every turn, and almost none of it
// is ever invoked. Measured with the repo's own tokenizer over 1,844 real request bodies:
// 21,889 declared tokens per session, of which 18,107 (82.7%) belong to declarations no
// session ever called; 27 items were declared by every session and invoked by none. Priced
// on the deployed corpus that is 142.2M avoidable cache-read tokens, $60.53 — one to two
// orders of magnitude above anything else in docs/results/measured-2026-08.md.
//
// It is nonetheless OPT-IN, per account, per name, and never inferred. The reasons are
// below, and each of them is a way this could turn a saving into a correctness bug.
//
// # 1. The API does not reject a transcript that references a removed tool
//
// Verified on production traffic before this was built: Claude Code already shrinks its own
// tool set mid-session, and a window of 66 such requests returned 64x HTTP 200 and 0x 400 —
// 20 of the captured bodies carried a historical `tool_use` whose name was absent from
// their own `tools[]` and completed normally. So removing a declaration does not break a
// transcript that already called it. TestFilterKeepsHistoricalToolUseForwardable pins the
// property this code depends on.
//
// # 2. The real hazard is SILENCE, not rejection
//
// Claude Code's system prompt keeps DESCRIBING its tools in prose ("Prefer dedicated tools
// over Bash when one fits (Read, Edit, Write)"). Strip a declaration while its prose
// survives and the model may write the call into prose instead of emitting a tool_use —
// which no error surfaces and no test upstream catches.
//
// Our proxy can see that prose but cannot safely EDIT it: it is hand-written English inside
// a block whose sentences reference several tools at once, so there is no span whose removal
// is known to leave a coherent instruction. This code therefore takes the conservative half
// of "strip a declaration and its prose together, or neither": a declaration whose name
// appears in the prose is KEPT, whatever the configuration says. proseReferenced is that
// gate. On the real Claude Code catalogue it keeps 9 of 24 tools and clears the other 15 —
// including every one of the never-used items that carries real weight.
//
// # 3. Determinism, because `tools` renders at position 0
//
// `tools` is hashed before `system` and `messages`, and no breakpoint sits on it, so ANY
// edit here re-anchors the entire cached prefix. A sibling proved the corollary the
// expensive way: gate a prefix transform on a per-turn condition and it re-anchors on EVERY
// turn. The only safe shapes are ALWAYS and NEVER for a given session.
//
// So every input to the decision is session-invariant:
//
//   - the remove list is configuration, read once per request from the built pipeline;
//   - the prose region is the system blocks (minus the environment snapshot cachesplit
//     already isolates as volatile) plus the first message. That is not a convenient
//     choice: it is exactly the text schema.SessionHead feeds to session.Scoped, so if it
//     changed we would be looking at a different session by this package's own definition;
//   - the output preserves input order and re-uses each kept element's raw bytes, so a
//     removal of nothing is byte-identical to the input and no map iteration can reorder
//     anything. TestFilterDeclarationsByteStable pins it across processes.
//
// Applied from a session's FIRST request the re-anchor costs nothing — that request is a
// cold start either way (measured: 1,105 of 1,127 session starts were cold). Turning it on
// MID-session re-anchors once: measured $0.70 for a 147,099-token prefix at the 1.25x
// creation tier, paid back inside two sessions. Same arithmetic, in reverse, for putting a
// tool back.

import (
	"strings"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// filterDeclarations drops the declarations named in remove from a raw request body.
// Returns the rewritten body, the tokens it stopped carrying, and how many declarations
// went. Fails open: any parse problem, an empty result, or nothing to do returns the input
// untouched and a saving of zero.
//
// remove holds exact declaration names, plus the bare form `mcp__<server>` which stands for
// every tool of that MCP server — the unit a user actually adds and removes.
func filterDeclarations(body []byte, remove []string) (out []byte, removedTokens, removed int) {
	if len(remove) == 0 {
		return body, 0, 0
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, 0, 0
	}
	names, servers := removeSets(remove)
	arr := tools.Array()
	// Pass 1: which elements are candidates, on name alone. Nothing is read off the
	// transcript yet, so a request that opted into names this catalogue does not declare
	// costs one cheap scan and no further work.
	cand := make([]bool, len(arr))
	any := false
	for i, t := range arr {
		n := declName(t)
		if n == "" {
			continue
		}
		if names[n] || (servers[mcpServer(n)] && mcpServer(n) != "") {
			cand[i], any = true, true
		}
	}
	if !any {
		return body, 0, 0
	}
	// A pending call for a tool we are about to un-declare: leave this request alone.
	//
	// This is narrower than "skip every request whose last turn is a pending tool call",
	// deliberately, because the broad form would VIOLATE the determinism rule above — a
	// session whose turns end in an unanswered tool_use would alternate filtered and
	// unfiltered and re-anchor on every one. The narrow form is unreachable in the steady
	// state instead of merely rare: with the filter on from turn 1 the model never saw the
	// declaration, so it cannot have emitted the call. The only way to arrive here is the
	// turn an account switches the filter ON mid-session, which re-anchors once by
	// definition.
	if pendingCallFor(body, names, servers) {
		return body, 0, 0
	}
	// Pass 2: the prose gate (hazard 2 above), on candidates only.
	prose := proseRegion(body)
	forced := forcedToolName(body)
	kept := make([]string, 0, len(arr))
	for i, t := range arr {
		n := declName(t)
		switch {
		case !cand[i]:
		case n == forced:
			// tool_choice names this tool: the request REQUIRES it. Removing it would turn a
			// forced call into a 400, which is the one failure mode this whole file is
			// arranged to avoid.
			cand[i] = false
		case proseReferenced(prose, n):
			cand[i] = false
		}
		if !cand[i] {
			kept = append(kept, t.Raw)
			continue
		}
		removedTokens += tokens.Count(t.Raw)
		removed++
	}
	// An empty `tools` is not the same request with less in it — a body that declares a
	// catalogue and a body that declares none are different shapes to the provider, and
	// some reject the empty array outright. Never produce one.
	if removed == 0 || len(kept) == 0 {
		return body, 0, 0
	}
	next, err := sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(kept, ",")+"]"))
	if err != nil {
		return body, 0, 0
	}
	return next, removedTokens, removed
}

// removeSets splits the configured list into exact names and whole-server entries.
func removeSets(remove []string) (names, servers map[string]bool) {
	names, servers = make(map[string]bool, len(remove)), map[string]bool{}
	for _, r := range remove {
		if s, ok := serverEntry(r); ok {
			servers[s] = true
			continue
		}
		names[r] = true
	}
	return names, servers
}

// serverEntry recognises the bare `mcp__<server>` form, which names a whole MCP server
// rather than one of its tools.
func serverEntry(n string) (string, bool) {
	const pre = "mcp__"
	if !strings.HasPrefix(n, pre) {
		return "", false
	}
	rest := n[len(pre):]
	if rest == "" || strings.Contains(rest, "__") {
		return "", false // `mcp__server__tool`: one tool, not the server
	}
	return rest, true
}

// mcpServer returns the server half of an MCP tool name, or "".
func mcpServer(n string) string {
	const pre = "mcp__"
	if !strings.HasPrefix(n, pre) {
		return ""
	}
	rest := n[len(pre):]
	i := strings.Index(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return ""
	}
	return rest[:i]
}

// declName reads a tool declaration's name in either dialect. Anthropic server tools
// (`web_search_*`, the mcp_toolset connector) name themselves through `type` or
// `mcp_server_name` instead, and are matched the same way the inventory reports them so a
// suggestion and a removal talk about the same string.
func declName(t gjson.Result) string {
	if n := t.Get("name").String(); n != "" {
		return n
	}
	if n := t.Get("function.name").String(); n != "" {
		return n
	}
	if n := t.Get("mcp_server_name").String(); n != "" {
		return n
	}
	return t.Get("type").String()
}

// forcedToolName is the tool `tool_choice` requires, in either dialect, or "".
func forcedToolName(body []byte) string {
	tc := gjson.GetBytes(body, "tool_choice")
	if !tc.Exists() || !tc.IsObject() {
		return ""
	}
	if n := tc.Get("name").String(); n != "" {
		return n
	}
	return tc.Get("function.name").String()
}

// pendingCallFor reports whether the LAST message is an unanswered tool call naming
// something we were asked to remove. See the call site for why the predicate is scoped to
// the remove set rather than to "any pending call".
func pendingCallFor(body []byte, names, servers map[string]bool) bool {
	last := ""
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		last = m.Raw
		return true
	})
	if last == "" {
		return false
	}
	m := gjson.Parse(last)
	if r := m.Get("role").String(); r != "assistant" {
		return false // a tool_result turn: the call is answered, nothing is pending
	}
	hit := false
	check := func(n string) {
		if n != "" && (names[n] || (mcpServer(n) != "" && servers[mcpServer(n)])) {
			hit = true
		}
	}
	m.Get("content").ForEach(func(_, blk gjson.Result) bool {
		if blk.Get("type").String() == "tool_use" {
			check(blk.Get("name").String())
		}
		return !hit
	})
	m.Get("tool_calls").ForEach(func(_, c gjson.Result) bool {
		check(c.Get("function.name").String())
		return !hit
	})
	return hit
}

// proseRegion is the text a prose reference to a tool may live in: every `system` block up
// to the environment snapshot prefixsplit.go already classifies as volatile, plus the first
// message. Session-invariant by construction — see the file comment.
//
// The volatile tail is excluded rather than scanned because it is a git/directory snapshot
// that changes between turns: a commit subject containing a tool's name would otherwise
// flip the decision mid-session, which is the one thing this filter must never do.
func proseRegion(body []byte) string {
	var b strings.Builder
	add := func(txt string) {
		if txt == "" {
			return
		}
		b.WriteString(stableHalf(txt))
		b.WriteByte('\n')
	}
	sys := gjson.GetBytes(body, "system")
	if sys.Type == gjson.String {
		add(sys.String())
	} else {
		sys.ForEach(func(_, blk gjson.Result) bool {
			add(blk.Get("text").String())
			return true
		})
	}
	first := gjson.GetBytes(body, "messages.0.content")
	if first.Type == gjson.String {
		add(first.String())
	} else {
		first.ForEach(func(_, blk gjson.Result) bool {
			add(blk.Get("text").String())
			return true
		})
	}
	return b.String()
}

// stableHalf drops the environment snapshot from a block, using the same markers the
// volatile-tail split uses — one definition of "volatile", shared, because two would drift.
func stableHalf(txt string) string {
	cut := -1
	for _, mk := range volatileTailMarkers {
		if i := strings.Index(txt, mk); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return txt
	}
	return txt[:cut]
}

// proseReferenced reports whether name occurs in prose as a WORD. A substring test would
// keep `Read` alive on the word "already"; an identifier-boundary test is what makes the
// gate discriminating enough to be useful (15 of 24 real tools clear it) instead of
// refusing everything.
func proseReferenced(prose, name string) bool {
	if name == "" {
		return true // unknown name: refuse, never guess
	}
	for i := 0; ; {
		j := strings.Index(prose[i:], name)
		if j < 0 {
			return false
		}
		at := i + j
		end := at + len(name)
		if !identByte(prose, at-1) && !identByte(prose, end) {
			return true
		}
		i = at + 1
	}
}

// identByte reports whether prose[i] continues an identifier (so a match there is part of a
// longer word). Out of range counts as a boundary.
func identByte(prose string, i int) bool {
	if i < 0 || i >= len(prose) {
		return false
	}
	c := prose[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
