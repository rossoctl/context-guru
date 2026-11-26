package bob

import (
	"net/http"
	"slices"
	"testing"

	"github.com/rossoctl/context-guru/agent"
)

// "bob" is the label the dashboard already gives Bob (dash/event.go, proxy/compactioncfg.go).
func TestNameIsTheDashboardLabel(t *testing.T) {
	if got := (Bob{}).Name(); got != "bob" {
		t.Fatalf("Name() = %q, want %q", got, "bob")
	}
}

func TestPathsIsTheModelRoute(t *testing.T) {
	want := []string{"/inference/v1/chat/completions"}
	if got := (Bob{}).Paths(); !slices.Equal(got, want) {
		t.Fatalf("Paths() = %#v, want %#v", got, want)
	}
}

func TestSessionIDIsTheXTaskIDHeader(t *testing.T) {
	h := http.Header{}
	h.Set("X-Task-Id", "009431a32bc317f3dae92fe48c7cdc96")
	if got := (Bob{}).SessionID(h); got != "009431a32bc317f3dae92fe48c7cdc96" {
		t.Fatalf("SessionID() = %q, want the X-Task-Id value", got)
	}
}

func TestSessionIDIsEmptyWithoutXTaskID(t *testing.T) {
	h := http.Header{}
	h.Set("x-context-guru-session", "s1")
	if got := (Bob{}).SessionID(h); got != "" {
		t.Fatalf("SessionID() = %q, want %q", got, "")
	}
}

func TestRegisteredBobIsFoundByItsPath(t *testing.T) {
	reg := agent.NewRegistry()
	if err := reg.Register(Bob{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := reg.ForPath(ModelPath); got != (Bob{}) {
		t.Fatalf("ForPath(%q) = %#v, want Bob{}", ModelPath, got)
	}
}

// init registers Bob in the default registry, so a host needs only the import.
func TestInitRegistersBobInTheDefaultRegistry(t *testing.T) {
	if got := agent.ForPath(ModelPath); got != (Bob{}) {
		t.Fatalf("agent.ForPath(%q) = %#v, want Bob{}", ModelPath, got)
	}
}

// Bob serves chat only on its own model path, and takes `Authorization: Apikey <key>`.
func TestModelTargetIsBobsModelPathWithApikey(t *testing.T) {
	want := agent.ModelTarget{Path: "/inference/v1/chat/completions", AuthScheme: "Apikey"}
	if got := (Bob{}).ModelTarget(); got != want {
		t.Fatalf("ModelTarget() = %+v, want %+v", got, want)
	}
}
