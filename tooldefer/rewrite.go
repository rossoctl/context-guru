package tooldefer

import (
	"sort"
	"strings"

	"github.com/rossoctl/context-guru/internal/deferred"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// searchTool is the server-side search tool's declaration. Fixed bytes: it sits in `tools`, at the
// head of the cached prefix.
//
// There is no `limit` here: the API takes `limit` in the model's search INPUT, not on the tool
// (docs: "Claude can set a limit in its search input"). The names list asks the model for limit 1.
const searchTool = `{"type":"` + SearchType + `","name":"` + SearchName + `"}`

// listHead starts the names list. Fixed text, so the list has the same bytes on every request.
const listHead = "Context Guru deferred these tools to save context. Their definitions are not loaded yet. " +
	"Before you call one, load it with " + SearchName + ": set query to its exact name and limit to 1. " +
	"Then call it directly. Deferred tools:"

// NamesList is the names list for a sorted set of names.
func NamesList(names []string) string {
	return listHead + "\n" + strings.Join(names, "\n")
}

// Rewrite defers the tools in set that this request declares and loads, adds the search tool, and
// puts the names list at messages[1]. It returns the names it deferred, sorted. ok=false means
// nothing changed (nothing to defer, or a shape it does not handle); out is then body.
//
// Guards, from the 400 errors the API returns:
//   - a server tool (any element with a `type`) and a tool the client already deferred are left
//     alone;
//   - at least one client tool stays loaded, so a cache_control mark always has a home: if the set
//     would defer every client tool, the last one in array order stays loaded;
//   - a deferred tool cannot carry cache_control: its mark moves to the last loaded tool.
func Rewrite(body []byte, set map[string]bool) (out []byte, names []string, ok bool) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(set) == 0 {
		return body, nil, false
	}
	arr := tools.Array()
	mark := make([]bool, len(arr))
	hasSearch, lastLoaded := false, -1
	for i, t := range arr {
		n := t.Get("name").String()
		if n == SearchName {
			hasSearch = true
		}
		if t.Get("type").Exists() || t.Get("defer_loading").Bool() {
			continue
		}
		if set[n] {
			mark[i] = true
		} else {
			lastLoaded = i
		}
	}
	if lastLoaded < 0 {
		// Every client tool is in the set: keep the last one loaded.
		for i := len(arr) - 1; i >= 0; i-- {
			if mark[i] {
				mark[i], lastLoaded = false, i
				break
			}
		}
	}
	moveMark := ""
	elems := make([]string, len(arr))
	for i, t := range arr {
		elems[i] = t.Raw
		if !mark[i] {
			continue
		}
		raw := t.Raw
		if cc := t.Get("cache_control"); cc.Exists() {
			if moveMark == "" {
				moveMark = cc.Raw
			}
			var err error
			if raw, err = sjson.Delete(raw, "cache_control"); err != nil {
				return body, nil, false
			}
		}
		raw, err := sjson.SetRaw(raw, "defer_loading", "true")
		if err != nil {
			return body, nil, false
		}
		elems[i] = raw
		names = append(names, t.Get("name").String())
	}
	if len(names) == 0 {
		return body, nil, false
	}
	if moveMark != "" && lastLoaded >= 0 && !arr[lastLoaded].Get("cache_control").Exists() {
		raw, err := sjson.SetRaw(elems[lastLoaded], "cache_control", moveMark)
		if err != nil {
			return body, nil, false
		}
		elems[lastLoaded] = raw
	}
	if !hasSearch {
		elems = append(elems, searchTool)
	}
	out, err := sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(elems, ",")+"]"))
	if err != nil {
		return body, nil, false
	}
	sort.Strings(names)
	if out, ok = placeList(out, NamesList(names)); !ok {
		return body, nil, false
	}
	return out, names, true
}

// placeList puts the names list at messages[1]: appended to a role "system" message there
// (Claude Code's own deferred-tools block, or its environment block when its search is off), else
// as a new system message inserted at index 1. Never at the end of the transcript, which moves.
func placeList(body []byte, list string) ([]byte, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() || len(msgs.Array()) == 0 {
		return body, false
	}
	m1 := gjson.GetBytes(body, "messages.1")
	if m1.Get("role").String() == "system" {
		c := m1.Get("content")
		switch {
		case c.Type == gjson.String:
			out, err := sjson.SetBytes(body, "messages.1.content", c.String()+"\n\n"+list)
			return out, err == nil
		case c.IsArray():
			blocks := c.Array()
			last := len(blocks) - 1
			if last >= 0 && blocks[last].Get("type").String() == "text" {
				path := "messages.1.content." + itoa(last) + ".text"
				out, err := sjson.SetBytes(body, path, blocks[last].Get("text").String()+"\n\n"+list)
				return out, err == nil
			}
			out, err := sjson.SetRawBytes(body, "messages.1.content.-1", textBlock(list))
			return out, err == nil
		}
		return body, false
	}
	arr := msgs.Array()
	elems := make([]string, 0, len(arr)+1)
	for i, m := range arr {
		if i == 1 {
			elems = append(elems, systemMsg(list))
		}
		elems = append(elems, m.Raw)
	}
	if len(arr) == 1 {
		elems = append(elems, systemMsg(list))
	}
	out, err := sjson.SetRawBytes(body, "messages", []byte("["+strings.Join(elems, ",")+"]"))
	return out, err == nil
}

func textBlock(s string) []byte {
	b, _ := sjson.SetBytes([]byte(`{"type":"text"}`), "text", s)
	return b
}

func systemMsg(s string) string {
	b, _ := sjson.SetBytes([]byte(`{"role":"system"}`), "content", s)
	return string(b)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for ; i > 0; i /= 10 {
		n--
		b[n] = byte('0' + i%10)
	}
	return string(b[n:])
}

// ClaudeCodeSearchOn reports whether Claude Code's own search is on in this request: its
// ToolSearch tool, its placeholder, or its deferred-tools list.
func ClaudeCodeSearchOn(body []byte) bool {
	found := false
	gjson.GetBytes(body, "tools").ForEach(func(_, t gjson.Result) bool {
		n := t.Get("name").String()
		found = n == claudeCodeSearch || n == placeholder
		return !found
	})
	return found || strings.Contains(string(body), deferred.Header)
}
