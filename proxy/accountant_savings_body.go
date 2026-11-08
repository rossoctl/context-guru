package proxy

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/accountant"
)

// accBodies hashes a request body in the provider's order: tools, system, messages. A string
// system prompt is one block. Raw JSON is hashed as sent, except for the cache marks (see below), so two
// hosts hash the same bytes alike. Each element of messages is one position, so Position{messages, i} is
// the wire message index. A body without messages (or with invalid JSON) gives an empty Body.
//
// THE HASH LEAVES OUT cache_control, THE SIZE KEEPS IT. The provider's own cache key ignores the marks,
// and the client moves its marks: Claude Code puts one on the newest block every turn. If the mark were
// in the hash, the previous message would have new bytes on each request, the library would find no
// common prefix after the system block, and every token it saved earlier would be priced at the write rate
// although the provider read it (accountant#21). The size is the wire size, with the marks, because the
// library checks the operations' bytes against the bytes that really went upstream.
func accBodies(raw []byte) accountant.Body {
	body, _ := accParse(raw)
	return body
}

// accSections are the three top-level values accParse reads, found in one pass over the body. Like gjson.Get, the
// first key of a name wins.
type accSections struct {
	tools, system, messages gjson.Result
}

func accScan(raw []byte) accSections {
	var sec accSections
	var seen [3]bool
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
		default:
			return true
		}
		if !seen[i] {
			seen[i], *dst = true, v
		}
		return !(seen[0] && seen[1] && seen[2])
	})
	return sec
}

// accParse is accBodies and accBreakpoints on one parse of the body.
func accParse(raw []byte) (accountant.Body, []accountant.Breakpoint) {
	sec := accScan(raw)
	if !sec.messages.IsArray() {
		return accountant.NewBody(nil, nil, nil), nil
	}
	var tools, system, msgs [][]byte
	var sizes []int // the wire size of each element, in hash order
	add := func(dst *[][]byte, v gjson.Result) {
		*dst = append(*dst, withoutCacheMarks(raw, v))
		sizes = append(sizes, len(v.Raw))
	}
	sec.tools.ForEach(func(_, v gjson.Result) bool { add(&tools, v); return true })
	if sec.system.IsArray() {
		sec.system.ForEach(func(_, v gjson.Result) bool { add(&system, v); return true })
	} else if sec.system.Exists() {
		add(&system, sec.system)
	}
	sec.messages.ForEach(func(_, v gjson.Result) bool { add(&msgs, v); return true })
	built := accountant.NewBody(tools, system, msgs)
	for i := range built.Pos {
		built.Pos[i].Bytes = int32(sizes[i])
	}
	// NewBody also keeps a table of cumulative bytes, built from the mark-free input, which the library prefers to
	// summing Pos[i].Bytes. A Body without that table sums the positions, so every byte count the library reads
	// (the token ratio, the span of a splice, sawOriginal) is the wire size, as the operations' sizes are. A body
	// decoded from a snapshot has no table either, so a restart changes nothing. The table is unexported: build the
	// value again from the exported fields, which drops it (review of #893, finding 2).
	return accountant.Body{Start: built.Start, Pos: built.Pos}, accBreakpointsOf(sec)
}

// withoutCacheMarks is one element's JSON without any cache_control: the element's own, the ones on its content
// blocks, and the ones nested deeper, such as a mark inside a tool_result block's own content array (issue #917).
// An element with none is returned as it is, byte for byte, so the common case costs no allocation. The result is
// only hashed. It is never sent anywhere.
func withoutCacheMarks(body []byte, v gjson.Result) []byte {
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
		path := findCacheMark(gjson.Parse(out), "")
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

// findCacheMark returns the gjson path of the first cache_control key under v, or "" when there is none. It looks
// at objects and arrays only, and does not look inside a string.
func findCacheMark(v gjson.Result, prefix string) string {
	var found string
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch {
	case v.IsObject():
		v.ForEach(func(k, c gjson.Result) bool {
			if k.String() == "cache_control" {
				found = join(strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?").Replace(k.String()))
				return false
			}
			if c.IsObject() || c.IsArray() {
				ek := strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?", "|", "\\|", "#", "\\#", "@", "\\@").Replace(k.String())
				if found = findCacheMark(c, join(ek)); found != "" {
					return false
				}
			}
			return true
		})
	case v.IsArray():
		i := 0
		v.ForEach(func(_, c gjson.Result) bool {
			if c.IsObject() || c.IsArray() {
				if found = findCacheMark(c, join(strconv.Itoa(i))); found != "" {
					return false
				}
			}
			i++
			return true
		})
	}
	return found
}

// accBreakpoints lists the cache_control marks, as positions in the same space as accBodies, one per
// position. A mark inside a message's content blocks belongs to that message, so an element mark
// and a block mark (or two block marks) can fall on one position: it is listed once, with the
// longer TTL (1h over 5m), because the provider keeps the entry for the longest TTL asked for at
// it. ttl defaults to "5m".
func accBreakpoints(raw []byte) []accountant.Breakpoint {
	_, bps := accParse(raw)
	return bps
}

func accBreakpointsOf(sec accSections) []accountant.Breakpoint {
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
	return out
}
