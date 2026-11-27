package testutil

import (
	"strings"
	"testing"
)

func TestGoListDepsIgnoresJSONInGOFLAGS(t *testing.T) {
	t.Setenv("GOFLAGS", "-json -mod=mod")
	out := GoListDeps(t, "strings")
	if strings.Contains(out, "{") || !strings.Contains(out, "unicode/utf8") {
		t.Fatalf("want plain import paths, got:\n%s", out)
	}
	if got := withoutJSON("-json -tags=x -json=true"); got != "-tags=x" {
		t.Fatalf("withoutJSON = %q, want -tags=x", got)
	}
}
