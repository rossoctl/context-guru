package tooldefer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The proxy adds its own deferred tools to Claude Code's ToolSearch result (#898, option B).
//
// Claude Code sends its own client-side `ToolSearch`. It finds the tools Claude Code deferred, not the
// ones the proxy deferred. So on a thread with ToolSearch (Record.Client) the proxy adds no
// server-side search. Every ToolSearch call goes to Claude Code, as before. In the NEXT request, the
// result is the tail of the transcript, and the proxy adds tool_reference blocks for its own deferred
// tools to it. Claude Code's own result stays as it is. The API expands a tool_reference for a tool
// with defer_loading, with no server-side search in `tools` (measured live, #898).
//
// The accepted query is only the exact select form (selectNames). Every other query passes through,
// also one that names a deferred tool: Claude Code's own search indexes every tool it has, so it
// answers such a query itself and the model loads the tool (#871). The proxy does not read a query
// in any other way. The measured behavior of Claude Code's search:
//   - `select:` is matched in any case ("Select:"), names are split on "," and trimmed, so a space
//     after the comma is fine;
//   - a name it does not know is dropped; when no name is known, the result is the text
//     noMatchText (not an error);
//   - the result is capped at max_results (default 5).
//
// Fail closed on a format change, never switch to another search tool: a hash of ToolSearch's
// description (DescHash) is the version key. An unknown hash defers nothing on the thread. A call to a
// deferred tool whose own result says it is not loaded marks the hash disabled (Gate.DisableClient)
// and the thread stops deferring: `tools` changes once, so that thread pays one cache rewrite.
//
// Every added list is written to the thread's record by tool_use id the first time, and replayed from
// there on every later request. The bytes are then the same on every request, also after a restart.

// knownDescHashes are the ToolSearch description hashes whose select syntax was checked live (#898):
// the hash is the format version. Add a hash here only after the live probe passes with that
// client version (see docs/tool-search-matrix.md). Any other hash defers nothing.
var knownDescHashes = map[string]bool{
	"88033eceb421f1c2": true, // Claude Code, Nov 2026 (109 captures)
}

// AddKnownDescHash marks a description hash as verified. For tests; production adds to the list above.
func AddKnownDescHash(h string) { knownDescHashes[h] = true }

// KnownDescHash reports whether the proxy has verified the select syntax of this description.
func KnownDescHash(h string) bool { return knownDescHashes[h] }

// maxAnswers bounds Record.Answers. Past it an answer is computed again each time (a pure function
// of the call and the result) and not kept.
const maxAnswers = 256

// defaultResults is ToolSearch's max_results default.
const defaultResults = 5

// noMatchText is Claude Code's result when no name matched.
const noMatchText = "No matching deferred tools found"

// HasClientToolSearch reports whether body declares Claude Code's own search: a client tool, loaded.
func HasClientToolSearch(body []byte) bool {
	return clientSearchTool_(body).Exists()
}

func clientSearchTool_(body []byte) gjson.Result {
	var found gjson.Result
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		if t.Get("name").String() == claudeCodeSearch && !t.Get("type").Exists() && !t.Get("defer_loading").Bool() {
			found = t
		}
		return !found.Exists()
	})
	return found
}

// DescHash is a short hash of ToolSearch's description, which documents the query syntax: the key of
// the format check. "" when body has no ToolSearch.
func DescHash(body []byte) string {
	t := clientSearchTool_(body)
	if !t.Exists() {
		return ""
	}
	h := sha256.Sum256([]byte(t.Get("description").String()))
	return hex.EncodeToString(h[:8])
}

// clientNamesListHead starts the names list when the client's search loads the tools. Fixed text.
const clientNamesListHead = "Context Guru deferred these tools to save context. Their definitions are not loaded yet. " +
	"Before you call one, load it with " + claudeCodeSearch + ": set query to select:<exact name>. " +
	"Then call it directly. Deferred tools:"

// ClientToolNamesList is the names list for a sorted set of names, in client-search mode.
func ClientToolNamesList(names []string) string {
	return clientNamesListHead + "\n" + strings.Join(names, "\n")
}

