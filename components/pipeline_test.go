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
