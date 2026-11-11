package agent

import (
	"fmt"
	"sync"
	"testing"
)

// routed is a test agent that claims the paths it is given.
type routed struct {
	Base
	name  string
	paths []string
}

func (r routed) Name() string    { return r.name }
func (r routed) Paths() []string { return r.paths }

func TestForPathReturnsTheAgentThatClaimsThePath(t *testing.T) {
	reg := NewRegistry()
	a := routed{name: "a", paths: []string{"/a/v1/chat", "/a/v2/chat"}}
	if err := reg.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for _, p := range a.paths {
		if got := reg.ForPath(p).Name(); got != "a" {
			t.Errorf("ForPath(%q).Name() = %q, want %q", p, got, "a")
		}
	}
}

func TestForPathReturnsNeutralAgentForUnclaimedPath(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(routed{name: "a", paths: []string{"/a"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := reg.ForPath("/v1/messages"); got != (Base{}) {
		t.Fatalf("ForPath(unclaimed) = %#v, want Base{}", got)
	}
}

func TestForPathOnEmptyRegistryReturnsNeutralAgent(t *testing.T) {
	if got := NewRegistry().ForPath("/a"); got != (Base{}) {
		t.Fatalf("ForPath = %#v, want Base{}", got)
	}
}

func TestRegisterRefusesAPathAnotherAgentClaims(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(routed{name: "a", paths: []string{"/shared"}}); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	err := reg.Register(routed{name: "b", paths: []string{"/b-only", "/shared"}})

	if err == nil {
		t.Fatal("second Register for /shared: got nil error, want a refusal")
	}
	if got := reg.ForPath("/shared").Name(); got != "a" {
		t.Errorf("ForPath(/shared).Name() = %q, want the first agent %q", got, "a")
	}
	// A refused agent is not half-registered.
	if got := reg.ForPath("/b-only"); got != (Base{}) {
		t.Errorf("ForPath(/b-only) = %#v, want Base{} (refused agent must claim nothing)", got)
	}
}

func TestRegisterRefusesOnePathTwiceInOneAgent(t *testing.T) {
	if err := NewRegistry().Register(routed{name: "a", paths: []string{"/x", "/x"}}); err == nil {
		t.Fatal("Register with a repeated path: got nil error, want a refusal")
	}
}

func TestRegisterRefusesAnEmptyPath(t *testing.T) {
	if err := NewRegistry().Register(routed{name: "a", paths: []string{""}}); err == nil {
		t.Fatal("Register with an empty path: got nil error, want a refusal")
	}
}

func TestRegisterRefusesNil(t *testing.T) {
	if err := NewRegistry().Register(nil); err == nil {
		t.Fatal("Register(nil): got nil error, want a refusal")
	}
}

func TestRegistryIsSafeForConcurrentRegisterAndForPath(t *testing.T) {
	reg := NewRegistry()
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		p := fmt.Sprintf("/agent/%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := reg.Register(routed{name: p, paths: []string{p}}); err != nil {
				t.Errorf("Register(%s): %v", p, err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = reg.ForPath(p)
		}()
	}
	wg.Wait()

	for i := range n {
		p := fmt.Sprintf("/agent/%d", i)
		if got := reg.ForPath(p).Name(); got != p {
			t.Errorf("ForPath(%s).Name() = %q, want %q", p, got, p)
		}
	}
}

func TestPackageRegisterPanicsOnDuplicatePath(t *testing.T) {
	t.Cleanup(resetDefault)
	Register(routed{name: "a", paths: []string{"/dup"}})
	if got := ForPath("/dup").Name(); got != "a" {
		t.Fatalf("ForPath(/dup).Name() = %q, want %q", got, "a")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("second Register for /dup did not panic")
		}
	}()
	Register(routed{name: "b", paths: []string{"/dup"}})
}

// resetDefault must not race with lookups still running: it clears under the lock, and never swaps
// the package variable (a swap is a data race under -race).
func TestResetDefaultIsSafeWhileLookupsRun(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			ForPath("/x")
		}
	}()
	for i := 0; i < 200; i++ {
		resetDefault()
	}
	<-done
}
