package schema

import (
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func loads(fn func(gjson.Result, func(string)), block string) []string {
	var got []string
	fn(gjson.Parse(block), func(n string) { got = append(got, n) })
	return got
}

func TestToolLoadReaders(t *testing.T) {
	server := `{"type":"tool_search_tool_result","content":{"type":"tool_search_tool_result","tool_references":[{"tool_name":"a"},{"tool_name":"b"}]}}`
	client := `{"type":"tool_result","content":[{"type":"text","text":"x"},{"type":"tool_reference","tool_name":"c"}]}`
	other := `{"type":"text","text":"tool_reference"}`
	for _, c := range []struct {
		name  string
		fn    func(gjson.Result, func(string))
		block string
		want  []string
	}{
		{"server result", SearchResultLoads, server, []string{"a", "b"}},
		{"server reader ignores a client result", SearchResultLoads, client, nil},
		{"client result", ResultReferenceLoads, client, []string{"c"}},
		{"client reader ignores a server result", ResultReferenceLoads, server, nil},
		{"both: server", BlockLoads, server, []string{"a", "b"}},
		{"both: client", BlockLoads, client, []string{"c"}},
		{"both: other", BlockLoads, other, nil},
	} {
		if got := loads(c.fn, c.block); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
