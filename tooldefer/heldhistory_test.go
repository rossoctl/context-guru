package tooldefer

import (
	"strings"
	"testing"

	cgschema "github.com/rossoctl/context-guru/schema"
)

// A Responses turn that holds history at the provider through conversation or item_reference is a
// chained turn, as previous_response_id is: it never makes a decision (#1298).
func TestApplySearchServerHeldHistoryForms(t *testing.T) {
	e := env(t, unusedHist())
	base := string(respBody())
	for name, b := range map[string]string{
		"conversation string": strings.Replace(base, `"input":`, `"conversation":"conv_1","input":`, 1),
		"conversation object": strings.Replace(base, `"input":`, `"conversation":{"id":"conv_1"},"input":`, 1),
		"item_reference":      strings.Replace(base, `"input":[`, `"input":[{"type":"item_reference","id":"msg_1"},`, 1),
	} {
		if o, _ := ApplySearch(e, cgschema.WireResponses, sreq([]byte(b))); o.Changed || o.Why != "previous_response_id" {
			t.Errorf("%s: %s", name, show(o))
		}
	}
}
