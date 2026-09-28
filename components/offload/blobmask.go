package offload

import (
	"regexp"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
)

func init() { components.Register("blobmask", newBlobMask) }

// embeddedBlob matches an inline base64 data URI sitting inside otherwise-normal tool-result
// text: `data:<mime>;base64,<payload>` (screenshots, browser-automation snapshots, file-read
// results that embed an image). Anchored on the SHAPE the bytes take, not on a field name: a
// tool's JSON envelope varies by integration, but a data URI's `;base64,` marker does not.
//
// Deliberately does NOT also match a bare long base64-alphabet run with no `data:` prefix.
// An earlier version did (any 200+-char run of [A-Za-z0-9+/], optionally hex-guarded), and
// review found two real false-positive classes it cannot be told apart from by shape alone: a
// long hex digest chain (an SRI integrity block, a concatenated sha256 listing — hex is a
// subset of the base64 alphabet) and, worse, an opaque API value the agent needs VERBATIM for
// a follow-up call — a pagination/continuation token or a Relay-style cursor, which is
// standard base64, no dots, and routinely exceeds 200 characters. A `data:mime;base64,` URI is
// an unambiguous declaration of intent ("this is encoded binary media"); a bare long run is
// not, and nothing in the bytes distinguishes "an image the agent will never need back" from
// "a token the agent needs on its very next tool call" — both are high-entropy opaque blobs.
// Measured against the deployed snapshot (306,785 `request_content` rows): every one of the
// 1,667 real matches carries an explicit `data:` URI; the bare-run arm matched zero of them on
// its own. So dropping it costs nothing measured here and removes both false-positive classes
// at once, rather than trying to patch them with a field-name denylist that would inevitably
// miss some API's naming convention.
var embeddedBlob = regexp.MustCompile(`data:[A-Za-z0-9.+-]+/[A-Za-z0-9.+-]+;base64,[A-Za-z0-9+/=]{200,}`)

// BlobMask collapses a tool output whose bulk is an embedded base64/binary payload — a
// screenshot, a rendered image, an encoded attachment — that the model cannot usefully read
// as text and that no other component's shape (JSON structure, ANSI noise, command-output
// pattern, repeated search prefix) matches. Reversible via the same stash+marker+expand
// contract as mask/failed_run; MinBlobFrac guards against masking a small aside that merely
// CONTAINS a short encoded value alongside real, useful text.
type BlobMask struct {
	minTokens int
	minFrac   float64
	mode      markerMode
	coldCache bool
}

type blobMaskConfig struct {
	MinTokens int `yaml:"min_tokens"`
	// MinBlobFrac is the share of the message's total length the largest embedded blob
	// must occupy before the WHOLE message is collapsed. Below it, the blob is presumably
	// incidental to text worth keeping, so leave the message alone.
	MinBlobFrac *float64 `yaml:"min_blob_frac"`
	MarkerMode  string   `yaml:"marker_mode"` // full (default) | summary | off
	ColdCache   *bool    `yaml:"cold_cache"`
}

func newBlobMask(raw []byte) (components.Component, error) {
	cfg := blobMaskConfig{MinTokens: 500}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	frac := 0.6
	if cfg.MinBlobFrac != nil {
		frac = *cfg.MinBlobFrac
	}
	return &BlobMask{minTokens: cfg.MinTokens, minFrac: frac,
		mode: parseMarkerMode(cfg.MarkerMode), coldCache: coldCacheDefault(cfg.ColdCache)}, nil
}

func (BlobMask) Name() string                 { return "blobmask" }
func (BlobMask) Enabled(*components.Ctx) bool { return true }

func (b *BlobMask) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	var keys []string
	changed := 0
	for i := range req.Input {
		msg := &req.Input[i]
		if msg.Role != bschemas.ChatMessageRoleTool {
			continue
		}
		if !schema.Rewritable(*msg) {
			rep.Gate("non_text_blocks") // would be dropped by a text rewrite
			continue
		}
		content := schema.MessageText(*msg)
		if content == "" {
			continue
		}
		if fk, _, ok := reapplyFrozen(c, rep, b.Name(), msg); ok {
			changed++
			keys = append(keys, fk...)
			continue
		}
		if gate, skip := skipReduce(c, content); skip {
			rep.Gate(gate)
			continue
		}
		if schema.TextTokens(content) < b.minTokens {
			rep.Gate("below_min_tokens")
			continue
		}
		loc := embeddedBlob.FindStringIndex(content)
		if loc == nil {
			rep.Gate("no_embedded_blob")
			continue
		}
		if float64(loc[1]-loc[0])/float64(len(content)) < b.minFrac {
			rep.Gate("blob_not_dominant") // a small encoded value beside real text — keep it
			continue
		}
		// Same cache-safety rule as mask/failed_run: a NEW collapse only in the uncached
		// tail, or on a turn whose prompt cache has provably expired (cold_cache). Masking
		// content the provider already cached would flip it full->masked and force a
		// cache-write of the whole suffix — the exact trap this deployment already pays
		// for elsewhere (see cachesplit's rationale).
		if !c.TailOnlyCold(i, b.coldCache) && !repairLostFreeze(c, b.Name(), content) {
			rep.Gate("cached_prefix")
			continue
		}
		newText, key, eff, ok := tryMark(c, b.mode, content, " [full output: call "+expand.ToolName+"]",
			func(tok string) string { return "[embedded binary payload masked] " + tok })
		if !ok {
			rep.Gate("marker_no_win") // rewrite+marker wouldn't shrink this message
			continue
		}
		if !commitMark(c, rep, eff, key, content) {
			continue // the store cannot back the marker; leave this message verbatim
		}
		schema.SetMessageText(msg, newText)
		freeze(c, b.Name(), content, newText) // freeze so later turns replay it (no churn)
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}
	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

func init() {
	components.RegisterFields("blobmask", blobMaskConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: 500, Min: 1,
			Hint: "Only mask a message above this many tokens."},
		{Key: "min_blob_frac", Type: components.FieldFloat, Default: 0.6,
			Hint: "Share of the message's length the largest embedded base64/data-URI payload must occupy before the whole message is masked. Keeps a short encoded value beside real text from taking the rest of the message with it."},
		markerModeField(),
		coldCacheField(),
	})
}
