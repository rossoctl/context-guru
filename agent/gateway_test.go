package agent

import (
	"slices"
	"testing"
)

// gateway is a test Gateway with the name, paths and spec it is given.
type gateway struct {
	routed
	spec GatewaySpec
}

func (g gateway) Gateway() GatewaySpec { return g.spec }

var _ Gateway = gateway{}

func names(as []Agent) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name())
	}
	return out
}

// An agent with two paths is in the map twice, but All gives it once.
func TestAllGivesEachAgentOnceSortedByName(t *testing.T) {
	reg := NewRegistry()
	for _, a := range []Agent{
		routed{name: "c", paths: []string{"/c"}},
		routed{name: "a", paths: []string{"/a/1", "/a/2"}},
		routed{name: "b"},
	} {
		if err := reg.Register(a); err != nil {
			t.Fatalf("Register(%s): %v", a.Name(), err)
		}
	}

	got := names(reg.All())

	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("All() names = %q, want %q", got, want)
	}
}

func TestAllOmitsARefusedAgent(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(routed{name: "a", paths: []string{"/x"}}); err != nil {
		t.Fatal(err)
	}
	_ = reg.Register(routed{name: "b", paths: []string{"/x"}})

	if got, want := names(reg.All()), []string{"a"}; !slices.Equal(got, want) {
		t.Fatalf("All() names = %q, want %q", got, want)
	}
}

func TestAllOnEmptyRegistryIsEmpty(t *testing.T) {
	if got := NewRegistry().All(); len(got) != 0 {
		t.Fatalf("All() = %#v, want none", got)
	}
}

// The caller may change the slice All gives; the registry keeps its own.
func TestAllGivesACopy(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(routed{name: "a"}); err != nil {
		t.Fatal(err)
	}
	all := reg.All()
	if len(all) != 1 {
		t.Fatalf("All() = %q, want [a]", names(all))
	}
	all[0] = routed{name: "z"}

	if got := names(reg.All()); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("All() names = %q after the caller changed its copy, want [a]", got)
	}
}

func TestRegisterRefusesASecondCatchAllGateway(t *testing.T) {
	reg := NewRegistry()
	first := gateway{routed: routed{name: "a", paths: []string{"/a"}}, spec: GatewaySpec{Dialect: "openai", CatchAll: true}}
	if err := reg.Register(first); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	err := reg.Register(gateway{routed: routed{name: "b", paths: []string{"/b"}}, spec: GatewaySpec{Dialect: "openai", CatchAll: true}})

	if err == nil {
		t.Fatal("second CatchAll Register: got nil error, want a refusal")
	}
	if got := reg.ForPath("/b"); got != (Base{}) {
		t.Errorf("ForPath(/b) = %#v, want Base{} (refused agent must claim nothing)", got)
	}
	if got := names(reg.All()); !slices.Equal(got, []string{"a"}) {
		t.Errorf("All() names = %q, want [a]", got)
	}
}

func TestRegisterAcceptsACatchAllAndAGatewayWithout(t *testing.T) {
	reg := NewRegistry()
	for _, g := range []gateway{
		{routed: routed{name: "a", paths: []string{"/a"}}, spec: GatewaySpec{Dialect: "openai", CatchAll: true}},
		{routed: routed{name: "b", paths: []string{"/b"}}, spec: GatewaySpec{Dialect: "openai"}},
	} {
		if err := reg.Register(g); err != nil {
			t.Fatalf("Register(%s): %v", g.name, err)
		}
	}
}

func TestRegisterRefusesAGatewayWithAnEmptyName(t *testing.T) {
	reg := NewRegistry()

	err := reg.Register(gateway{routed: routed{paths: []string{"/a"}}, spec: GatewaySpec{Dialect: "openai"}})

	if err == nil {
		t.Fatal("Register of a Gateway with an empty Name: got nil error, want a refusal")
	}
	if got := reg.ForPath("/a"); got != (Base{}) {
		t.Errorf("ForPath(/a) = %#v, want Base{}", got)
	}
}

// The host keys a Gateway's upstream by its Name, so two Gateways with one Name would share it.
func TestRegisterRefusesASecondGatewayWithTheSameName(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(gateway{routed: routed{name: "x", paths: []string{"/a"}}, spec: GatewaySpec{Dialect: "openai"}}); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	err := reg.Register(gateway{routed: routed{name: "x", paths: []string{"/b"}}, spec: GatewaySpec{Dialect: "openai"}})

	if err == nil {
		t.Fatal("second Gateway named x: got nil error, want a refusal")
	}
	if got := reg.ForPath("/b"); got != (Base{}) {
		t.Errorf("ForPath(/b) = %#v, want Base{}", got)
	}
}

// An agent that is not a Gateway may still have an empty Name: the shared code picks its label.
func TestRegisterAcceptsANonGatewayWithAnEmptyName(t *testing.T) {
	if err := NewRegistry().Register(routed{paths: []string{"/a"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestPackageRegisterPanicsOnASecondCatchAll(t *testing.T) {
	t.Cleanup(resetDefault)
	Register(gateway{routed: routed{name: "a"}, spec: GatewaySpec{CatchAll: true}})
	if got := names(All()); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("All() names = %q, want [a]", got)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("second CatchAll Register did not panic")
		}
	}()
	Register(gateway{routed: routed{name: "b"}, spec: GatewaySpec{CatchAll: true}})
}

// resetDefault clears All and the catch-all too, so a test that registers one leaves none behind.
func TestResetDefaultClearsAllAndTheCatchAll(t *testing.T) {
	t.Cleanup(resetDefault)
	Register(gateway{routed: routed{name: "a"}, spec: GatewaySpec{CatchAll: true}})

	resetDefault()

	if got := All(); len(got) != 0 {
		t.Fatalf("All() after resetDefault = %q, want none", names(got))
	}
	Register(gateway{routed: routed{name: "b"}, spec: GatewaySpec{CatchAll: true}})
}
