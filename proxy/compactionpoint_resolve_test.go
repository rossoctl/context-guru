package proxy

import (
	"context"
	"testing"

	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// The proxy takes C from the operator's MODEL_PRICES table when it states one, and from the bootstrap
// table otherwise. With no value stated, nothing changes.
func TestResolveCompactionPointPrefersTheOperatorTable(t *testing.T) {
	tb, err := modelinfo.ParseTable([]byte("models:\n  - {match: \"rits/zai-org/glm-5-3\", in: 1.0, out: 4.0, window: 200000, compaction_point: 167000}\n"))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"rits/zai-org/glm-5-3"}`)
	h := &Handler{opts: Options{Windows: modelinfo.Chain{tb}}}
	p := h.resolveCompactionPoint(context.Background(), body, 200_000)
	if p.Source != compactionpoint.Operator || p.Tokens(200_000) != 167_000 {
		t.Fatalf("point = %+v, want the operator's 167000", p)
	}
	// No stated value: byte-identical to the bootstrap table.
	h = &Handler{opts: Options{Windows: modelinfo.Chain{}}}
	if got, want := h.resolveCompactionPoint(context.Background(), body, 200_000), compactionpoint.For("rits/zai-org/glm-5-3"); got != want {
		t.Fatalf("with no field: %+v, want %+v", got, want)
	}
	h = &Handler{}
	if got, want := h.resolveCompactionPoint(context.Background(), body, 200_000), compactionpoint.For("rits/zai-org/glm-5-3"); got != want {
		t.Fatalf("with no resolver: %+v, want %+v", got, want)
	}
}
