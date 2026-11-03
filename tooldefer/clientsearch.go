package tooldefer

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/rossoctl/context-guru/internal/tokens"
	cgschema "github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Our own tool search, for models that ignore defer_loading (#825, "option B").
//
// GPT, GLM and Kimi render every declared tool in full: through our gateway, defer_loading saves
// nothing on Chat Completions, and only gpt-5.4+ has a search of its own (on the Responses API). So
// on these models a deferred tool is REMOVED from `tools`, and the model loads it through our
// search tool, context_guru_tools, which the host answers itself:
//
//   - At the thread's first request the rule (Decide, unchanged) picks the set. The tools in it are
//     removed, the search tool goes in after the client's tools, and a names list goes in at a
//     fixed early place: a system message after the leading system or developer message (Chat
//     Completions), or a developer message at input[0] (Responses). Same bytes on every request.
//   - When the model calls the search tool, the host adds the named tools back and answers the call
//     in band, on a follow-up request the client never sees. A tool added back stays, at the END of
//     `tools`, for the rest of the thread: the record changes once, on purpose. That is the one cache
//     miss this design accepts; at the end of `tools`, the tools before it stay cached.
//   - A load counts as a use (NoteLoads): the user's later sessions keep the tool loaded.
//   - A search in the same response as a client tool call goes to the client whole: the host cannot
//     continue a turn whose other call only the client can run. The tools are added back at once,
//     and the client's answer to our call (an "unknown tool" error) is replaced with ours on every
//     later request (repairSearchResults), with the same bytes each time.
//
// The rule and the history read Anthropic-shaped bodies, so both read View: the request with its
// tools and tool calls written the Anthropic way. Fail open: any problem returns the input.

// ClientSearchName is our search tool.
const ClientSearchName = "context_guru_tools"

const clientSearchDesc = "Load tools that Context Guru deferred. Pass the exact names from the deferred tools list. " +
	"The tools are declared after this call; then call them directly."

const clientSearchParams = `{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},` +
	`"description":"Exact names of the deferred tools to load."}},"required":["names"]}`

// clientSearchTool is the search tool's declaration on one wire. Fixed bytes: it sits in `tools`.
func clientSearchTool(wire string) string {
	if cgschema.IsResponses(wire) {
		return `{"type":"function","name":"` + ClientSearchName + `","description":"` + clientSearchDesc +
			`","parameters":` + clientSearchParams + `}`
	}
	return `{"type":"function","function":{"name":"` + ClientSearchName + `","description":"` + clientSearchDesc +
		`","parameters":` + clientSearchParams + `}}`
}

// clientListHead starts the names list of our search. Fixed text.
const clientListHead = "Context Guru deferred these tools to save context. They are not declared yet. " +
	"Before you call one, call " + ClientSearchName + " with its exact name in names. " +
	"Then call it directly. Deferred tools:"

// ClientNamesList is the names list for a sorted set of names.
func ClientNamesList(names []string) string {
	return clientListHead + "\n" + strings.Join(names, "\n")
}

// ClientWire reports whether our search runs on this wire: Chat Completions or Responses.
func ClientWire(wire string) bool { return wire == cgschema.WireOpenAI || cgschema.IsResponses(wire) }

// toolName is a tool element's name on wire, and whether it is a client function tool (the only kind
// our search defers).
func toolName(wire string, t gjson.Result) (string, bool) {
	if t.Get("type").String() != "function" {
		return "", false
	}
	if cgschema.IsResponses(wire) {
		n := t.Get("name").String()
		return n, n != ""
	}
	n := t.Get("function.name").String()
	return n, n != ""
}

// forcedClientTool is the function tool_choice forces, or "".
func forcedClientTool(wire string, body []byte) string {
	tc := gjson.GetBytes(body, "tool_choice")
	if !tc.IsObject() || tc.Get("type").String() != "function" {
		return ""
	}
	if cgschema.IsResponses(wire) {
		return tc.Get("name").String()
	}
	return tc.Get("function.name").String()
}

