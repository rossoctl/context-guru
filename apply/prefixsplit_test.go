package apply

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// splitTail drops the byte-shift bookkeeping splitVolatileTail reports for the
// writeback's benefit; these tests are about the split itself.
func splitTail(body []byte, p bschemas.ModelProvider) ([]byte, bool) {
	out, split, _, _ := splitVolatileTail(body, p)
	return out, split
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A system block shaped like Claude Code's: a large stable instruction body with a
// live git snapshot appended to the end.
func blockWithGitTail(stableTokens int) (full, stable, volatile string) {
	stable = strings.Repeat("You follow the repository conventions exactly.\n", stableTokens/10)
	volatile = "Current branch: main\nRecent commits:\n0898367954 SWE-bench\n"
	return stable + "\n" + volatile, stable + "\n", volatile
}

func sysBody(blocks ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet-5",
		"system":   blocks,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	return b
}

func textBlock(text string, cc bool) map[string]any {
	m := map[string]any{"type": "text", "text": text}
	if cc {
		m["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return m
}

// The core behaviour: the churning tail is separated so the breakpoint covers only
// the stable half.
func TestSplitsGitTailOffStableBody(t *testing.T) {
	full, stable, volatile := blockWithGitTail(6000)
	got, split := splitTail(sysBody(textBlock(full, true)), bschemas.Anthropic)
	if !split {
		t.Fatal("did not split a block with a git snapshot tail")
	}
	blocks := gjson.GetBytes(got, "system").Array()
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Get("text").String() != stable {
		t.Fatal("stable half is not the instruction body")
	}
	if blocks[1].Get("text").String() != volatile {
		t.Fatalf("volatile half is wrong: %.60q", blocks[1].Get("text").String())
	}
	// The breakpoint must move to the stable half, and the volatile half must not
	// carry one — that is the whole point.
	if !blocks[0].Get("cache_control").Exists() {
		t.Fatal("breakpoint did not move to the stable half")
	}
	if blocks[1].Get("cache_control").Exists() {
		t.Fatal("volatile half carries a breakpoint, so the hash still covers the churn")
	}
}

// LOSSLESSNESS is the safety story: adjacent text blocks concatenate, so the model
// must see a byte-identical prompt. Asserted on the concatenation, not per block.
func TestSplitIsConcatenationIdentical(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	in := sysBody(textBlock("preamble", false), textBlock(full, true))
	got, split := splitTail(in, bschemas.Anthropic)
	if !split {
		t.Fatal("did not split")
	}
	join := func(b []byte) string {
		var sb strings.Builder
		for _, x := range gjson.GetBytes(b, "system").Array() {
			sb.WriteString(x.Get("text").String())
		}
		return sb.String()
	}
	if join(in) != join(got) {
		t.Fatal("model-visible text changed — the split is not lossless")
	}
}

// Only meaningful where breakpoints are explicit. Implicit caches already stop at the
// divergence, so splitting adds a block for no gain.
func TestSplitOnlyOnExplicitBreakpointProviders(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	in := sysBody(textBlock(full, true))
	for _, p := range []bschemas.ModelProvider{
		bschemas.OpenAI, bschemas.Azure, bschemas.Gemini,
		bschemas.ModelProvider("local-llama"),
	} {
		if _, split := splitTail(in, p); split {
			t.Fatalf("%s: split fired where breakpoints are not explicit", p)
		}
	}
	for _, p := range []bschemas.ModelProvider{
		bschemas.Anthropic, bschemas.Bedrock, bschemas.Vertex,
	} {
		if _, split := splitTail(in, p); !split {
			t.Fatalf("%s: split did not fire", p)
		}
	}
}

// A block with no volatile marker, or too small to be worth a breakpoint slot, must
// be left exactly as it is.
func TestInertWithoutAVolatileTail(t *testing.T) {
	big := strings.Repeat("stable instruction line.\n", 3000)
	if _, split := splitTail(sysBody(textBlock(big, true)), bschemas.Anthropic); split {
		t.Fatal("split a block with no volatile marker")
	}
	// Below minSplitTokens on the stable half: the marker is present but the stable
	// part is tiny, so there is nothing worth caching separately.
	small := "Current branch: main\nRecent commits:\nabc fix\n"
	if _, split := splitTail(sysBody(textBlock(small, true)), bschemas.Anthropic); split {
		t.Fatal("split a block whose stable half is below the minimum")
	}
}

// Degenerate bodies must pass through rather than be half-rewritten.
func TestInertOnDegenerateBodies(t *testing.T) {
	for _, b := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"system":"a plain string system prompt","messages":[]}`),
		[]byte(`{"system":[],"messages":[]}`),
		[]byte(`{"messages":[]}`),
	} {
		got, split := splitTail(b, bschemas.Anthropic)
		if split || string(got) != string(b) {
			t.Fatalf("mutated a degenerate body: %s", b)
		}
	}
}

// Splits at most ONE block: each split consumes a breakpoint slot and the provider
// caps them at four.
func TestSplitsAtMostOneBlock(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	got, split := splitTail(
		sysBody(textBlock(full, true), textBlock(full, true)), bschemas.Anthropic)
	if !split {
		t.Fatal("did not split")
	}
	if n := len(gjson.GetBytes(got, "system").Array()); n != 3 {
		t.Fatalf("want 3 blocks (one split, one untouched), got %d", n)
	}
}

// --------------------------------------------------------------------------- //
// wiring through the real entry point
// --------------------------------------------------------------------------- //

func pipeWith(t *testing.T, yaml string) *components.Pipeline {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runBody(t *testing.T, pipe *components.Pipeline, body []byte, bypass bool) ([]byte, bool) {
	t.Helper()
	return BodyFull(context.Background(), pipe, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "sess-1", bypass, components.ModelSpec{}, 0, "auto")
}

// The split must reach the wire through BodyFull AND be reported as changed, or the
// host forwards the original and the whole thing is a no-op in production.
func TestSplitAppliedThroughBodyFull(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	got, changed := runBody(t, pipeWith(t, "pipeline: [cacheinject]\n"),
		sysBody(textBlock(full, true)), false)
	if !changed {
		t.Fatal("body not reported as changed — the host would forward the original")
	}
	if n := len(gjson.GetBytes(got, "system").Array()); n != 2 {
		t.Fatalf("split did not reach the wire: %d blocks", n)
	}
}

// Gated: a pipeline with neither cachesplit nor cacheinject must leave the body
// byte-identical, so `off` stays a true passthrough control for A/B runs.
func TestSplitGatedOnConfig(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	in := sysBody(textBlock(full, true))
	got, changed := runBody(t, pipeWith(t, "pipeline: [format]\n"), in, false)
	if changed || string(got) != string(in) {
		t.Fatal("split fired with neither cachesplit nor cacheinject configured")
	}
}

// `cachesplit` alone enables the split. This is what keeps #32's preset change (dropping
// the unmeasured cacheinject) from silently disabling the measured split along with it.
func TestSplitEnabledByCachesplitAlone(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	in := sysBody(textBlock(full, true))
	got, changed := runBody(t, pipeWith(t, "pipeline: [cachesplit]\n"), in, false)
	if !changed {
		t.Fatal("cachesplit did not enable the volatile-tail split")
	}
	if n := gjson.GetBytes(got, "system.#").Int(); n != 2 {
		t.Fatalf("expected the system block to split into 2, got %d", n)
	}
	// cachesplit must add NO breakpoint of its own — it is not a placement policy.
	if before, after := wireBreakpoints(in), wireBreakpoints(got); after != before {
		t.Fatalf("cachesplit changed the breakpoint count %d -> %d", before, after)
	}
}

// Bypass must be absolute.
func TestBypassSkipsSplit(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	in := sysBody(textBlock(full, true))
	got, changed := runBody(t, pipeWith(t, "pipeline: [cacheinject]\n"), in, true)
	if changed || string(got) != string(in) {
		t.Fatal("split ran under bypass")
	}
}

// The split rewrites `body` BEFORE `msgsRaw` is used to rebuild the messages array.
// If anything downstream depended on byte offsets into the original body rather than
// on JSON paths, splitting first would corrupt the messages. This asserts the two are
// independent: messages must survive a split byte-for-byte.
func TestSplitDoesNotDisturbMessages(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	in, _ := json.Marshal(map[string]any{
		"model":  "claude-sonnet-5",
		"system": []map[string]any{textBlock(full, true)},
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "first turn with \"quotes\" and \\backslashes\\"}}},
			{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "reply ünïcode 🎯"}}},
			{"role": "user", "content": "a bare string turn"},
		},
	})
	got, changed := runBody(t, pipeWith(t, "pipeline: [cacheinject]\n"), in, false)
	if !changed {
		t.Fatal("expected the split to report a change")
	}
	// cacheinject legitimately ADDS a cache_control breakpoint to the last markable
	// message (rule 1), so the raw JSON is expected to differ. What must NOT change is
	// the model-visible TEXT of every message — that is the losslessness guarantee,
	// and it is what would break if anything downstream relied on byte offsets into
	// the pre-split body.
	texts := func(b []byte) []string {
		var out []string
		for _, m := range gjson.GetBytes(b, "messages").Array() {
			c := m.Get("content")
			if c.IsArray() {
				for _, blk := range c.Array() {
					out = append(out, m.Get("role").String()+":"+blk.Get("text").String())
				}
				continue
			}
			out = append(out, m.Get("role").String()+":"+c.String())
		}
		return out
	}
	before, after := texts(in), texts(got)
	if strings.Join(before, "\x00") != strings.Join(after, "\x00") {
		t.Fatalf("message TEXT changed across the split:\n before: %q\n after:  %q",
			before, after)
	}
	if n := len(gjson.GetBytes(got, "system").Array()); n != 2 {
		t.Fatalf("split did not happen: %d system blocks", n)
	}
}

// A non-text system block (an image, or any future block type) must pass through
// untouched. The split reconstructs blocks as {"type":"text",...} from their `text`
// field, so a block whose payload lives elsewhere would be silently destroyed if it
// were ever fed through that path.
func TestNonTextBlocksSurvive(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	img := map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}
	in := sysBody(img, textBlock(full, true))
	got, split := splitTail(in, bschemas.Anthropic)
	if !split {
		t.Fatal("did not split the text block")
	}
	blocks := gjson.GetBytes(got, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("want 3 blocks (image + split pair), got %d", len(blocks))
	}
	if blocks[0].Get("type").String() != "image" {
		t.Fatalf("image block was rewritten: %s", blocks[0].Raw)
	}
	if blocks[0].Get("source.data").String() != "iVBORw0KGgo=" {
		t.Fatalf("image payload lost: %s", blocks[0].Raw)
	}
}

// A text block carrying fields the split does not know about (citations, a future
// key) is reconstructed from `type`+`text`+`cache_control` only, so those fields would
// be dropped. Guard against silently losing them: if a splittable block has unknown
// keys, leave it alone rather than rebuild it lossily.
func TestUnknownFieldsNotDropped(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	blk := textBlock(full, true)
	blk["citations"] = []map[string]any{{"type": "char_location", "start_char_index": 3}}
	in := sysBody(blk)
	got, split := splitTail(in, bschemas.Anthropic)
	if !split {
		// Acceptable: declining to split preserves the field.
		if gjson.GetBytes(got, "system.0.citations").Exists() {
			return
		}
		t.Fatal("declined to split but still lost citations")
	}
	// If it DID split, the unknown field must have survived somewhere.
	if !gjson.GetBytes(got, "system.0.citations").Exists() {
		t.Fatalf("split dropped an unknown block field (citations); blocks: %s",
			gjson.GetBytes(got, "system").Raw[:200])
	}
}

// A Bedrock/Vertex system block can carry `cachePoint` where Anthropic writes
// `cache_control`. The split COPIES the block, so deleting only one spelling leaves the
// other on BOTH halves and turns one breakpoint into two. wireBreakpoints counts both
// spellings, so that silently eats a slot from the provider's cap of four — and from a
// request already at the cap it pushes the wire to five and takes a 400.
func TestSplitDoesNotDuplicateBedrockCachePoint(t *testing.T) {
	full, _, _ := blockWithGitTail(6000)
	block := map[string]any{"type": "text", "text": full, "cachePoint": map[string]any{"type": "default"}}
	in := sysBody(block)

	before := wireBreakpoints(in)
	got, split := splitTail(in, bschemas.Bedrock)
	if !split {
		t.Fatal("expected the git-tail block to split")
	}
	if after := wireBreakpoints(got); after != before {
		t.Fatalf("split changed the wire breakpoint count %d -> %d; a copied cachePoint "+
			"burns a slot from the provider's cap of four", before, after)
	}
	// and the churning half must not carry a breakpoint at all
	for _, b := range gjson.GetBytes(got, "system").Array() {
		if strings.Contains(b.Get("text").String(), "Recent commits:") &&
			(b.Get("cachePoint").Exists() || b.Get("cache_control").Exists()) {
			t.Fatal("the volatile half kept a breakpoint — the churn is back inside a hashed prefix")
		}
	}
}
