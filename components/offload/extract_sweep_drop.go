package offload

import (
	"encoding/json"
	"fmt"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

// THE DROP PATH for the cold-sweep adjudicator: what a dropped tool output leaves behind, and how
// it stays recoverable.
//
// Two properties are load-bearing here, and each is a test.
//
// THE RESIDUE TRANSPORTS NOTHING. It is computed from the output's SHAPE — class, size, line and
// record counts — by this code, and it contains no byte of the output itself. That is stricter than
// the merged design's residue, which fell back to a 96-character head peek for unstructured content.
// A head peek is our code copying rather than the model copying, so it is not the failure cc1aa9f
// was about, but it is still content in the request that nothing verified, and the whole point of an
// adjudicator over a compactor is that no content moves. It also does not help where it looks like
// it should: for a record set the first rows say nothing about whether the field you want is in
// there, which is the argument the merged design already accepted for structured content and then
// declined to apply to the rest.
//
// THE DROP IS REVERSIBLE, AND THE MODEL IS NOT TOLD SO. A full marker stashes the original and
// `expand` restores it. A drop advertised as reversible that is not would be a worse defect than no
// drop at all, so it is verified end to end rather than assumed from the fact that commitMark was
// called. Note the asymmetry with the prompt, which never mentions recoverability: measured,
// reassuring the model that removals stay recoverable produced 91% removal at 6% live-kept, so the
// operator gets the safety net and the model does not get to hear about it.

// sweepDescriptor renders the shape residue for a dropped output.
//
// It answers "what was here", never "what did it say". contentClass supplies the kind from the same
// head-sniffing regexes the economic gate is calibrated on, so the descriptor and the gate cannot
// disagree about what a candidate is.
func sweepDescriptor(content string) string {
	kind := "tool output"
	if name, _, ok := contentClass(content); ok {
		kind = name
	}
	lines := strings.Count(content, "\n") + 1
	shape := fmt.Sprintf("%s, %d lines, %d tokens", kind, lines, schema.TextTokens(content))
	if n, ok := recordCount(content); ok {
		shape = fmt.Sprintf("%s, %d records, %d lines, %d tokens", kind, n, lines,
			schema.TextTokens(content))
	}
	return "[context-guru removed a spent tool output — " + shape + "]"
}

// recordCount counts the top-level elements of a JSON array, which is the one "how much was here"
// figure a line count cannot supply: a multi-megabyte API result is routinely a SINGLE line, and
// "1 line" is a useless thing to tell an agent about 200 records.
//
// Only a top-level array, and only when it parses. A partial or streaming payload gets the line
// count alone rather than a guess, because the descriptor's whole value is that every number in it
// is true.
func recordCount(content string) (int, bool) {
	s := strings.TrimSpace(content)
	if !strings.HasPrefix(s, "[") {
		return 0, false
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(s), &rows); err != nil {
		return 0, false
	}
	return len(rows), true
}

// applySweepDrop replaces one adjudicated-spent tool output with its shape descriptor plus the
// marker, stashing the original so `expand` can restore it. It reports the store key it wrote (empty
// in the degraded marker modes) and whether the message was changed at all.
//
// Serial by contract, like extract_llm's own splice: the store write and the message mutation are
// not concurrency-safe.
//
// It goes through tryMark rather than writing the text directly, which is what makes the
// never-worse check MARKER-INCLUSIVE. The descriptor is small, but the marker plus its recovery hint
// is not free, and the pipeline's aggregate guard is per-request rather than per-message — so
// without this a drop just above the floor could grow the message it was meant to shrink.
func applySweepDrop(c *components.Ctx, rep *components.Report, mode markerMode,
	msg *bschemas.ChatMessage, content string) (key string, ok bool) {
	return sweepDrop(c, rep, mode, msg, content, false)
}

// applySweepDropReplay is applySweepDrop for a drop THIS SESSION ALREADY MADE on an earlier
// turn, replayed to keep the request prefix byte-stable.
//
// It exists because the two cases must answer a refused payload differently, and sharing one
// function made them answer it the same way. A NEW drop declines — nothing has been promised
// yet, so leaving the output verbatim costs only tokens. A REPLAY cannot decline: the provider's
// cached prefix already holds the removed form, so sending the output back in full is itself the
// cache-destructive move, and it cannot un-send the marker that went out turns ago. So the
// replay proceeds and a missing payload is diagnosed rather than obeyed (see commitRefresh).
func applySweepDropReplay(c *components.Ctx, rep *components.Report, mode markerMode,
	msg *bschemas.ChatMessage, content string) (key string, ok bool) {
	return sweepDrop(c, rep, mode, msg, content, true)
}

func sweepDrop(c *components.Ctx, rep *components.Report, mode markerMode,
	msg *bschemas.ChatMessage, content string, replay bool) (key string, ok bool) {
	desc := sweepDescriptor(content)
	hint := " [full output: call " + expand.ToolName + "]"
	newText, key, eff, ok := tryMark(c, mode, content, hint, func(tok string) string {
		if tok == "" {
			return desc
		}
		return desc + "\n" + tok
	})
	if !ok {
		return "", false
	}
	if replay {
		commitRefresh(c, key, content) // never refuses; a false answer is counted as dangling
		recordOwner(c, key)
	} else if !commitMark(c, rep, eff, key, content) {
		return "", false // the store cannot back the marker; the drop does not happen
	}
	schema.SetMessageText(msg, newText)
	return key, true
}
