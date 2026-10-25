// Package adjudicate declares the context-maintenance tool the cold-sweep adjudication asks with, and
// the wire helpers that keep it byte-stable in the prompt-cache prefix.
//
// WHY A TOOL AT ALL, rather than only asking for a JSON array in the reply text. The ask carries the
// AGENT's tools, because `tools` are in the cache key and stripping them costs the read — so the only
// question is what the model does with that freedom. Three arms, same transcript and ask model, three
// benchmark passes each, run sequentially:
//
//	tool_choice          verdict tool   asks replied   unusable       answered via tool_use
//	{"type":"none"}      no             20             6  (30.0%)     0
//	(omitted)            NO             24             14 (58.3%)     0
//	(omitted)            yes            77             7  (9.1%)      43 (55.8%)
//
// Fisher two-tailed: row 1 vs row 3 p = 0.0245, row 2 vs row 3 p = 0.0000.
//
// The MIDDLE row is why this package exists, and it is the arm nobody had run: removing
// tool_choice:none on its own is WORSE than leaving it in. Freed to call a tool and offered only the
// agent's own plus context_guru_expand, the model calls one of those; logging every reply's content
// blocks caught it as `thinking,tool_use:context_guru_expand` with no text block at all, which the
// text-only extraction reads as "" and files as unusable. That arm also lost 5 asks to the 90 s
// llmCallTimeout against 0 and 1 in the others.
//
// So the two halves are one change. Suppressing the call trades a lost answer for prose (row 1's
// failures); allowing it with nothing worth calling loses the answer outright (row 2); allowing it and
// declaring the right tool gets 55.8% of replies back schema-shaped at cache-read price, with the rest
// still read by the unchanged prose parser (row 3).
//
// FORCING the tool by name is separately not free: it produced a second cache entry (8,378 written
// against the 8,268 already cached), so tool_choice DOES participate in the key when it names a tool,
// even though `none` does not.
//
// WHERE IT IS INJECTED. On every request of an Anthropic route whose pipeline contains
// extract_llm_sweep, and nowhere else. Every-request matters because `tools` hashes before system and
// messages, so a tool that appears when the sweep fires and vanishes on the next turn invalidates the
// prefix from position zero -- the flap expand's `always` mode exists to prevent. But that argument
// only forbids gating on something that varies PER TURN: pipeline membership is fixed per config
// DOCUMENT and the provider by the route, so both conditions are byte-stable for every request under a
// given config. Not "fixed at config load", which is what this used to claim: proxy/tenancy.go rebuilds
// a tenant's *Pipeline when the config document changes, mid-session. That rebuild is a
// prefix-invalidating event in its own right, so the byte-stability argument holds where it matters.
// Injecting without
// them cost a measured 946 bytes at the head of the cacheable prefix of every preset including `off`,
// the control arm of every published comparison here, and including presets with no sweep at all.
//
// This does not replace the text path. extract.ParseVerdicts is still
// the fallback, and a model that answers in prose anyway is still read exactly as before; the tool
// only changes which reply shape is PREFERRED.
package adjudicate

