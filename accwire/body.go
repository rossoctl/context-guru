package accwire

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/accountant"
)

// body hashes a request body in the provider's order: tools, system, messages. A string
// system prompt is one block. The OpenAI Responses wire names the last two instructions and input: a body
// with no messages array is read that way, instructions as one block and each input item (or a string input)
// as one position, so Position{messages, i} is the input item index (#899). Raw JSON is hashed as sent, except for the cache marks (see below), so two
// hosts hash the same bytes alike. Each element of messages is one position, so Position{messages, i} is
// the wire message index. A body without messages (or with invalid JSON) gives an empty Body.
//
// THE HASH LEAVES OUT cache_control, THE SIZE KEEPS IT. The provider's own cache key ignores the marks,
// and the client moves its marks: Claude Code puts one on the newest block every turn. If the mark were
// in the hash, the previous message would have new bytes on each request, the library would find no
// common prefix after the system block, and every token it saved earlier would be priced at the write rate
// although the provider read it (accountant#21). The size is the wire size, with the marks, because the
// library checks the operations' bytes against the bytes that really went upstream.
func (k kind) body(raw []byte) accountant.Body {
	body, _ := k.parse(raw, nil)
	return body
}

// sections are the top-level values parse reads, found in one pass over the body. Like gjson.Get, the
// first key of a name wins. instructions and input are the Responses wire's system and messages; they are used only
// when the body has no messages array (responsesSections). auto is the body's top-level cache_control: Anthropic's
// automatic caching, which puts one breakpoint on the last cacheable block of the request (#1272).
type sections struct {
	tools, system, messages gjson.Result
	instructions, input     gjson.Result
	auto                    gjson.Result
}

// scan stops early only when tools, system, cache_control and a messages ARRAY are all seen: such a body has
// nothing left for responsesSections to change, since it acts only when messages is not an array. A messages value
// that is not an array (null, say) does not stop the scan, so an input after it is still read and the field order
// does not change the body (#1272).
func scanTop(raw []byte) sections {
	var sec sections
	var seen [6]bool
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return sec
	}
	root.ForEach(func(k, v gjson.Result) bool {
		var i int
		var dst *gjson.Result
		switch k.String() {
		case "tools":
			i, dst = 0, &sec.tools
		case "system":
			i, dst = 1, &sec.system
		case "messages":
			i, dst = 2, &sec.messages
		case "instructions":
			i, dst = 3, &sec.instructions
		case "input":
			i, dst = 4, &sec.input
		case "cache_control":
			i, dst = 5, &sec.auto
		default:
			return true
		}
		if !seen[i] {
			seen[i], *dst = true, v
		}
		return !(seen[0] && seen[1] && seen[2] && seen[5] && sec.messages.IsArray())
	})
	return sec
}

// scan reads the sections of a body the way this wire reads them. Only the Responses wire takes its
// system and messages from instructions and input.
func (k kind) scan(raw []byte) sections {
	sec := scanTop(raw)
	if k == responses {
		return sec.responsesSections()
	}
	return sec
}

// responsesSections reads a Responses body: with no messages array and an input (an array of items, or one
// string), instructions is the system section and input the messages. A string input is one position, so it is
// wrapped as a one-element array. A body with messages is returned as it is, whatever else it carries.
func (sec sections) responsesSections() sections {
	if sec.messages.IsArray() || !(sec.input.IsArray() || sec.input.Type == gjson.String) {
		return sec
	}
	if !sec.system.Exists() {
		sec.system = sec.instructions
	}
	sec.messages = sec.input
	if sec.input.Type == gjson.String {
		sec.messages = gjson.Parse("[" + sec.input.Raw + "]")
	}
	return sec
}

