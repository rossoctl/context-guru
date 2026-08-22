package extract

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/internal/tokens"
)

func goBody(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\t%d\tfunc helper%d() error { return nil }\n", i+1, i)
	}
	return b.String()
}

// A result made only of markers must be refused whatever the marker says. The wording is
// deliberately varied because the keep-ratio floor — not isElisionMarker's vocabulary — is what
// catches these, so a marker phrasing the prompt never taught must be refused too.
func TestMarkerOnlyResultsAreRefusedWhateverTheWording(t *testing.T) {
	body := goBody(280)
	cfg := DefaultCfg()
	cfg.Rewrite = true
	for _, m := range []string{
		"# … 463 lines elided …",
		"... 463 lines omitted ...",
		"[truncated]",
		"<snip>",
		"# rest of file removed for brevity",
		"…",
	} {
		ok, why := validateExtraction(m, body, nil, cfg)
		t.Logf("marker=%-40q accepted=%v why=%q derivation=%.2f", m, ok, why, derivationRatio(m, body))
		if ok {
			t.Errorf("MARKER-ONLY ACCEPTED for %q", m)
		}
	}
}

// Markers must not count as kept content. Before stripElisionMarkers, 21 markers plus ONE surviving
// line — 0.36% of a 280-line body — cleared the 5% floor, because the markers supplied 150 of the
// 158 tokens. Same hole as the live 7,414-token total loss, one step diluted.
func TestMarkersCannotInflatePastTheKeepRatioFloor(t *testing.T) {
	body := goBody(280)
	bodyTok := tokensCountShim(body)
	cfg := DefaultCfg()
	cfg.Rewrite = true
	// One real line, plus many markers, until the token count clears 5%.
	lines := []string{"\t1\tfunc helper0() error { return nil }"}
	for i := 0; i < 400; i++ {
		lines = append(lines, fmt.Sprintf("... %d lines elided ...", i+1))
		res := strings.Join(lines, "\n")
		if ok, _ := validateExtraction(res, body, nil, cfg); ok {
			t.Fatalf("ACCEPTED with %d markers + 1 real line: resTok=%d bodyTok=%d ratio=%.4f",
				i+1, tokensCountShim(res), bodyTok, float64(tokensCountShim(res))/float64(bodyTok))
		}
	}
	// The point of the loop: no amount of marker mass buys acceptance for one real line.
}

// MaxChars 0 must leave the deterministic projection unable to shrink at all, so a withheld window
// becomes a refusal rather than a truncation.
func TestWithheldWindowLeavesTheProjectionUnableToShrink(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&b, "./components/offload/x.go:%d:func TestSomething%d(t *testing.T) {\n", 100+i, i)
	}
	body := b.String()
	cfg := DefaultCfg()
	cfg.Mode, cfg.Rewrite = "deterministic", true
	cfg.MaxChars = 0
	out, _, strat, why := RunExtractionDetail(context.Background(), body, "find the tests", nil, len(body)/4, cfg, nil)
	t.Logf("withheld-window deterministic: strat=%q out=%d why=%q", strat, len(out), why)
	if strat != "none" {
		t.Errorf("deterministic returned something with the window withheld: %q", strat)
	}
}

func tokensCountShim(s string) int { return tokens.Count(s) }
