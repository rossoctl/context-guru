package expand

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Call is one model request to expand offloaded content: CallID identifies the
// tool call (for the continuation's tool_result), HashID is the store key.
type Call struct {
	CallID string
	HashID string
}

// ResponseCalls parses a provider response for expand tool calls. otherTools is
// true if the model also called some OTHER tool — in that case the host cannot
// safely auto-continue (the client must resolve the other tools), so the loop
// bails and returns the response as-is.
// otherProxyTools are additional names the caller owns. otherTools means "a tool the CLIENT
// implements", which is what makes the response loop hand the turn over untouched — so a second
// proxy-injected tool counted as "other" caused the loop to bail and stream that proxy tool_use to
// the client. Variadic so the existing callers, which advertise only expand, are unchanged.
func ResponseCalls(provider string, resp []byte, otherProxyTools ...string) (calls []Call, otherTools bool) {
	ours := func(name string) bool {
		if name == ToolName {
			return true
		}
		for _, t := range otherProxyTools {
			if t != "" && name == t {
				return true
			}
		}
		return false
	}
	switch provider {
	case "anthropic":
		gjson.GetBytes(resp, "content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() != "tool_use" {
				return true
			}
			switch name := blk.Get("name").String(); {
			case name == ToolName:
				calls = append(calls, Call{CallID: blk.Get("id").String(), HashID: blk.Get("input.id").String()})
			case !ours(name):
				otherTools = true
			}
			return true
		})
	case "responses":
		gjson.GetBytes(resp, "output").ForEach(func(_, item gjson.Result) bool {
			typ := item.Get("type").String()
			if typ != "function_call" && typ != "custom_tool_call" {
				return true
			}
			name := item.Get("name").String()
			if name == ToolName {
				args := item.Get("arguments").String()
				if typ == "custom_tool_call" {
					args = item.Get("input").String()
				}
				calls = append(calls, Call{CallID: item.Get("call_id").String(), HashID: gjson.Get(args, "id").String()})
			} else if !ours(name) {
				otherTools = true
			}
			return true
		})
	default: // openai and compatibles
		gjson.GetBytes(resp, "choices.0.message.tool_calls").ForEach(func(_, tc gjson.Result) bool {
			switch name := tc.Get("function.name").String(); {
			case name == ToolName:
				hash := gjson.Get(tc.Get("function.arguments").String(), "id").String()
				calls = append(calls, Call{CallID: tc.Get("id").String(), HashID: hash})
			case !ours(name):
				otherTools = true
			}
			return true
		})
	}
	return calls, otherTools
}

