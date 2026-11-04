package tooldefer

import (
	"sort"
	"strings"
	"unicode"

	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The client's own search answers the proxy's deferred tools (#898, option B, Claude Code).
//
// Claude Code sends its own client-side `ToolSearch`. It finds the tools Claude Code deferred, but
// not the ones the proxy deferred. So on a thread with ToolSearch (Record.Client) the proxy adds no
// server-side search. It answers the client's search calls itself, on the REQUEST side: a search
// runs in the client, free, and its tool_result comes back on the next request. There the proxy
// replaces the result with tool_reference blocks over every deferred tool, the client's and its own.
// This is the pattern of repairSearchResults (clientsearch.go). It needs no second upstream round,
// and a search next to another tool call needs no special case: each result is replaced on its own.
//
// The API expands a tool_reference to the tool's definition for any tool with defer_loading, with no
// server-side search in `tools` (measured live, #898).
//
// A query that matches only the client's own deferred tools passes through: the client's result
// stays as it is. Every answer, and every pass-through, is written to the thread's record by tool
// call id (Record.Answers) the first time, and read from there on every later request. The bytes are
// then the same on every request, also after a restart, and the cached prefix holds. A search whose
// query is changed or deleted by the client in a later request is not our concern: the id is the key.

// maxAnswers bounds Record.Answers. Past it an answer is computed again each time (a pure function
// of the call and the declared tools) and not kept.
const maxAnswers = 256

// defaultResults is ToolSearch's max_results default (its schema says "default: 5").
const defaultResults = 5

// maxResults caps a model-chosen max_results.
const maxResults = 25

// HasClientToolSearch reports whether body declares Claude Code's own search: a client tool, loaded.
func HasClientToolSearch(body []byte) bool {
	found := false
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		found = t.Get("name").String() == claudeCodeSearch && !t.Get("type").Exists() && !t.Get("defer_loading").Bool()
		return !found
	})
	return found
}

// clientNamesListHead starts the names list when the client's search loads the tools. Fixed text.
const clientNamesListHead = "Context Guru deferred these tools to save context. Their definitions are not loaded yet. " +
	"Before you call one, load it with " + claudeCodeSearch + ": set query to select:<exact name>. " +
	"Then call it directly. Deferred tools:"

// ClientToolNamesList is the names list for a sorted set of names, in client-search mode.
func ClientToolNamesList(names []string) string {
	return clientNamesListHead + "\n" + strings.Join(names, "\n")
}

// searchCall is one ToolSearch call of the transcript.
type searchCall struct {
	query string
	max   int
}

// candidate is one deferred tool a search may return.
type candidate struct {
	name, desc string
	ours       bool
}

// AnswerSearches replaces, in body, the result of every ToolSearch call whose answer touches a tool
// the proxy deferred (deferred is the thread's set). It returns the new body, the names newly loaded
// by an answer written now (for the history and the ledger), and how many answers that was. rs,
// session and thread name the record that keeps the answers. Fail open: any problem returns body.
func AnswerSearches(rs store.Store, session, thread string, body []byte, deferred map[string]bool) (out []byte, loads []string, n int) {
	out = body
	if !strings.Contains(string(body), claudeCodeSearch) {
		return body, nil, 0
	}
	calls := map[string]searchCall{}
	msgs := gjson.GetBytes(body, "messages").Array()
	for _, m := range msgs {
		if m.Get("role").String() != "assistant" {
			continue
		}
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			if b.Get("type").String() == "tool_use" && b.Get("name").String() == claudeCodeSearch {
				max := int(b.Get("input.max_results").Int())
				if max <= 0 {
					max = defaultResults
				}
				if max > maxResults {
					max = maxResults
				}
				calls[b.Get("id").String()] = searchCall{query: b.Get("input.query").String(), max: max}
			}
			return true
		})
	}
	if len(calls) == 0 {
		return body, nil, 0
	}
	var cands []candidate
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		name := t.Get("name").String()
		if name != "" && !t.Get("type").Exists() && t.Get("defer_loading").Bool() {
			cands = append(cands, candidate{name: name, desc: t.Get("description").String(), ours: deferred[name]})
		}
		return true
	})
	recMu.Lock()
	defer recMu.Unlock()
	rec, ok := Load(rs, session, thread)
	if !ok {
		return body, nil, 0
	}
	changed := false
	for i, m := range msgs {
		if m.Get("role").String() != "user" {
			continue
		}
		blocks := m.Get("content")
		if !blocks.IsArray() {
			continue
		}
		for j, b := range blocks.Array() {
			id := b.Get("tool_use_id").String()
			call, isCall := calls[id]
			if b.Get("type").String() != "tool_result" || !isCall {
				continue
			}
			names, known := rec.Answers[id]
			if !known {
				names = searchNames(call, cands)
				if len(rec.Answers) < maxAnswers {
					if rec.Answers == nil {
						rec.Answers = map[string][]string{}
					}
					rec.Answers[id] = append([]string{}, names...)
					changed = true
				}
				for _, nm := range names {
					if deferred[nm] {
						loads = append(loads, nm)
					}
				}
				n++
			}
			if len(names) == 0 {
				continue // a pure client query: the client's result stays
			}
			refs := make([]string, len(names))
			for k, nm := range names {
				r, _ := sjson.Set(`{"type":"tool_reference"}`, "tool_name", nm)
				refs[k] = r
			}
			path := "messages." + itoa(i) + ".content." + itoa(j)
			next, err := sjson.SetRawBytes(out, path+".content", []byte("["+strings.Join(refs, ",")+"]"))
			if err != nil {
				return body, nil, 0
			}
			if b.Get("is_error").Exists() {
				if next, err = sjson.DeleteBytes(next, path+".is_error"); err != nil {
					return body, nil, 0
				}
			}
			out = next
		}
	}
	if changed {
		Save(rs, session, thread, rec)
	}
	return out, loads, n
}

