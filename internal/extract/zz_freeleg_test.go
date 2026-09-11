package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// NOTE ON THE FILENAME: do not end it `..._arm_test.go`. Go reads a trailing `_arm` as the GOARCH
// filename build constraint, so the file lands in IgnoredGoFiles on every other architecture and the
// test silently never runs -- `go test` reports "no tests to run" and PASSES. That happened here.
//
// TestDeterministicOnlyArmOverCapturedBodies replays captured production candidate bodies through
// the deterministic projection ALONE, with no model available, and writes a TSV comparing what the
// free leg produces against what production's paid leg actually produced on the same input.
//
// It exists because "would the free character window have done as well?" had never been measured,
// and it is the only arm that answers it without spending anything: with model == nil,
// strategyOrder collapses to ["deterministic"] and no call is issued.
//
// It skips without CG_H0_DIR, so it costs CI nothing. To build the corpus from a dashboard DB
// (before_gz is capped at defaultContentCap = 16<<10 BYTES, so 46.4 % of production blobs are
// truncated and MUST be excluded -- they end with the literal marker below):
//
//	select request_id, seq, candidate_tokens, saved_tokens, cost_usd, latency_ms,
//	       accepted, strategy, rejection, before_gz, after_gz
//	  from extraction_calls where component='extract_llm' and before_gz is not null;
//
// write each before_gz gunzipped to <dir>/<request_id>_<seq>.before, drop any whose contents end
// with "[truncated: content capture cap reached]", and write the metadata rows as <dir>/meta.json
// in the h0meta shape. Compare tokens.Count(body) against candidate_tokens to confirm the
// remainder is faithful.
//
// KNOWN INTERNAL-VALIDITY LIMIT, because it decides how the output may be read: the untruncated
// population tops out at 6,405 candidate tokens, so the 8-15k band -- the only size band whose
// measured cost per MTok saved clears break-even -- has ZERO representation, and 62 % of the
// corpus is <=3k where the paid leg is already underwater. The comparison is therefore biased
// TOWARD the free leg and cannot speak to the paid leg's one winning case.
func TestDeterministicOnlyArmOverCapturedBodies(t *testing.T) {
	dir := os.Getenv("CG_H0_DIR")
	if dir == "" {
		t.Skip("set CG_H0_DIR to an exported candidate corpus; see the doc comment")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta []h0meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	// Production's own configuration: no tenant sets max_chars or rewrite, so these are the
	// shipped defaults and the arm is comparable to the ledger it is scored against.
	cfg := DefaultCfg()
	cfg.Mode = "deterministic"
	cfg.Rewrite = true
	out, err := os.Create(filepath.Join(dir, "h0.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	fmt.Fprintln(out, "name\tprod_acc\tprod_strat\tprod_saved\tfree_ok\tbase_tok\tres_tok\tbase_chars\tres_chars\tfree_reason")
	var ok, n int
	for _, m := range meta {
		body, err := os.ReadFile(filepath.Join(dir, m.Name+".before"))
		if err != nil {
			t.Fatal(err)
		}
		n++
		res, _, strat, reason := RunExtractionDetail(context.Background(), string(body),
			"compact this tool output", nil, m.Cand, cfg, nil)
		free := 0
		if strat == "deterministic" && res != "" {
			free, ok = 1, ok+1
		}
		fmt.Fprintf(out, "%s\t%d\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", m.Name, m.Acc, m.Strat, m.Saved,
			free, tokens.Count(string(body)), tokens.Count(res), len(body), len(res), reason)
	}
	if n == 0 {
		t.Fatal("empty corpus; the arm measured nothing")
	}
	t.Logf("%d bodies, free leg produced an accepted result on %d; wrote %s",
		n, ok, filepath.Join(dir, "h0.tsv"))
}

// h0meta is one row of the production ledger for the body it names.
type h0meta struct {
	Name  string  `json:"name"`
	Cand  int     `json:"cand"`
	Saved int     `json:"saved"`
	Cost  float64 `json:"cost"`
	Ms    float64 `json:"ms"`
	Acc   int     `json:"acc"`
	Strat string  `json:"strat"`
	Rej   string  `json:"rej"`
}