import (
	"strconv"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// strayAnswered counts tool_results rewritten because the AGENT called this tool. Surfaced in /stats
// as adjudicate_stray: the rate is the only signal for whether the description is doing its job.
// MEASURED at 0 across ~4,900 requests with the description below, which is why the answer text is a
// cheap insurance policy rather than a hot path.
var strayAnswered atomic.Int64

// StrayAnswered returns how many stray calls have been answered, on either path.
func StrayAnswered() int64 { return strayAnswered.Load() }

// NoteAnsweredInBand counts stray calls answered on the RESPONSE path, before the client saw them.
// The same counter as the request-path repair on purpose: /stats publishes "the agent called a tool it
// was told not to call, n times", and which of the two defences caught it does not change that number
// — it is the rate that says whether the description is still working. Which path caught it is in the
// cg.adjudicate_stray log line, where it belongs.
func NoteAnsweredInBand(n int) {
	if n > 0 {
		strayAnswered.Add(int64(n))
	}
}

// ToolName is the wire name. Prefixed like the expand tool so an operator reading a transcript can
// tell at a glance which tools the proxy injected and which the client owns.
const ToolName = "context_guru_adjudicate"

// StrayAnswer is what a stray call from the AGENT gets. The model does call an advertised tool it was
// told to leave alone -- directly observed with context_guru_expand, which a run called at step 2 --
// and the client cannot execute a tool the proxy injected, so it answers something like
// "Tool 'context_guru_adjudicate' not found" and the agent loses a turn to a dead end. This gives it a
// definite, uninteresting answer instead.
const StrayAnswer = "Context maintenance runs automatically in the background. No action is required " +
	"from you, and you do not need to call this tool. Continue with the task."

// toolDesc tells the model who the tool is for. "Do not call it yourself" does not GUARANTEE it will
// not (see StrayAnswer), but it measured 0 strays in ~4,900 requests and costs nothing.
const toolDesc = "Internal to the context manager. Reports which earlier tool outputs are spent and " +
	"safe to remove from the transcript. This is invoked by the context manager, not by you - do not " +
	"call it yourself."

// schemaJSON constrains the answer, and its field names are exactly extract.Verdict's JSON tags so the
// existing parser reads a tool input unchanged.
//
// The label is a small INTEGER, never the tool_use id: asked for opaque ids the model REGULARISED them
// (answering toolu_01..07 for toolu_probe_00..07), because reproducing a random identifier from
// thousands of tokens back is a copying task rather than a judgement. With integer labels it was 0 bad
// labels across 40+ trials.
const schemaJSON = `{"type":"object","properties":{"verdicts":{"type":"array","description":` +
	`"One entry per label you were shown. Answer for EVERY label.","items":{"type":"object","properties":{` +
	`"i":{"type":"integer","description":"The label you were shown."},` +
	`"needed_by":{"type":"string","enum":["a","b","c","none"],"description":` +
	`"Which outstanding obligation still needs this output, or none if it is spent."},` +
	`"quote":{"type":"string","description":"Verbatim transcript text creating that obligation; empty when needed_by is none."},` +
	`"verdict":{"type":"string","enum":["keep","drop"],"description":"drop requires needed_by to be none."}},` +
	`"required":["i","needed_by","verdict"]}}},"required":["verdicts"]}`

// anthropicDef and openAIDef are the tool definitions, kept as raw JSON so injection is a byte splice
// and the cached prefix stays stable to the byte -- a re-marshalled map would be free to reorder keys.
const anthropicDef = `{"name":"` + ToolName + `","description":"` + toolDesc + `","input_schema":` + schemaJSON + `}`

const openAIDef = `{"type":"function","function":{"name":"` + ToolName + `","description":"` + toolDesc +
	`","parameters":` + schemaJSON + `}}`

const responsesDef = `{"type":"function","name":"` + ToolName + `","description":"` + toolDesc +
	`","parameters":` + schemaJSON + `}`

// ToolDefRaw returns the provider-shaped tool definition.
func ToolDefRaw(provider string) []byte {
	if provider == "anthropic" {
		return []byte(anthropicDef)
	}
	if provider == "responses" {
		return []byte(responsesDef)
	}
	return []byte(openAIDef)
}

// HasTool reports whether body already declares the tool. This is the ADVERTISE test, and a host must
// answer stray calls exactly when it is true.
func HasTool(provider string, body []byte) bool {
	field := "function.name"
	if provider == "anthropic" || provider == "responses" {
		field = "name"
	}
	for _, t := range gjson.GetBytes(body, "tools").Array() {
		if t.Get(field).String() == ToolName {
			return true
		}
	}
	return false
}

// Inject appends the tool to body's tools array, byte-stably and idempotently.
//
// Appended LAST so the client's own tools keep their exact order, and skipped when a forcing
// tool_choice is present so tool selection is never perturbed. Also skipped when the request declares
// no tools at all: handing the model its first tool changes what it believes it can do, and that is
// the riskiest case for a model that penalizes an unexpected tool. Fail-open — any trouble returns the
// original body.
func Inject(provider string, body []byte) (out []byte, injected bool) {
	if tc := gjson.GetBytes(body, "tool_choice"); tc.Exists() && !toolChoiceIsAuto(tc) {
		return body, false
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || len(tools.Array()) == 0 {
		return body, false
	}
	if HasTool(provider, body) {
		return body, false
	}
	nb, err := sjson.SetRawBytes(body, "tools.-1", ToolDefRaw(provider))
	if err != nil {
		return body, false // fail open
	}
	return nb, true
}

// toolChoiceIsAuto reports whether a tool_choice leaves the model free to choose. OpenAI: the string
// "auto". Anthropic: {"type":"auto"}. Anything else (none/required/any/a named tool) is forcing, and
// injection is skipped.
func toolChoiceIsAuto(tc gjson.Result) bool {
	if tc.Type == gjson.String {
		return tc.String() == "auto"
	}
	if tc.IsObject() {
		return tc.Get("type").String() == "auto"
	}
	return false
}

// ResponseCallIDs returns the ids of calls the AGENT made to this tool in an upstream RESPONSE.
//
// The request-path repair (AnswerStrayCalls) is a backstop and cannot be the primary defence: by the
// time it runs, the client has already SEEN the call, already failed to execute a tool it never
// declared, and already spent a turn answering "not found". Answering the call in-band on the
// response path -- before the client is written to -- is what makes the repair a backstop, and these
// ids are what the response loop needs to build that answer.
//
// "Backstop" is not "unreachable", and this comment used to imply it was. The response loop declines
// the in-band answer, deliberately, when the assistant turn ALSO calls a client tool: it cannot
// continue a turn whose other tool_use only the client can execute, so it hands the round over and
// leaves the next request's repair to fix it. On that path the repair is the ONLY defence and the agent
// does pay one turn.
func ResponseCallIDs(provider string, resp []byte) (ids []string) {
	if provider == "responses" {
		gjson.GetBytes(resp, "output").ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "function_call" && item.Get("name").String() == ToolName {
				if id := item.Get("call_id").String(); id != "" {
					ids = append(ids, id)
				}
			}
			return true
		})
		return ids
	}
	if provider == "anthropic" {
		gjson.GetBytes(resp, "content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() == "tool_use" && blk.Get("name").String() == ToolName {
				if id := blk.Get("id").String(); id != "" {
					ids = append(ids, id)
				}
			}
			return true
		})
		return ids
	}
	gjson.GetBytes(resp, "choices.0.message.tool_calls").ForEach(func(_, tc gjson.Result) bool {
		if tc.Get("function.name").String() == ToolName {
			if id := tc.Get("id").String(); id != "" {
				ids = append(ids, id)
			}
		}
		return true
	})
	return ids
}

