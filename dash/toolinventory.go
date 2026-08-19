package dash

// Tool / MCP / skill inventory capture: what a request DECLARED and what it USED.
//
// Why this exists as its own file and its own tables: the request row stores a tool
// COUNT (`requests.tools`) and nothing else, so "how much of what you carry do you
// actually use" was unanswerable — the single largest measured waste in the corpus
// (see docs/tool-inventory.md) had no data behind it.
//
// What is stored, and what deliberately is not:
//
//   - NAMES and TOKEN WEIGHTS of declarations, plus which of them were invoked. A tool
//     name, an MCP server name and a skill name are IDENTIFIERS of the user's own
//     configuration — the same sensitivity class as `tool_choice`, which this package
//     already stores — so they are gated on tenant scoping (every row carries
//     tenant_id) rather than on transcript-capture consent. Requiring consent would
//     deny the feature to the accounts that declined transcript capture, over data that
//     is not their transcript.
//   - NEVER the description text, never a prompt, never a message. The listing that
//     declares skills is read for names and measured for size; its prose is discarded
//     on the request goroutine and never reaches the writer, let alone the disk.
//
// Cost discipline. This runs on every request, so the hot path is: one structural
// gjson scan of `tools`, two byte searches for the skills listing, one hash, one map
// lookup — and on a hit (every request after a session's first) nothing else. All
// tokenizing and all parsing happen once per distinct declaration SET, memoized by
// its digest. See BenchmarkScanInventory for the measured per-request figure.

import (
	"bytes"
	"hash/maphash"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
)

// Declaration kinds. They are stored as text because they are read back as labels and
// grouped on, exactly like `kind` on request_components.
const (
	// KindTool is a plain client-side tool (Claude Code's built-ins, an agent's own).
	KindTool = "tool"
	// KindMCPTool is one tool of an MCP server, named `mcp__<server>__<tool>`; Server
	// carries the server half so a per-server rollup needs no string work at read time.
	KindMCPTool = "mcp_tool"
	// KindServerTool is a provider-side tool (`web_search_*`, `code_execution_*`,
	// `memory_*`, the `mcp_toolset` connector): declared with a `type` and usually no
	// input_schema, and not removable the way a client tool is.
	KindServerTool = "server_tool"
	// KindSkill is one skill from the Skill tool's prose listing.
	KindSkill = "skill"
	// KindSkillListing is ONE marker row per declaration set carrying the listing's own
	// token cost and the PARSE STATE in Server (SkillsOK | SkillsUnknown). It is what
	// keeps a failed parse from reading as "this session declared no skills": with
	// state=unknown there are no KindSkill rows and the reader must say so.
	KindSkillListing = "skill_listing"
)

// Skill-listing parse states, stored in the marker row's Server column.
const (
	// SkillsOK: the header was found and at least one entry parsed.
	SkillsOK = "ok"
	// SkillsUnknown: a listing is present but nothing parsed out of it — a Claude Code
	// version whose format moved. The size is still honest; the inventory is not known.
	SkillsUnknown = "unknown"
)

// skillsHeader is the anchor. It is load-bearing rather than cosmetic: the agent-types
// listing sitting immediately above it in the SAME message uses the identical
// `- name: description` shape, so an unanchored scrape counts subagents as skills.
const skillsHeader = "The following skills are available for use with the Skill tool:"

// reminderEnd bounds the listing. Both this and the header appear literally in the
// JSON-escaped body (no escapable characters), which is what lets the hot path find
// the region without unescaping anything.
const reminderEnd = "</system-reminder>"

// Decl is one declaration: its name, its classification, and what carrying it costs
// in tokens (the whole JSON element for a tool, the listing entry for a skill).
type Decl struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	Tokens int    `json:"tokens"`
}

// Used is one invoked capability: a tool name, or the Skill tool plus the skill it
// ran (which is the only way a skill invocation is identifiable).
type Used struct {
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	Skill  string `json:"skill,omitempty"`
	Calls  int    `json:"calls"`
}