// View is body written the Anthropic way, for the rule and the history: each function tool as an
// element with a top-level name (its own bytes, so its weight is about the same), every other tool
// with its type (the rule leaves those alone), tool_choice as {"type":"tool"}, and one message per
// transcript item with a tool_use block for each tool call. nil when body has no tools array.
func View(wire string, body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return nil
	}
	var elems []string
	tools.ForEach(func(_, t gjson.Result) bool {
		n, fn := toolName(wire, t)
		if !fn {
			elems = append(elems, t.Raw)
			return true
		}
		raw, err := sjson.Delete(t.Raw, "type")
		if err == nil {
			raw, err = sjson.Set(raw, "name", n)
		}
		if err != nil {
			return true
		}
		elems = append(elems, raw)
		return true
	})
	var msgs []string
	use := func(role string, names []string) {
		m := `{"role":"` + role + `","content":[]}`
		for _, n := range names {
			b, _ := sjson.Set(`{"type":"tool_use"}`, "name", n)
			m, _ = sjson.SetRaw(m, "content.-1", b)
		}
		msgs = append(msgs, m)
	}
	if cgschema.IsResponses(wire) {
		gjson.GetBytes(body, "input").ForEach(func(_, it gjson.Result) bool {
			switch it.Get("type").String() {
			case "function_call", "custom_tool_call":
				use("assistant", []string{it.Get("name").String()})
			default:
				role := it.Get("role").String()
				if role == "" {
					role = "user"
				}
				use(role, nil)
			}
			return true
		})
	} else {
		gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
			var names []string
			m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
				names = append(names, tc.Get("function.name").String())
				return true
			})
			use(m.Get("role").String(), names)
			return true
		})
	}
	out := `{"tools":[` + strings.Join(elems, ",") + `],"messages":[` + strings.Join(msgs, ",") + `]}`
	if f := forcedClientTool(wire, body); f != "" {
		out, _ = sjson.Set(out, "tool_choice", map[string]string{"type": "tool", "name": f})
	}
	return []byte(out)
}

// LoadView is a body that says "these tools were declared and used", for the history: a load
// through our search counts as a use, and the client's transcript never shows an in-band load.
func LoadView(names []string) []byte {
	out := `{"tools":[],"messages":[{"role":"assistant","content":[]}]}`
	for _, n := range names {
		t, _ := sjson.Set(`{}`, "name", n)
		out, _ = sjson.SetRaw(out, "tools.-1", t)
		b, _ := sjson.Set(`{"type":"tool_use"}`, "name", n)
		out, _ = sjson.SetRaw(out, "messages.0.content.-1", b)
	}
	return []byte(out)
}

// ── the thread record of our search ─────────────────────────────────────

// SearchRecord is one thread's decision for our search. Tool names only: metadata.
type SearchRecord struct {
	// Deferred is the set decided at the thread's first request. It never changes; the names list
	// is built from it.
	Deferred []string `json:"d"`
	// Added is the deferred tools a search loaded, in the order they were loaded. They go at the end
	// of `tools`, in this order, from then on.
	Added []string `json:"a,omitempty"`
	// Calls is the searches the client received (made together with a client tool call), by call
	// id: the names each asked for. A chained Responses turn carries the client's answer to such a
	// call but not the call, so the repair reads the names here. Bounded by maxCalls.
	Calls map[string][]string `json:"c,omitempty"`
}

// maxCalls bounds SearchRecord.Calls; past it the record keeps the calls it has.
const maxCalls = 32

// recMu serialises the read-modify-write of a record (AddBack, NoteCalls): two requests of one
// thread at once must not both report a tool as added now.
var recMu sync.Mutex

// searchKey is the store key of one thread's SearchRecord: apart from the Record of the
// defer_loading path, under the same pinned prefix, so the disk store keeps it and a purge drops it.
func searchKey(session, thread string) string { return Key(session, thread) + "/s" }

