package offload

import (
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func TestMaskDeepBatchLiftsTheCachedPrefixGateOnlyWhenEnoughOutputsWait(t *testing.T) {
	big := func(i string) string { return strings.Repeat("log line "+i+" with some words in it\n", 60) }
	for _, tc := range []struct {
		batch  string
		masked int
	}{{"", 0}, {"deep_batch: 5\n", 0}, {"deep_batch: 3\n", 3}} {
		c, err := newMask([]byte("keep_recent: 1\nmin_tokens: 20\n" + tc.batch))
		if err != nil {
			t.Fatal(err)
		}
		req := toolMsgs(big("a"), big("b"), big("c"), big("d"))
		ctx := ctxFor(store.NewMemory(store.Options{MaxEntries: 100}))
		ctx.CacheAware, ctx.MaxCachedIdx = true, len(req.Input)-1 // everything is inside the cached prefix
		rep := &components.Report{Component: "mask"}
		if _, err := c.(components.Offload).Offload(req, rep, ctx); err != nil {
			t.Fatal(err)
		}
		n := 0
		for i := range req.Input {
			if strings.Contains(schema.MessageText(req.Input[i]), "masked") {
				n++
			}
		}
		if n != tc.masked {
			t.Errorf("deep_batch %q: masked %d outputs, want %d (gates %v)", tc.batch, n, tc.masked, rep.Gates)
		}
	}
}