// Continuation builds the next request body: it appends the assistant's
// tool-call turn and a tool_result turn carrying each resolved original, so the
// model can finish with the full content in hand. resolved maps CallID ->
// original text. ok=false if the shapes weren't as expected (caller returns the
// response unchanged — fail open).
func Continuation(provider string, reqBody, resp []byte, resolved map[string]string) ([]byte, bool) {
	switch provider {
	case "anthropic":
		content := gjson.GetBytes(resp, "content")
		if !content.Exists() {
			return nil, false
		}
		asst, err := sjson.SetRaw(`{"role":"assistant"}`, "content", content.Raw)
		if err != nil {
			return nil, false
		}
		out, err := sjson.SetRawBytes(reqBody, "messages.-1", []byte(asst))
		if err != nil {
			return nil, false
		}
		user := `{"role":"user","content":[]}`
		for callID, orig := range resolved {
			blk, _ := sjson.Set(`{"type":"tool_result"}`, "tool_use_id", callID)
			blk, _ = sjson.Set(blk, "content", orig)
			user, _ = sjson.SetRaw(user, "content.-1", blk)
		}
		out, err = sjson.SetRawBytes(out, "messages.-1", []byte(user))
		if err != nil {
			return nil, false
		}
		return out, true
	case "responses":
		items := gjson.GetBytes(reqBody, "input")
		output := gjson.GetBytes(resp, "output")
		if !items.IsArray() || !output.IsArray() {
			return nil, false
		}
		// A Responses summary stash is an array of the exact input items it
		// replaced. Restore those items at their original point before the
		// continuation, so images/audio retain their modality rather than being
		// flattened into a function-output string. Ordinary text stashes still
		// travel as the tool output below.
		replacements := map[int][]gjson.Result{}
		restoredInPlace := map[string]bool{}
		for _, call := range output.Array() {
			id := call.Get("call_id").String()
			orig, ok := resolved[id]
			if !ok || call.Get("name").String() != ToolName {
				continue
			}
			var rawArgs string
			if call.Get("type").String() == "custom_tool_call" {
				rawArgs = call.Get("input").String()
			} else {
				rawArgs = call.Get("arguments").String()
			}
			hash := gjson.Get(rawArgs, "id").String()
			span := gjson.Parse(orig)
			if hash == "" || !span.IsArray() || len(span.Array()) == 0 {
				continue
			}
			valid := true
			for _, original := range span.Array() {
				if !responsesInputItem(original) {
					valid = false
					break
				}
			}
			if !valid {
				continue // not wire input items: answer with the flattened string instead
			}
			for i, item := range items.Array() {
				content := item.Get("content").String()
				if role := item.Get("role").String(); (role == "user" || role == "assistant") && isSummaryText(content) &&
					strings.Contains(content, Marker(hash)) {
					if _, duplicate := replacements[i]; duplicate {
						return nil, false
					}
					replacements[i] = span.Array()
					restoredInPlace[id] = true
					break
				}
			}
		}
		out := reqBody
		if len(replacements) > 0 {
			var b bytes.Buffer
			b.WriteByte('[')
			first := true
			appendItem := func(raw string) {
				if !first {
					b.WriteByte(',')
				}
				first = false
				b.WriteString(raw)
			}
			for i, item := range items.Array() {
				if span, ok := replacements[i]; ok {
					for _, original := range span {
						appendItem(original.Raw)
					}
				} else {
					appendItem(item.Raw)
				}
			}
			b.WriteByte(']')
			var err error
			out, err = sjson.SetRawBytes(out, "input", b.Bytes())
			if err != nil {
				return nil, false
			}
		}
		for _, item := range output.Array() {
			var err error
			out, err = sjson.SetRawBytes(out, "input.-1", []byte(item.Raw))
			if err != nil {
				return nil, false
			}
		}
		for _, item := range output.Array() {
			id := item.Get("call_id").String()
			orig, ok := resolved[id]
			if !ok {
				continue
			}
			typ := item.Get("type").String()
			if typ != "function_call" && typ != "custom_tool_call" {
				return nil, false
			}
			answerType := "function_call_output"
			if typ == "custom_tool_call" {
				answerType = "custom_tool_call_output"
			}
			answer := `{"type":"` + answerType + `"}`
			answer, _ = sjson.Set(answer, "call_id", id)
			if restoredInPlace[id] {
				orig = "[expand: original history restored in place above]"
			}
			answer, _ = sjson.Set(answer, "output", orig)
			var err error
			out, err = sjson.SetRawBytes(out, "input.-1", []byte(answer))
			if err != nil {
				return nil, false
			}
		}
		return out, true

	default: // openai
		msg := gjson.GetBytes(resp, "choices.0.message")
		if !msg.Exists() {
			return nil, false
		}
		out, err := sjson.SetRawBytes(reqBody, "messages.-1", []byte(msg.Raw))
		if err != nil {
			return nil, false
		}
		for callID, orig := range resolved {
			tool, _ := sjson.Set(`{"role":"tool"}`, "tool_call_id", callID)
			tool, _ = sjson.Set(tool, "content", orig)
			out, err = sjson.SetRawBytes(out, "messages.-1", []byte(tool))
			if err != nil {
				return nil, false
			}
		}
		return out, true
	}
}

// The summary headers, mirrored from schema.SummaryHeader and schema.CacheAwareSummaryHeader (this
// leaf package imports no context-guru package); TestSummaryHeadersMatchSchema pins them equal.
const (
	summaryHeader           = "=== History Summary ==="
	cacheAwareSummaryHeader = "=== Compacted Context (cache-aware summary) ==="
)

// isSummaryText reports whether content is a summarizer's spliced summary, from either summarizer.
func isSummaryText(content string) bool {
	return strings.HasPrefix(content, summaryHeader) || strings.HasPrefix(content, cacheAwareSummaryHeader)
}

// responsesInputItem reports whether a stashed element can go back into a Responses `input` array
// as it is. summarize stashes the wire items it replaced, which always can. cache_aware_summarizer
// stashes NORMALIZED chat messages, whose tool exchanges are a role=tool message and an assistant
// message carrying tool_calls: spliced into `input` those make the Responses API return 400, so such
// a stash is answered as a flattened string instead (#727 review, round 3).
func responsesInputItem(v gjson.Result) bool {
	if !v.IsObject() {
		return false
	}
	switch role := v.Get("role").String(); role {
	case "":
		return v.Get("type").String() != ""
	case "user", "assistant", "system", "developer":
		return !v.Get("tool_calls").Exists() && !v.Get("tool_call_id").Exists()
	default:
		return false
	}
}
