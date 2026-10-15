package components

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// With the LiteLLM map not loaded, a listed model still lets the fraction trigger resolve (#515).
func TestFracResolvableWithoutMapForListedModel(t *testing.T) {
	c := modelinfo.NewChain(modelinfo.NewLiteLLM("http://127.0.0.1:1/none", &http.Client{Timeout: 50 * time.Millisecond}, time.Hour))
	for _, id := range []string{"claude-sonnet-5", "aws/claude-opus-5[1m]", "anthropic/claude-fable-5"} {
		w, exact, ok := c.WindowExact(context.Background(), id)
		if !ok {
			t.Fatalf("%s: no window", id)
		}
		ctx := &Ctx{CtxWindow: w, CtxWindowExact: exact, PrevBilledInput: 1000}
		if !(Trigger{MinRequestFrac: 0.9}).FracResolvable(ctx) {
			t.Errorf("%s: FracResolvable false (window=%d exact=%v)", id, w, exact)
		}
	}
}