// selectNames reads the exact select form: "select:" in any case, then names split on ",", each
// trimmed. ok=false for any other query.
func selectNames(q string) (names []string, ok bool) {
	q = strings.TrimSpace(q)
	if len(q) < 7 || !strings.EqualFold(q[:7], "select:") {
		return nil, false
	}
	for _, n := range strings.Split(q[7:], ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names, true
}

// Augment is what AugmentSearches did on one request.
type Augment struct {
	Body []byte
	// Loads is the deferred tools that results written now carry; Answered how many calls were new.
	Loads    []string
	Answered int
}

// AugmentSearches adds, to the result of every ToolSearch call in body, a tool_reference for each
// deferred tool (deferred is the thread's set) that the call's exact select query names. rs, session
// and thread name the record that keeps the lists. Fail open: any problem returns body.
func AugmentSearches(rs store.Store, session, thread string, body []byte, deferred map[string]bool) (a Augment) {
	a.Body = body
	if !strings.Contains(string(body), claudeCodeSearch) {
		return a
	}
	type call struct {
		query string
		max   int
	}
	calls := map[string]call{}
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
				calls[b.Get("id").String()] = call{b.Get("input.query").String(), max}
			}
			return true
		})
	}
	if len(calls) == 0 {
		return a
	}
	recMu.Lock()
	defer recMu.Unlock()
	rec, ok := Load(rs, session, thread)
	if !ok {
		return a
	}
	out, changed := body, false
	for i, m := range msgs {
		if m.Get("role").String() != "user" || !m.Get("content").IsArray() {
			continue
		}
		for j, b := range m.Get("content").Array() {
			id := b.Get("tool_use_id").String()
			c, isCall := calls[id]
			if b.Get("type").String() != "tool_result" || !isCall {
				continue
			}
			res := b.Get("content")
			// The result shapes the proxy extends: the no-match text, or references only. An error or
			// any other shape stands as the client wrote it, and books nothing.
			var refs []string
			switch {
			case res.Type == gjson.String && res.String() == noMatchText:
			case res.IsArray() && onlyRefs(res):
				res.ForEach(func(_, r gjson.Result) bool {
					refs = append(refs, r.Raw)
					return true
				})
			default:
				continue
			}
			names, known := rec.Answers[id]
			if !known {
				if sel, isSel := selectNames(c.query); isSel {
					names = ourNames(sel, deferred, res, c.max)
				}
			}
			have := map[string]bool{}
			res.ForEach(func(_, r gjson.Result) bool { have[r.Get("tool_name").String()] = true; return true })
			var add []string
			for _, nm := range names {
				if !have[nm] {
					add = append(add, nm)
				}
			}
			if !known && len(add) > 0 {
				// Kept only when the proxy really writes a reference, so a call that gets no addition
				// is not booked as a load.
				if len(rec.Answers) < maxAnswers {
					if rec.Answers == nil {
						rec.Answers = map[string][]string{}
					}
					rec.Answers[id] = append([]string{}, add...)
					changed = true
				}
				a.Loads = append(a.Loads, add...)
				a.Answered++
			}
			if known {
				add = names
			}
			if len(add) == 0 {
				continue
			}
			for _, nm := range add {
				if !have[nm] {
					r, _ := sjson.Set(`{"type":"tool_reference"}`, "tool_name", nm)
					refs = append(refs, r)
				}
			}
			next, err := sjson.SetRawBytes(out, "messages."+itoa(i)+".content."+itoa(j)+".content", []byte("["+strings.Join(refs, ",")+"]"))
			if err != nil {
				a.Body = body
				return Augment{Body: body}
			}
			out = next
		}
	}
	if changed {
		Save(rs, session, thread, rec)
	}
	a.Body = out
	return a
}

// onlyRefs reports whether every element of a result array is a tool_reference block.
func onlyRefs(res gjson.Result) bool {
	ok := true
	res.ForEach(func(_, r gjson.Result) bool {
		ok = ok && r.Get("type").String() == "tool_reference"
		return ok
	})
	return ok
}

// ourNames is the deferred tools to add for the names of a select query, in query order, once each.
// The client's own result holds some of the cap; ours fill what is left of max.
func ourNames(sel []string, deferred map[string]bool, res gjson.Result, max int) []string {
	room := max
	if res.IsArray() {
		room -= len(res.Array())
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range sel {
		if room <= 0 {
			break
		}
		if deferred[n] && !seen[n] {
			seen[n] = true
			out = append(out, n)
			room--
		}
	}
	return out
}

// ReadCalls reads the proxy's deferred tools that the model called in the transcript. broke: a call's
// own tool_result is an error that says the tool is not loaded or does not exist. direct: calls that
// no search loaded and that did not fail that way. The API accepts a direct call, so it is a use, not a
// break. keep is the set of tools tool_choice forced on this request: their calls are never a break.
func ReadCalls(body []byte, deferred map[string]bool) (broke bool, direct []string) {
	loaded := Loaded(body)
	called := map[string]string{} // tool_use id -> deferred tool name
	var order []string
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, b gjson.Result) bool {
			switch b.Get("type").String() {
			case "tool_use":
				if n := b.Get("name").String(); deferred[n] {
					called[b.Get("id").String()] = n
					order = append(order, b.Get("id").String())
				}
			case "tool_result":
				id := b.Get("tool_use_id").String()
				if _, ok := called[id]; ok && b.Get("is_error").Bool() {
					t := strings.ToLower(b.Get("content").Raw)
					if strings.Contains(t, "not loaded") || strings.Contains(t, "no such tool") ||
						strings.Contains(t, "does not exist") {
						broke = true
						delete(called, id)
					}
				}
			}
			return true
		})
		return true
	})
	seen := map[string]bool{}
	for _, id := range order {
		if n, ok := called[id]; ok && !loaded[n] && !seen[n] {
			seen[n] = true
			direct = append(direct, n)
		}
	}
	return broke, direct
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
