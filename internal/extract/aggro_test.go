package extract

import (
	"strings"
	"testing"
)

// The three levels must be genuinely different instructions, must all keep the
// non-negotiables, and must all teach the SUMMARY — an example without one teaches the
// model to omit the digest the agent reads next to the recovery marker.
func TestAggressivenessBlocksDifferAndAllTeachTheSummary(t *testing.T) {
	blocks := map[Aggressiveness]string{
		AggroLow:    aggroBlock(AggroLow),
		AggroMedium: aggroBlock(AggroMedium),
		AggroHigh:   aggroBlock(AggroHigh),
	}
	seen := map[string]Aggressiveness{}
	for level, b := range blocks {
		if prev, dup := seen[b]; dup {
			t.Fatalf("%s and %s send identical text, so the dial does nothing", level, prev)
		}
		seen[b] = level
		if !strings.Contains(b, "SUMMARY") {
			t.Errorf("%s never mentions SUMMARY: its examples teach the model to omit the digest", level)
		}
		if !strings.Contains(b, "COMPACTION TARGET") {
			t.Errorf("%s states no target", level)
		}
		// Every level must exercise the shapes real traffic actually contains, or the
		// model only ever sees one kind of noise demonstrated.
		for _, shape := range []string{"JSON", "log", "prose", "FILE READ"} {
			if !strings.Contains(b, shape) {
				t.Errorf("%s has no %s example", level, shape)
			}
		}
	}
	// Aggressiveness must not buy reduction by weakening what survives: the verbatim rule
	// lives in the general contract, which is the same block for every level.
	if !strings.Contains(codeContract, "PRESERVE EXACTLY") {
		t.Fatal("the general contract no longer states the verbatim-preservation rule")
	}
	if !strings.Contains(aggroBlock(AggroHigh), "BYTE-IDENTICAL") {
		t.Fatal("the high level must restate that ids/paths/errors are never negotiable")
	}
}

func TestParseAggressiveness(t *testing.T) {
	for in, want := range map[string]Aggressiveness{
		"": AggroMedium, "medium": AggroMedium, "low": AggroLow, "high": AggroHigh,
	} {
		got, err := ParseAggressiveness(in)
		if err != nil || got != want {
			t.Fatalf("ParseAggressiveness(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	if _, err := ParseAggressiveness("maximum"); err == nil {
		t.Fatal("an unknown level was accepted; a typo must fail the config save, not " +
			"silently fall back to medium")
	}
}

// The general contract must be the FIRST block and byte-identical across levels: a
// provider caches a prefix, so if the level-specific text came first, two tenants on
// different levels would share no cached prefix at all.
func TestGeneralContractIsTheSharedFirstBlock(t *testing.T) {
	low := codeSystemBlocks(true, AggroLow)
	high := codeSystemBlocks(true, AggroHigh)
	if len(low) != 2 || len(high) != 2 {
		t.Fatalf("expected 2 system blocks, got %d/%d", len(low), len(high))
	}
	if low[0] != high[0] {
		t.Fatal("block 0 differs between levels, so it is not a prefix two tenants can share")
	}
	if low[1] == high[1] {
		t.Fatal("block 1 is identical between levels, so the dial does nothing")
	}
	// And rewrite mode still selects a different contract, in block 0 where it belongs.
	if codeSystemBlocks(false, AggroLow)[0] == low[0] {
		t.Fatal("deletion-only and rewrite contracts must differ")
	}
}

// Switching levels MUST miss the global result cache. Without this a tenant that moves to
// high keeps being served the low-aggressiveness extraction it already has on file, with
// nothing to notice.
func TestResultKeyChangesWithAggressiveness(t *testing.T) {
	base := DefaultCfg()
	key := func(a Aggressiveness) string {
		c := base
		c.Aggressiveness = a
		return ResultKey("content-key", "model-x", c)
	}
	l, m, h := key(AggroLow), key(AggroMedium), key(AggroHigh)
	if l == m || m == h || l == h {
		t.Fatalf("result keys collide across levels: low=%s medium=%s high=%s", l, m, h)
	}
	// Same level, same key — the reuse the cache exists for.
	if key(AggroHigh) != h {
		t.Fatal("the key is not stable for a fixed level")
	}
}

// The summary is spliced into the transcript, so it needs a bound. Before this, an
// over-long summary did not produce a long marker — it produced NO compaction, because
// the never-worse check abandoned the whole splice.
func TestClipSummary(t *testing.T) {
	if got := clipSummary("  pytest: 3 failed, 710 passed  "); got != "pytest: 3 failed, 710 passed" {
		t.Fatalf("a short summary must pass through trimmed, got %q", got)
	}
	if got := clipSummary("first line\nsecond line"); got != "first line" {
		t.Fatalf("multi-line summary must be reduced to one line, got %q", got)
	}
	long := strings.Repeat("very wordy summary ", 40)
	got := clipSummary(long)
	if n := len([]rune(got)); n > maxSummaryRunes {
		t.Fatalf("clipped summary is %d runes, over the %d bound", n, maxSummaryRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a clipped summary should show it was cut, got %q", got)
	}
	// Runes, not bytes: cutting mid-rune would splice invalid UTF-8 into the transcript.
	if got := clipSummary(strings.Repeat("é", 400)); !strings.HasPrefix(got, "é") {
		t.Fatalf("multi-byte clip mangled: %q", got)
	}
}
