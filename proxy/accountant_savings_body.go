package proxy

import (
	"github.com/tidwall/gjson"

	"github.com/rossoctl/accountant"
)

// accBodies hashes a request body in the provider's order: tools, system, messages. A string
// system prompt is one block. Raw JSON is hashed as sent, so two hosts hash the same bytes alike.
// Each element of messages is one position, so Position{messages, i} is the wire message index.
// A body without messages (or with invalid JSON) gives an empty Body.
func accBodies(raw []byte) accountant.Body {
	if !gjson.GetBytes(raw, "messages").IsArray() {
		return accountant.NewBody(nil, nil, nil)
	}
	var tools, system, msgs [][]byte
	gjson.GetBytes(raw, "tools").ForEach(func(_, v gjson.Result) bool {
		tools = append(tools, []byte(v.Raw))
		return true
	})
	if s := gjson.GetBytes(raw, "system"); s.IsArray() {
		s.ForEach(func(_, v gjson.Result) bool { system = append(system, []byte(v.Raw)); return true })
	} else if s.Exists() {
		system = append(system, []byte(s.Raw))
	}
	gjson.GetBytes(raw, "messages").ForEach(func(_, v gjson.Result) bool {
		msgs = append(msgs, []byte(v.Raw))
		return true
	})
	return accountant.NewBody(tools, system, msgs)
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