// LoadSearch reads a thread's SearchRecord. ok=false on a miss.
func LoadSearch(st store.Store, session, thread string) (SearchRecord, bool) {
	if st == nil || session == "" {
		return SearchRecord{}, false
	}
	b, ok := st.Get(searchKey(session, thread))
	if !ok {
		return SearchRecord{}, false
	}
	var r SearchRecord
	if json.Unmarshal(b, &r) != nil {
		return SearchRecord{}, false
	}
	return r, true
}

// SaveSearch writes a thread's SearchRecord.
func SaveSearch(st store.Store, session, thread string, r SearchRecord) {
	if st == nil || session == "" {
		return
	}
	if r.Deferred == nil {
		r.Deferred = []string{}
	}
	if b, err := json.Marshal(r); err == nil {
		st.Put(searchKey(session, thread), b)
	}
}

// AddBack adds names to the thread's Added list, keeping the order, and returns the names it added
// now: the deferred ones not added before. A name outside the set is ignored.
func AddBack(st store.Store, session, thread string, names []string) []string {
	recMu.Lock()
	defer recMu.Unlock()
	rec, ok := LoadSearch(st, session, thread)
	if !ok {
		return nil
	}
	set := map[string]bool{}
	for _, n := range rec.Deferred {
		set[n] = true
	}
	for _, n := range rec.Added {
		delete(set, n)
	}
	var now []string
	for _, n := range names {
		if set[n] {
			delete(set, n)
			now = append(now, n)
		}
	}
	if len(now) > 0 {
		rec.Added = append(rec.Added, now...)
		SaveSearch(st, session, thread, rec)
	}
	return now
}

// NoteCalls keeps the searches the client received, by call id, so a later request can answer
// them even when it does not carry the call (repairSearchResults).
func NoteCalls(st store.Store, session, thread string, calls []SearchCall) {
	recMu.Lock()
	defer recMu.Unlock()
	rec, ok := LoadSearch(st, session, thread)
	if !ok {
		return
	}
	changed := false
	for _, c := range calls {
		if c.ID == "" || len(rec.Calls) >= maxCalls {
			continue
		}
		if _, dup := rec.Calls[c.ID]; dup {
			continue
		}
		if rec.Calls == nil {
			rec.Calls = map[string][]string{}
		}
		rec.Calls[c.ID] = append([]string{}, c.Names...)
		changed = true
	}
	if changed {
		SaveSearch(st, session, thread, rec)
	}
}

// ── the request side ─────────────────────────────────────────────────────

