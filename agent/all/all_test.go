package all_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/agent/all"
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
	if !slices.Contains(names, "bob") {
		t.Fatalf("All() names = %q, want bob among them", names)
	}
}

func TestIsClientCompaction(t *testing.T) {
	plain := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	phrase := []byte(`{"input":"You are performing a CONTEXT CHECKPOINT COMPACTION. x"}`)
	h := http.Header{}
	h.Set(agent.HeaderRequestKind, "compaction")
	for _, tc := range []struct {
		name string
		h    http.Header
		body []byte
		want bool
	}{
		{"plugin mark", h, plain, true},
		{"nil headers, phrase", nil, phrase, true},
		{"nil headers, plain", nil, plain, false},
		{"headers, plain", http.Header{}, plain, false},
	} {
		if got := all.IsClientCompaction(tc.h, tc.body); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
