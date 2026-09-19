package tokens_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// TestBilledFactorByContentClass measures f — the provider's token count over ours — per
// CONTENT CLASS, on generated text, at whatever n you ask for.
//
// Why this exists beside apply.TestBilledPairColdReplay: that test bills real captured
// turns, which is the right end-to-end instrument but yields only a few INDEPENDENT
// removals, because a handful of sessions supply all of them and successive turns of one
// session repeat the same delta. This test decouples the two questions:
//
//  1. how far apart are the two tokenizers on a given KIND of text  <- here, unlimited n
//  2. what kinds of text does the corpus consist of                  <- prod METADATA
//
// It deliberately uses GENERATED text rather than stored production content. request_content
// holds real tenants' transcripts — their source code and tool output — and sending that to
// an external gateway to measure a tokenizer is not a decision a measurement gets to make.
//
// Each arm differs only in the message text, so the request envelope cancels in the
// difference, exactly as in the end-to-end test.
//
//	CG_LIVE_URL=https://<gw> CG_LIVE_KEY=<tok> CG_LIVE_MODEL=claude-haiku-4-5 \
//	  go test ./internal/tokens/ -run BilledFactorByContentClass -v -timeout 20m
func TestBilledFactorByContentClass(t *testing.T) {
	base, key := os.Getenv("CG_LIVE_URL"), os.Getenv("CG_LIVE_KEY")
	if base == "" || key == "" {
		t.Skip("set CG_LIVE_URL and CG_LIVE_KEY to run this (it SPENDS MONEY)")
	}
	model := os.Getenv("CG_LIVE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	reps := 3
	if v := os.Getenv("CG_CLASS_REPS"); v != "" {
		fmt.Sscanf(v, "%d", &reps)
	}
	client := &http.Client{Timeout: 120 * time.Second}

	type result struct {
		class        string
		ours, billed int
		f            float64
	}
	var out []result
	var sumO, sumB int

	for _, cl := range contentClasses {
		var co, cb int
		for i := 0; i < reps; i++ {
			rng := rand.New(rand.NewSource(int64(i) + 1))
			big := cl.gen(rng, 900)   // the "before" text
			small := cl.gen(rng, 300) // what a component left behind
			ours := tokens.Count(big) - tokens.Count(small)
			if ours <= 0 {
				continue
			}
			bb, e1 := billOne(client, base, key, model, big)
			ba, e2 := billOne(client, base, key, model, small)
			if e1 != nil || e2 != nil {
				t.Logf("%s rep %d: %v / %v", cl.name, i, e1, e2)
				continue
			}
			co += ours
			cb += bb - ba
		}
		if co == 0 {
			continue
		}
		out = append(out, result{cl.name, co, cb, float64(cb) / float64(co)})
		sumO += co
		sumB += cb
	}
	if len(out) == 0 {
		t.Skip("nothing measurable")
	}

	sort.Slice(out, func(i, j int) bool { return out[i].f < out[j].f })
	fmt.Printf("\n=== f by content class, model=%s, %d reps each ===\n", model, reps)
	fmt.Printf("%-18s %10s %10s %8s\n", "class", "ours", "billed", "f")
	for _, r := range out {
		fmt.Printf("%-18s %10d %10d %8.4f\n", r.class, r.ours, r.billed, r.f)
	}
	fmt.Printf("\nf spans %.4f (%s) to %.4f (%s) — a SINGLE global factor is not defensible\n",
		out[0].f, out[0].class, out[len(out)-1].f, out[len(out)-1].class)
	fmt.Printf("f unweighted pooled = %.4f\n", float64(sumB)/float64(sumO))
	bf, meas := tokens.BilledDeltaFactor(model)
	fmt.Printf("tokens.BilledDeltaFactor(%q) = %.4f (measured=%v)\n", model, bf, meas)

	for _, r := range out {
		if r.f <= 0 {
			t.Errorf("%s: f = %v", r.class, r.f)
		}
	}
}

// contentClasses are the kinds of text a coding agent's transcript is actually made of.
var contentClasses = []struct {
	name string
	gen  func(rng *rand.Rand, n int) string
}{
	{"english_prose", func(rng *rand.Rand, n int) string {
		w := strings.Fields(`the quick brown fox jumps over a lazy dog while the system
			processes each request and returns a response to the caller without delay`)
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(w[rng.Intn(len(w))])
			b.WriteByte(' ')
		}
		return b.String()
	}},
	{"go_code", func(rng *rand.Rand, n int) string {
		var b strings.Builder
		for i := 0; i < n/6; i++ {
			fmt.Fprintf(&b, "func handler%d(ctx context.Context, w http.ResponseWriter) error {\n"+
				"\tif err := s.store.Put(ctx, %d); err != nil {\n\t\treturn fmt.Errorf(\"put: %%w\", err)\n\t}\n\treturn nil\n}\n",
				i, rng.Intn(9999))
		}
		return b.String()
	}},
	{"tool_result_json", func(rng *rand.Rand, n int) string {
		type row struct {
			ID    int    `json:"id"`
			Path  string `json:"path"`
			Hash  string `json:"hash"`
			Bytes int    `json:"bytes"`
		}
		rows := make([]row, 0, n/8)
		for i := 0; i < n/8; i++ {
			rows = append(rows, row{i, fmt.Sprintf("pkg/mod/file_%d.go", i),
				fmt.Sprintf("%08x%08x", rng.Uint32(), rng.Uint32()), rng.Intn(1 << 20)})
		}
		out, _ := json.MarshalIndent(rows, "", "  ")
		return string(out)
	}},
	{"grep_output", func(rng *rand.Rand, n int) string {
		var b strings.Builder
		for i := 0; i < n/4; i++ {
			fmt.Fprintf(&b, "dash/overview.go:%d:\to.TotalSavedUSD += cachesplitHist.USD // %d\n",
				rng.Intn(1200), rng.Intn(999))
		}
		return b.String()
	}},
	{"hex_hashes", func(rng *rand.Rand, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "%08x%08x ", rng.Uint32(), rng.Uint32())
		}
		return b.String()
	}},
	{"stack_trace", func(rng *rand.Rand, n int) string {
		var b strings.Builder
		for i := 0; i < n/5; i++ {
			fmt.Fprintf(&b, "goroutine %d [running]:\nmain.step%d(0x%x, 0x%x)\n\t/src/main.go:%d +0x%x\n",
				rng.Intn(500), i, rng.Uint32(), rng.Uint32(), rng.Intn(900), rng.Intn(4096))
		}
		return b.String()
	}},
}

// billOne bills one text as a single unmarked user message and returns the provider's own
// input-token count for it.
func billOne(c *http.Client, base, key, model, text string) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 1,
		"messages": []any{map[string]any{"role": "user", "content": text}},
	})
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("authorization", "Bearer "+key)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d: %.180s", resp.StatusCode, b)
	}
	var v struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
			Read        int `json:"cache_read_input_tokens"`
			Write       int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, err
	}
	if v.Usage.Read != 0 || v.Usage.Write != 0 {
		return 0, fmt.Errorf("unexpected cache activity on an unmarked body")
	}
	if v.Usage.InputTokens == 0 {
		return 0, fmt.Errorf("no input_tokens in reply")
	}
	return v.Usage.InputTokens, nil
}