// parseCounted is parse with the token count of each thinking block the proxy saw a model make
// (thinkcount, accountant design D16). count is looked up by each signature's accountant.OpaqueKey; a nil
// count gives the fixed price to every block, as parse does. The hashes and sizes do not depend on it.
func (k kind) parse(raw []byte, count accountant.OpaqueCount) (accountant.Body, []accountant.Breakpoint) {
	sec := k.scan(raw)
	if !sec.messages.IsArray() {
		return accountant.NewBody(nil, nil, nil), nil
	}
	var tools, system, msgs [][]byte
	var sizes []int // the wire size of each element, in hash order
	add := func(dst *[][]byte, v gjson.Result) {
		*dst = append(*dst, WithoutCacheMarks(raw, v))
		sizes = append(sizes, len(v.Raw))
	}
	sec.tools.ForEach(func(_, v gjson.Result) bool { add(&tools, v); return true })
	if sec.system.IsArray() {
		sec.system.ForEach(func(_, v gjson.Result) bool { add(&system, v); return true })
	} else if sec.system.Exists() {
		add(&system, sec.system)
	}
	sec.messages.ForEach(func(_, v gjson.Result) bool { add(&msgs, v); return true })
	built := accountant.NewBodyCounted(tools, system, msgs, count)
	for i := range built.Pos {
		built.Pos[i].Bytes = int32(sizes[i])
	}
	// NewBody also keeps a table of cumulative bytes, built from the mark-free input, which the library prefers to
	// summing Pos[i].Bytes. A Body without that table sums the positions, so every byte count the library reads
	// (the token ratio, the span of a splice, sawOriginal) is the wire size, as the operations' sizes are. A body
	// decoded from a snapshot has no table either, so a restart changes nothing. The table is unexported: build the
	// value again from the exported fields, which drops it (review of #893, finding 2).
	return accountant.Body{Start: built.Start, Pos: built.Pos}, breakpointsOf(sec)
}

// WithoutCacheMarks is one element's JSON without its cache_control marks: the element's own, the ones on its
// content blocks, and the ones nested deeper in content, such as a mark inside a tool_result block's own content
// array (issue #917). A key named cache_control anywhere else is data, not a mark, and stays (see FindCacheMark).
// An element with none is returned as it is, byte for byte, so the common case costs no allocation. The result is
// only hashed. It is never sent anywhere.
func WithoutCacheMarks(body []byte, v gjson.Result) []byte {
	raw := v.Raw
	if !strings.Contains(raw, "cache_control") {
		// Slice the original bytes when Index points at this very element, else copy. The result is only read.
		if i := v.Index; i > 0 && i+len(raw) <= len(body) && string(body[i:i+len(raw)]) == raw {
			return body[i : i+len(raw) : i+len(raw)]
		}
		return []byte(raw)
	}
	out := raw
	// Each delete removes one mark and the walk starts again, so the paths stay right after the edit. A body has
	// at most four marks on the wire, so the loop is short.
	for n := 0; n < 64; n++ {
		path := FindCacheMark(gjson.Parse(out), "")
		if path == "" {
			break
		}
		next, err := sjson.Delete(out, path)
		if err != nil {
			break
		}
		out = next
	}
	return []byte(out)
}

// FindCacheMark returns the gjson path of the first cache-control mark under v, or "" when there is none. A mark
// is a cache_control key on v itself or on a block of a content array, at any depth of content nesting (a
// tool_result block's own content). It goes down only through "content" arrays, so a cache_control key anywhere
// else stays in the hash: a tool's input_schema.properties.cache_control parameter, or a tool_use block's input
// value, is part of what the provider caches, and a change to it must change the hash (#1272). It never looks
// inside a string.
func FindCacheMark(v gjson.Result, prefix string) string {
	if !v.IsObject() {
		return ""
	}
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	var found string
	v.ForEach(func(k, c gjson.Result) bool {
		switch k.String() {
		case "cache_control":
			found = join("cache_control")
		case "content":
			if c.IsArray() {
				i := 0
				c.ForEach(func(_, blk gjson.Result) bool {
					found = FindCacheMark(blk, join("content."+strconv.Itoa(i)))
					i++
					return found == ""
				})
			}
		}
		return found == ""
	})
	return found
}

