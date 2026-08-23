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
// gate. Measured over the four interactive captures on this box
// (cg-research/bench/{short,long,mixed,cold}.jsonl, 70 requests, one session each): the real
// Claude Code catalogue is 31 tools and the gate keeps 6 of them — Agent, Bash, Edit, Read,
// Skill, Write — clearing the other 25, including every one of the never-used items that
// carries real weight. On the harness corpora (/tmp/cg-runs/capture-{tb,swebench}.jsonl,
// 1,844 requests, 53 sessions) the catalogue is 24 tools and it keeps those 6 plus
// TaskCreate.
//
// # 3. Determinism, because `tools` renders at position 0
//
// `tools` is hashed before `system` and `messages`, and no breakpoint sits on it, so ANY
// edit here re-anchors the entire cached prefix. A sibling proved the corollary the
// expensive way: gate a prefix transform on a per-turn condition and it re-anchors on EVERY
// turn. The only safe shapes are ALWAYS and NEVER for a given session.
//
// So every input to the decision is session-invariant, with one bounded exception named
// below (`tool_choice`):
//
//   - the remove list is configuration, read once per request from the built pipeline;
//   - the prose region is the system blocks ONLY, minus the environment snapshot cachesplit
//     already isolates as volatile and minus the billing-header block. It is
//     session-invariant because of what it leaves OUT, not "by construction".
//
//     The old argument was that the region is the text schema.SessionHead feeds to
//     session.Scoped, so a change to it would already be a different session. That is FALSE:
//     it only holds when the session id is DERIVED, and Claude Code sends an explicit one
//     (metadata.user_id), which wins in explicitSession, so the region could move while the
//     session id did not. MEASURED on a real 35-request Claude Code session
//     (cg-research/bench/long.jsonl): `messages.0` changed twice — the agent's own
//     auto-compaction rewrites the first user message into a conversation SUMMARY, taking
//     the region from 32,512 to 53,752 bytes — and `system[0]` changed once (its
//     x-anthropic-billing-header cc_version). A summary is MODEL-WRITTEN prose that names
//     tools, so one naming a filtered tool would put its declaration BACK for that turn and
//     re-anchor `tools` — the one block a client-side compaction otherwise preserves (19.8%
//     of an interactive request, 59.2% on SWE-bench).
//
//     So `messages.0` is no longer scanned unless its role is `system`/`developer` (the
//     OpenAI dialect keeps its system prompt there, and dropping it would leave those
//     requests with no prose gate at all), and the billing-header block is skipped. The
//     cost is that the gate is less conservative: a tool described ONLY in the user's own
//     first message can now be stripped. MEASURED over all six corpora — 1,914 requests, 57
//     sessions, the interactive captures plus capture-tb and capture-swebench — the number of
//     declarations prose-referenced only via `messages.0` and not via any system block is
//     ZERO, so on real traffic the loosening removes nothing extra and the kept set above is
//     unchanged. That is the trade the alternative (a session-keyed monotone keep-set) loses:
//     it would give a prefix transform mutable state, and the filter runs BEFORE
//     session.Scoped resolves an id, so there is not even a key to hang it on; and state lost
//     to a process restart flips the set back, which is the bug again.
//     TestFilterRemovedSetSurvivesCompaction and TestFilterProseSetStableOverCapture are the
//     guards; see docs/how-to/declaration-removal.md;
//
//   - `tool_choice` is read per request (forcedToolName) and is the one input still NOT
//     session-invariant. It is not fixable without the state the bullet above rejects: a
//     forced declaration cannot be removed (that is a 400), and whether a turn forces it is
//     the client's choice. Bounded rather than fixed: a client can only force a tool it
//     declared, so the flip needs an account to remove a name its client forces on some
//     turns and not others; measured over those 1,914 requests, 0 carried a `tool_choice`
//     that names a tool at all;
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
	"bytes"
	"strings"

	"github.com/rossoctl/context-guru/internal/skills"
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

