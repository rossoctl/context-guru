package apply_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/sjson"
)

// TestBilledPairColdReplay measures f against what the provider ACTUALLY BILLS, which is
// the only measurement issue #240's claim can be settled on.
//
// Why this test exists rather than a count_tokens comparison: /v1/messages/count_tokens on
// the IBM LiteLLM gateway returns the SAME number with `system` (27 KB) and `tools` (106 KB)
// present and absent — it counts messages only, with its own tokenizer. Measured, not
// assumed. So it compares two ESTIMATORS and cannot validate either against a bill.
//
// The design: take one captured request, run the real pipeline to get the compacted body,
// then send BOTH bodies to the provider as COLD requests and read `usage` off each reply.
// A cold request bills its whole prompt (fresh_input + cache_write, cache_read = 0), so the
// reply states the provider's own count of that exact body. Two cold arms of the same turn
// give a genuine billed counterfactual — the thing the dashboard never has.
//
//	f = (billed_before − billed_after) / (ours_before − ours_after)
//
// Each arm gets a unique nonce in `system` so neither can hit the other's cached prefix
// (or a previous run's), and `max_tokens: 1` keeps the completion cost near zero — we are
// buying the usage block, not the answer.
//
//	CONTEXT_GURU_CAPTURE=/path/capture.jsonl \
//	CG_LIVE_URL=https://<gateway> CG_LIVE_KEY=<token> CG_LIVE_MODEL=claude-haiku-4-5 \
//	  go test ./apply/ -run BilledPairColdReplay -v -timeout 30m
func TestBilledPairColdReplay(t *testing.T) {
	path := os.Getenv("CONTEXT_GURU_CAPTURE")
	base, key := os.Getenv("CG_LIVE_URL"), os.Getenv("CG_LIVE_KEY")
	if path == "" || base == "" || key == "" {
		t.Skip("set CONTEXT_GURU_CAPTURE, CG_LIVE_URL and CG_LIVE_KEY to run this (it SPENDS MONEY)")
	}
	model := os.Getenv("CG_LIVE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	maxPairs := 6
	if v := os.Getenv("CG_LIVE_PAIRS"); v != "" {
		fmt.Sscanf(v, "%d", &maxPairs)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("cannot read capture %q: %v", path, err)
	}
	defer f.Close()

	preset := os.Getenv("CG_PRESET")
	if preset == "" {
		preset = "agent"
	}
	cfg := pipe(t, "preset: "+preset+"\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	client := &http.Client{Timeout: 300 * time.Second}
	nonce := time.Now().UnixNano()

	type pair struct {
		oursB, oursA     int
		billedB, billedA int
		f                float64
	}
	var pairs []pair
	var sumOurs, sumBilled int
	seen, done := 0, 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() && done < maxPairs {
		var rec struct {
			Provider string          `json:"provider"`
			Body     json.RawMessage `json:"body"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || len(rec.Body) == 0 {
			continue
		}
		seen++
		before := append([]byte(nil), rec.Body...)
		res := apply.BodyOpts(context.Background(), p, st, apply.Opts{
			Provider: bschemas.ModelProvider(rec.Provider),
			Body:     before,
			Mode:     components.ModeSync,
		})
		if res.Run == nil || res.Run.TokensBefore-res.Run.TokensAfter <= 0 {
			continue
		}
		ob, oa := res.Run.TokensBefore, res.Run.TokensAfter

		bb, e1 := billedPrompt(t, client, base, key, model, before, fmt.Sprintf("%d-%d-b", nonce, seen))
		ba, e2 := billedPrompt(t, client, base, key, model, res.Body, fmt.Sprintf("%d-%d-a", nonce, seen))
		if e1 != nil || e2 != nil {
			t.Logf("pair %d: live call failed (%v / %v)", seen, e1, e2)
			continue
		}
		done++
		fv := float64(bb-ba) / float64(ob-oa)
		pairs = append(pairs, pair{ob, oa, bb, ba, fv})
		sumOurs += ob - oa
		sumBilled += bb - ba
		fmt.Printf("pair %2d  ours %7d -> %7d (d=%6d)   BILLED %7d -> %7d (d=%6d)   f=%7.4f\n",
			done, ob, oa, ob-oa, bb, ba, bb-ba, fv)
	}

	if len(pairs) == 0 {
		t.Skipf("no measurable pair in %d captured requests", seen)
	}
	fs := make([]float64, len(pairs))
	for i, p := range pairs {
		fs[i] = p.f
	}
	sort.Float64s(fs)
	q := func(pp float64) float64 {
		return fs[min(len(fs)-1, max(0, int(pp*float64(len(fs)-1)+0.5)))]
	}
	pooled := float64(sumBilled) / float64(sumOurs)
	fmt.Printf("\n=== f against the PROVIDER'S BILL, model=%s, n=%d pairs ===\n", model, len(pairs))
	fmt.Printf("f  min %.4f  p25 %.4f  MEDIAN %.4f  p75 %.4f  max %.4f\n",
		fs[0], q(.25), q(.5), q(.75), fs[len(fs)-1])
	fmt.Printf("f  POOLED = %.4f   [%d billed tokens / %d our tokens]\n", pooled, sumBilled, sumOurs)
	fmt.Printf("issue #240 asserts our count is 0.876 of billed, i.e. f = 1.1416\n")

	if pooled <= 0 {
		t.Fatalf("pooled f = %v: the instrument measured nothing", pooled)
	}
}

// billedPrompt sends one UNCACHED request and returns the provider's own count of this
// exact body (all of it billed as fresh input).
//
// Every `cache_control` mark is stripped rather than made unique with a nonce. A nonce
// appended to `system` does NOT work and the first version of this test proved it: the
// cacheable PREFIX (tools, then the head of system) stayed byte-identical, so the arm hit
// a 34,467-token cached prefix and its billed count was not a count of this body. With no
// breakpoints there is nothing to write and nothing to read, so `input_tokens` alone is
// the provider's count of the whole prompt — which is what we want to compare.
func billedPrompt(t *testing.T, c *http.Client, base, key, model string, body []byte, _ string) (int, error) {
	t.Helper()
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, err
	}
	doc = stripCacheControl(doc)
	// Claude Code injects messages with role "system" (local-command output, reminders).
	// The gateway's Anthropic dialect rejects those with a 400, so they are re-roled to
	// "user". Applied IDENTICALLY to both arms, on a message present in both, so its token
	// contribution cancels in the difference this test measures. It does not touch the
	// pipeline, which ran on the pristine body.
	doc = reroleSystemMessages(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}
	out, _ = sjson.SetBytes(out, "model", model)
	out, _ = sjson.SetBytes(out, "max_tokens", 1)
	out, _ = sjson.SetBytes(out, "stream", false)
	for _, k := range []string{"thinking", "output_config", "service_tier", "metadata"} {
		out, _ = sjson.DeleteBytes(out, k)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", bytes.NewReader(out))
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
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("messages %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	var v struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, err
	}
	u := v.Usage
	// With every breakpoint stripped these must both be zero. If they are not, the request
	// was cached after all and its count is not a clean count of this body.
	if u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0 {
		return 0, fmt.Errorf("expected an uncached arm, got cache_read=%d cache_write=%d",
			u.CacheReadInputTokens, u.CacheCreationInputTokens)
	}
	if u.InputTokens == 0 {
		return 0, fmt.Errorf("no usage in reply: %s", truncate(string(b), 300))
	}
	return u.InputTokens, nil
}

// stripCacheControl removes every "cache_control" key at any depth.
func stripCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "cache_control")
		for k, vv := range x {
			x[k] = stripCacheControl(vv)
		}
		return x
	case []any:
		for i := range x {
			x[i] = stripCacheControl(x[i])
		}
		return x
	}
	return v
}

// reroleSystemMessages rewrites messages[].role "system" to "user". See the call site.
func reroleSystemMessages(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	msgs, ok := m["messages"].([]any)
	if !ok {
		return v
	}
	for _, mm := range msgs {
		if e, ok := mm.(map[string]any); ok && e["role"] == "system" {
			e["role"] = "user"
		}
	}
	return m
}

// truncate bounds an error body so a 4 MB upstream error does not become the test log.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
