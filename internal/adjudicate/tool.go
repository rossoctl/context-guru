// Package adjudicate declares the context-maintenance tool the compaction pipeline uses to ask the
// request's own model which tool outputs are spent, and the wire helpers that keep it byte-stable in
// the prompt-cache prefix.
//
// WHY A TOOL AT ALL, rather than asking for JSON in the reply text. Measured against the live route:
//
//	tool_choice          reply shape                 cache            verdict coverage
//	{"type":"none"}      prose / thinking only       read (free)      0 of 6  -- no answer at all
//	{"type":"tool",...}  tool_use                    MISS + rewrite   6 of 6
//	(omitted)            tool_use                    read (free)      6 of 6, 4 trials out of 4
//
// Three things follow, and each was the opposite of an earlier assumption in this repo:
//
//   - Setting tool_choice:none to stop the model answering with a tool_use is what forced it into
//     PROSE. That reply was then scored as an unparseable failure, which is a large part of what read
//     as "the model declines to act" across three iterations.
//   - FORCING the tool is not free: it produced a separate cache entry (8378 tokens written against
//     the 8268 already cached), so tool_choice does participate in the key when it names a tool, even
//     though `none` does not.
//   - Merely DECLARING the tool, with a description that says who it is for, gets a schema-shaped
//     answer covering the whole batch, at cache-read price.
//
// The tool is therefore injected on EVERY request rather than only when the pipeline is about to ask.
// `tools` hashes before system and messages, so a tool that appears and disappears invalidates the
// prefix from position zero — the same flap that expand's `always` mode exists to prevent.
package adjudicate

import (
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// strayAnswered counts tool_results rewritten because the AGENT called this tool. Non-zero is
// expected rather than alarming -- models do call advertised tools they were told to leave alone --
// but the rate is the signal for whether the description is doing its job.
var strayAnswered atomic.Int64

// StrayAnswered returns how many stray calls have been answered. Surfaced in /stats.
func StrayAnswered() int64 { return strayAnswered.Load() }

// ToolName is the wire name. Prefixed like the expand tool so an operator reading a transcript can
// tell at a glance which tools the proxy injected and which the client owns.
const ToolName = "context_guru_adjudicate"

// strayAnswer is what a stray call from the AGENT gets. The model will call an advertised tool it was
// told not to — directly observed with the expand tool, which it called at step 2 of a run — and the
// client cannot execute a tool the proxy injected, so it answers "not found" and the agent loses a
// turn to a dead end. This gives it a definite, uninteresting answer instead.
const strayAnswer = "Context maintenance runs automatically in the background. No action is required " +
	"from you, and you do not need to call this tool. Continue with the task."

// anthropicDef and openAIDef are the tool definitions, kept as raw JSON so injection is a byte splice
// and the cached prefix stays stable to the byte.
const anthropicDef = `{"name":"` + ToolName + `","description":"` + toolDesc + `","input_schema":` + schemaJSON + `}`

const openAIDef = `{"type":"function","function":{"name":"` + ToolName + `","description":"` + toolDesc +
	`","parameters":` + schemaJSON + `}}`

// toolDesc tells the model who the tool is for. "Do not call this yourself" does not reliably stop it
// (see strayAnswer), but it costs nothing and reduces the rate.
const toolDesc = "Internal to the context manager. Reports which earlier tool outputs are spent and " +
	"safe to remove from the transcript. This is invoked by the context manager, not by you - do not " +
	"call it yourself."

// schemaJSON constrains the answer. The label is a small INTEGER, never the tool_use id: asked for
// opaque ids the model regularised them (answering toolu_01..07 for toolu_probe_00..07), because
// reproducing a random identifier from thousands of tokens back is a copying task rather than a
// judgement. With integer labels it was 0 bad labels across 40+ trials.
const schemaJSON = `{"type":"object","properties":{"verdicts":{"type":"array","description":` +
	`"One entry per label you were shown. Answer for EVERY label.","items":{"type":"object","properties":{` +
	`"i":{"type":"integer","description":"The label you were shown."},` +
	`"needed_by":{"type":"string","enum":["a","b","c","none"],"description":` +
	`"Which outstanding obligation still needs this output, or none if it is spent."},` +
	`"quote":{"type":"string","description":"Verbatim transcript text creating that obligation; empty when needed_by is none."},` +
	`"verdict":{"type":"string","enum":["keep","drop"],"description":"drop requires needed_by to be none."}},` +
	`"required":["i","needed_by","verdict"]}}},"required":["verdicts"]}`

// ToolDefRaw returns the provider-shaped tool definition.
func ToolDefRaw(provider string) []byte {
	if provider == "anthropic" {
		return []byte(anthropicDef)
	}
	return []byte(openAIDef)
}

// HasTool reports whether body already declares the tool. This is the ADVERTISE test, and a host must
// answer stray calls exactly when it is true.
func HasTool(provider string, body []byte) bool {
	field := "function.name"
	if provider == "anthropic" {
		field = "name"
	}
	found := false
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		if t.Get(field).String() == ToolName {
			found = true
			return false
		}
		return true
	})
	return found
}