// removeSets splits the configured list into exact names and whole-server entries, and drops the
// skill entries: those are removed from the listing prose by filterSkillListing, and leaving
// `skill__foo` in the tool-name set would mean a tool actually called `skill__foo` was removed by
// a request to remove a skill. No such tool exists today, which is exactly why the guard belongs
// here rather than in a bug report later.
func removeSets(remove []string) (names, servers map[string]bool) {
	names, servers = make(map[string]bool, len(remove)), map[string]bool{}
	for _, r := range remove {
		if strings.HasPrefix(r, skills.RemovePrefix) {
			continue
		}
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

// proseRegion is the text a prose reference to a tool may live in: every `system` block, less
// three exclusions. All three are excluded for ONE reason — a region that moves mid-session
// lets the gate flip, and a flipped gate re-anchors `tools` (file comment, §3):
//
//   - the environment snapshot prefixsplit.go classifies as volatile, because it is a
//     git/directory snapshot that changes between turns and a commit subject containing a
//     tool's name would otherwise decide whether that tool is filtered;
//   - the billing-header block, which is a header Claude Code passes through the body rather
//     than prose, and whose cc_version changes when the client self-updates mid-session;
//   - `messages.0` when it is the USER turn, which Claude Code's auto-compaction rewrites
//     into a model-written conversation summary that names tools.
//
// The last exclusion is by ROLE, not by position, because the OpenAI dialect has no top-level
// `system` at all — its system prompt IS messages[0], with role "system" (or "developer").
// That block is the same hand-written class of text as an Anthropic `system` block and is
// scanned; dropping it wholesale would leave every OpenAI-dialect request with an EMPTY prose
// region, which is not a determinism fix but a silent removal of the gate.
//
// What is left is the agent's own hand-written instructions, which do not change within a
// session. That is measured, not assumed: see the file comment and the two guard tests.
func proseRegion(body []byte) string {
	var b strings.Builder
	add := func(txt string) {
		if txt == "" || strings.HasPrefix(txt, billingHeaderBlock) {
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
	// The OpenAI dialect's system prompt, and only that: any other role on messages[0] is the
	// user turn a compaction summary replaces.
	switch gjson.GetBytes(body, "messages.0.role").String() {
	case "system", "developer":
		first := gjson.GetBytes(body, "messages.0.content")
		if first.Type == gjson.String {
			add(first.String())
		} else {
			first.ForEach(func(_, blk gjson.Result) bool {
				add(blk.Get("text").String())
				return true
			})
		}
	}
	return b.String()
}

// billingHeaderBlock prefixes the pseudo-system block Claude Code uses to smuggle a header
// through the request body ("x-anthropic-billing-header: cc_version=...; cc_entrypoint=...").
// It is system[0] on every capture on this box, it is not prose, and its cc_version moves
// mid-session — so proseRegion skips the whole block rather than trusting it.
const billingHeaderBlock = "x-anthropic-billing-header:"

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
// gate discriminating enough to be useful (25 of 31 real tools clear it) instead of
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

// ── skills ──────────────────────────────────────────────────────────────────

// filterSkillListing removes the named skills' entries from the skills listing.
//
// # Why this is a different mechanism from the tools filter, and a SAFER one
//
// The listing is prose in a role:"system" MESSAGE (measured on real traffic: messages[1], a
// plain string, 6,867 bytes), not an element of `tools`. So the two hazards are inverted.
//
// The tools filter's danger is SILENCE: strip a declaration whose prose survives and the model
// may narrate the call instead of emitting one, which nothing surfaces. A skill cannot fail that
// way. The `Skill` tool's schema takes a free-form string with NO enum — verified against a real
// declaration — and its description says only that names come from the listing. So an unlisted
// skill the model names anyway still RUNS. The failure mode of over-removing here is that the
// model does not know a skill exists, which is exactly what the account asked for, and the
// failure mode of the model remembering one anyway is that it works. Fail-open by construction.
//
// # What it shares with the tools filter: determinism
//
// It edits a message deep inside the cached prefix, so any per-turn variation would re-anchor
// the prefix on every turn (see §3 of this file's header). The inputs are the same
// session-invariant ones — the configured list, and the prose region minus the listing itself —
// so for a given account the answer is the same on every turn of every session. Switching it on
// mid-session re-anchors ONCE, the same one-time charge the tools half pays and the same
// arithmetic in reverse for putting a skill back.
//
// The prose gate is kept for consistency with the tools half, with the listing SUBTRACTED from
// the region first: the gate has to mean "named somewhere other than its own listing entry", or
// a client that puts its listing in messages[0] with role system would have every skill
// permanently pinned by the entry that is the thing being removed.
//
// Fails open on everything: no listing, no terminator, a body shape it does not recognise, a
// re-encode that errors — all return the input untouched and a saving of zero.
func filterSkillListing(body []byte, remove []string) (out []byte, removedTokens, removed int) {
	drop := map[string]bool{}
	for _, r := range remove {
		if n := strings.TrimPrefix(r, skills.RemovePrefix); n != r && n != "" {
			drop[n] = true
		}
	}
	if len(drop) == 0 {
		return body, 0, 0
	}
	// Which message holds the listing, and — when its content is an array — which block. The
	// FIRST match, the same choice dash's reader makes, so the page and the filter describe the
	// same listing on a body that somehow carries two.
	path, text := skillListingPath(body)
	if path == "" {
		return body, 0, 0
	}
	i := strings.Index(text, skills.Header)
	if i < 0 {
		return body, 0, 0 // path said there was one: refuse rather than guess
	}
	head := text[:i+len(skills.Header)]
	rest := text[i+len(skills.Header):]
	tail := ""
	if j := strings.Index(rest, skills.ReminderEnd); j >= 0 {
		rest, tail = rest[:j], rest[j:]
	}
	// The prose gate, with the listing itself removed from the region it tests against.
	prose := strings.Replace(proseRegion(body), text, "", 1)
	l := skills.Parse(rest)
	for name := range drop {
		if proseReferenced(prose, name) {
			delete(drop, name)
		}
	}
	if len(drop) == 0 {
		return body, 0, 0
	}
	for _, e := range l.Entries {
		if drop[e.Name] {
			removedTokens += tokens.Count(l.Text(e))
		}
	}
	next, n := l.Without(drop)
	if n == 0 {
		return body, 0, 0
	}
	nb, err := sjson.SetBytes(body, path, head+next+tail)
	if err != nil {
		return body, 0, 0
	}
	return nb, removedTokens, n
}

// skillListingPath finds the listing: the sjson path of the string that holds it, and that
// string. "" when no message carries one.
//
// A byte search over the raw body first, because both anchors survive JSON escaping and the
// overwhelming majority of requests have nothing to do here — a body with no listing must not
// pay a walk of a multi-megabyte messages array. Only on a hit does it walk to find the path.
func skillListingPath(body []byte) (string, string) {
	// bytes.Contains, not strings.Contains(string(body), ...): the body runs to megabytes and the
	// conversion would copy all of it on the request goroutine, on every request, to answer a
	// question that is usually "no".
	if !bytes.Contains(body, []byte(skills.Header)) {
		return "", ""
	}
	path, text := "", ""
	gjson.GetBytes(body, "messages").ForEach(func(idx, m gjson.Result) bool {
		if !strings.Contains(m.Raw, skills.Header) {
			return true
		}
		c := m.Get("content")
		if c.Type == gjson.String {
			if strings.Contains(c.String(), skills.Header) {
				path, text = "messages."+idx.String()+".content", c.String()
				return false
			}
			return true
		}
		c.ForEach(func(bidx, blk gjson.Result) bool {
			t := blk.Get("text")
			if t.Exists() && strings.Contains(t.String(), skills.Header) {
				path = "messages." + idx.String() + ".content." + bidx.String() + ".text"
				text = t.String()
				return false
			}
			return true
		})
		return path == ""
	})
	return path, text
}