// Inventory is one request's contribution: the declaration set it carried (identified
// by Digest, so the writer stores each distinct set once per session) and the tool
// calls visible in its last tool-using turn.
type Inventory struct {
	Digest string
	Decls  []Decl
	Used   []Used
	// UseFingerprint identifies the turn Used came from. Claude Code resends the whole
	// transcript, so consecutive requests usually show the SAME last tool-using turn;
	// the writer skips a repeat rather than counting those calls again per resend.
	UseFingerprint uint64
}

var declSeed = maphash.MakeSeed()

// declCache memoizes the parse+tokenize of a declaration SET by its digest. Sized like
// the tokenizer's own cache and cleared wholesale past the cap: a working set of live
// sessions re-fills in one request each, and the alternative (an LRU) is bookkeeping
// for a map whose churn is a handful of entries a day.
var (
	declMu    sync.Mutex
	declCache = map[uint64][]Decl{}
)

const declCacheCap = 4096

// ScanInventory reads one request's declared inventory and used set off the raw body.
// Returns nil when the request declares no tools at all — an agent that uses none has
// no inventory, and a phantom row for it would make "nothing declared" and "nothing
// captured" the same number.
//
// provider selects the dialect: Anthropic names tools at `tools.#.name` and calls them
// in `tool_use` content blocks; OpenAI nests both a level deeper
// (`tools.#.function.name`, `tool_calls.#.function.name`).
func ScanInventory(provider string, body []byte) *Inventory {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || tools.Get("#").Int() == 0 {
		return nil
	}
	region := skillRegion(body)
	// One digest over both halves of the declaration set: a tool added and a skill added
	// are the same event as far as "is this the set we already measured" goes.
	var h maphash.Hash
	h.SetSeed(declSeed)
	h.WriteString(tools.Raw)
	h.WriteString("\x00")
	h.WriteString(region)
	digest := h.Sum64()

	declMu.Lock()
	decls, hit := declCache[digest]
	declMu.Unlock()
	if !hit {
		decls = declsFrom(provider, tools)
		if region != "" {
			decls = append(decls, skillDecls(skillsText(body))...)
		}
		declMu.Lock()
		if len(declCache) >= declCacheCap {
			declCache = map[uint64][]Decl{}
		}
		declCache[digest] = decls
		declMu.Unlock()
	}
	inv := &Inventory{Digest: strconv.FormatUint(digest, 16), Decls: decls}
	inv.Used, inv.UseFingerprint = usedFrom(provider, body)
	return inv
}

// declsFrom classifies every element of the tools array and measures what it costs to
// carry. The token weight is the WHOLE element (name, description and schema), because
// that is what the request pays for and what removing it would give back — measured
// with the repo's own BPE tokenizer, not a chars/4 estimate.
func declsFrom(provider string, tools gjson.Result) []Decl {
	nameField := "function.name"
	if provider == "anthropic" || provider == "" {
		nameField = "name"
	}
	var out []Decl
	for _, t := range tools.Array() {
		name := t.Get(nameField).String()
		kind := KindTool
		switch {
		case t.Get("type").Exists():
			// A provider-side tool. The mcp_toolset connector form declares a whole
			// server in one element and names it here rather than per tool.
			kind = KindServerTool
			if name == "" {
				name = t.Get("mcp_server_name").String()
			}
			if name == "" {
				name = t.Get("type").String()
			}
		case name == "":
			continue // malformed element: no name to attribute anything to
		}
		d := Decl{Kind: kind, Name: name, Tokens: tokens.Count(t.Raw)}
		if server, _, ok := SplitMCPName(name); ok {
			d.Kind, d.Server = KindMCPTool, server
		}
		// A tool declared with defer_loading is advertised but NOT sent to the model, so
		// charging its schema to the request would double-count tool search.
		if t.Get("defer_loading").Bool() {
			d.Tokens = 0
		}
		out = append(out, d)
	}
	return out
}

// SplitMCPName splits an MCP tool name into its server and tool halves.
// `mcp__<server>__<tool>`, non-greedy on the server: real tool names contain `__`
// (mcp__plugin_context7_context7__resolve-library-id), and there is no escape for a
// server whose own name does — that case splits at the first delimiter and is
// reported as the server it looks like, which is the only choice the convention
// leaves.
func SplitMCPName(name string) (server, tool string, ok bool) {
	const pre = "mcp__"
	if !strings.HasPrefix(name, pre) {
		return "", "", false
	}
	rest := name[len(pre):]
	i := strings.Index(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return "", "", false // `mcp__foo`, `mcp__foo__`: not a tool declaration
	}
	return rest[:i], rest[i+2:], true
}

