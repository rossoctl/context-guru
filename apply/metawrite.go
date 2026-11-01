package apply

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/context-guru/schema"
)

// Metadata writes: the narrow exception to the losslessness guard.
//
// bifrost cannot round-trip an Anthropic assistant turn that carries a `tool_use`
// block — it drops `id`, `name` and `input` on unmarshal — so the writeback loop
// discards any change to such a message rather than splice a lossy re-marshal.
// That is correct for CONTENT. But cacheinject's only possible targets on real
// agent traffic are exactly those messages, so every breakpoint it placed was
// thrown away before the request was sent (measured: 46 applied, 0 forwarded over
// 40 captured requests — issue #32).
//
// `cache_control` is metadata, not content: it changes nothing the model reads and
// needs no message model to express. So when a component's ONLY change to a
// message is adding `cache_control` to content blocks, we write those keys at their
// exact paths on the ORIGINAL raw bytes instead of splicing a re-marshal. Nothing
// else in the message is read or rewritten, so no provider field can be dropped
// and the guard's reason to exist does not arise.
//
// Anything beyond added `cache_control` keys — a text edit, a removed key, a
// changed block — is still discarded.

// metaWrite is one metadata key to set, as a path relative to the message and the
// raw JSON to write there.
type metaWrite struct {
	path string // "content.<b>.cache_control"
	raw  string
}

// metadataOnlyWrites compares a message's pre- and post-pipeline canonical
// marshals and returns the metadata writes that reproduce the change, or ok=false
// if the change is anything other than added content-block `cache_control` keys.
func metadataOnlyWrites(pre, post []byte) (writes []metaWrite, ok bool) {
	preBlocks := gjson.GetBytes(pre, "content")
	postBlocks := gjson.GetBytes(post, "content")
	if !preBlocks.IsArray() || !postBlocks.IsArray() {
		return nil, false // string content carries no block to mark
	}
	pb, qb := preBlocks.Array(), postBlocks.Array()
	if len(pb) != len(qb) {
		return nil, false // blocks added/removed — not a metadata change
	}
	stripped := post
	for b := range qb {
		p := "content." + strconv.Itoa(b) + ".cache_control"
		cc := qb[b].Get("cache_control")
		if !cc.Exists() || pb[b].Get("cache_control").Exists() {
			continue
		}
		writes = append(writes, metaWrite{path: p, raw: cc.Raw})
		var err error
		if stripped, err = sjson.DeleteBytes(stripped, p); err != nil {
			return nil, false
		}
	}
	if len(writes) == 0 {
		return nil, false
	}
	// The decisive check: with the added keys removed, the post-pipeline message must
	// be identical to the pre-pipeline one. Otherwise something else changed too.
	if !jsonEqual(stripped, pre) {
		return nil, false
	}
	return writes, true
}

// applyMetaWrites sets each metadata key on ONE raw message's bytes. It refuses
// (ok=false, msg untouched) if the raw message's block layout does not match what
// the writes assume, so a shape the normalizer and the raw body disagree about can
// never be written to the wrong block.
//
// It takes the message rather than the whole body because metaWrite.path is already
// message-relative and every gjson/sjson call here would otherwise re-scan and re-copy
// the entire request per write. The result is identical either way: sjson's splice is
// local (prefix + edited subtree + suffix), so the caller splices the message back at
// its own byte span — see spliceMessages.
func applyMetaWrites(msg []byte, nBlocks int, writes []metaWrite) ([]byte, bool) {
	blocks := gjson.GetBytes(msg, "content")
	if !blocks.IsArray() || len(blocks.Array()) != nBlocks {
		return msg, false
	}
	out := msg
	for _, w := range writes {
		if gjson.GetBytes(out, w.path).Exists() {
			return msg, false // caller already set it — never overwrite
		}
		next, err := sjson.SetRawBytes(out, w.path, []byte(w.raw))
		if err != nil {
			return msg, false
		}
		out = next
	}
	return out, true
}

// maxWireBreakpoints is the provider's hard cap on cache_control directives per request.
const maxWireBreakpoints = schema.MaxBreakpoints

// Breakpoints is where a request's breakpoints sit, by location. See schema.Breakpoints.
type Breakpoints = schema.Breakpoints

// CountBreakpoints counts the breakpoints the provider will see in this request, by location,
// except a mark whose key is spelled with a \u escape: this is the hot-path count (the cap check
// runs it twice per request) and its prefilter is "cache" only. schema.CountBreakpointsEscaped
// counts those too; hasCacheBreakpoint uses it. See schema.CountBreakpoints.
func CountBreakpoints(body []byte) Breakpoints { return schema.CountBreakpoints(body) }

// wireBreakpoints is the total, which is what the provider's cap applies to.
func wireBreakpoints(body []byte) int { return CountBreakpoints(body).Total() }
