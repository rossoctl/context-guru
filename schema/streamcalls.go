package schema

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// CallStream rebuilds the tool calls of a streamed response from its events, one event at a time.
// It reads the three dialects with one set of rules, and a stream holds only one dialect:
//
//	Anthropic   content_block_start (a tool_use block), then input_json_delta pieces, by block index
//	Chat        choices[0].delta.tool_calls[], by index: id and name once, arguments in pieces
//	Responses   output_item.added (a function_call or custom_tool_call item), the argument delta
//	            events, output_item.done, and the output of the terminal response event
//
// It is built for bytes that may be incomplete, such as the head and tail of a long response. A
// call whose pieces are missing still comes out, with the arguments it has. The caller checks
// that the arguments are JSON. The result matches OutputCalls on the whole message.
//
// Every reader of a streamed call uses this type: the facts of a response (respfacts) and the
// rebuild of a whole message for the expand loop (expand).
type CallStream struct {
	calls []*streamCall
	byKey map[string]*streamCall
}

type streamCall struct {
	id, name string
	custom   bool
	start    string          // the arguments as the opening event gave them
	pieces   strings.Builder // the arguments as delta events gave them
	full     string          // the arguments as a closing event gave them, when one did
	hasFull  bool
}

func (c *streamCall) args() string {
	switch {
	case c.hasFull:
		return c.full
	case len(strings.TrimSpace(c.pieces.String())) > 0:
		return strings.TrimSpace(c.pieces.String())
	}
	return c.start
}

// StreamCalls rebuilds the tool calls of a whole event stream, in the order they open.
func StreamCalls(body []byte) []OutputCall {
	var s CallStream
	SSEData(body, func(d []byte) bool {
		s.Add(d)
		return true
	})
	return s.Calls()
}

// Calls is the calls rebuilt so far, in the order they opened.
func (s *CallStream) Calls() []OutputCall {
	out := make([]OutputCall, 0, len(s.calls))
	for _, c := range s.calls {
		out = append(out, OutputCall{ID: c.id, Name: c.name, Args: c.args(), Custom: c.custom})
	}
	return out
}

// callMarkers are the words an event needs for Add to read it: "_call" is in function_call,
// custom_tool_call and tool_calls, "_use" is in tool_use, and input_json_delta is the Anthropic
// argument piece. Text and thinking deltas have none of them, and they are most of a stream, so
// they cost these byte searches and no JSON parse. Longer words such as "tool_use" would be slower:
// a search for a word that starts with a common letter is slow.
var callMarkers = [...][]byte{[]byte("_call"), []byte("_use"), []byte("input_json_delta")}

// Add reads the data of one event. An event that is not a tool call event changes nothing.
func (s *CallStream) Add(data []byte) {
	hit := false
	for _, m := range callMarkers {
		if bytes.Contains(data, m) {
			hit = true
			break
		}
	}
	if !hit {
		return
	}
	ev := gjson.ParseBytes(data)
	if tcs := ev.Get("choices.0.delta.tool_calls"); tcs.IsArray() {
		tcs.ForEach(func(_, tc gjson.Result) bool {
			s.addChat(tc)
			return true
		})
		return
	}
	t := ev.Get("type").String()
	switch {
	case t == "content_block_start":
		if cb := ev.Get("content_block"); cb.Get("type").String() == "tool_use" {
			c := s.open("a"+ev.Get("index").String(), true)
			c.id, c.name, c.start = cb.Get("id").String(), cb.Get("name").String(), cb.Get("input").Raw
		}
	case t == "content_block_delta":
		if ev.Get("delta.type").String() == "input_json_delta" {
			if c := s.byKey["a"+ev.Get("index").String()]; c != nil {
				c.pieces.WriteString(ev.Get("delta.partial_json").String())
			}
		}
	case strings.HasPrefix(t, "response."):
		s.addResponses(t, ev)
	}
}