// AnswerStrayCalls replaces the client's tool_result for any call the AGENT made to this tool with a
// definite answer, and reports how many it replaced.
//
// Same request-path shape as expand.RepairToolResults, and for the same reason: the client executes
// the real tools itself, finds no tool by this name because the PROXY injected it, and answers
// something like "Tool 'context_guru_adjudicate' not found". Left alone, the model reads a failure it
// cannot act on and may retry. Rewriting it on the next request works WITH the client's loop instead
// of against it, needs nothing implemented client-side, and is deterministic — the same substitution
// every turn, so the prefix does not flap.
//
// Reading the tool_use out of the transcript rather than trusting the tool_result is what makes this
// safe: a client's tool_result is rewritten only when the assistant turn it answers called OUR tool.
// A body with no such call comes back byte-identical, because this runs on every request and a
// gratuitous rewrite would change the prefix and cost a cache write for nothing.
//
// The response loop answers most strays before this ever runs, but NOT all of them: an assistant turn
// that calls this tool alongside a CLIENT tool is handed to the client whole, so for that turn this
// repair is the only defence rather than a second one. See the co-call note in the response loop.
func AnswerStrayCalls(provider string, body []byte) (out []byte, answered int) {
	if provider == "responses" {
		return answerResponsesStrayCalls(body)
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, 0
	}
	// An id-less tool_use would make "" a live key, and then a tool_result carrying no tool_use_id --
	// someone else's block -- would match it and be overwritten. Both halves of the pair must name an id.
	ours := map[string]bool{}
	for _, m := range msgs.Array() {
		if provider == "anthropic" {
			for _, blk := range m.Get("content").Array() {
				if blk.Get("type").String() == "tool_use" && blk.Get("name").String() == ToolName {
					if id := blk.Get("id").String(); id != "" {
						ours[id] = true
					}
				}
			}
			continue
		}
		for _, tc := range m.Get("tool_calls").Array() {
			if tc.Get("function.name").String() == ToolName {
				if id := tc.Get("id").String(); id != "" {
					ours[id] = true
				}
			}
		}
	}
	if len(ours) == 0 {
		return body, 0
	}
	out = body
	for mi, m := range msgs.Array() {
		base := "messages." + strconv.Itoa(mi)
		if provider != "anthropic" {
			if m.Get("role").String() != "tool" || !ours[m.Get("tool_call_id").String()] {
				continue
			}
			if nb, err := sjson.SetBytes(out, base+".content", StrayAnswer); err == nil {
				out, answered = nb, answered+1
			}
			continue
		}
		for bi, blk := range m.Get("content").Array() {
			if blk.Get("type").String() != "tool_result" || !ours[blk.Get("tool_use_id").String()] {
				continue
			}
			path := base + ".content." + strconv.Itoa(bi)
			nb, err := sjson.SetBytes(out, path+".content", StrayAnswer)
			if err != nil {
				continue
			}
			out, answered = nb, answered+1
			// The block is no longer an error: leaving is_error set tells the model its own call
			// failed while handing it the answer to that call.
			if blk.Get("is_error").Exists() {
				if nb, err := sjson.SetBytes(out, path+".is_error", false); err == nil {
					out = nb
				}
			}
		}
	}
	if answered > 0 {
		strayAnswered.Add(int64(answered))
	}
	return out, answered
}

func answerResponsesStrayCalls(body []byte) ([]byte, int) {
	items := gjson.GetBytes(body, "input")
	if !items.IsArray() {
		return body, 0
	}
	ours := map[string]bool{}
	for _, item := range items.Array() {
		if item.Get("type").String() == "function_call" && item.Get("name").String() == ToolName {
			if id := item.Get("call_id").String(); id != "" {
				ours[id] = true
			}
		}
	}
	if len(ours) == 0 {
		return body, 0
	}
	out, answered := body, 0
	for i, item := range items.Array() {
		if item.Get("type").String() != "function_call_output" || !ours[item.Get("call_id").String()] {
			continue
		}
		if next, err := sjson.SetBytes(out, "input."+strconv.Itoa(i)+".output", StrayAnswer); err == nil {
			out, answered = next, answered+1
		}
	}
	if answered > 0 {
		strayAnswered.Add(int64(answered))
	}
	return out, answered
}
