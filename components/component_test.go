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

// TestTheModelSourceFallbackIsReportable pins that a component can tell whether it got the
// source it asked for. `source: incoming` means "spend on the credential the caller is already
// paying for"; falling back to the static model spends a DIFFERENT credential on a DIFFERENT
// endpoint. That substitution was silent, and its invisibility cost a real investigation: an
// authentication failure could not be attributed to either credential.
func TestTheModelSourceFallbackIsReportable(t *testing.T) {
	inc, stat := stubNamed("incoming-client"), stubNamed("static-client")

	for _, tc := range []struct {
		name, source string
		spec         ModelSpec
		wantModel    Model
		wantUsed     string
	}{
		{"incoming when present", "incoming", ModelSpec{Incoming: inc, Static: stat}, inc, "incoming"},
		{"falls back and says so", "incoming", ModelSpec{Static: stat}, stat, "config"},
		{"config asked for, config used", "config", ModelSpec{Incoming: inc, Static: stat}, stat, "config"},
		{"unset behaves as incoming", "", ModelSpec{Incoming: inc, Static: stat}, inc, "incoming"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, used := tc.spec.ForSource(tc.source)
			if got != tc.wantModel {
				t.Fatalf("model = %v, want %v", got, tc.wantModel)
			}
			if used != tc.wantUsed {
				t.Fatalf("used = %q, want %q", used, tc.wantUsed)
			}
			// For must stay byte-identical in behaviour to before the split.
			if plain := tc.spec.For(tc.source); plain != tc.wantModel {
				t.Fatalf("For disagrees with ForSource: %v vs %v", plain, got)
			}
		})
	}
}

// TailOnlyCold must be a pure WIDENING of TailOnly, and only on the one input that earns
// it: opted in AND a provably cold cache. Anything else has to agree with TailOnly
// index-for-index — including the fail-open MaxCachedIdx < 0 case, which already returns
// true for every index (a compaction reset opens it on turns that then cache-HIT, so the
// lift must not be able to make that hole any wider than it already is).
func TestTailOnlyColdOnlyWidensWhenOptedInAndCold(t *testing.T) {
	for _, c := range []*Ctx{
		{CacheAware: true, MaxCachedIdx: 3},
		{CacheAware: true, MaxCachedIdx: 3, ColdCache: true},
		{CacheAware: true, MaxCachedIdx: -1, ColdCache: true},
		{CacheAware: false, MaxCachedIdx: 3, ColdCache: true},
		{CacheAware: true, MaxCachedIdx: 0},
	} {
		for i := 0; i < 6; i++ {
			base := c.TailOnly(i)
			if got := c.TailOnlyCold(i, false); got != base {
				t.Fatalf("optIn=false must equal TailOnly: idx %d cold=%v got %v want %v",
					i, c.ColdCache, got, base)
			}
			got := c.TailOnlyCold(i, true)
			if want := base || c.ColdCache; got != want {
				t.Fatalf("optIn=true: idx %d cold=%v got %v want %v", i, c.ColdCache, got, want)
			}
			if !base && got && !c.ColdCache {
				t.Fatalf("idx %d: lifted the gate on a WARM turn", i)
			}
		}
	}
}