// ApplySearch runs our search on one Chat Completions or Responses request. The checks are those
// of Apply, less the probe (our search needs no provider feature) and with the model test turned
// round: Claude models use Apply's defer_loading path.
//
// Before the rewrite it answers, on the request side, any call to our search the client received
// (a search together with another tool call, or a response the host could not hold), and adds
// those tools back. Fail open: on any problem Body is the input.
func ApplySearch(env Env, wire string, r Req) (o Outcome, loaded []string) {
	if r.Body == nil {
		r.Body = r.Orig
	}
	o.Body = r.Body
	defer func() {
		if p := recover(); p != nil {
			o, loaded = Outcome{Body: r.Body, Why: "panic"}, nil
		}
	}()
	switch {
	case !ClientWire(wire):
		o.Why = "wire"
		return o, nil
	case r.Session == "":
		o.Why = "no_session"
		return o, nil
	case env.RecordStore() == nil:
		o.Why = "no_durable_store"
		return o, nil
	case r.AccountOff:
		o.Why = "account_off"
		return o, nil
	case AnthropicModel(r.Model):
		o.Why = "anthropic"
		return o, nil
	case env.Gate.Broken(r.Tenant, r.Model):
		o.Why = "turned_off_after_400"
		return o, nil
	case !gjson.GetBytes(r.Orig, "tools").IsArray():
		o.Why = "no_tools"
		return o, nil
	}
	// A Responses turn that chains on previous_response_id holds only the new items: the transcript,
	// the names list included, is stored at the provider. Such a turn never makes a decision. But a
	// thread our search already rewrote keeps it: its turns must declare the same tools (the search
	// tool, and the tools added back), or the names list the model reads points at a tool that is
	// gone. The host holds the response and answers a search in band, as on any other turn.
	chained := gjson.GetBytes(r.Orig, "previous_response_id").Exists()
	if !chained && cgschema.IsResponses(wire) && !gjson.GetBytes(r.Orig, "input").IsArray() {
		o.Why = "input_not_array"
		return o, nil
	}
	rs := env.RecordStore()
	if l, isL := rs.(interface{ LoadSession(string) }); isL {
		l.LoadSession(r.Session)
	}
	rec, ok := LoadSearch(rs, r.Session, r.Thread)
	if !ok && chained {
		o.Why = "previous_response_id"
		return o, nil
	}
	if !ok {
		if SearchOff(rs, r.Session) || env.Gate.TakePending(r.Tenant, r.Session, rs) {
			o.Why = "tool_search_false"
			SaveSearch(rs, r.Session, r.Thread, SearchRecord{})
			return o, nil
		}
		// No record: this is the thread's first request, or its record is gone (the records are
		// durable, so that is rare). Either way the rule decides now. The rule leaves out every tool
		// the transcript has called, so a re-decision cannot remove a tool the model is using.
		view := View(wire, r.Orig)
		rec = SearchRecord{Deferred: Decide(env.History, Input{Tenant: r.Tenant, Project: r.Project, Body: view,
			Session: r.Session, Keep: r.Keep})}
		SaveSearch(rs, r.Session, r.Thread, rec)
		o.Decided = true
		if env.Gate != nil {
			env.Gate.Decisions.Add(1)
		}
	}
	if len(rec.Deferred) == 0 {
		if o.Why == "" {
			o.Why = "nothing_to_defer"
		}
		return o, nil
	}
	set := make(map[string]bool, len(rec.Deferred))
	for _, n := range rec.Deferred {
		set[n] = true
	}
	body := r.Body
	// A call to our search that reached the client: answer it here, and add its tools back.
	body, asked := repairSearchResults(wire, body, set, rec.Calls)
	// A tool tool_choice forces must be declared: it is added back for good.
	if f := forcedClientTool(wire, body); f != "" {
		asked = append(asked, f)
	}
	if len(asked) > 0 {
		loaded = AddBack(rs, r.Session, r.Thread, asked)
		if len(loaded) > 0 {
			rec, _ = LoadSearch(rs, r.Session, r.Thread)
		}
	}
	out, removed, changed := rewriteSearch(wire, body, rec, !chained)
	if !changed {
		o.Why = "rewrite_declined"
		return o, loaded
	}
	weigh := map[string]bool{}
	for _, n := range removed {
		weigh[n] = true
	}
	gjson.GetBytes(r.Orig, "tools").ForEach(func(_, t gjson.Result) bool {
		if n, fn := toolName(wire, t); fn && weigh[n] {
			o.DeferredTokens += tokens.Count(t.Raw)
		}
		return true
	})
	o.Body, o.Changed, o.Deferred = out, true, removed
	o.ListTokens = tokens.Count(ClientNamesList(rec.Deferred))
	if env.Gate != nil {
		env.Gate.Deferred.Add(1)
	}
	return o, loaded
}

// RewriteSearch removes the deferred tools that are not added back, appends our search tool and
// then the added tools in load order, and puts the names list in place. It returns the names it
// removed, sorted. The client's other tools keep their order and their bytes. ok=false: out is body.
func RewriteSearch(wire string, body []byte, rec SearchRecord) (out []byte, removed []string, ok bool) {
	return rewriteSearch(wire, body, rec, true)
}

