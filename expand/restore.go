package expand

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/context-guru/store"
)

// RESTORING EXPAND RESULTS ON THE REQUEST PATH.
//
// Why this exists rather than a response-side interception. The response loop can only auto-continue
// when the expand call arrives ALONE: if the model also called a real tool, the client -- not the proxy
// -- must execute it, so the response has to be relayed and the loop bails (`otherTools` in
// ResponseCalls). Measured on live traffic, that is the DOMINANT case: of 107 turns whose expand call
// was refused, 102 carried two or more tool_use blocks and only 5 carried one. So a response-side fix
// addresses 5% of the problem.
//
// The client then executes the real tools, finds no `context_guru_expand` among its own tools -- the
// proxy injected it, so the client has never heard of it -- and answers with something like
// `Tool 'context_guru_expand' not found`. The model loses its recovery path and re-runs the original
// tool instead, paying a full tool execution plus fresh output and enlarging the transcript that
// provoked the cut.
//
// This works WITH the client's loop instead of against it. On the next request, the client's own
// tool_result for the expand call is replaced with the stashed original before the request goes
// upstream. No response splitting, no need for the client to implement anything, and the mixed-call
// case is covered because the real tools were executed normally by whoever owns them.
//
// DETERMINISM IS REQUIRED, NOT OPTIONAL. The client resends its whole history every turn, so this
// substitution must be applied identically on every turn or the prefix flaps and the model sees
// content appear and disappear. Both consequences are real: a flapping prefix is a cache miss from the
// point of the change, and a model that saw a failure and reasoned about it should not later find the
// record contradicting itself. Being keyed only by marker id and store contents, the rewrite is
// deterministic for as long as the stash lives -- which is what MarkKeptVerbatim protects, by ensuring
// expanded content is not re-compacted and so needs no marker on later turns.
//
// See docs/proposals/coref-compaction.md, "reversibility": the durability of the stash is what makes
// this coherent, and `expand_unresolved_missing` in /stats counts the cases where it was not.

// RestoreResults replaces the client's tool_result for every expand call with the stashed original.
// Returns the rewritten body and the ids restored, so the caller can protect them from re-compaction.
// Unresolvable ids are left exactly as the client wrote them: the model is already reading a failure,
// and inventing a different failure string would only add a second story.
func RestoreResults(provider string, body []byte, s store.Store) ([]byte, []string) {
	if s == nil {
		return body, nil
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, nil
	}
	// tool_use id -> marker hash, for expand calls only.
	want := map[string]string{}
	msgs.ForEach(func(_, m gjson.Result) bool {
		m.Get("content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() == "tool_use" && blk.Get("name").String() == ToolName {
				if id := blk.Get("id").String(); id != "" {
					want[id] = blk.Get("input.id").String()
				}
			}
			return true
		})
		// OpenAI dialect: tool_calls on the assistant message.
		m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			if tc.Get("function.name").String() == ToolName {
				if id := tc.Get("id").String(); id != "" {
					want[id] = gjson.Get(tc.Get("function.arguments").String(), "id").String()
				}
			}
			return true
		})
		return true
	})
	if len(want) == 0 {
		return body, nil
	}

	out := body
	var restored []string
	arr := gjson.GetBytes(out, "messages").Array()
	for i := range arr {
		base := "messages." + itoa(i)
		// Anthropic: tool_result blocks inside a user message.
		blocks := gjson.GetBytes(out, base+".content")
		if blocks.IsArray() {
			for b, blk := range blocks.Array() {
				if blk.Get("type").String() != "tool_result" {
					continue
				}
				hash, ok := want[blk.Get("tool_use_id").String()]
				if !ok {
					continue
				}
				orig, found := Resolve(s, hash)
				if !found {
					continue // leave the client's own failure text in place
				}
				if nb, err := sjson.SetBytes(out, base+".content."+itoa(b)+".content", orig); err == nil {
					out = nb
					restored = append(restored, hash)
				}
			}
			continue
		}
		// OpenAI: a role=tool message answering one call.
		if gjson.GetBytes(out, base+".role").String() == "tool" {
			hash, ok := want[gjson.GetBytes(out, base+".tool_call_id").String()]
			if !ok {
				continue
			}
			if orig, found := Resolve(s, hash); found {
				if nb, err := sjson.SetBytes(out, base+".content", orig); err == nil {
					out = nb
					restored = append(restored, hash)
				}
			}
		}
	}
	if len(restored) > 0 {
		restoredCount.Add(int64(len(restored)))
	}
	return out, restored
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
