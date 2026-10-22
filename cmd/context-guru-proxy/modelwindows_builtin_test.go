package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/modelinfo/litellm"
)

// The default (public map) path must put the built-in exact list between the map and DefaultStatic
// (#515). The map is not reachable in a test, so the chain is checked by shape, which cannot race
// with the background download.
func TestModelWindowsDefaultPathHasBuiltinBetweenMapAndStatic(t *testing.T) {
	t.Setenv("MODEL_PRICES", "")
	t.Setenv("MODEL_INFO", "")
	t.Setenv("MODEL_INFO_URL", "")
	chain, ok := modelWindows().(modelinfo.Chain)
	if !ok || len(chain) != 3 {
		t.Fatalf("default chain has %d links (chain=%v); want {LiteLLM, Builtin, Static}", len(chain), ok)
	}
	if _, ok := chain[0].(*litellm.LiteLLM); !ok {
		t.Errorf("chain[0] = %T; want *LiteLLM", chain[0])
	}
	if _, ok := chain[1].(modelinfo.Builtin); !ok {
		t.Errorf("chain[1] = %T; want Builtin", chain[1])
	}
	if _, ok := chain[2].(modelinfo.Static); !ok {
		t.Errorf("chain[2] = %T; want Static", chain[2])
	}
}

// Behind an explicit MODEL_INFO_URL the document is the authority. A model it omits must not be
// answered exact by the built-in list.
func TestModelWindowsExplicitURLDoesNotUseBuiltin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"some-other-model":{"max_input_tokens":64000}}`))
	}))
	defer srv.Close()
	t.Setenv("MODEL_PRICES", "")
	t.Setenv("MODEL_INFO", "")
	t.Setenv("MODEL_INFO_URL", srv.URL)
	r := modelWindows()
	if w, exact, ok := modelinfo.Exact(r, context.Background(), "some-other-model"); !ok || !exact || w != 64000 {
		t.Fatalf("document model = %d,%v,%v; want 64000,true,true", w, exact, ok)
	}
	if _, exact, _ := modelinfo.Exact(r, context.Background(), "claude-opus-5"); exact {
		t.Error("claude-opus-5 is exact behind an explicit URL that omits it; the built-in list must not answer there")
	}
}
