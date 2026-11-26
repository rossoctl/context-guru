package apply

import (
	"testing"

	"github.com/tidwall/gjson"
)

// wireOrderValid is the last check of the count-change rebuild. It must reject an order that a matching
// mistake could produce, so the caller forwards the original body.
func TestWireOrderValid(t *testing.T) {
	orig := gjson.Parse(`[
		{"role":"user","content":"u"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"o"},{"type":"text","text":"u"}]}
	]`).Array()
	for _, c := range []struct {
		name  string
		order []int
		want  bool
	}{
		{"original order", []int{0, 1, 2}, true},
		{"a dropped middle", []int{0, 2}, true}, // a lone tool_result is the splice's own business
		{"tool_result before its tool_use", []int{2, 1}, false},
		{"relative order of kept messages changed", []int{1, 0, 2}, false},
		{"a message twice", []int{0, 0}, false},
	} {
		if got := wireOrderValid(orig, c.order); got != c.want {
			t.Errorf("%s: wireOrderValid(%v) = %v, want %v", c.name, c.order, got, c.want)
		}
	}
}
