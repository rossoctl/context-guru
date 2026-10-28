package proxy

import (
	"github.com/tidwall/gjson"

	cf "github.com/rossoctl/context-guru/counterfactual"
)

// cfBodies hashes a request body in the provider's order: tools, system, messages. A string
// system prompt is one block. Raw JSON is hashed as sent, so two hosts hash the same bytes alike.
// Each element of messages is one position, so Position{messages, i} is the wire message index.
// A body without messages (or with invalid JSON) gives an empty Body.
func cfBodies(raw []byte) cf.Body {
	if !gjson.GetBytes(raw, "messages").IsArray() {
		return cf.NewBody(nil, nil, nil)
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
	return cf.NewBody(tools, system, msgs)
}

// cfBreakpoints lists every cache_control mark, as a position in the same space as cfBodies.
// A mark inside a message's content blocks belongs to that message. ttl defaults to "5m".
func cfBreakpoints(raw []byte) []cf.Breakpoint {
	if !gjson.GetBytes(raw, "messages").IsArray() {
		return nil
	}
	var out []cf.Breakpoint
	ttl := func(cc gjson.Result) string {
		if cc.Get("ttl").String() == "1h" {
			return "1h"
		}
		return "5m"
	}
	scan := func(sec cf.Section, arr gjson.Result) {
		i := 0
		arr.ForEach(func(_, el gjson.Result) bool {
			if cc := el.Get("cache_control"); cc.Exists() {
				out = append(out, cf.Breakpoint{Pos: cf.Position{Section: sec, Index: i}, TTL: ttl(cc)})
			}
			if content := el.Get("content"); content.IsArray() {
				content.ForEach(func(_, blk gjson.Result) bool {
					if cc := blk.Get("cache_control"); cc.Exists() {
						out = append(out, cf.Breakpoint{Pos: cf.Position{Section: sec, Index: i}, TTL: ttl(cc)})
					}
					return true
				})
			}
			i++
			return true
		})
	}
	scan(cf.SectionTools, gjson.GetBytes(raw, "tools"))
	if s := gjson.GetBytes(raw, "system"); s.IsArray() {
		scan(cf.SectionSystem, s)
	}
	scan(cf.SectionMessages, gjson.GetBytes(raw, "messages"))
	return out
}