// breakpoints lists the cache_control marks, as positions in the same space as body, one per
// position. A mark inside a message's content blocks belongs to that message, so an element mark
// and a block mark (or two block marks) can fall on one position: it is listed once, with the
// longer TTL (1h over 5m), because the provider keeps the entry for the longest TTL asked for at
// it. ttl defaults to "5m".
//
// A top-level cache_control (automatic caching) adds one breakpoint at the body's last position, which holds the
// request's last cacheable block. When an explicit mark already names that position it is not listed twice, and
// the longer TTL wins. The provider walks back past a last block it cannot cache; this does not, because a
// position here is a whole message, not one block of it (#1272).
func (k kind) breakpoints(raw []byte) []accountant.Breakpoint {
	_, bps := k.parse(raw, nil)
	return bps
}

func breakpointsOf(sec sections) []accountant.Breakpoint {
	if !sec.messages.IsArray() {
		return nil
	}
	var out []accountant.Breakpoint
	at := map[accountant.Position]int{} // position -> index in out
	add := func(p accountant.Position, ttl string) {
		if i, ok := at[p]; ok {
			if ttl == "1h" {
				out[i].TTL = "1h"
			}
			return
		}
		at[p] = len(out)
		out = append(out, accountant.Breakpoint{Pos: p, TTL: ttl})
	}
	ttl := func(cc gjson.Result) string {
		if cc.Get("ttl").String() == "1h" {
			return "1h"
		}
		return "5m"
	}
	// mark finds the first cache_control and the first content of an object in one pass, as Get does for each key.
	mark := func(el gjson.Result) (cc, content gjson.Result) {
		if !el.IsObject() {
			return
		}
		var gotCC, gotContent bool
		el.ForEach(func(k, v gjson.Result) bool {
			switch k.String() {
			case "cache_control":
				if !gotCC {
					gotCC, cc = true, v
				}
			case "content":
				if !gotContent {
					gotContent, content = true, v
				}
			}
			return !(gotCC && gotContent)
		})
		return
	}
	scan := func(sec accountant.Section, arr gjson.Result) {
		i := 0
		arr.ForEach(func(_, el gjson.Result) bool {
			cc, content := mark(el)
			if cc.Exists() {
				add(accountant.Position{Section: sec, Index: i}, ttl(cc))
			}
			if content.IsArray() {
				content.ForEach(func(_, blk gjson.Result) bool {
					if cc, _ := mark(blk); cc.Exists() {
						add(accountant.Position{Section: sec, Index: i}, ttl(cc))
					}
					return true
				})
			}
			i++
			return true
		})
	}
	scan(accountant.SectionTools, sec.tools)
	if sec.system.IsArray() {
		scan(accountant.SectionSystem, sec.system)
	}
	scan(accountant.SectionMessages, sec.messages)
	if sec.auto.IsObject() {
		if p, ok := lastPosition(sec); ok {
			add(p, ttl(sec.auto))
		}
	}
	return out
}

// lastPosition is the last position of the body parse builds from sec: the last message, else the last
// system position, else the last tool. ok is false when the body has no positions.
func lastPosition(sec sections) (accountant.Position, bool) {
	count := func(v gjson.Result) int {
		if v.IsArray() {
			return len(v.Array())
		}
		return 0
	}
	if n := count(sec.messages); n > 0 {
		return accountant.Position{Section: accountant.SectionMessages, Index: n - 1}, true
	}
	n := count(sec.system)
	if !sec.system.IsArray() && sec.system.Exists() {
		n = 1
	}
	if n > 0 {
		return accountant.Position{Section: accountant.SectionSystem, Index: n - 1}, true
	}
	if n := count(sec.tools); n > 0 {
		return accountant.Position{Section: accountant.SectionTools, Index: n - 1}, true
	}
	return accountant.Position{}, false
}