// skillRegion returns the RAW (still JSON-escaped) slice of the body holding the
// skills listing, or "" when there is none. Header and terminator both survive JSON
// escaping, so this is two byte searches and no unescaping — cheap enough to run on
// every request, and it is what feeds the digest.
func skillRegion(body []byte) string {
	i := bytes.Index(body, []byte(skillsHeader))
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := bytes.Index(rest, []byte(reminderEnd)); j > 0 {
		return string(rest[:j])
	}
	// No terminator: bound the region rather than hashing the rest of the transcript.
	if len(rest) > maxListing {
		return string(rest[:maxListing])
	}
	return string(rest)
}

// maxListing caps how much of a body an unterminated listing may claim. The measured
// real listing is ~6 KB; 256 KB is room for a much larger skill set and still a bound.
const maxListing = 256 << 10

// skillsText returns the UNESCAPED listing text. Only called on a digest miss (once
// per distinct declaration set), which is why it can afford to walk the messages
// array: the listing lives in a {"role":"system"} MESSAGE, not in the system prompt.
func skillsText(body []byte) string {
	var out string
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		if !strings.Contains(m.Raw, skillsHeader) {
			return true
		}
		c := m.Get("content")
		if c.Type == gjson.String {
			out = c.String()
			return false
		}
		var b strings.Builder
		c.ForEach(func(_, blk gjson.Result) bool {
			b.WriteString(blk.Get("text").String())
			return true
		})
		out = b.String()
		return false
	})
	return out
}

// skillDecls parses the skill listing out of a {"role":"system"} message.
//
// Fail-safe by construction, because this inventory is the thing a later filter would
// authorise stripping from a real request:
//
//   - The header anchor is required. Without it: no rows at all.
//   - Entries are read only AFTER the header and only until the reminder ends, so the
//     agent-types listing above it (identical shape) cannot leak in.
//   - A name must be a bare skill identifier — letters, digits and `._:-/` (plugin
//     skills are `plugin:skill`, directory-scoped ones `apps/web:deploy`). Anything
//     else is not a name we recognise and is skipped.
//   - A listing that yields NO entries returns the marker row alone, with state
//     `unknown` and the measured size. It never returns an empty-but-confident
//     inventory: "we could not read this" and "there is nothing here" are different
//     answers, and only one of them makes it safe to remove something.
func skillDecls(text string) []Decl {
	i := strings.Index(text, skillsHeader)
	if i < 0 {
		return nil
	}
	body := text[i+len(skillsHeader):]
	if j := strings.Index(body, reminderEnd); j >= 0 {
		body = body[:j]
	}
	marker := Decl{Kind: KindSkillListing, Server: SkillsUnknown,
		Tokens: tokens.Count(skillsHeader + body)}
	out := []Decl{marker}
	// Entries are line-anchored `- name: description`, each running to the next such
	// line. A description containing its own "\n- " line would truncate that entry's
	// weight, so the span is taken to the next line that parses as a NAME.
	lines := strings.Split(body, "\n")
	name, start := "", 0
	flush := func(end int) {
		if name == "" {
			return
		}
		out = append(out, Decl{Kind: KindSkill, Name: name,
			Tokens: tokens.Count(strings.Join(lines[start:end], "\n"))})
	}
	for n, ln := range lines {
		if nm, ok := skillEntryName(ln); ok {
			flush(n)
			name, start = nm, n
		}
	}
	flush(len(lines))
	if len(out) > 1 {
		out[0].Server = SkillsOK
	}
	return out
}

