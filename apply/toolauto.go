package apply

// Toolfilter auto mode: remove declarations the account's own history says it never calls, and
// give the model a way to get one back.
//
// # Cache discipline (the whole design)
//
// `tools` hashes first, so ANY change to it rewrites the whole prefix at 1.25x. So:
//
//   - the removed set is chosen ONCE per session, on the first request (no assistant turn yet:
//     that request is a cold start anyway) or on the first proven-cold request (ColdCache: the
//     prefix is being rewritten anyway), and stored; every later request applies the same set,
//     whatever the history now says. A warm session mid-flight is never touched.
//   - a restore is the only later change. It happens ONCE per restore call, on the upstream
//     request that continues that very turn (the model needs the tool now, so waiting for a cold
//     turn is not an option), batches every name the model asked for, and re-renders the stub in
//     the same rewrite. Cost: one full-prefix write (1.25x of the context) per restore event,
//     which the model chose to pay by calling the stub. Restores are sticky for the session.
//   - never-worse guard: nothing is removed unless the removed declarations weigh at least
//     autoMinGain x the stub that replaces them.
//
// Never removed: context_guru_* tools, tools called anywhere in this transcript, tools named in the
// system prompt's prose, a tool forced by tool_choice (all via filterDeclarations), and nothing at
// all is decided under a non-auto tool_choice. Anthropic dialect only.
//
// Determinism caveats: the prose gate and tool_choice are re-evaluated per request, so the fixed set holds
// only while they stay constant; a pending call to a hidden tool makes filterDeclarations skip that one
// request. Threads: the stub and any restore are limited to tools removed from the request's own body.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RestoreToolName is the stub tool the model calls to get a removed tool back.
const RestoreToolName = "context_guru_tools"

// autoMinGain: removed tokens must be at least this multiple of the stub's own tokens.
const autoMinGain = 2

// autoRec is the session's record. Removed is the name set the first thread decided on; every
// thread (main, each subagent: identified by its own tool-name set) then keeps its OWN state, so a
// restore by one thread never changes the bytes another thread sends.
type autoRec struct {
	Removed []string               `json:"removed"`
	Threads map[string]*autoThread `json:"threads"`
}

type autoThread struct {
	Tools    []json.RawMessage `json:"tools"`   // this thread's full declarations, as it sent them
	Removed  []string          `json:"removed"` // names actually dropped from this thread (post prose gate)
	Restored []string          `json:"restored,omitempty"`
}

// threadKey identifies a thread by the set of tools its client declares (proxy tools excluded).
func threadKey(names []string) string {
	var ns []string
	for _, n := range names {
		if !strings.HasPrefix(n, "context_guru_") {
			ns = append(ns, n)
		}
	}
	sort.Strings(ns)
	h := sha256.Sum256([]byte(strings.Join(ns, "\x00")))
	return hex.EncodeToString(h[:8])
}

func loadAuto(st store.Store, session string) *autoRec {
	b, ok := st.Get(store.AutoToolPrefix + session)
	if !ok {
		return nil
	}
	var r autoRec
	if json.Unmarshal(b, &r) != nil || len(r.Removed) == 0 {
		return nil
	}
	return &r
}

func saveAuto(st store.Store, session string, r *autoRec) {
	if b, err := json.Marshal(r); err == nil {
		st.Put(store.AutoToolPrefix+session, b)
	}
}

func (r *autoThread) active() map[string]bool {
	gone := map[string]bool{}
	for _, n := range r.Removed {
		gone[n] = true
	}
	for _, n := range r.Restored {
		delete(gone, n)
	}
	return gone
}

func declNames(arr gjson.Result) []string {
	var out []string
	arr.ForEach(func(_, t gjson.Result) bool { out = append(out, declName(t)); return true })
	return out
}

// calledInTranscript returns every tool name the transcript has already called.
func calledInTranscript(body []byte) map[string]bool {
	out := map[string]bool{}
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_use" {
				out[b.Get("name").String()] = true
			}
			return true
		})
		return true
	})
	return out
}

func hasAssistantTurn(body []byte) bool {
	found := false
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		found = m.Get("role").String() == "assistant"
		return !found
	})
	return found
}

func toolChoiceAutoOrAbsent(body []byte) bool {
	tc := gjson.GetBytes(body, "tool_choice")
	return !tc.Exists() || tc.Get("type").String() == "auto"
}

// stubDef renders the restore tool for the declarations still removed. Deterministic: declaration
// order, fixed text, so the bytes are a pure function of the stored record.
func stubDef(removed []json.RawMessage) []byte {
	var b strings.Builder
	b.WriteString("These tools are NOT available until you restore them (Context Guru hid them to save tokens). " +
		"Before you need one, call this tool with its exact name(s); it then becomes callable. Hidden: ")
	for i, raw := range removed {
		if i > 0 {
			b.WriteString("; ")
		}
		d := gjson.ParseBytes(raw).Get("description").String()
		if j := strings.IndexAny(d, ".\n"); j >= 0 {
			d = d[:j]
		}
		if r := []rune(d); len(r) > 70 {
			d = string(r[:70])
		}
		b.WriteString(declName(gjson.ParseBytes(raw)) + " - " + strings.TrimSpace(d))
	}
	def := map[string]any{} // keys marshal sorted: stable
	def["name"] = RestoreToolName
	def["description"] = b.String()
	def["input_schema"] = map[string]any{
		"type": "object",
		"properties": map[string]any{"names": map[string]any{
			"type": "array", "items": map[string]any{"type": "string", "enum": enumNames(removed)},
			"description": "Exact names of the hidden tools to restore."}},
		"required": []string{"names"},
	}
	out, _ := json.Marshal(def)
	return out
}

