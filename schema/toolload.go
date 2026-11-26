package schema

import "github.com/tidwall/gjson"

// SearchResultLoads calls fn with the name of each tool a server-side search result block loaded: a
// tool_search_tool_result block's content.tool_references. It does nothing for any other block. fn
// gets the name as written, empty names too; a caller that needs a clean set drops them itself.
func SearchResultLoads(b gjson.Result, fn func(name string)) {
	if b.Get("type").String() == "tool_search_tool_result" {
		searchResultNames(b, fn)
	}
}

func searchResultNames(b gjson.Result, fn func(name string)) {
	b.Get("content.tool_references").ForEach(func(_, r gjson.Result) bool {
		fn(r.Get("tool_name").String())
		return true
	})
}

// ResultReferenceLoads calls fn with the name of each tool a client tool_result block loaded: the
// tool_reference blocks in its content, which Claude Code's own ToolSearch writes. It does nothing for
// any other block.
func ResultReferenceLoads(b gjson.Result, fn func(name string)) {
	if b.Get("type").String() == "tool_result" {
		resultReferenceNames(b, fn)
	}
}

func resultReferenceNames(b gjson.Result, fn func(name string)) {
	b.Get("content").ForEach(func(_, r gjson.Result) bool {
		if r.Get("type").String() == "tool_reference" {
			fn(r.Get("tool_name").String())
		}
		return true
	})
}

// BlockLoads is both readers: the tools one content block loaded, whoever ran the search.
func BlockLoads(b gjson.Result, fn func(name string)) {
	switch b.Get("type").String() {
	case "tool_search_tool_result":
		searchResultNames(b, fn)
	case "tool_result":
		resultReferenceNames(b, fn)
	}
}