// skillEntryName reads a listing line's name, or reports that the line is not an entry.
func skillEntryName(line string) (string, bool) {
	if !strings.HasPrefix(line, "- ") {
		return "", false
	}
	rest := line[2:]
	i := strings.Index(rest, ":")
	if i <= 0 {
		return "", false
	}
	name := rest[:i]
	// A plugin skill is `plugin:skill`, so one colon may be part of the name: take the
	// longer candidate when the first colon is not followed by a space.
	if !strings.HasPrefix(rest[i:], ": ") {
		if j := strings.Index(rest[i+1:], ": "); j >= 0 {
			name = rest[:i+1+j]
		} else {
			return "", false
		}
	}
	if name == "" || len(name) > 128 {
		return "", false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-' || r == '/':
		default:
			return "", false
		}
	}
	return name, true
}

// usedFrom collects the tool calls of the LAST tool-using turn, plus a fingerprint of
// that turn. Not the whole transcript: the agent resends it every request, so counting
// all of it would multiply every session's call counts by its request count. The
// fingerprint lets the writer recognise the same turn arriving again on the next
// request and skip it.
func usedFrom(provider string, body []byte) ([]Used, uint64) {
	oai := !(provider == "anthropic" || provider == "")
	marker := `"tool_use"`
	if oai {
		marker = `"tool_calls"`
	}
	last := ""
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		if strings.Contains(m.Raw, marker) {
			last = m.Raw
		}
		return true
	})
	if last == "" {
		return nil, 0
	}
	var h maphash.Hash
	h.SetSeed(declSeed)
	h.WriteString(last)
	fp := h.Sum64()

	byKey := map[Used]int{}
	add := func(name, skill string) {
		if name == "" {
			return
		}
		u := Used{Name: name, Skill: skill}
		if server, _, ok := SplitMCPName(name); ok {
			u.Server = server
		}
		byKey[u]++
	}
	msg := gjson.Parse(last)
	if oai {
		msg.Get("tool_calls").ForEach(func(_, c gjson.Result) bool {
			name := c.Get("function.name").String()
			// OpenAI sends arguments as a JSON STRING, so the skill argument needs a
			// second parse rather than a path into the object.
			add(name, skillArg(name, gjson.Parse(c.Get("function.arguments").String())))
			return true
		})
	} else {
		msg.Get("content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() != "tool_use" {
				return true
			}
			name := blk.Get("name").String()
			add(name, skillArg(name, blk.Get("input")))
			return true
		})
	}
	out := make([]Used, 0, len(byKey))
	for u, n := range byKey {
		u.Calls = n
		out = append(out, u)
	}
	return out, fp
}

