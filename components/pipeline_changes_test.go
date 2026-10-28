package components

import (
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func twoMsgReq(second string) *schemas.BifrostChatRequest {
	r := reqWith("hi")
	s := second
	r.Input = append(r.Input, schemas.ChatMessage{
		Role: schemas.ChatMessageRoleTool, Content: &schemas.ChatMessageContent{ContentStr: &s}})
	return r
}

// dropFirst is a Reformat that changes the message count.
type dropFirst struct{}

func (dropFirst) Name() string      { return "dropfirst" }
func (dropFirst) Enabled(*Ctx) bool { return true }
func (dropFirst) Reformat(req *schemas.BifrostChatRequest, _ *Report, _ *Ctx) error {
	req.Input = req.Input[1:]
	return nil
}

func TestRunOne_RecordsByteChangesAndStableHash(t *testing.T) {
	big := strings.Repeat("word      ", 400)
	run := func() Report {
		rr := NewPipeline([]Component{shrink{}}, nil).Run(twoMsgReq(big), testCtx())
		return rr.Components[0]
	}
	a, b := run(), run()
	if len(a.Changes) != 1 {
		t.Fatalf("Changes = %+v, want one", a.Changes)
	}
	c := a.Changes[0]
	if c.Idx != 1 || c.BytesBefore != len(big) || c.BytesAfter <= 0 || c.BytesBefore <= c.BytesAfter {
		t.Fatalf("change = %+v", c)
	}
	if len(c.Hash) != 12 || c.Hash != b.Changes[0].Hash {
		t.Fatalf("hash %q vs %q: must be 12 chars and equal for equal input", c.Hash, b.Changes[0].Hash)
	}
	if a.CountChanged {
		t.Fatal("CountChanged must be false when the count is unchanged")
	}
}

func TestRunOne_CountChangeRecordsOneSplice(t *testing.T) {
	rep := NewPipeline([]Component{dropFirst{}}, nil).Run(twoMsgReq("x y z"), testCtx()).Components[0]
	if rep.Reverted {
		t.Fatal("reverted by guard")
	}
	if !rep.CountChanged || len(rep.Changes) != 1 || rep.Changes[0].Splice == nil {
		t.Fatalf("CountChanged=%v Changes=%+v", rep.CountChanged, rep.Changes)
	}
	if s := *rep.Changes[0].Splice; s != (Splice{At: 0, Removed: 1, Inserted: 0}) {
		t.Fatalf("splice = %+v", s)
	}
}

func msgOf(role schemas.ChatMessageRole, text string) schemas.ChatMessage {
	t := text
	return schemas.ChatMessage{Role: role, Content: &schemas.ChatMessageContent{ContentStr: &t}}
}

// rewriter is a Reformat that replaces req.Input with a fixed function of it.
type rewriter struct {
	f func([]schemas.ChatMessage) []schemas.ChatMessage
}

func (rewriter) Name() string      { return "rewriter" }
func (rewriter) Enabled(*Ctx) bool { return true }
func (r rewriter) Reformat(req *schemas.BifrostChatRequest, _ *Report, _ *Ctx) error {
	req.Input = r.f(req.Input)
	return nil
}

func spliceReq() *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		msgOf(schemas.ChatMessageRoleUser, "a"), msgOf(schemas.ChatMessageRoleTool, "bbbb"),
		msgOf(schemas.ChatMessageRoleTool, "cccc"), msgOf(schemas.ChatMessageRoleUser, "dddddddddd"),
	}}
}

func runSplice(t *testing.T, f func([]schemas.ChatMessage) []schemas.ChatMessage) Report {
	t.Helper()
	rep := NewPipeline([]Component{rewriter{f}}, nil).Run(spliceReq(), testCtx()).Components[0]
	if rep.Reverted || !rep.CountChanged || len(rep.Changes) != 1 || rep.Changes[0].Splice == nil {
		t.Fatalf("report = %+v", rep)
	}
	return rep
}

func TestRunOne_SpliceInTheMiddle(t *testing.T) {
	rep := runSplice(t, func(in []schemas.ChatMessage) []schemas.ChatMessage {
		return []schemas.ChatMessage{in[0], msgOf(schemas.ChatMessageRoleUser, "s"), in[3]}
	})
	c := rep.Changes[0]
	if *c.Splice != (Splice{At: 1, Removed: 2, Inserted: 1}) || c.Idx != 1 {
		t.Fatalf("splice = %+v idx %d", c.Splice, c.Idx)
	}
	if c.BytesBefore != 8 || c.BytesAfter != 1 || len(c.Hash) != 12 {
		t.Fatalf("change = %+v", c)
	}
}

func TestRunOne_SplicePureRemoval(t *testing.T) {
	rep := runSplice(t, func(in []schemas.ChatMessage) []schemas.ChatMessage {
		return []schemas.ChatMessage{in[0], in[3]}
	})
	c := rep.Changes[0]
	if *c.Splice != (Splice{At: 1, Removed: 2, Inserted: 0}) || c.BytesBefore != 8 || c.BytesAfter != 0 {
		t.Fatalf("change = %+v splice %+v", c, c.Splice)
	}
}

// A pure insertion cannot come through Run (the never-worse guard reverts a run that grows the
// request), so spliceOf is exercised directly.
func TestSpliceOf_PureInsertion(t *testing.T) {
	in := spliceReq().Input
	out := append([]schemas.ChatMessage{}, in[:2]...)
	out = append(out, msgOf(schemas.ChatMessageRoleUser, "new"))
	out = append(out, in[2:]...)
	c, ok := spliceOf(in, out)
	if !ok || *c.Splice != (Splice{At: 2, Removed: 0, Inserted: 1}) || c.BytesBefore != 0 || c.BytesAfter != 3 {
		t.Fatalf("change = %+v ok %v", c, ok)
	}
	if c.Hash == "e3b0c44298fc" {
		t.Fatal("hash of nothing: the ChangeID would be constant")
	}
}

func TestRunOne_RevertedRunRecordsNoChanges(t *testing.T) {
	rep := NewPipeline([]Component{grow{}}, nil).Run(reqWith("small"), testCtx()).Components[0]
	if !rep.Reverted || rep.Changes != nil || rep.CountChanged {
		t.Fatalf("reverted report = %+v", rep)
	}
}
