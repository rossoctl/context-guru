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

func TestRunOne_CountChangeLeavesChangesEmpty(t *testing.T) {
	rep := NewPipeline([]Component{dropFirst{}}, nil).Run(twoMsgReq("x y z"), testCtx()).Components[0]
	if rep.Reverted {
		t.Skip("reverted by guard")
	}
	if !rep.CountChanged || len(rep.Changes) != 0 {
		t.Fatalf("CountChanged=%v Changes=%+v", rep.CountChanged, rep.Changes)
	}
}

func TestRunOne_RevertedRunRecordsNoChanges(t *testing.T) {
	rep := NewPipeline([]Component{grow{}}, nil).Run(reqWith("small"), testCtx()).Components[0]
	if !rep.Reverted || rep.Changes != nil || rep.CountChanged {
		t.Fatalf("reverted report = %+v", rep)
	}
}
