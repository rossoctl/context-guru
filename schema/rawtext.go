package schema

import (
	"strings"

	"github.com/tidwall/gjson"
)

// ContentText returns the text of a raw wire `content` (or `output`) value. It is the gjson twin
// of MessageText and follows the same rule:
//   - a string is returned as it is;
//   - an array gives the text of its blocks joined with no separator, in order;
//   - a block that carries no string `text` (an image, a nested object) adds nothing;
//   - any other value (null, number, missing) gives "".
//
// Use it instead of gjson's String() on a value that may be an array. String() on an array returns
// the raw JSON, quotes and escapes included, so a comparison against the original text never
// matches and a token count is wrong.
func ContentText(v gjson.Result) string {
	switch {
	case v.Type == gjson.String:
		return v.String()
	case v.IsArray():
		var b strings.Builder
		v.ForEach(func(_, blk gjson.Result) bool {
			if t := blk.Get("text"); t.Type == gjson.String {
				b.WriteString(t.String())
			}
			return true
		})
		return b.String()
	}
	return ""
}

// ToolResultText returns the text of an Anthropic tool_result block: its `content`, as a string or
// as an array of blocks.
func ToolResultText(blk gjson.Result) string {
	return ContentText(blk.Get("content"))
}
