package components

import (
	"context"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/store"
)

// --- fakes ---

type shrink struct{}             // Reformat: collapses runs of spaces (lossless-ish, always smaller)
func (shrink) Name() string      { return "shrink" }
func (shrink) Enabled(*Ctx) bool { return true }
func (shrink) Reformat(req *schemas.BifrostChatRequest, _ *Report, _ *Ctx) error {
	for i := range req.Input {
		t := msgText(req.Input[i])
		setText(&req.Input[i], strings.Join(strings.Fields(t), " "))
	}
	return nil
}

type dropStash struct{}             // Offload: replaces content with a marker, stashes original
func (dropStash) Name() string      { return "dropstash" }
func (dropStash) Enabled(*Ctx) bool { return true }
func (dropStash) Offload(req *schemas.BifrostChatRequest, _ *Report, c *Ctx) ([]string, error) {
	key := "k1"
	c.Store.Put(key, []byte(msgText(req.Input[0])))
	setText(&req.Input[0], "<<cg:"+key+">>")
	return []string{key}, nil
}

type boom struct{}                                                     // panics — must be caught, request reverted
func (boom) Name() string                                              { return "boom" }
func (boom) Enabled(*Ctx) bool                                         { return true }
func (boom) Reformat(*schemas.BifrostChatRequest, *Report, *Ctx) error { panic("kaboom") }

type grow struct{}             // grows the request — never-worse guard must revert
func (grow) Name() string      { return "grow" }
func (grow) Enabled(*Ctx) bool { return true }
func (grow) Reformat(req *schemas.BifrostChatRequest, _ *Report, _ *Ctx) error {
	setText(&req.Input[0], strings.Repeat("padding ", 200))
	return nil
}

type badOffload struct{}             // drops but returns empty key — contract violation, revert
func (badOffload) Name() string      { return "badoffload" }
func (badOffload) Enabled(*Ctx) bool { return true }
func (badOffload) Offload(req *schemas.BifrostChatRequest, _ *Report, _ *Ctx) ([]string, error) {
	setText(&req.Input[0], "x")
	return nil, nil
}

// --- helpers ---

func msgText(m schemas.ChatMessage) string {
	if m.Content != nil && m.Content.ContentStr != nil {
		return *m.Content.ContentStr
	}
	return ""
}
func setText(m *schemas.ChatMessage, s string) {
	m.Content = &schemas.ChatMessageContent{ContentStr: &s}
}
func reqWith(text string) *schemas.BifrostChatRequest {
	t := text
	return &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &t}},
	}}
}
func testCtx() *Ctx {
	return &Ctx{Ctx: context.Background(), Session: "s", Store: store.NewMemory(store.Options{})}
}

// --- tests ---

func TestReformatShrinks(t *testing.T) {
	req := reqWith("hello      world   foo")
	rr := NewPipeline([]Component{shrink{}}, nil).Run(req, testCtx())
	if got := msgText(req.Input[0]); got != "hello world foo" {
		t.Fatalf("shrink not applied: %q", got)
	}
	if rr.Saved() < 0 || rr.TokensAfter > rr.TokensBefore {
		t.Fatalf("expected savings, got before=%d after=%d", rr.TokensBefore, rr.TokensAfter)
	}
}

func TestOffloadStashesAndReports(t *testing.T) {
	req := reqWith(strings.Repeat("some large tool output line\n", 50))
	c := testCtx()
	rr := NewPipeline([]Component{dropStash{}}, nil).Run(req, c)
	if len(rr.Components) != 1 || len(rr.Components[0].CacheKeys) != 1 || rr.Components[0].CacheKeys[0] != "k1" {
		t.Fatalf("expected offload report with cache_key k1, got %+v", rr.Components)
	}
	if _, ok := c.Store.Get("k1"); !ok {
		t.Fatal("original not stashed in store")
	}
	if !strings.Contains(msgText(req.Input[0]), "<<cg:k1>>") {
		t.Fatalf("marker not written: %q", msgText(req.Input[0]))
	}
}

func TestFailOpenOnPanic(t *testing.T) {
	req := reqWith("keep me intact")
	rr := NewPipeline([]Component{boom{}}, nil).Run(req, testCtx())
	if msgText(req.Input[0]) != "keep me intact" {
		t.Fatalf("panic must revert; got %q", msgText(req.Input[0]))
	}
	if !rr.Components[0].Reverted || rr.Components[0].Err == nil {
		t.Fatalf("expected reverted+err report, got %+v", rr.Components[0])
	}
}

func TestNeverWorseReverts(t *testing.T) {
	req := reqWith("small")
	rr := NewPipeline([]Component{grow{}}, nil).Run(req, testCtx())
	if msgText(req.Input[0]) != "small" {
		t.Fatalf("never-worse must revert a growing component; got len %d", len(msgText(req.Input[0])))
	}
	if !rr.Components[0].Reverted {
		t.Fatal("expected reverted report")
	}
}

func TestOffloadEmptyKeyReverts(t *testing.T) {
	req := reqWith("original content here")
	rr := NewPipeline([]Component{badOffload{}}, nil).Run(req, testCtx())
	if msgText(req.Input[0]) != "original content here" {
		t.Fatalf("empty-key offload must revert; got %q", msgText(req.Input[0]))
	}
	if !rr.Components[0].Reverted {
		t.Fatal("expected reverted report for empty cache_key")
	}
}

func TestBypassSkipsEverything(t *testing.T) {
	req := reqWith("hello      world")
	c := testCtx()
	c.Bypass = true
	rr := NewPipeline([]Component{shrink{}}, nil).Run(req, c)
	if msgText(req.Input[0]) != "hello      world" || len(rr.Components) != 0 {
		t.Fatal("bypass must skip the pipeline")
	}
}