// rewriteSearch is RewriteSearch; list=false leaves the names list out (a chained Responses turn:
// the list is in the transcript the provider stored).
func rewriteSearch(wire string, body []byte, rec SearchRecord, list bool) (out []byte, removed []string, ok bool) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(rec.Deferred) == 0 {
		return body, nil, false
	}
	set := map[string]bool{}
	for _, n := range rec.Deferred {
		set[n] = true
	}
	decl := map[string]string{} // a deferred tool's declaration, by name
	var elems []string
	hasSearch := false
	tools.ForEach(func(_, t gjson.Result) bool {
		n, fn := toolName(wire, t)
		switch {
		case fn && n == ClientSearchName:
			hasSearch = true // a body that already went through here (a follow-up): kept as it is
			elems = append(elems, t.Raw)
		case fn && set[n]:
			if _, dup := decl[n]; !dup {
				decl[n] = t.Raw
			}
		default:
			elems = append(elems, t.Raw)
		}
		return true
	})
	if hasSearch {
		return body, nil, false // already rewritten: never twice
	}
	elems = append(elems, clientSearchTool(wire))
	added := map[string]bool{}
	for _, n := range rec.Added {
		if raw, ok := decl[n]; ok && !added[n] {
			elems = append(elems, raw)
			added[n] = true
		}
	}
	for n := range decl {
		if !added[n] {
			removed = append(removed, n)
		}
	}
	sort.Strings(removed)
	out, err := sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(elems, ",")+"]"))
	if err != nil {
		return body, nil, false
	}
	if !list {
		return out, removed, true
	}
	names := append([]string(nil), rec.Deferred...)
	sort.Strings(names)
	if out, ok = placeClientList(wire, out, ClientNamesList(names)); !ok {
		return body, nil, false
	}
	return out, removed, true
}

// placeClientList puts the names list at its fixed early place. Chat Completions: a system message
// after the leading system or developer message, else at messages[0]. Responses: a developer
// message at input[0] (the system prompt is in `instructions`, ahead of input).
func placeClientList(wire string, body []byte, list string) ([]byte, bool) {
	path, at, msg := "messages", 0, ""
	if cgschema.IsResponses(wire) {
		path = "input"
		b, _ := sjson.Set(`{"type":"message","role":"developer","content":[{"type":"input_text"}]}`, "content.0.text", list)
		msg = b
	} else {
		b, _ := sjson.Set(`{"role":"system"}`, "content", list)
		msg = b
		if r := gjson.GetBytes(body, "messages.0.role").String(); r == "system" || r == "developer" {
			at = 1
		}
	}
	arr := gjson.GetBytes(body, path)
	if !arr.IsArray() {
		return body, false
	}
	items := arr.Array()
	elems := make([]string, 0, len(items)+1)
	for i, m := range items {
		if i == at {
			elems = append(elems, msg)
		}
		elems = append(elems, m.Raw)
	}
	if at >= len(items) {
		elems = append(elems, msg)
	}
	out, err := sjson.SetRawBytes(body, path, []byte("["+strings.Join(elems, ",")+"]"))
	return out, err == nil
}

// ── the response side ────────────────────────────────────────────────────

// SearchCall is one call to our search tool in a response.
type SearchCall struct {
	ID    string
	Names []string
}

// SearchCalls reads one response (a Chat Completions object or a Responses object): the calls to our
// search, and whether it called a client tool too. proxyTools are the host's other tools: a call to
// one of them is not a client call.
func SearchCalls(wire string, msg []byte, proxyTools ...string) (calls []SearchCall, other bool) {
	add := func(id, name, args string) {
		if name != ClientSearchName {
			for _, p := range proxyTools {
				if name == p {
					return
				}
			}
			other = true
			return
		}
		calls = append(calls, SearchCall{ID: id, Names: searchArgs(args)})
	}
	if cgschema.IsResponses(wire) {
		gjson.GetBytes(msg, "output").ForEach(func(_, it gjson.Result) bool {
			switch it.Get("type").String() {
			case "function_call":
				add(it.Get("call_id").String(), it.Get("name").String(), it.Get("arguments").String())
			case "custom_tool_call":
				add(it.Get("call_id").String(), it.Get("name").String(), "")
			}
			return true
		})
		return calls, other
	}
	gjson.GetBytes(msg, "choices.0.message.tool_calls").ForEach(func(_, tc gjson.Result) bool {
		add(tc.Get("id").String(), tc.Get("function.name").String(), tc.Get("function.arguments").String())
		return true
	})
	return calls, other
}

