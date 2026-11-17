package thinkcount

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// Limits on what one response may hold. Past them the response gives no count, and its blocks keep
// the accountant's fixed price. A signature is a few KB; these limits are far above any real response.
const (
	maxLine     = 4 << 20  // one SSE line
	maxOpaque   = 16 << 20 // the opaque strings of one response, together
	maxBlocks   = 4096     // the blocks of one response
	eventStream = "event-stream"
)

// FromBody reads one response: an Anthropic Messages JSON body, or its SSE stream when contentType is
// an event stream. Any other body gives an empty Response.
func FromBody(contentType string, body []byte) Response {
	if strings.Contains(contentType, eventStream) {
		var c Collector
		c.Write(body)
		return c.Result()
	}
	return FromJSON(body)
}

// FromJSON reads an Anthropic Messages response: the "signature" of each "thinking" block and the
// "data" of each "redacted_thinking" block in content, and usage.output_tokens_details.thinking_tokens.
func FromJSON(body []byte) Response {
	var r Response
	if !bytes.Contains(body, []byte(`thinking`)) {
		return r
	}
	v := gjson.ParseBytes(body)
	size := 0
	v.Get("content").ForEach(func(_, b gjson.Result) bool {
		if s := opaqueOf(b); s != "" {
			size += len(s)
			r.Opaques = append(r.Opaques, s)
		}
		return size <= maxOpaque && len(r.Opaques) <= maxBlocks
	})
	if size > maxOpaque || len(r.Opaques) > maxBlocks {
		return Response{}
	}
	r.Tokens, r.HasTokens = thinkingTokens(v.Get("usage"))
	return r
}

// opaqueOf is the opaque string of one content block, or "".
func opaqueOf(b gjson.Result) string {
	switch b.Get("type").String() {
	case "thinking":
		return b.Get("signature").String()
	case "redacted_thinking":
		return b.Get("data").String()
	}
	return ""
}

// thinkingTokens reads usage.output_tokens_details.thinking_tokens.
func thinkingTokens(u gjson.Result) (int, bool) {
	t := u.Get("output_tokens_details.thinking_tokens")
	if t.Type != gjson.Number || t.Int() < 0 {
		return 0, false
	}
	return int(t.Int()), true
}

// Collector reads an Anthropic SSE stream as it goes by, with no copy of the stream: Write takes each
// chunk as the proxy forwards it. It keeps the signature of each thinking block (from
// content_block_start and its signature_delta events) and the last thinking_tokens a message_delta
// or message_start reported. Its zero value is ready to use. It is not safe for concurrent use.
type Collector struct {
	line   []byte
	blocks map[int64]*strings.Builder // by content block index
	order  []int64
	size   int
	tokens int
	has    bool
	broken bool // a limit was passed: the stream gives no count
}

// Write reads the next bytes of the stream. It never fails.
func (c *Collector) Write(p []byte) {
	for len(p) > 0 && !c.broken {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			c.keep(p)
			return
		}
		c.keep(p[:i])
		if !c.broken {
			c.event(c.line)
		}
		c.line = c.line[:0]
		p = p[i+1:]
	}
}

// keep adds part of a line to the line buffer, within maxLine.
func (c *Collector) keep(p []byte) {
	if len(c.line)+len(p) > maxLine {
		c.broken = true
		return
	}
	c.line = append(c.line, p...)
}

// event reads one line of the stream. Only data lines of the four event types it needs are parsed; a
// text_delta line, most of a stream, costs one prefix test and four byte searches.
func (c *Collector) event(line []byte) {
	line = bytes.TrimRight(line, "\r")
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	switch {
	case bytes.Contains(data, []byte(`"signature_delta"`)):
		v := gjson.ParseBytes(data)
		if b := c.blocks[v.Get("index").Int()]; b != nil {
			c.add(b, v.Get("delta.signature").String())
		}
	case bytes.Contains(data, []byte(`"content_block_start"`)):
		v := gjson.ParseBytes(data)
		blk := v.Get("content_block")
		if t := blk.Get("type").String(); t != "thinking" && t != "redacted_thinking" {
			return
		}
		if c.blocks == nil {
			c.blocks = map[int64]*strings.Builder{}
		}
		idx := v.Get("index").Int()
		if _, seen := c.blocks[idx]; !seen {
			if len(c.order) >= maxBlocks {
				c.broken = true
				return
			}
			c.order = append(c.order, idx)
		}
		b := &strings.Builder{}
		c.blocks[idx] = b
		c.add(b, opaqueOf(blk))
	case bytes.Contains(data, []byte(`"message_delta"`)), bytes.Contains(data, []byte(`"message_start"`)):
		v := gjson.ParseBytes(data)
		u := v.Get("usage")
		if !u.Exists() {
			u = v.Get("message.usage")
		}
		if n, ok := thinkingTokens(u); ok {
			c.tokens, c.has = n, true
		}
	}
}

// add appends part of a signature, within maxOpaque.
func (c *Collector) add(b *strings.Builder, s string) {
	if c.size+len(s) > maxOpaque {
		c.broken = true
		return
	}
	c.size += len(s)
	b.WriteString(s)
}

// Result is what the stream said. A stream that passed a limit gives an empty Response. A line with no
// newline at the end of the stream is read too.
func (c *Collector) Result() Response {
	if len(c.line) > 0 && !c.broken {
		c.event(c.line)
		c.line = c.line[:0]
	}
	if c.broken {
		return Response{}
	}
	r := Response{Tokens: c.tokens, HasTokens: c.has}
	for _, idx := range c.order {
		if s := c.blocks[idx].String(); s != "" {
			r.Opaques = append(r.Opaques, s)
		}
	}
	return r
}
