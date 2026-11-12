package compactionpoint

import "testing"

func TestResolvePrefersTheOperatorValue(t *testing.T) {
	p := Resolve("rits/zai-org/glm-5-3", 167_000, 200_000)
	if p.Source != Operator || p.Tokens(200_000) != 167_000 {
		t.Fatalf("point = %+v, tokens %d, want the operator's 167000", p, p.Tokens(200_000))
	}
	if p.Frac < 0.834 || p.Frac > 0.836 {
		t.Fatalf("frac = %v, want 0.835 for the span readers", p.Frac)
	}
	// An operator value beats a bootstrap entry for a listed model too.
	if p := Resolve("claude-haiku-4-5", 150_000, 200_000); p.Source != Operator || p.Tokens(200_000) != 150_000 {
		t.Fatalf("a listed model ignored the operator: %+v", p)
	}
}

func TestResolveWithoutAValueIsTheBootstrapTable(t *testing.T) {
	for _, m := range []string{"claude-haiku-4-5", "claude-opus-5", "rits/zai-org/glm-5-3", ""} {
		if got, want := Resolve(m, 0, 200_000), For(m); got != want {
			t.Errorf("%q: Resolve(0) = %+v, want For = %+v", m, got, want)
		}
	}
	if p := Resolve("rits/zai-org/glm-5-3", 0, 200_000); p.Source != WindowFallback {
		t.Fatalf("an unlisted model must stay window_fallback: %+v", p)
	}
}

func TestAnOperatorValueDoesNotMoveWithTheWindow(t *testing.T) {
	p := Resolve("glm", 167_000, 400_000)
	if p.Tokens(400_000) != 167_000 || p.Tokens(0) != 167_000 {
		t.Fatalf("tokens = %d / %d, want 167000 whatever the window", p.Tokens(400_000), p.Tokens(0))
	}
}