func enumNames(removed []json.RawMessage) []string {
	out := []string{}
	for _, r := range removed {
		out = append(out, declName(gjson.ParseBytes(r)))
	}
	return out
}

func rawByName(tools []json.RawMessage, names map[string]bool) (out []json.RawMessage) {
	for _, t := range tools {
		if names[declName(gjson.ParseBytes(t))] {
			out = append(out, t)
		}
	}
	return out
}

// AutoFilterTools applies auto mode to one outgoing Anthropic body. eligible is the account's
// history-derived candidate list; cold is Trace.ColdCache. Fails open: returns the input.
func AutoFilterTools(st store.Store, session string, body []byte, eligible []string, cold bool) (out []byte, removedTokens, removed int) {
	tools := gjson.GetBytes(body, "tools")
	if st == nil || session == "" || !tools.IsArray() || len(tools.Array()) == 0 {
		return body, 0, 0
	}
	for _, n := range declNames(tools) {
		if n == RestoreToolName {
			return body, 0, 0
		}
	}
	rec := loadAuto(st, session)
	if rec == nil {
		if len(eligible) == 0 || !(cold || !hasAssistantTurn(body)) || !toolChoiceAutoOrAbsent(body) {
			return body, 0, 0
		}
		called := calledInTranscript(body)
		var want []string
		for _, n := range eligible {
			if !called[n] && !strings.HasPrefix(n, "context_guru_") {
				want = append(want, n)
			}
		}
		kept, tok, n := filterDeclarations(body, want)
		if n == 0 {
			return body, 0, 0
		}
		left := map[string]bool{}
		for _, k := range declNames(gjson.GetBytes(kept, "tools")) {
			left[k] = true
		}
		var gone []string
		for _, k := range declNames(tools) {
			if !left[k] {
				gone = append(gone, k)
			}
		}
		th := &autoThread{Removed: gone}
		tools.ForEach(func(_, t gjson.Result) bool { th.Tools = append(th.Tools, json.RawMessage(t.Raw)); return true })
		r := &autoRec{Removed: gone, Threads: map[string]*autoThread{threadKey(declNames(tools)): th}}
		stub := stubDef(rawByName(th.Tools, th.active())) // same body: snapshot == this request
		if tok < autoMinGain*tokens.Count(string(stub)) {
			return body, 0, 0
		}
		saveAuto(st, session, r)
		next, err := sjson.SetRawBytes(kept, "tools.-1", stub)
		if err != nil {
			return body, 0, 0
		}
		return next, tok - tokens.Count(string(stub)), n
	}
	// Fixed set, per thread. A thread seen for the first time (a subagent) is filtered by the
	// session's name set against ITS OWN tools; from then on its removed/restored sets are its own.
	key := threadKey(declNames(tools))
	th := rec.Threads[key]
	if th == nil {
		kept, _, n := filterDeclarations(body, rec.Removed)
		if n == 0 {
			return body, 0, 0
		}
		left := map[string]bool{}
		for _, k := range declNames(gjson.GetBytes(kept, "tools")) {
			left[k] = true
		}
		th = &autoThread{}
		for _, k := range declNames(tools) {
			if !left[k] {
				th.Removed = append(th.Removed, k)
			}
		}
		tools.ForEach(func(_, t gjson.Result) bool { th.Tools = append(th.Tools, json.RawMessage(t.Raw)); return true })
		rec.Threads[key] = th
		saveAuto(st, session, rec)
	}
	// Restores the client saw (a stub call co-called with a client tool) are picked up from the
	// transcript.
	th.Restored = append(th.Restored, transcriptRestores(body)...)
	act := th.active()
	if len(act) == 0 {
		return body, 0, 0
	}
	var names []string
	for n := range act {
		names = append(names, n)
	}
	kept, tok, n := filterDeclarations(body, names)
	if n == 0 {
		return body, 0, 0
	}
	// The stub names only what was removed from THIS request's own tools (a subagent thread has a
	// narrower list than the main thread whose snapshot is stored): never a tool it was not given.
	left := map[string]bool{}
	for _, k := range declNames(gjson.GetBytes(kept, "tools")) {
		left[k] = true
	}
	mine := map[string]bool{}
	for _, k := range declNames(tools) {
		if !left[k] {
			mine[k] = true
		}
	}
	var own []json.RawMessage
	tools.ForEach(func(_, t gjson.Result) bool {
		if mine[declName(t)] {
			own = append(own, json.RawMessage(t.Raw))
		}
		return true
	})
	stub := stubDef(own)
	next, err := sjson.SetRawBytes(kept, "tools.-1", stub)
	if err != nil {
		return body, 0, 0
	}
	return next, tok - tokens.Count(string(stub)), n
}

