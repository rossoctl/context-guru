package apply

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// volatileTailMarkers start the environment snapshot that old Claude Code versions
// appended to the end of the main system block. The tool filter uses them to leave that
// snapshot out of the prose it checks. Matched with a leading newline so prose that
// merely mentions the phrase is not a hit.
//
// Claude Code 2.1.328 to 2.1.332 no longer put the snapshot in `system`: it sits in the
// first user message. These markers match nothing there, which is the safe result.
var volatileTailMarkers = []string{
	"\nRecent commits:\n",
	"\nCurrent branch: ",
	"\ngitStatus:",
	"\nHere is a snapshot of the current directory",
}

// explicitBreakpointProvider reports whether this backend honours Anthropic-style
// `cache_control`. OpenAI- and Gemini-shaped wires cache an implicit longest prefix.
func explicitBreakpointProvider(p bschemas.ModelProvider) bool {
	return modelinfo.HonoursCacheControl(p)
}
