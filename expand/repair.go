package expand

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Unavailable is what a caller substitutes for an id the store no longer holds. One string
// for both halves of reversibility — the continuation loop and RepairToolResults — because
// an aged-out stash must read the same to the model however it was noticed.
func Unavailable(hashID string) string {
	return "[expand: original for id " + hashID + " is no longer available]"
}

// RepairToolResults answers, on the way UPSTREAM, an expand call the CLIENT had to answer
// itself.
//
// No client implements context_guru_expand: it is this proxy's tool, advertised by this
// proxy, and this proxy's marker text is what tells the model to call it. So a client that
// receives the call answers with its own error — Claude Code's is
// `<tool_use_error>Error: No such tool available: context_guru_expand</tool_use_error>` —
// and the model concludes the content it asked for cannot be had.
//
// The response side intercepts the call wherever it can (see proxy/ssepeek.go), but some
// cases are structural: the model batched expand with a tool only the client owns, the
// continuation round cap, an event stream that will not reconstruct, and a bypassed request
// carrying markers minted on an earlier turn. Every one of them ends up here, on the next
// request, which is the single place all of them pass through — so there is one repair
// rather than six.
//
// resolve returns the stored original for a marker id; ids it cannot resolve get
// Unavailable, which at least tells the model the content aged out instead of inviting it to
// retry a tool that "does not exist". restored holds the originals actually recovered, for
// the caller's accounting.
//
// It is deliberately idempotent and byte-stable: the client keeps its own copy of the error,
// so the same transcript arrives needing the same repair on every later turn, and repairing
// it to the same bytes each time is what keeps the provider's prefix cache warm.
func RepairToolResults(provider string, body []byte, resolve func(id string) (string, bool)) (out []byte, restored []string) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, nil
	}
	// Only OUR tool's calls, mapped from the id the answer will carry to the marker id the
	// model asked for. Reading the tool_use out of the transcript rather than trusting the
	// tool_result is what makes this safe: a client's tool_result is only rewritten when the
	// assistant turn it answers called context_guru_expand.
	ours := map[string]string{}
	for _, m := range msgs.Array() {
		if provider == "anthropic" {
			for _, blk := range m.Get("content").Array() {
				// An id-less tool_use would make "" a live key, and then a tool_result that
				// carries no tool_use_id — someone else's block — matches it and is
				// overwritten. Both halves of the pair must name an id.
				if blk.Get("type").String() == "tool_use" && blk.Get("name").String() == ToolName {
					if id := blk.Get("id").String(); id != "" {
						ours[id] = blk.Get("input.id").String()
					}
				}
			}
			continue
		}
		for _, tc := range m.Get("tool_calls").Array() {
			if tc.Get("function.name").String() == ToolName {
				if id := tc.Get("id").String(); id != "" {
					ours[id] = gjson.Get(tc.Get("function.arguments").String(), "id").String()
				}
			}
		}
	}
	if len(ours) == 0 {
		return body, nil
	}
	out = body
	for mi, m := range msgs.Array() {
		if provider != "anthropic" {
			if m.Get("role").String() != "tool" {
				continue
			}
			out, restored = repairOne(out, restored, ours, m.Get("tool_call_id").String(),
				"messages."+strconv.Itoa(mi), false, resolve)
			continue
		}
		for bi, blk := range m.Get("content").Array() {
			if blk.Get("type").String() != "tool_result" {
				continue
			}
			out, restored = repairOne(out, restored, ours, blk.Get("tool_use_id").String(),
				"messages."+strconv.Itoa(mi)+".content."+strconv.Itoa(bi),
				blk.Get("is_error").Exists(), resolve)
		}
	}
	return out, restored
}

// repairOne rewrites the content of one tool_result at path, if it answers our tool. Every
// failure leaves the block exactly as it arrived — a repair that cannot be made must never
// cost the model a turn it would otherwise have had.
func repairOne(body []byte, restored []string, ours map[string]string, callID, path string,
	hasErrFlag bool, resolve func(id string) (string, bool)) ([]byte, []string) {
	if callID == "" {
		return body, restored // answers nothing; not ours to touch
	}
	hashID, ok := ours[callID]
	if !ok {
		return body, restored
	}
	orig, found := resolve(hashID)
	if !found {
		orig = Unavailable(hashID)
	}
	nb, err := sjson.SetBytes(body, path+".content", orig)
	if err != nil {
		return body, restored
	}
	body = nb
	if hasErrFlag {
		// The block is no longer an error: leaving is_error set tells the model its own
		// tool call failed while handing it the result of that call.
		if nb, err := sjson.SetBytes(body, path+".is_error", false); err == nil {
			body = nb
		}
	}
	if found {
		restored = append(restored, orig)
	}
	return body, restored
}
