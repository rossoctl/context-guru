package components

import (
	"context"
	"testing"
)

type fakeModel struct{ id string }

func (m fakeModel) Complete(context.Context, string) (string, error) { return m.id, nil }

func idOf(m Model) string {
	if m == nil {
		return "nil"
	}
	return m.(fakeModel).id
}

func TestModelSpecFor(t *testing.T) {
	inc, stat := fakeModel{"incoming"}, fakeModel{"static"}
	cases := []struct {
		name   string
		spec   ModelSpec
		source string
		want   string
	}{
		{"config picks static", ModelSpec{Incoming: inc, Static: stat}, "config", "static"},
		{"incoming picks incoming", ModelSpec{Incoming: inc, Static: stat}, "incoming", "incoming"},
		{"default picks incoming", ModelSpec{Incoming: inc, Static: stat}, "", "incoming"},
		{"incoming falls back to static", ModelSpec{Static: stat}, "incoming", "static"},
		{"config with no static is nil", ModelSpec{Incoming: inc}, "config", "nil"},
		{"nothing available is nil", ModelSpec{}, "", "nil"},
	}
	for _, c := range cases {
		if got := idOf(c.spec.For(c.source)); got != c.want {
			t.Errorf("%s: For(%q)=%s want %s", c.name, c.source, got, c.want)
		}
	}
}

// A cheap compaction model on the caller's OWN endpoint and credential.
//
// The incoming client is the proxied request's model, which for a coding agent is a frontier
// model, and compaction on one cannot pay: a real cold-cache sweep through the hosted service
// cut the provider bill by $0.63 and spent $1.25 of opus doing it. The static source cannot
// supply a cheap model on a multi-tenant deployment (the operator does not spend its own
// credential on tenant traffic), so the model id has to be swappable on the incoming client.
func TestForModelSwapsTheModelButNotTheEndpoint(t *testing.T) {
	inc := remodelable{id: "opus"}
	spec := ModelSpec{Incoming: inc, Static: stubNamed("static")}

	if got := spec.ForModel("incoming", "haiku"); got.(remodelable).id != "haiku" {
		t.Errorf("ForModel did not swap the model: %v", got)
	}
	// No id: unchanged, so an unset field cannot quietly re-point anything.
	if got := spec.ForModel("incoming", ""); got.(remodelable).id != "opus" {
		t.Errorf("an empty id changed the model: %v", got)
	}
	// A client that cannot be re-pointed still runs, on its own model. Degrading to a working
	// component beats refusing to compact because one field could not be honoured.
	plain := ModelSpec{Incoming: stubNamed("fixed")}
	if got := plain.ForModel("incoming", "haiku"); got == nil {
		t.Error("a client that cannot be re-pointed was dropped instead of used as-is")
	}
	// And nothing available is still nothing, not a panic.
	if got := (ModelSpec{}).ForModel("incoming", "haiku"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

type remodelable struct{ id string }

func (r remodelable) Complete(context.Context, string) (string, error) { return "", nil }
func (r remodelable) AsModel(id string) Model                          { r.id = id; return r }

type stubNamed string

func (stubNamed) Complete(context.Context, string) (string, error) { return "", nil }
