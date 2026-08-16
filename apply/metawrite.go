package apply

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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

// maxWireBreakpoints is the provider's hard cap on cache_control directives per
// request. Over it, the request 400s.
const maxWireBreakpoints = 4

// blockMarks are the two spellings a breakpoint takes on an array ELEMENT — a system
// block, a tool, or a content block. The cap applies across every location together, and
// the count stays STRUCTURAL (a field, per gjson) for the same reason hasCacheBreakpoint
// does: a tool output whose text merely contains "cache_control" must not count.
//
// `cachePoint` is the Bedrock Converse spelling, and Bedrock places it as its OWN entry in
// the `system` and `tools` arrays — the two locations defect 2 is about. Both must be
// counted or the cap stays breachable on Bedrock exactly as it was on Anthropic
// (constructed: 6 on the wire, counter blind to 3 of them).
var blockMarks = [...]string{"cache_control", "cachePoint"}

// mayHold is the cheap prefilter guarding every structural parse below: neither key can
// be present as a FIELD of raw if their shared prefix is not even a substring of raw's
// bytes. A plain substring scan is memchr-fast, while gjson.Get on a message whose
// tool_result carries 50 KB of text parses that text. It errs only toward doing the parse
// anyway (a payload that merely mentions "cache"), never toward missing a real mark — so
// the count stays exactly as structural as it was.
func mayHold(raw string) bool { return strings.Contains(raw, "cache") }

// wireBreakpoints counts every breakpoint the provider will see in this request.
//
// A component cannot count these for itself. `system` and `tools` never reach it at
// all, and the `tool_result` blocks this package normalizes into synthetic role=tool
// messages lose their mark on the way in (see toolMessage) — so on real Claude Code
// traffic all three of the agent's own breakpoints were invisible to it (2 in
// `system`, 1 on a `tool_result` block), and it computed 3 free slots when 1 was free:
// 6 on the wire, and a 400 (issue #32).
//
// One pass per top-level array, not one gjson `#.field` query per location. The path
// form re-scanned the whole body seven times and squashed every matched array on the
// way, which made this ~22% of the rewrite path's CPU on real 600 KB Claude Code
// requests — and it runs twice per request (inbound count + the cap check).
func wireBreakpoints(body []byte) int {
	n := 0
	// One walk of the top-level object rather than a Get per field: `messages` is the
	// bulk of the body and gjson has to scan past it to reach whatever follows.
	gjson.ParseBytes(body).ForEach(func(key, val gjson.Result) bool {
		switch key.String() {
		case "system", "tools":
			val.ForEach(func(_, v gjson.Result) bool {
				n += elemMarks(v)
				return true
			})
		case "messages":
			val.ForEach(func(_, m gjson.Result) bool {
				if !mayHold(m.Raw) {
					return true
				}
				// A MESSAGE carries only the Anthropic spelling (cachePoint is a block).
				if m.Get("cache_control").IsObject() {
					n++
				}
				m.Get("content").ForEach(func(_, blk gjson.Result) bool {
					n += elemMarks(blk)
					return true
				})
				return true
			})
		}
		return true
	})
	return n
}

// elemMarks counts the breakpoint marks on one array element.
func elemMarks(v gjson.Result) int {
	if !mayHold(v.Raw) {
		return 0
	}
	n := 0
	for _, k := range blockMarks {
		if strings.Contains(v.Raw, k) && v.Get(k).IsObject() {
			n++
		}
	}
	return n
}
