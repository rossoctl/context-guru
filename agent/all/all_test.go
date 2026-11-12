package all_test

import (
	"testing"

	"github.com/rossoctl/context-guru/agent"
	_ "github.com/rossoctl/context-guru/agent/all"
)

// Bob's model route (bob.ModelPath), written out: an import of agent/bob for its constant would
// register Bob too, and the test would prove nothing about agent/all.
const bobModelPath = "/inference/v1/chat/completions"

func TestImportingAllRegistersBob(t *testing.T) {
	if got := agent.ForPath(bobModelPath).Name(); got != "bob" {
		t.Fatalf("ForPath(%s).Name() = %q, want %q", bobModelPath, got, "bob")
	}
}

func TestImportingAllListsBob(t *testing.T) {
	var names []string
	for _, a := range agent.All() {
		names = append(names, a.Name())
	}
	if len(names) != 1 || names[0] != "bob" {
		t.Fatalf("All() names = %q, want [bob]", names)
	}
}
