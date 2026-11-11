package apply

import (
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/reformat"
)

// envelopeAdapter supplies only wire-shape readers. The decisions about which
// declarations to remove, and how to report them, remain shared.
type envelopeAdapter interface {
	pendingCallFor([]byte, map[string]bool, map[string]bool) bool
	proseRegion([]byte) string
	skillListingPath([]byte) (string, string)
}

// transformEnvelope is the shared host for components that operate on fields
// outside the normalized message view. Shape differences belong in the small
// wire readers used by the transforms, not in separate component algorithms.
// It runs before an adapter records any write-back path or byte offset.
func transformEnvelope(body []byte, pipe *components.Pipeline, bypass bool, wire envelopeAdapter) (out []byte, toolSchema bool, filteredTokens, filteredDecls int) {
	out = body
	if bypass || pipe == nil {
		return
	}
	if pipe.Has("toolschema") {
		out, toolSchema = reformat.CompactToolSchemas(out)
	}
	if tf, ok := pipe.Find("toolfilter").(interface{ Removed() []string }); ok {
		out, filteredTokens, filteredDecls = filterDeclarationsWithAdapter(out, tf.Removed(), wire)
		var skillTokens, skillDecls int
		out, skillTokens, skillDecls = filterSkillListingWithAdapter(out, tf.Removed(), wire)
		filteredTokens += skillTokens
		filteredDecls += skillDecls
	}
	return
}
