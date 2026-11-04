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
	if !gjson.GetBytes(raw, "messages").IsArray() {
		return accountant.NewBody(nil, nil, nil)
	}
	var tools, system, msgs [][]byte
	var sizes []int // the wire size of each element, in hash order
	add := func(dst *[][]byte, v gjson.Result) {
		*dst = append(*dst, withoutCacheMarks(v.Raw))
		sizes = append(sizes, len(v.Raw))
	}
	gjson.GetBytes(raw, "tools").ForEach(func(_, v gjson.Result) bool { add(&tools, v); return true })
	if s := gjson.GetBytes(raw, "system"); s.IsArray() {
		s.ForEach(func(_, v gjson.Result) bool { add(&system, v); return true })
	} else if s.Exists() {
		add(&system, s)
	}
	gjson.GetBytes(raw, "messages").ForEach(func(_, v gjson.Result) bool { add(&msgs, v); return true })
	b := accountant.NewBody(tools, system, msgs)
	for i := range b.Pos {
		b.Pos[i].Bytes = int32(sizes[i])
	}
	return b
}

// withoutCacheMarks is one element's JSON without any cache_control: the element's own, and the ones on
// its content blocks. An element with none is returned as it is, byte for byte, so the common case costs
// no allocation. The result is only hashed. It is never sent anywhere.
func withoutCacheMarks(raw string) []byte {
	if !strings.Contains(raw, "cache_control") {
		return []byte(raw)
	}
	out := raw
	if gjson.Get(out, "cache_control").Exists() {
		if next, err := sjson.Delete(out, "cache_control"); err == nil {
			out = next
		}
	}
	if content := gjson.Get(out, "content"); content.IsArray() {
		i := 0
		content.ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("cache_control").Exists() {
				if next, err := sjson.Delete(out, "content."+strconv.Itoa(i)+".cache_control"); err == nil {
					out = next
				}
			}
			i++
			return true
		})
	}
	return []byte(out)
}

// accBreakpoints lists the cache_control marks, as positions in the same space as accBodies, one per
// position. A mark inside a message's content blocks belongs to that message, so an element mark
// and a block mark (or two block marks) can fall on one position: it is listed once, with the
// longer TTL (1h over 5m), because the provider keeps the entry for the longest TTL asked for at
// it. ttl defaults to "5m".
func accBreakpoints(raw []byte) []accountant.Breakpoint {
	if !gjson.GetBytes(raw, "messages").IsArray() {
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
	scan := func(sec accountant.Section, arr gjson.Result) {
		i := 0
		arr.ForEach(func(_, el gjson.Result) bool {
			if cc := el.Get("cache_control"); cc.Exists() {
				add(accountant.Position{Section: sec, Index: i}, ttl(cc))
			}
			if content := el.Get("content"); content.IsArray() {
				content.ForEach(func(_, blk gjson.Result) bool {
					if cc := blk.Get("cache_control"); cc.Exists() {
						add(accountant.Position{Section: sec, Index: i}, ttl(cc))
					}
					return true
				})
			}
			i++
			return true
		})
	}
	scan(accountant.SectionTools, gjson.GetBytes(raw, "tools"))
	if s := gjson.GetBytes(raw, "system"); s.IsArray() {
		scan(accountant.SectionSystem, s)
	}
	scan(accountant.SectionMessages, gjson.GetBytes(raw, "messages"))
	return out
}