// Block is the Anthropic tool_use block at a content block index, when the stream has one. The
// expand loop rebuilds a whole message, so it has to place each call among the other blocks.
func (s *CallStream) Block(index int) (OutputCall, bool) {
	c := s.byKey["a"+strconv.Itoa(index)]
	if c == nil {
		return OutputCall{}, false
	}
	return OutputCall{ID: c.id, Name: c.name, Args: c.args()}, true
}

// open is the call at key, new when asked to be (a start event) or when there is none.
func (s *CallStream) open(key string, fresh bool) *streamCall {
	if c := s.byKey[key]; c != nil && !fresh {
		return c
	}
	c := &streamCall{}
	if s.byKey == nil {
		s.byKey = map[string]*streamCall{}
	}
	if old := s.byKey[key]; old != nil {
		for i, x := range s.calls { // a block that starts again at an index replaces the first
			if x == old {
				s.calls[i] = c
			}
		}
	} else {
		s.calls = append(s.calls, c)
	}
	s.byKey[key] = c
	return c
}

// addChat reads one delta.tool_calls entry. The name comes once per call, in the chunk that opens
// it. A DIFFERENT name at the same index is a new call: some gateways give every call index 0. A
// gateway that sends the same name on every chunk of one call stays one call.
func (s *CallStream) addChat(tc gjson.Result) {
	key := "c" + tc.Get("index").String()
	c := s.byKey[key]
	name := tc.Get("function.name").String()
	if c == nil || (name != "" && c.name != "" && name != c.name) {
		c = &streamCall{}
		if s.byKey == nil {
			s.byKey = map[string]*streamCall{}
		}
		s.byKey[key] = c
		s.calls = append(s.calls, c)
	}
	if v := tc.Get("id").String(); v != "" {
		c.id = v
	}
	if name != "" {
		c.name = name
	}
	c.pieces.WriteString(tc.Get("function.arguments").String())
}

func (s *CallStream) addResponses(t string, ev gjson.Result) {
	switch t {
	case "response.output_item.added":
		if item := ev.Get("item"); isResponsesCall(item) {
			s.setItem(s.open("r"+ev.Get("output_index").String(), true), item, false)
		}
	case "response.output_item.done":
		if item := ev.Get("item"); isResponsesCall(item) {
			s.setItem(s.open("r"+ev.Get("output_index").String(), false), item, true)
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if c := s.byKey["r"+ev.Get("output_index").String()]; c != nil {
			c.pieces.WriteString(ev.Get("delta").String())
		}
	case "response.function_call_arguments.done":
		if c := s.byKey["r"+ev.Get("output_index").String()]; c != nil {
			if v := ev.Get("arguments").String(); v != "" { // an empty one must not erase the pieces
				c.full, c.hasFull = v, true
			}
		}
	case "response.custom_tool_call_input.done":
		if c := s.byKey["r"+ev.Get("output_index").String()]; c != nil {
			if v := ev.Get("input").String(); v != "" {
				c.full, c.hasFull = v, true
			}
		}
	case "response.completed", "response.incomplete":
		// The terminal event holds the whole output. It wins over the pieces.
		ev.Get("response.output").ForEach(func(i, item gjson.Result) bool {
			if isResponsesCall(item) {
				s.setItem(s.open("r"+strconv.Itoa(int(i.Int())), false), item, true)
			}
			return true
		})
	}
}

func isResponsesCall(item gjson.Result) bool {
	t := item.Get("type").String()
	return t == "function_call" || t == "custom_tool_call"
}

// setItem copies a Responses item into a call. closed says the item is final, so its arguments
// replace the pieces.
func (s *CallStream) setItem(c *streamCall, item gjson.Result, closed bool) {
	c.custom = item.Get("type").String() == "custom_tool_call"
	if v := item.Get("call_id").String(); v != "" {
		c.id = v
	}
	if v := item.Get("name").String(); v != "" {
		c.name = v
	}
	args := item.Get("arguments").String()
	if c.custom {
		args = item.Get("input").String()
	}
	switch {
	case closed && args != "":
		c.full, c.hasFull = args, true
	case !closed:
		c.start = args
	}
}
