package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/rossoctl/accountant"
)

// TestAccParseMatchesReference holds accParse to the implementation it replaced (kept below as oldAccBodies and
// oldAccBreakpoints): the same Body (Start, every Pos hash and Bytes) and the same breakpoints.
func TestAccParseMatchesReference(t *testing.T) {
	big := strings.Repeat("x", 2000)
	cases := map[string]string{
		"empty":            ``,
		"invalid":          `{"messages":[{"role":"user"`,
		"not object":       `[1,2,3]`,
		"no messages":      `{"system":"s","tools":[{"name":"a"}]}`,
		"messages object":  `{"messages":{"a":1}}`,
		"string system":    `{"system":"be nice","messages":[{"role":"user","content":"hi"}]}`,
		"array system":     `{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"hi"}]}`,
		"tools":            `{"tools":[{"name":"a","cache_control":{"type":"ephemeral"}},{"name":"b"}],"system":"s","messages":[{"role":"user","content":"hi"}]}`,
		"msg mark":         `{"messages":[{"role":"user","content":"hi","cache_control":{"type":"ephemeral"}},{"role":"assistant","content":"yo"}]}`,
		"block mark":       `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]}]}`,
		"nested tool_res":  `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":[{"type":"text","text":"r","cache_control":{"type":"ephemeral"}}]}]}]}`,
		"mark in string":   `{"messages":[{"role":"user","content":"cache_control in text"}]}`,
		"messages first":   `{"messages":[{"role":"user","content":"` + big + `"}],"system":"s","tools":[{"name":"t"}]}`,
		"duplicate keys":   `{"messages":[{"role":"user","content":"a"}],"messages":[{"role":"user","content":"b"}],"system":"x","system":"y"}`,
		"whitespace":       " \n{ \"messages\" : [ { \"role\" : \"user\" } ] , \"system\" : [ \"s\" ] }",
		"escaped key text": `{"messages":[{"role":"user","content":"a\"messages\":[]"}],"nested":{"messages":[1]}}`,
		"nested only":      `{"x":{"messages":[{"role":"user"}]}}`,
	}
	cases["bench"] = string(benchBody())
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte(in)
			wb, wbp := oldAccBodies(raw), oldAccBreakpoints(raw)
			gb, gbp := accParse(raw)
			if gb.Start != wb.Start || fmt.Sprint(gb.Pos) != fmt.Sprint(wb.Pos) {
				t.Fatalf("body\n got %+v\nwant %+v", gb, wb)
			}
			if fmt.Sprint(gbp) != fmt.Sprint(wbp) || (gbp == nil) != (wbp == nil) {
				t.Fatalf("breakpoints got %+v want %+v", gbp, wbp)
			}
			if g := accBodies(raw); fmt.Sprint(g.Pos) != fmt.Sprint(wb.Pos) {
				t.Fatalf("accBodies wrapper differs")
			}
			if g := accBreakpoints(raw); fmt.Sprint(g) != fmt.Sprint(wbp) {
				t.Fatalf("accBreakpoints wrapper differs")
			}
		})
	}
}

// oldAccBodies is the reference copy.
func oldAccBodies(raw []byte) accountant.Body {
	if !gjson.GetBytes(raw, "messages").IsArray() {
		return accountant.NewBody(nil, nil, nil)
	}
	var tools, system, msgs [][]byte
	var sizes []int // the wire size of each element, in hash order
	add := func(dst *[][]byte, v gjson.Result) {
		*dst = append(*dst, oldWithoutCacheMarks(v.Raw))
		sizes = append(sizes, len(v.Raw))
	}
	gjson.GetBytes(raw, "tools").ForEach(func(_, v gjson.Result) bool { add(&tools, v); return true })
	if s := gjson.GetBytes(raw, "system"); s.IsArray() {
		s.ForEach(func(_, v gjson.Result) bool { add(&system, v); return true })
	} else if s.Exists() {
		add(&system, s)
	}
	gjson.GetBytes(raw, "messages").ForEach(func(_, v gjson.Result) bool { add(&msgs, v); return true })
	built := accountant.NewBody(tools, system, msgs)
	for i := range built.Pos {
		built.Pos[i].Bytes = int32(sizes[i])
	}
	// NewBody also keeps a table of cumulative bytes, built from the mark-free input, which the library prefers to
	// summing Pos[i].Bytes. A Body without that table sums the positions, so every byte count the library reads
	// (the token ratio, the span of a splice, sawOriginal) is the wire size, as the operations' sizes are. A body
	// decoded from a snapshot has no table either, so a restart changes nothing. The table is unexported: build the
	// value again from the exported fields, which drops it (review of #893, finding 2).
	return accountant.Body{Start: built.Start, Pos: built.Pos}
}

// withoutCacheMarks is one element's JSON without any cache_control: the element's own, the ones on its content
// blocks, and the ones nested deeper, such as a mark inside a tool_result block's own content array (issue #917).
// An element with none is returned as it is, byte for byte, so the common case costs no allocation. The result is
// only hashed. It is never sent anywhere.
func oldWithoutCacheMarks(raw string) []byte {
	if !strings.Contains(raw, "cache_control") {
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

// accBreakpoints lists the cache_control marks, as positions in the same space as accBodies, one per
// position. A mark inside a message's content blocks belongs to that message, so an element mark
// and a block mark (or two block marks) can fall on one position: it is listed once, with the
// longer TTL (1h over 5m), because the provider keeps the entry for the longest TTL asked for at
// it. ttl defaults to "5m".
func oldAccBreakpoints(raw []byte) []accountant.Breakpoint {
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
