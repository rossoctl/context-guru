package thread

import (
	"hash"
	"hash/fnv"
	"testing"

	"github.com/tidwall/gjson"
)

// canon writes v's thread-identifying shape to h. See Fingerprint for what it leaves out.
func canonRef(h hash.Hash64, v gjson.Result) {
	switch {
	case v.IsObject():
		switch v.Get("type").String() {
		case "thinking", "redacted_thinking", "reasoning":
			h.Write([]byte("~"))
			return
		case "tool_result":
			h.Write([]byte("tool_result:"))
			h.Write([]byte(v.Get("tool_use_id").String()))
			return
		case "function_call_output": // Responses
			h.Write([]byte("function_call_output:"))
			h.Write([]byte(v.Get("call_id").String()))
			return
		}
		if v.Get("role").String() == "tool" { // OpenAI chat
			h.Write([]byte("tool:"))
			h.Write([]byte(v.Get("tool_call_id").String()))
			return
		}
		h.Write([]byte("{"))
		v.ForEach(func(k, val gjson.Result) bool {
			if k.Str == "cache_control" {
				return true
			}
			h.Write([]byte(k.Str))
			h.Write([]byte(":"))
			if k.Str == "content" && val.Type == gjson.String {
				// Claude Code sends the newest user turn's content as a one-element block
				// array (so it can carry a cache_control breakpoint); once that turn is no
				// longer the newest, it resends the SAME message as a plain string. The
				// provider normalizes both to one text block and its cache still hits, so
				// this package must agree: hash the string exactly as it would hash
				// [{"type":"text","text":<the string>}] — val.Raw is already the properly
				// quoted/escaped JSON string literal, so splicing it in produces byte-for-
				// byte the same canon() output a genuine one-text-block array would. Scoped
				// to the "content" key only, and only this one direction (string ->
				// canonical array shape): a content array is left exactly as canon already
				// handles it, whether it holds one block or many.
				canonRef(h, gjson.Parse(`[{"type":"text","text":`+val.Raw+`}]`))
			} else {
				canonRef(h, val)
			}
			h.Write([]byte(","))
			return true
		})
		h.Write([]byte("}"))
	case v.IsArray():
		h.Write([]byte("["))
		v.ForEach(func(_, val gjson.Result) bool {
			canonRef(h, val)
			h.Write([]byte(","))
			return true
		})
		h.Write([]byte("]"))
	default:
		h.Write([]byte(v.Raw))
	}
}

// canonRef above is canon as it was before the single-pass rewrite (it looked each discriminating
// key up with Get and re-parsed a copy of every string content). The rewrite must write the same
// bytes for every input, since the thread ids are hashes of them.
func TestCanonSinglePassWritesTheSameBytesAsTheOriginal(t *testing.T) {
	cases := []string{
		`{"role":"user","content":"hello"}`,
		`{"role":"user","content":"a \"quoted\" é line\nnext"}`,
		`{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"y"}]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"out"}]}`,
		`{"type":"tool_result","tool_use_id":"tu_9","content":"out"}`,
		`{"type":"function_call_output","call_id":"c1","output":"o"}`,
		`{"role":"tool","tool_call_id":"call_7","content":"result"}`,
		`{"role":"tool","content":"no id"}`,
		`{"type":"reasoning","summary":[]}`,
		`{"role":"user","role":"assistant","content":"dup keys"}`,
		`{"type":"a","type":"tool_result","tool_use_id":"x","tool_use_id":"y"}`,
		`{"cache_control":{"type":"ephemeral"},"role":"user","content":"cc first"}`,
		`{"content":"content first","role":"user"}`,
		`{"role":"user","content":""}`,
		`{"role":"user","content":null}`,
		`{"role":"user","content":{"nested":{"content":"inner","type":"tool_result"}}}`,
		`{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}],"content":null}`,
		`{"role":"tool","tool_call_id":"esc"}`,
		`{"role":"user","content":"x","type":5}`,
		`{"type":null,"role":7,"content":"y"}`,
		`[{"type":"text","text":"top-level array"}]`,
		`"a bare string"`,
		`42`,
		`{}`,
	}
	for _, c := range cases {
		a, b := fnv.New64a(), fnv.New64a()
		canonRef(hash.Hash64(a), gjson.Parse(c))
		canon(hash.Hash64(b), gjson.Parse(c))
		if a.Sum64() != b.Sum64() {
			t.Errorf("canon changed its output for %s", c)
		}
	}
}
