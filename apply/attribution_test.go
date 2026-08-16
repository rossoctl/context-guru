package apply_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// toolShrinker is a test-only component that shrinks every tool message's text. Two of
// them stacked is the case exact attribution has to get right: the diff shows the
// cumulative rewrite, so naming one author would be wrong.
type toolShrinker struct {
	name string
	to   string
	fail bool
}

func (c toolShrinker) Name() string               { return c.name }
func (toolShrinker) Enabled(*components.Ctx) bool { return true }
func (c toolShrinker) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleTool {
			schema.SetMessageText(&req.Input[i], c.to)
		}
	}
	if c.fail {
		// Fail AFTER mutating, so the pipeline rolls the change back. A component that
		// was reverted must not be credited with the change that survived.
		return errors.New("deliberate failure so the pipeline reverts")
	}
	return nil
}

func init() {
	for _, c := range []toolShrinker{
		{name: "attr1", to: strings.Repeat("first pass output line\n", 4)},
		{name: "attr2", to: "second pass"},
		{name: "attrrevert", to: "never reaches the wire", fail: true},
	} {
		comp := c
		components.Register(comp.name, func([]byte) (components.Component, error) { return comp, nil })
	}
}

func toolBody() []byte {
	return []byte(`{"model":"gpt-x","messages":[
		{"role":"user","content":"run it"},
		{"role":"tool","tool_call_id":"a","content":"` +
		strings.Repeat("a long original tool output line that both components will rewrite\\n", 8) +
		`"}
	]}`)
}

func changesOf(t *testing.T, pipeline string) []apply.Change {
	t.Helper()
	cfg := pipe(t, "pipeline: ["+pipeline+"]\n")
	p, err := cfg.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.OpenAI, Body: toolBody(),
	})
	return res.Trace.Changes
}

// TestChangeAttributesEveryComponentInOrder: the diff view must be able to say WHICH
// components produced a change instead of inferring it from the after-text. Two
// components rewrite one message, so the answer is both of them, in the order they ran.
func TestChangeAttributesEveryComponentInOrder(t *testing.T) {
	changes := changesOf(t, "attr1, attr2")
	if len(changes) != 1 {
		t.Fatalf("expected one rewritten message, got %d: %+v", len(changes), changes)
	}
	if want := []string{"attr1", "attr2"}; !reflect.DeepEqual(changes[0].Components, want) {
		t.Fatalf("attribution = %v, want %v (both, in pipeline order)", changes[0].Components, want)
	}
}

// TestRevertedComponentNotAttributed: a component whose change the pipeline rolled back
// never reached the wire, so crediting it would be a lie in the direction that matters —
// it would show a user a diff attributed to a component that did nothing.
func TestRevertedComponentNotAttributed(t *testing.T) {
	changes := changesOf(t, "attr1, attrrevert")
	if len(changes) != 1 {
		t.Fatalf("expected one rewritten message, got %d: %+v", len(changes), changes)
	}
	if want := []string{"attr1"}; !reflect.DeepEqual(changes[0].Components, want) {
		t.Fatalf("attribution = %v, want %v — a reverted component must not be credited",
			changes[0].Components, want)
	}
	if strings.Contains(changes[0].After, "never reaches the wire") {
		t.Fatal("the reverted component's text reached the change record")
	}
}
