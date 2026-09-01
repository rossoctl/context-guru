package components

import (
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

// HasOffload's whole selling point is that it cannot rot the way a list of component names would,
// so it is worth a test in its own package rather than only proxy-level integration coverage.
//
// It is a type assertion over the pipeline's components, and the property that matters is that it
// answers by INTERFACE: a component that drops bytes must satisfy components.Offload, and adding
// one must not require anybody to remember to update a second list somewhere. These fakes stand in
// for the three shapes a real component takes.

type offloadStub struct{ name string }

func (o offloadStub) Name() string      { return o.name }
func (o offloadStub) Enabled(*Ctx) bool { return true }
func (o offloadStub) Offload(*schemas.BifrostChatRequest, *Report, *Ctx) ([]string, error) {
	return []string{"key"}, nil
}

type reformatStub struct{ name string }

func (r reformatStub) Name() string                                              { return r.name }
func (r reformatStub) Enabled(*Ctx) bool                                         { return true }
func (r reformatStub) Reformat(*schemas.BifrostChatRequest, *Report, *Ctx) error { return nil }

func TestHasOffload(t *testing.T) {
	for _, c := range []struct {
		name  string
		comps []Component
		want  bool
	}{
		{"nil pipeline", nil, false},
		{"empty pipeline — the A/B control arm", []Component{}, false},
		{"reformatters only", []Component{reformatStub{"format"}, reformatStub{"cachesplit"}}, false},
		{"one offloader", []Component{offloadStub{"linecap"}}, true},
		{"an offloader among reformatters", []Component{
			reformatStub{"format"}, offloadStub{"mask"}, reformatStub{"cachesplit"}}, true},
		// Position must not matter: the host asks "can this pipeline ever mint a marker", which is
		// a property of the set, not of the order.
		{"offloader last", []Component{reformatStub{"format"}, offloadStub{"extract"}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p *Pipeline
			if c.comps != nil || c.name != "nil pipeline" {
				p = NewPipeline(c.comps, nil)
			}
			if got := p.HasOffload(); got != c.want {
				t.Errorf("HasOffload() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHasOffloadIsNilSafe: the host calls this on a per-request pipeline that a fail-open path may
// leave nil, and a panic there would take down a request rather than degrade it.
func TestHasOffloadIsNilSafe(t *testing.T) {
	var p *Pipeline
	if p.HasOffload() {
		t.Error("a nil pipeline reported an offloader")
	}
}