// growAndInflate is the shape that DEFEATED the never-worse guard: it grows the request
// and also writes to rep.TokensBefore, the field the guard compares against. mask and
// failed_run both did the second half for real (`rep.TokensBefore += saved`, with a
// comment claiming the pipeline recomputes it — it does not; runOne sets TokensBefore
// BEFORE the component runs and afterwards recomputes only `after`). A component that
// inflates the baseline by more than it grows the request therefore passed the guard and
// reached the wire, which voids the product's central safety claim.
type growAndInflate struct{}

func (growAndInflate) Name() string      { return "growinflate" }
func (growAndInflate) Enabled(*Ctx) bool { return true }
func (growAndInflate) Reformat(req *schemas.BifrostChatRequest, rep *Report, _ *Ctx) error {
	rep.TokensBefore += 10_000 // move the goalpost
	setText(&req.Input[0], strings.Repeat("padding ", 200))
	return nil
}

func TestNeverWorseGuardIgnoresAComponentInflatingItsOwnBaseline(t *testing.T) {
	req := reqWith("small")
	orig := msgText(req.Input[0])
	p := NewPipeline([]Component{growAndInflate{}}, nil)
	rr := p.Run(req, &Ctx{Ctx: context.Background(), Store: store.Nop{}})

	if got := msgText(req.Input[0]); got != orig {
		t.Errorf("never-worse guard was bypassed: request grew from %q to %d bytes", orig, len(got))
	}
	if len(rr.Components) != 1 || !rr.Components[0].Reverted {
		t.Errorf("component that grew the request was not reported as reverted: %+v", rr.Components)
	}
	if rr.TokensAfter > rr.TokensBefore {
		t.Errorf("run grew the request: %d -> %d", rr.TokensBefore, rr.TokensAfter)
	}
}

// The run keeps ONE snapshot and re-syncs it as components land (see runOne/resync)
// instead of deep-cloning the whole transcript per component. These two tests are what
// make that safe: the snapshot must never end up ALIASING req.Input, and it must track
// changes that survived.

// TestRevertedComponentDoesNotLeakIntoTheNextRevert: after a revert, req.Input holds the
// copy the pipeline was carrying, so the next component's in-place write would corrupt
// the snapshot and its revert would keep the change (the request would GROW).
func TestRevertedComponentDoesNotLeakIntoTheNextRevert(t *testing.T) {
	req := reqWith("small")
	rr := NewPipeline([]Component{grow{}, grow{}, boom{}}, nil).Run(req, testCtx())
	if got := msgText(req.Input[0]); got != "small" {
		t.Fatalf("revert after revert must restore the original; got %d bytes", len(got))
	}
	for i := range rr.Components {
		if !rr.Components[i].Reverted {
			t.Errorf("component %d not reported reverted: %+v", i, rr.Components[i])
		}
	}
}

// TestRevertAfterASurvivingChangeKeepsThatChange: the snapshot must track what survived,
// or a later revert rolls the request back past a component that succeeded.
func TestRevertAfterASurvivingChangeKeepsThatChange(t *testing.T) {
	req := reqWith("hello      world   foo")
	NewPipeline([]Component{shrink{}, grow{}}, nil).Run(req, testCtx())
	if got := msgText(req.Input[0]); got != "hello world foo" {
		t.Fatalf("revert discarded the earlier surviving change: %q", got)
	}
}

// TestReformatGetsContentDerivedCacheKeys: a Reformat stashes nothing, so before this it
// took metrics' keyless fallback (SavedUnique += saved, unconditionally) and its
// overcount_ratio was 1.0 by construction — reported as "every token it removes is new
// money" for the four folds. The pipeline now derives one key per rewritten message from
// its PRE-fold text, so the same tool output re-sent on a later turn maps to the same key
// and dedups exactly the way an Offload's stash keys do.
func TestReformatGetsContentDerivedCacheKeys(t *testing.T) {
	const text = "hello      world   foo"
	var first []string
	for turn := 0; turn < 3; turn++ {
		req := reqWith(text) // the agent re-sends the ORIGINAL every turn
		rr := NewPipeline([]Component{shrink{}}, nil).Run(req, testCtx())
		keys := rr.Components[0].CacheKeys
		if len(keys) != 1 {
			t.Fatalf("turn %d: want 1 dedup key for 1 rewritten message, got %v", turn, keys)
		}
		if turn == 0 {
			first = keys
			continue
		}
		if keys[0] != first[0] {
			t.Fatalf("turn %d key %q != turn 0 key %q — a re-fold of the same content must "+
				"dedup, or SavedUnique re-counts it per turn", turn, keys[0], first[0])
		}
	}
}

// A Reformat that declines must claim no keys: an empty run credits nothing, and a key
// here would poison the seen-set for content it never touched.
func TestDecliningReformatClaimsNoCacheKeys(t *testing.T) {
	req := reqWith("nothing to shrink")
	rr := NewPipeline([]Component{shrink{}}, nil).Run(req, testCtx())
	if got := rr.Components[0].CacheKeys; len(got) != 0 {
		t.Fatalf("unchanged reformat claimed keys %v", got)
	}
}

// Two different messages folded in one run get two keys, so metrics' proportional
// attribution (saved * newKeys / len(keys)) has the right denominator.
func TestReformatKeysAreOnePerRewrittenMessage(t *testing.T) {
	a, b := "aaa      aaa", "bbb      bbb"
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &a}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &b}},
	}}
	rr := NewPipeline([]Component{shrink{}}, nil).Run(req, testCtx())
	keys := rr.Components[0].CacheKeys
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("want 2 distinct keys for 2 folded messages, got %v", keys)
	}
}