// searchArgs reads the names out of our search call's arguments: {"names":[...]}, or a lone
// {"name":"..."} from a model that guessed the shape.
func searchArgs(args string) []string {
	var out []string
	if v := gjson.Get(args, "names"); v.Type == gjson.String {
		args = `{"names":[` + v.Raw + `]}` // a lone name as a string
	}
	gjson.Get(args, "names").ForEach(func(_, n gjson.Result) bool {
		if s := strings.TrimSpace(n.String()); s != "" {
			out = append(out, s)
		}
		return true
	})
	if s := strings.TrimSpace(gjson.Get(args, "name").String()); s != "" && len(out) == 0 {
		out = append(out, s)
	}
	return out
}

// SearchAnswer is the result our search returns for names. It depends only on the thread's fixed
// set, so a repaired answer has the same bytes on every request.
func SearchAnswer(names []string, deferred map[string]bool) string {
	var ok, unknown []string
	for _, n := range names {
		if deferred[n] {
			ok = append(ok, n)
		} else {
			unknown = append(unknown, n)
		}
	}
	var b strings.Builder
	if len(ok) > 0 {
		b.WriteString("Loaded: " + strings.Join(ok, ", ") + ". They are declared now; call them directly.")
	}
	if len(unknown) > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString("Not deferred, so nothing to load: " + strings.Join(unknown, ", ") +
			". A tool you have is already declared; call it directly.")
	}
	if b.Len() == 0 {
		b.WriteString("No names given. Pass the exact names from the deferred tools list.")
	}
	return b.String()
}

// AppendTools appends the declarations of names, taken from src (the body before our rewrite), to
// the end of body's tools, in the order given. This is what RewriteSearch does with the Added list,
// so the follow-up and the client's next request carry the same `tools`.
func AppendTools(wire string, body, src []byte, names []string) ([]byte, bool) {
	if len(names) == 0 {
		return body, true
	}
	decl := map[string]string{}
	gjson.GetBytes(src, "tools").ForEach(func(_, t gjson.Result) bool {
		if n, fn := toolName(wire, t); fn {
			if _, dup := decl[n]; !dup {
				decl[n] = t.Raw
			}
		}
		return true
	})
	out := body
	for _, n := range names {
		raw, ok := decl[n]
		if !ok {
			continue
		}
		var err error
		if out, err = sjson.SetRawBytes(out, "tools.-1", []byte(raw)); err != nil {
			return body, false
		}
	}
	return out, true
}

// repairSearchResults answers, on the request side, every call to our search in the transcript:
// the client's own answer (it has no such tool) is replaced with SearchAnswer. It returns the names
// those calls asked for. The same transcript gets the same bytes on every request.
func repairSearchResults(wire string, body []byte, deferred map[string]bool, known map[string][]string) ([]byte, []string) {
	if len(known) == 0 && !strings.Contains(string(body), ClientSearchName) {
		return body, nil
	}
	answers := map[string]string{}
	for id, ns := range known {
		answers[id] = SearchAnswer(ns, deferred)
	}
	var asked []string
	note := func(id, args string) {
		if id == "" {
			return
		}
		ns := searchArgs(args)
		answers[id] = SearchAnswer(ns, deferred)
		asked = append(asked, ns...)
	}
	out := body
	if cgschema.IsResponses(wire) {
		items := gjson.GetBytes(body, "input").Array()
		for _, it := range items {
			if it.Get("type").String() == "function_call" && it.Get("name").String() == ClientSearchName {
				note(it.Get("call_id").String(), it.Get("arguments").String())
			}
		}
		for i, it := range items {
			if it.Get("type").String() != "function_call_output" {
				continue
			}
			a, ok := answers[it.Get("call_id").String()]
			if !ok || it.Get("output").String() == a {
				continue
			}
			if b, err := sjson.SetBytes(out, "input."+itoa(i)+".output", a); err == nil {
				out = b
			}
		}
		return out, asked
	}
	msgs := gjson.GetBytes(body, "messages").Array()
	for _, m := range msgs {
		m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if tc.Get("function.name").String() == ClientSearchName {
				note(tc.Get("id").String(), tc.Get("function.arguments").String())
			}
			return true
		})
	}
	for i, m := range msgs {
		if m.Get("role").String() != "tool" {
			continue
		}
		a, ok := answers[m.Get("tool_call_id").String()]
		if !ok || m.Get("content").String() == a {
			continue
		}
		if b, err := sjson.SetBytes(out, "messages."+itoa(i)+".content", a); err == nil {
			out = b
		}
	}
	return out, asked
}

