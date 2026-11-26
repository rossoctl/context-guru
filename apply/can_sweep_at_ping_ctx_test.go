package apply_test

import (
	"context"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// ctxProbe records the two keep-alive hooks a component sees.
type ctxProbe struct{ sweep, old func() bool }

var probeSeen ctxProbe

func (ctxProbe) Name() string                 { return "ctxprobe1459" }
func (ctxProbe) Enabled(*components.Ctx) bool { return true }
func (ctxProbe) Reformat(_ *bschemas.BifrostChatRequest, _ *components.Report, c *components.Ctx) error {
	probeSeen = ctxProbe{c.CanSweepAtPing, c.CanKeepAlive}
	return nil
}

func init() {
	components.Register("ctxprobe1459", func([]byte) (components.Component, error) { return ctxProbe{}, nil })
}

// #1459: a component that reads the deprecated Ctx.CanKeepAlive gets the same answer as CanSweepAtPing,
// for both the old and the new Opts field, on the chat path.
func TestCtxHooksGiveTheSameAnswer(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[{"role":"user","content":"go"}]}`)
	for _, tc := range []struct {
		name string
		opts apply.Opts
		want bool
	}{
		{"new Opts field, true", apply.Opts{CanSweepAtPing: func(_, _ string) bool { return true }}, true},
		{"new Opts field, false", apply.Opts{CanSweepAtPing: func(_, _ string) bool { return false }}, false},
		{"old Opts field, true", apply.Opts{CanKeepAlive: func(_, _ string) bool { return true }}, true},
		{"no hook", apply.Opts{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probeSeen = ctxProbe{}
			p, _ := pipe(t, "pipeline: [ctxprobe1459]\n").Build(nil)
			o := tc.opts
			o.Provider, o.Body, o.Session = bschemas.Anthropic, body, "s1"
			apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), o)
			get := func(f func() bool) bool { return f != nil && f() }
			if (probeSeen.sweep == nil) != (probeSeen.old == nil) {
				t.Fatalf("one hook is nil and the other is not")
			}
			if a, b := get(probeSeen.sweep), get(probeSeen.old); a != tc.want || b != tc.want {
				t.Errorf("CanSweepAtPing=%v CanKeepAlive=%v, want %v", a, b, tc.want)
			}
		})
	}
}