// skillArg reads the invoked skill's name off a Skill call. The Skill tool's own
// schema has NO enum, so `input.skill` is the only place a skill invocation is
// identifiable. Bounded and charset-checked like a declared name: it is client text.
func skillArg(toolName string, input gjson.Result) string {
	if toolName != "Skill" {
		return ""
	}
	s := input.Get("skill").String()
	if name, ok := skillEntryName("- " + s + ": x"); ok {
		return name
	}
	return ""
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// invMsg is one request's inventory plus the row identity it belongs to.
type invMsg struct {
	tenant, session string
	ts              int64
	inv             *Inventory
}

// invWriter serializes inventory writes off the request path.
//
// The package rule is that one goroutine owns the database; this is a second one, and
// it takes the same discipline: a buffered channel with a `default:` branch (a full
// queue DROPS, it never blocks a request), small transactions, and nothing on the
// request goroutine but the send. It exists rather than writing from the request path
// because finish() runs before the handler RETURNS — a synchronous commit there is
// paid by the next request on a keep-alive connection.
//
// Writes are rare by construction: a declaration set is written once per session (the
// digest dedupes every later request), and a used-tool row once per new name. A
// 65-request session writes ~1 transaction of declarations and a handful of usage
// upserts, not 65 of anything.
type invWriter struct {
	db *DB
	ch chan invMsg
	// seen dedupes at the writer, where it is single-threaded and needs no lock:
	// session -> the digests already written and the last usage turn already counted.
	seen map[string]*invSession
}

type invSession struct {
	digests map[string]bool
	lastFP  uint64
}

const (
	invQueue    = 512
	invMaxSeen  = 4096
	invFlushGap = 2 * time.Second
)

// invWriters holds one writer per Recorder, created on first use. A package-level map
// rather than a Recorder field so this feature is additive to the capture pipeline
// instead of a change to it; the writer still shuts down with its recorder (it selects
// on the recorder's own done channel).
var invWriters sync.Map // *Recorder -> *invWriter

// RecordInventory hands one request's declared/used inventory to the inventory writer.
// Safe on a nil Recorder and from any goroutine, never blocks, never fails: a full
// queue drops and counts, exactly like Record.
func (r *Recorder) RecordInventory(tenant, session string, ts int64, inv *Inventory) {
	if r == nil || inv == nil || session == "" {
		return
	}
	w, ok := invWriters.Load(r)
	if !ok {
		nw := &invWriter{db: r.db, ch: make(chan invMsg, invQueue), seen: map[string]*invSession{}}
		var loaded bool
		w, loaded = invWriters.LoadOrStore(r, nw)
		if !loaded {
			go nw.run(r)
		}
	}
	select {
	case w.(*invWriter).ch <- invMsg{tenant: tenant, session: session, ts: ts, inv: inv}:
	default:
		r.dropped.Add(1)
	}
}

// run drains the queue in batches until the recorder closes.
func (w *invWriter) run(r *Recorder) {
	defer invWriters.Delete(r)
	t := time.NewTicker(invFlushGap)
	defer t.Stop()
	batch := make([]invMsg, 0, 64)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.write(batch); err != nil {
			// Logged through the recorder's error counter, which the dashboard shows: an
			// observability layer that hides its own gaps is worse than one that admits them.
			r.errors.Add(1)
		}
		batch = batch[:0]
	}
	for {
		select {
		case <-r.done:
			// Drain what is already queued, then stop. Best effort: the database may
			// already be closing, and a lost tail of inventory rows is the same class of
			// loss as a dropped event.
			for {
				select {
				case m := <-w.ch:
					batch = append(batch, m)
					continue
				default:
				}
				break
			}
			flush()
			return
		case m := <-w.ch:
			batch = append(batch, m)
			if len(batch) >= 64 {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// write applies one batch in a single transaction, skipping everything already known
// for the session (the digest, and a usage turn already counted).
func (w *invWriter) write(batch []invMsg) error {
	tx, err := w.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	declStmt, err := tx.Prepare(`INSERT INTO tool_declarations(
		tenant_id, session_id, digest, kind, name, server, tokens, ts
	) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	defer declStmt.Close()
	useStmt, err := tx.Prepare(`INSERT INTO tool_uses(
		tenant_id, session_id, name, server, skill, calls, first_ts, last_ts
	) VALUES (?,?,?,?,?,?,?,?)
	ON CONFLICT(session_id, name, skill) DO UPDATE SET
		calls = calls + excluded.calls, last_ts = excluded.last_ts`)
	if err != nil {
		return err
	}
	defer useStmt.Close()

	for _, m := range batch {
		st := w.session(m.session)
		if !st.digests[m.inv.Digest] {
			st.digests[m.inv.Digest] = true
			for _, d := range m.inv.Decls {
				if _, err := declStmt.Exec(m.tenant, m.session, m.inv.Digest,
					d.Kind, d.Name, d.Server, d.Tokens, m.ts); err != nil {
					return err
				}
			}
		}
		if m.inv.UseFingerprint == 0 || m.inv.UseFingerprint == st.lastFP {
			continue // same last tool-using turn as the previous request: already counted
		}
		st.lastFP = m.inv.UseFingerprint
		for _, u := range m.inv.Used {
			if _, err := useStmt.Exec(m.tenant, m.session, u.Name, u.Server, u.Skill,
				u.Calls, m.ts, m.ts); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// session returns the writer's dedup state for a session. Bounded the same way the
// tokenizer's cache is: cleared wholesale past the cap, because a re-warm costs one
// conflicting INSERT per live session and an LRU costs bookkeeping forever.
func (w *invWriter) session(id string) *invSession {
	if st := w.seen[id]; st != nil {
		return st
	}
	if len(w.seen) >= invMaxSeen {
		w.seen = map[string]*invSession{}
	}
	st := &invSession{digests: map[string]bool{}}
	w.seen[id] = st
	return st
}
