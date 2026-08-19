package offload

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/tidwall/gjson"
)

// This file answers the one question that decides whether readlifecycle is worth
// enabling: with the cache-tail gate on it removes ZERO tokens on every warm arm (a real
// SWE-bench session declined 1,125 stale-Read candidates as `cached_prefix`), so the only
// way it pays warm is `stale_at_depth`, which costs a re-anchor. This measures the size
// of that re-anchor on REAL captured traffic, so the doc's dollar figure is measured
// rather than argued.
//
// It reads a raw Anthropic capture (one request body per line, the format
// CONTEXT_GURU_CAPTURE writes) named by CG_RL_CAPTURE, and skips when unset — captures
// live outside the repo and are far too large to commit. The committed
// testdata/read_lifecycle.json fixture drives every correctness test; this one only
// produces a number.
//
//	CG_RL_CAPTURE=/tmp/cg-runs/capture-swebench.jsonl go test ./components/offload \
//	    -run ReanchorCost -v
//
// Token counts come from internal/tokens (o200k), never bytes/4.
func TestReanchorCostOfStaleAtDepth(t *testing.T) {
	path := os.Getenv("CG_RL_CAPTURE")
	if path == "" {
		t.Skip("set CG_RL_CAPTURE to a raw request capture")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 512<<20)

	// A stale Read is charged its re-anchor ONCE, at the first turn it is stale: the
	// freeze replays the same bytes on every later turn, so no later turn re-anchors.
	// The incremental cost is only the tokens between the Read and the PREVIOUS turn's
	// cache boundary — everything past that boundary is new content the provider was
	// going to cache-write on this turn regardless.
	charged := map[string]bool{}
	var (
		turns, transitions      int
		removedTok, reanchorTok int
		prevMsgs                int
		distSum, distMax        int
	)
	for sc.Scan() {
		body := gjson.Get(sc.Text(), "body")
		if !body.Exists() {
			continue
		}
		msgs := body.Get("messages").Array()
		turns++
		type ev struct {
			idx  int
			edit bool
			path string
			key  string
			text string
		}
		var evs []ev
		calls := map[string]gjson.Result{}
		for i, m := range msgs {
			for _, b := range m.Get("content").Array() {
				switch b.Get("type").String() {
				case "tool_use":
					calls[b.Get("id").String()] = b
				case "tool_result":
					call, ok := calls[b.Get("tool_use_id").String()]
					if !ok {
						continue
					}
					name := call.Get("name").String()
					p := call.Get("input.file_path").String()
					if p == "" {
						continue
					}
					if editTools[name] {
						evs = append(evs, ev{idx: i, edit: true, path: p})
						continue
					}
					if name != "Read" {
						continue
					}
					txt, allText := resultText(b)
					if !allText || txt == "" {
						continue // image Read: never rewritten
					}
					evs = append(evs, ev{idx: i, path: p, key: p + "\x00" + txt, text: txt}) //nolint:gocritic
				}
			}
		}
		for k, e := range evs {
			if e.edit {
				continue
			}
			stale := false
			for _, later := range evs[k+1:] {
				if later.edit && later.path == e.path {
					stale = true
					break
				}
			}
			if !stale || charged[e.key] {
				continue
			}
			charged[e.key] = true
			transitions++
			removedTok += tokens.Count(e.text)
			// Everything strictly after this Read that the PREVIOUS turn had already sent
			// is what a rewrite here re-anchors. prevMsgs-1 is the previous turn's highest
			// index, i.e. the harness's MaxCachedIdx.
			for j := e.idx + 1; j < prevMsgs && j < len(msgs); j++ {
				reanchorTok += tokens.Count(msgs[j].Get("content").String())
			}
			d := prevMsgs - 1 - e.idx
			if d < 0 {
				d = 0
			}
			distSum += d
			if d > distMax {
				distMax = d
			}
		}
		prevMsgs = len(msgs)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if transitions == 0 {
		t.Skipf("no stale Read transition in %s (%d turns)", path, turns)
	}
	// The recurring saving is the removed body, gone from every turn after the
	// transition; the one-time cost is the re-anchor. Priced at the tier this corpus is
	// actually billed at — ~90% cache_read = 0.1x fresh — for the saving, and at
	// cache_write = 1.25x fresh for the invalidation, which is the honest asymmetry.
	const fresh = 2.00 / 1e6 // $/tok, the gateway's aws/claude-sonnet-5 input price
	t.Logf("turns=%d stale transitions=%d", turns, transitions)
	t.Logf("removed per turn after transition: %d tok", removedTok)
	t.Logf("one-time re-anchor: %d tok  (already-cached messages after the Read)", reanchorTok)
	t.Logf("distance Read→cache boundary at transition: mean %.1f msgs, max %d",
		float64(distSum)/float64(transitions), distMax)
	t.Logf("break-even: re-anchor $%.4f@write vs $%.4f/turn@read → pays after %.1f turns",
		float64(reanchorTok)*fresh*1.25, float64(removedTok)*fresh*0.1,
		float64(reanchorTok)*12.5/float64(max(removedTok, 1)))
}

// resultText renders a tool_result's content and says whether it is ALL text (an image
// block means the component must not rewrite it).
func resultText(b gjson.Result) (string, bool) {
	c := b.Get("content")
	if c.Type == gjson.String {
		return c.String(), true
	}
	var sb strings.Builder
	allText := true
	for _, blk := range c.Array() {
		if blk.Get("type").String() != "text" {
			allText = false
			continue
		}
		sb.WriteString(blk.Get("text").String())
	}
	return sb.String(), allText
}
