package extract

import (
	"encoding/json"
	"strconv"
	"strings"
)

// The cache-riding triage ask: one question over the request's own cached transcript that returns,
// per candidate tool output, keep | extract (with the extract) | park. It reuses AdjudicationItem
// (integer labels, tool_use id and head line as locators) and stripFences from the sweep's ask.

// Rider actions.
const (
	RideKeep    = "keep"
	RideExtract = "extract"
	RidePark    = "park"
)

// RideDecision is one parsed answer.
type RideDecision struct {
	Label   int    `json:"label"`
	Action  string `json:"action"`
	Extract string `json:"extract,omitempty"`
}

const rideContract = `[OPERATOR INSTRUCTION - this is not a task request]

The conversation above is your own. Do not continue the task and do not call any tool. This turn has one job: triage the tool outputs listed below.

For each one decide what the rest of the task still needs from it:
  keep    - most of it is still needed verbatim, or you are not sure. When in doubt, keep.
  extract - only part of it is still needed. Put that part in "extract": copy the needed lines VERBATIM (paths, identifiers, numbers, error text, line numbers). You may add one first line starting with "SUMMARY:" saying what was left out. The extract must be much shorter than the output.
  park    - none of it is needed now. It is set aside behind a handle and can be fetched back.

Judge the outputs against each other and against everything that came after them: an output whose content was already acted on, replaced by a later output, or repeated is a candidate for extract or park.

Reply with ONLY a JSON array, one object per output, no prose, for example:
[{"label":0,"action":"keep"},{"label":1,"action":"extract","extract":"SUMMARY: ...\nfile.go:42: ..."},{"label":2,"action":"park"}]
`

// BuildRideAsk renders the instruction plus the inventory.
func BuildRideAsk(items []AdjudicationItem) string {
	var b strings.Builder
	b.WriteString(rideContract)
	b.WriteString("\nTOOL OUTPUTS UNDER CONSIDERATION. Refer to them by these labels only:\n")
	for _, it := range items {
		b.WriteString("  [" + strconv.Itoa(it.Label) + "] " + strconv.Itoa(it.SizeTokens) + " tokens")
		if it.ID != "" {
			b.WriteString(", tool_use id " + it.ID)
		}
		b.WriteString(", begins: " + it.Head + "\n")
	}
	return b.String()
}

// ParseRideDecisions reads the reply. ok is false when no JSON array of objects could be read; an
// empty array is a legitimate "keep everything" and parses ok.
func ParseRideDecisions(reply string) ([]RideDecision, bool) {
	s := stripFences(strings.TrimSpace(reply))
	for i := strings.Index(s, "["); i >= 0; {
		for j := strings.LastIndex(s, "]"); j > i; j = strings.LastIndex(s[:j], "]") {
			var out []RideDecision
			if json.Unmarshal([]byte(s[i:j+1]), &out) == nil {
				for k := range out {
					out[k].Action = strings.ToLower(strings.TrimSpace(out[k].Action))
				}
				return out, true
			}
		}
		n := strings.Index(s[i+1:], "[")
		if n < 0 {
			break
		}
		i += 1 + n
	}
	return nil, false
}

// GroundedExtract reports whether at least minFrac of the extract's substantive lines (trimmed,
// at least 6 bytes, not the SUMMARY: line) occur verbatim in the original. An extract the model
// wrote from memory rather than copied fails it. An extract with no substantive line fails too.
func GroundedExtract(original, extract string, minFrac float64) bool {
	var n, hit int
	for _, ln := range strings.Split(extract, "\n") {
		ln = strings.TrimSpace(ln)
		if len(ln) < 6 || strings.HasPrefix(ln, "SUMMARY:") {
			continue
		}
		n++
		if strings.Contains(original, ln) {
			hit++
		}
	}
	return n > 0 && float64(hit) >= minFrac*float64(n)
}
