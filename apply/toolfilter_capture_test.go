package apply_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// TestFilterCapturedTraffic is the MEASUREMENT, on real captured Claude Code requests
// rather than on a projection: replay a capture with a removal list, and report the tokens
// the filter actually stopped sending together with what they would have cost at the tier
// each request is billed at.
//
// It is a test rather than a script so the numbers in
// docs/how-to/declaration-removal.md are reproducible, and so the two properties the
// measurement depends on are checked while it runs: `tools` must be byte-identical on every
// request of a session (or the saving is cache invalidation instead), and `messages` must
// come out untouched.
//
// Skipped unless CONTEXT_GURU_CAPTURE names a readable capture:
//
//	CONTEXT_GURU_CAPTURE=/tmp/cg-runs/capture-tb.jsonl \
//	CG_FILTER_REMOVE=CronCreate,CronDelete,... \
//	  go test ./apply -run FilterCapturedTraffic -v
func TestFilterCapturedTraffic(t *testing.T) {
	path := os.Getenv("CONTEXT_GURU_CAPTURE")
	remove := os.Getenv("CG_FILTER_REMOVE")
	if path == "" || remove == "" {
		t.Skip("set CONTEXT_GURU_CAPTURE and CG_FILTER_REMOVE to run this")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("cannot read capture %q: %v", path, err)
	}
	defer f.Close()

	yaml := "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    remove: [" + remove + "]\n"
	p, err := pipe(t, yaml).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})

	// The corpus carries no usage, so the tier is assigned by the shape this corpus was
	// MEASURED to have: a session's first request writes the prefix (cache creation, 1.25x)
	// and every later one reads it (0.1x). 1,105 of 1,127 real session starts were cold, so
	// crediting the first request at the read rate would understate it. Rates are the IBM
	// gateway's aws/claude-sonnet-5 input price, the same default ab.sh uses.
	const inRate = 2.00 / 1e6
	var requests, filtered, firstTokens, laterTokens int
	toolsPerSession := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var rec struct {
			Body     json.RawMessage `json:"body"`
			Provider string          `json:"provider"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || len(rec.Body) == 0 {
			continue
		}
		requests++
		res := apply.BodyOpts(context.Background(), p, st, apply.Opts{
			Provider: bschemas.Anthropic, Body: rec.Body, Session: "", Tenant: "cap",
		})
		if a, b := gjson.GetBytes(rec.Body, "messages").Raw, gjson.GetBytes(res.Body, "messages").Raw; a != b {
			t.Fatalf("request %d had its messages rewritten", requests)
		}
		if res.FilteredDeclTokens == 0 {
			continue
		}
		filtered++
		tools := gjson.GetBytes(res.Body, "tools").Raw
		if prev, seen := toolsPerSession[res.Session]; seen {
			if prev != tools {
				t.Fatalf("session %s sent different tools on request %d: cache invalidation, not saving",
					res.Session, requests)
			}
			laterTokens += res.FilteredDeclTokens
			continue
		}
		toolsPerSession[res.Session] = tools
		firstTokens += res.FilteredDeclTokens
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if filtered == 0 {
		t.Fatalf("nothing was filtered across %d requests; the removal list matched nothing", requests)
	}
	usd := float64(firstTokens)*inRate*1.25 + float64(laterTokens)*inRate*0.1
	t.Logf("capture=%s requests=%d filtered=%d sessions=%d\n"+
		"  removed tokens: %d at cache creation (session firsts) + %d at cache read = %d\n"+
		"  avoided: $%.4f  (per session: %d tokens/request)\n"+
		"  removal list: %s",
		trimPath(path), requests, filtered, len(toolsPerSession),
		firstTokens, laterTokens, firstTokens+laterTokens, usd,
		(firstTokens+laterTokens)/filtered, remove)
}

func trimPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
