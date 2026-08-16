package apply

import (
	"encoding/json"
	"log/slog"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Volatile-tail split: keep a churning environment snapshot out of the cached
// prefix.
//
// The provider hashes the request prefix cumulatively (tools -> system ->
// messages), and a cache entry at a breakpoint covers everything before it. So a
// block whose tail changes every session makes its own breakpoint unmatchable, even
// when the rest of the block is identical.
//
// Claude Code appends a live environment snapshot to the END of its main system
// block:
//
//	Current branch: main
//	...
//	Recent commits:
//	0898367954 SWE-bench
//	76e0151ea0 Added "Bugfixes" section to release notes for 3.1.2.
//
// Measured across 50 SWE-bench tasks, that block is ~7,017 tokens of which the
// first 6,921 (98.4%) are byte-identical across sessions; only the trailing git/env
// snapshot differs. Because the block is ONE cacheable unit with its breakpoint at
// the end, the hash covers the volatile tail.
//
// Unlike a header, this tail is REAL content — it cannot be moved or dropped
// without lying to the model about the repo state. What it can be is SPLIT: emit
// [shared prefix][volatile tail] as two text blocks with the same concatenated
// text, and put the breakpoint on the first. Adjacent text blocks concatenate, so
// the model sees a byte-identical prompt while the provider gains a hash boundary
// that excludes the churn.
//
// Only meaningful where breakpoints are EXPLICIT (Anthropic family). Under an
// implicit longest-prefix cache the match already ends at the divergence, so a
// block boundary buys nothing.
//
// Split point = the last line break before the first known volatile marker. Line
// granularity keeps the halves human-meaningful and avoids splitting mid-word.

// volatileTailMarkers start the environment snapshot Claude Code appends. Matched
// with a leading newline so prose that merely mentions the phrase is not a hit.
var volatileTailMarkers = []string{
	"\nRecent commits:\n",
	"\nCurrent branch: ",
	"\ngitStatus:",
	"\nHere is a snapshot of the current directory",
}

// minSplitTokens keeps this from firing on small blocks, where the extra breakpoint
// slot costs more optionality than the split can recover.
const minSplitTokens = 1024

// splitVolatileTail splits the FIRST system block large enough to matter that ends
// in an environment snapshot into [stable][volatile], moving that block's
// cache_control onto the stable half so the provider's hash stops covering the
// volatile part. Text is unchanged in concatenation, so the model sees exactly the
// same prompt. Fails open: any parse problem returns the input untouched.
//
// It also reports how the rewrite moved the rest of the body: everything at or after
// byte `shiftAt` in the INPUT sits `shift` bytes later in the output. sjson replaces the
// `system` value in place (prefix + new value + suffix), so that is the whole of it — and
// the writeback needs it to keep using the byte offsets it already has for each message
// instead of re-parsing the body to find them again. shift is 0 when nothing split.
func splitVolatileTail(body []byte, provider bschemas.ModelProvider) (out []byte, split bool, shiftAt, shift int) {
	if !explicitBreakpointProvider(provider) {
		return body, false, 0, 0
	}
	sys := gjson.GetBytes(body, "system")
	if !sys.Exists() || !sys.IsArray() {
		return body, false, 0, 0 // a string system prompt carries no block to split
	}
	blocks := sys.Array()
	if len(blocks) == 0 {
		return body, false, 0, 0
	}

	sysOut := make([]json.RawMessage, 0, len(blocks)+1)
	for _, b := range blocks {
		txt := b.Get("text").String()
		at := -1
		if !split && len(txt)/4 >= minSplitTokens {
			for _, mk := range volatileTailMarkers {
				if i := strings.Index(txt, mk); i >= 0 && (at < 0 || i < at) {
					at = i + 1 // keep the newline with the stable half
				}
			}
		}
		// Require both halves to be non-trivial: a split that leaves a ~0-token
		// stable half buys nothing and burns one of the four breakpoint slots.
		if at <= 0 || at/4 < minSplitTokens || at >= len(txt) {
			sysOut = append(sysOut, json.RawMessage(b.Raw))
			continue
		}

		// Rebuild from the ORIGINAL block so any field this code does not know about
		// (citations today, whatever the API adds tomorrow) survives verbatim. Building
		// a fresh {"type","text"} map instead would silently drop them, which would
		// break the losslessness guarantee in a way no existing test would catch.
		var orig map[string]any
		if json.Unmarshal([]byte(b.Raw), &orig) != nil {
			sysOut = append(sysOut, json.RawMessage(b.Raw))
			continue
		}
		stable := make(map[string]any, len(orig))
		volatile := make(map[string]any, len(orig))
		for k, v := range orig {
			stable[k] = v
			volatile[k] = v
		}
		stable["text"] = txt[:at]
		volatile["text"] = txt[at:]
		// The breakpoint belongs on the stable half only; leaving one on the volatile
		// half would put the churn back inside a hashed prefix. BOTH spellings must go:
		// Bedrock/Vertex write `cachePoint` where Anthropic writes `cache_control`, and
		// wireBreakpoints counts both (metawrite.go). Deleting only one DUPLICATES the
		// other — the split copies the block, so a system block carrying an inline
		// cachePoint turns 1 breakpoint into 2 and can push the wire past the provider's
		// cap of four (measured: 4 inbound -> 5 on the wire -> 400).
		delete(volatile, "cache_control")
		delete(volatile, "cachePoint")
		sb, err1 := json.Marshal(stable)
		vb, err2 := json.Marshal(volatile)
		if err1 != nil || err2 != nil {
			sysOut = append(sysOut, json.RawMessage(b.Raw))
			continue
		}
		sysOut = append(sysOut, sb, vb)
		split = true
	}
	if !split {
		return body, false, 0, 0
	}
	enc, err := json.Marshal(sysOut)
	if err != nil {
		return body, false, 0, 0
	}
	next, err := sjson.SetRawBytes(body, "system", enc)
	if err != nil {
		return body, false, 0, 0
	}
	slog.Debug("context-guru: split volatile tail out of a system block",
		"provider", provider)
	// sjson spliced the new value over [sys.Index, sys.Index+len(sys.Raw)); every byte
	// after that range moved by the length difference.
	return next, true, sys.Index + len(sys.Raw), len(next) - len(body)
}

// explicitBreakpointProvider reports whether this backend honours Anthropic-style
// `cache_control`, which is what makes the split meaningful. OpenAI- and
// Gemini-shaped wires cache an implicit longest prefix and stop at the divergence on
// their own, so there is nothing for a block boundary to add there.
func explicitBreakpointProvider(p bschemas.ModelProvider) bool {
	switch p {
	case bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex:
		return true
	}
	return false
}