// transcriptRestores lists names a stub call in the transcript asked for.
func transcriptRestores(body []byte) (out []string) {
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_use" && b.Get("name").String() == RestoreToolName {
				b.Get("input.names").ForEach(func(_, n gjson.Result) bool { out = append(out, n.String()); return true })
			}
			return true
		})
		return true
	})
	return out
}

// RestoreCall is one model call to the stub.
type RestoreCall struct {
	CallID string
	Names  []string
}

// RestoreCalls finds the stub calls in an Anthropic response message.
func RestoreCalls(msg []byte) (out []RestoreCall) {
	gjson.GetBytes(msg, "content").ForEach(func(_, b gjson.Result) bool {
		if b.Get("type").String() == "tool_use" && b.Get("name").String() == RestoreToolName {
			c := RestoreCall{CallID: b.Get("id").String()}
			b.Get("input.names").ForEach(func(_, n gjson.Result) bool { c.Names = append(c.Names, n.String()); return true })
			out = append(out, c)
		}
		return true
	})
	return out
}

// RestoreTools re-inserts only the requested names into THIS request's own tools array, and only
// those the request's own stub lists as hidden (its enum), so a restricted thread (a subagent with a
// narrower allowlist) can never gain a tool it was not declared. Each name goes back after the nearest
// preceding tool of the stored snapshot that is present, i.e. its original relative position; the stub
// is re-rendered for what is still hidden. answer is the stub call's tool_result text.
func RestoreTools(st store.Store, session string, body []byte, names []string) (out []byte, answer string) {
	rec := loadAuto(st, session)
	if rec == nil {
		return body, "No tools are hidden in this session."
	}
	stubAt := -1
	hidden := map[string]bool{}
	var cur []string
	gjson.GetBytes(body, "tools").ForEach(func(i, t gjson.Result) bool {
		if declName(t) == RestoreToolName {
			stubAt = int(i.Int())
			t.Get("input_schema.properties.names.items.enum").ForEach(func(_, e gjson.Result) bool {
				hidden[e.String()] = true
				return true
			})
			return true
		}
		cur = append(cur, t.Raw)
		return true
	})
	if stubAt < 0 {
		return body, "No tools are hidden in this request."
	}
	all := make([]string, 0, len(cur)+len(hidden))
	for _, c := range cur {
		all = append(all, declName(gjson.Parse(c)))
	}
	for n := range hidden {
		all = append(all, n)
	}
	th := rec.Threads[threadKey(all)]
	if th == nil {
		return body, "No tools are hidden in this request."
	}
	snap := map[string]json.RawMessage{}
	var order []string
	for _, t := range th.Tools {
		n := declName(gjson.ParseBytes(t))
		snap[n] = t
		order = append(order, n)
	}
	idx := map[string]int{}
	for i, n := range order {
		idx[n] = i
	}
	var ok, no []string
	for _, n := range names {
		if hidden[n] && snap[n] != nil {
			ok = append(ok, n)
			th.Restored = append(th.Restored, n)
			delete(hidden, n)
		} else {
			no = append(no, n)
		}
	}
	if len(ok) == 0 {
		return body, "Not hidden (nothing changed): " + strings.Join(no, ", ")
	}
	saveAuto(st, session, rec)
	present := map[string]bool{}
	for _, c := range cur {
		present[declName(gjson.Parse(c))] = true
	}
	// Insert in snapshot order so the restored names keep their original relative order.
	for _, n := range order {
		isOK := false
		for _, o := range ok {
			isOK = isOK || o == n
		}
		if !isOK || present[n] {
			continue
		}
		pos := 0 // after the nearest preceding snapshot tool that is present in this request
		for k := idx[n] - 1; k >= 0 && pos == 0; k-- {
			for j, c := range cur {
				if declName(gjson.Parse(c)) == order[k] {
					pos = j + 1
					break
				}
			}
		}
		cur = append(cur[:pos], append([]string{string(snap[n])}, cur[pos:]...)...)
		present[n] = true
	}
	// Stub first, then the proxy's other injected tools (expand): the order AutoFilterTools +
	// expand.Inject produce on the client's next request.
	var stillHidden []json.RawMessage
	for _, n := range order {
		if hidden[n] {
			stillHidden = append(stillHidden, snap[n])
		}
	}
	arr := cur
	if len(stillHidden) > 0 {
		at := len(arr) // before expand, as on the client path
		for j, c := range arr {
			if declName(gjson.Parse(c)) == "context_guru_expand" {
				at = j
				break
			}
		}
		arr = append(arr[:at], append([]string{string(stubDef(stillHidden))}, arr[at:]...)...)
	}
	next, err := sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(arr, ",")+"]"))
	if err != nil {
		return body, "Restore failed; continue without: " + strings.Join(names, ", ")
	}
	ans := "Restored: " + strings.Join(ok, ", ") + ". Call them directly now."
	if len(no) > 0 {
		ans += " Not hidden: " + strings.Join(no, ", ") + "."
	}
	return next, ans
}