// HasClientSearch reports whether body declares our search tool: the host holds and reads the
// response exactly when it does.
func HasClientSearch(wire string, body []byte) bool {
	found := false
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		n, fn := toolName(wire, t)
		found = fn && n == ClientSearchName
		return !found
	})
	return found
}

// AggregateChatSSE rebuilds a Chat Completions object from its event stream: the first choice's
// text and tool calls (by index, arguments joined), its finish reason and the usage chunk. ok=false
// when the stream holds no chunk.
func AggregateChatSSE(raw []byte) ([]byte, bool) {
	type call struct {
		id, name string
		args     strings.Builder
	}
	var text, reasoning strings.Builder
	calls := map[int64]*call{}
	var order []int64
	finish, usage, id, model := "", "", "", ""
	seen := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		p := strings.TrimSpace(line[5:])
		if p == "" || p == "[DONE]" || !gjson.Valid(p) {
			continue
		}
		ev := gjson.Parse(p)
		seen = true
		if id == "" {
			id, model = ev.Get("id").String(), ev.Get("model").String()
		}
		if u := ev.Get("usage"); u.IsObject() {
			usage = u.Raw
		}
		ch := ev.Get("choices.0")
		if !ch.Exists() {
			continue
		}
		if f := ch.Get("finish_reason").String(); f != "" {
			finish = f
		}
		d := ch.Get("delta")
		text.WriteString(d.Get("content").String())
		reasoning.WriteString(d.Get("reasoning_content").String())
		d.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			i := tc.Get("index").Int()
			c := calls[i]
			if c == nil {
				c = &call{}
				calls[i] = c
				order = append(order, i)
			}
			if v := tc.Get("id").String(); v != "" {
				c.id = v
			}
			if v := tc.Get("function.name").String(); v != "" {
				c.name = v
			}
			c.args.WriteString(tc.Get("function.arguments").String())
			return true
		})
	}
	if !seen {
		return nil, false
	}
	msg := `{"role":"assistant"}`
	msg, _ = sjson.Set(msg, "content", text.String())
	if reasoning.Len() > 0 {
		// GLM and Kimi stream their reasoning: the follow-up's assistant turn keeps it.
		msg, _ = sjson.Set(msg, "reasoning_content", reasoning.String())
	}
	sort.Slice(order, func(a, b int) bool { return order[a] < order[b] })
	for _, i := range order {
		c := calls[i]
		tc, _ := sjson.Set(`{"type":"function","function":{}}`, "id", c.id)
		tc, _ = sjson.Set(tc, "function.name", c.name)
		tc, _ = sjson.Set(tc, "function.arguments", c.args.String())
		msg, _ = sjson.SetRaw(msg, "tool_calls.-1", tc)
	}
	out := `{"object":"chat.completion","choices":[{"index":0}]}`
	out, _ = sjson.Set(out, "id", id)
	out, _ = sjson.Set(out, "model", model)
	out, _ = sjson.SetRaw(out, "choices.0.message", msg)
	out, _ = sjson.Set(out, "choices.0.finish_reason", finish)
	if usage != "" {
		out, _ = sjson.SetRaw(out, "usage", usage)
	}
	return []byte(out), true
}