// Inject appends the tool to body's tools array, byte-stably and idempotently.
//
// Appended LAST so the client's own tools keep their order, and skipped when a forcing tool_choice is
// present so tool selection is never perturbed. Also skipped when the request declares no tools at
// all: adding the first tool to a tool-free request changes what the model believes it can do.
// Fail-open — any trouble returns the original body.
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
		return body, false
	}
	return nb, true
}

func toolChoiceIsAuto(tc gjson.Result) bool {
	if tc.Type == gjson.String {
		s := tc.String()
		return s == "auto" || s == ""
	}
	t := tc.Get("type").String()
	return t == "" || t == "auto"
}

// AnswerStrayCalls replaces the client's tool_result for any call the AGENT made to this tool with a
// definite answer, and reports how many it replaced.
//
// Same request-path shape as expand's RestoreResults, and for the same reason: the client executes the
// real tools itself, finds no tool by this name because the proxy injected it, and answers something
// like "Tool 'context_guru_adjudicate' not found". Left alone, the model reads a failure it cannot act
// on and may retry. Rewriting it on the next request works WITH the client's loop instead of against
// it, needs nothing implemented client-side, and is deterministic — the same substitution every turn,
// so the prefix does not flap.
func AnswerStrayCalls(provider string, body []byte) (out []byte, answered int) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, 0
	}
	ours := map[string]bool{} // tool_use id -> made by this tool
	msgs.ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() == "tool_use" && blk.Get("name").String() == ToolName {
				if id := blk.Get("id").String(); id != "" {
					ours[id] = true
				}
			}
			return true
		})
		m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if tc.Get("function.name").String() == ToolName {
				if id := tc.Get("id").String(); id != "" {
					ours[id] = true
				}
			}
			return true
		})
		return true
	})
	if len(ours) == 0 {
		return body, 0
	}
	out = body
	for i := range gjson.GetBytes(out, "messages").Array() {
		base := "messages." + itoa(i)
		blocks := gjson.GetBytes(out, base+".content")
		if blocks.IsArray() {
			for b, blk := range blocks.Array() {
				if blk.Get("type").String() != "tool_result" || !ours[blk.Get("tool_use_id").String()] {
					continue
				}
				if nb, err := sjson.SetBytes(out, base+".content."+itoa(b)+".content", strayAnswer); err == nil {
					out, answered = nb, answered+1
				}
			}
			continue
		}
		if gjson.GetBytes(out, base+".role").String() == "tool" &&
			ours[gjson.GetBytes(out, base+".tool_call_id").String()] {
			if nb, err := sjson.SetBytes(out, base+".content", strayAnswer); err == nil {
				out, answered = nb, answered+1
			}
		}
	}
	if answered > 0 {
		strayAnswered.Add(int64(answered))
	}
	return out, answered
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
