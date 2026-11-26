package agent

import (
	"net/http"
	"testing"
)

func TestPluginMarksCompaction(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    string
		want bool
	}{{"mark", "compaction", true}, {"mark, odd case", " Compaction ", true}, {"other kind", "title", false}, {"absent", "", false}} {
		h := http.Header{}
		if tc.v != "" {
			h.Set(HeaderRequestKind, tc.v)
		}
		if got := PluginMarksCompaction(h); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
	if PluginMarksCompaction(nil) {
		t.Error("nil header: want false")
	}
}