// searchNames is the answer to one call: the names to load, or nil when the call matches only the
// client's own deferred tools (the client answers it) or nothing at all.
func searchNames(c searchCall, cands []candidate) []string {
	var names []string
	if rest, ok := strings.CutPrefix(strings.TrimSpace(c.query), "select:"); ok {
		have := map[string]bool{}
		for _, cd := range cands {
			have[cd.name] = true
		}
		seen := map[string]bool{}
		for _, nm := range strings.Split(rest, ",") {
			if nm = strings.TrimSpace(nm); nm != "" && have[nm] && !seen[nm] {
				seen[nm] = true
				names = append(names, nm)
			}
		}
	} else {
		names = rank(c, cands)
	}
	ours := map[string]bool{}
	for _, cd := range cands {
		if cd.ours {
			ours[cd.name] = true
		}
	}
	for _, nm := range names {
		if ours[nm] {
			return names
		}
	}
	return nil
}

// rank is a keyword search over the deferred tools: terms are matched against the tool name (3
// points) and its description (1 point); "+term" is required. Highest score first, ties in the
// order the tools are declared. At most c.max names.
func rank(c searchCall, cands []candidate) []string {
	var terms, need []string
	for _, w := range strings.Fields(strings.ToLower(c.query)) {
		req := strings.HasPrefix(w, "+")
		for _, t := range strings.FieldsFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			terms = append(terms, t)
			if req {
				need = append(need, t)
			}
		}
	}
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		name  string
		score int
		at    int
	}
	var hits []scored
	for i, cd := range cands {
		name, desc := strings.ToLower(cd.name), strings.ToLower(cd.desc)
		match := func(t string) int {
			s := 0
			if strings.Contains(name, t) {
				s += 3
			}
			if strings.Contains(desc, t) {
				s++
			}
			return s
		}
		ok := true
		for _, t := range need {
			ok = ok && match(t) > 0
		}
		if !ok {
			continue
		}
		s := 0
		for _, t := range terms {
			s += match(t)
		}
		if s > 0 {
			hits = append(hits, scored{cd.name, s, i})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].score > hits[b].score })
	if len(hits) > c.max {
		hits = hits[:c.max]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.name
	}
	return out
}

// ClientProbeRoute is added to the probe key's route, so the client-search probe has its own verdict.
const ClientProbeRoute = "#client-search"

// ClientProbeBody is the probe request for client-search mode: a plain ToolSearch tool, a deferred
// tool with a unique name, and a finished search round whose result is a tool_reference to it. The
// model is asked to call the tool. No server-side search is declared.
func ClientProbeBody(model, nonce string) []byte {
	tool := "cg_probe_" + nonce
	body := []byte(`{"max_tokens":1000,"tools":[` +
		`{"name":"ToolSearch","description":"Fetch deferred tools by query.","input_schema":{"type":"object","properties":{"query":{"type":"string"},"max_results":{"type":"integer"}},"required":["query"]}},` +
		`{"name":"","description":"Report the probe code. Call it with code set to the number you are given.","input_schema":{"type":"object","properties":{"code":{"type":"integer"}},"required":["code"]},"defer_loading":true}],` +
		`"messages":[{"role":"user","content":""},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01cgprobe","name":"ToolSearch","input":{"query":"","max_results":1}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01cgprobe","content":[{"type":"tool_reference","tool_name":""}]}]}]}`)
	body, _ = sjson.SetBytes(body, "model", model)
	body, _ = sjson.SetBytes(body, "tools.1.name", tool)
	body, _ = sjson.SetBytes(body, "messages.0.content", "Probe "+nonce+". Load the tool "+tool+" with ToolSearch, then call it with code 7. Do nothing else.")
	body, _ = sjson.SetBytes(body, "messages.1.content.0.input.query", "select:"+tool)
	body, _ = sjson.SetBytes(body, "messages.2.content.0.content.0.tool_name", tool)
	return body
}

// ClientProbeVerdict reads a client-search probe response: Pass only when the model calls the tool
// the tool_reference loaded.
func ClientProbeVerdict(resp []byte, nonce string) Verdict {
	pass := false
	gjson.GetBytes(resp, "content").ForEach(func(_, b gjson.Result) bool {
		pass = b.Get("type").String() == "tool_use" && b.Get("name").String() == "cg_probe_"+nonce
		return !pass
	})
	if pass {
		return Pass
	}
	return Fail
}
