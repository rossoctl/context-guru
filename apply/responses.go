package apply

// Native OpenAI Responses request adaptation. Responses input is deliberately
// not converted to a Chat Completions envelope: compatible items get a temporary
// component view and changed text is spliced back into its original wire field.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/skills"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/session"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type responseSlot struct {
	path   string
	text   string
	pre    []byte
	opaque bool // normalized placeholder; never write its text onto the wire
}

// The shared envelope transforms ask this adapter only where Responses keeps
// stable instructions, skill listings and outstanding tool calls.
type responsesEnvelopeAdapter struct{}

func (responsesEnvelopeAdapter) proseRegion(body []byte) string {
	var prose strings.Builder
	add := func(text string) {
		if text != "" {
			prose.WriteString(stableHalf(text))
			prose.WriteByte('\n')
		}
	}
	add(gjson.GetBytes(body, "instructions").String())
	// Leading system/developer input items are part of the stable prompt; do
	// not scan later user or assistant turns, whose prose changes each turn.
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		if role := item.Get("role").String(); role != "system" && role != "developer" {
			return false
		}
		content := item.Get("content")
		if content.Type == gjson.String {
			add(content.String())
		} else {
			content.ForEach(func(_, block gjson.Result) bool {
				add(block.Get("text").String())
				return true
			})
		}
		return true
	})
	return prose.String()
}

func (responsesEnvelopeAdapter) pendingCallFor(body []byte, names, servers map[string]bool) bool {
	pending := map[string]string{}
	unpaired := false
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "function_call", "custom_tool_call":
			name := item.Get("name").String()
			if id := item.Get("call_id").String(); id != "" {
				pending[id] = name
			} else if names[name] || (mcpServer(name) != "" && servers[mcpServer(name)]) {
				unpaired = true
			}
		case "function_call_output", "custom_tool_call_output":
			delete(pending, item.Get("call_id").String())
		}
		return true
	})
	if unpaired {
		return true
	}
	for _, name := range pending {
		if names[name] || (mcpServer(name) != "" && servers[mcpServer(name)]) {
			return true
		}
	}
	return false
}

func (responsesEnvelopeAdapter) skillListingPath(body []byte) (string, string) {
	if !bytes.Contains(body, []byte(skills.Header)) {
		return "", ""
	}
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Type == gjson.String && strings.Contains(instructions.String(), skills.Header) {
		return "instructions", instructions.String()
	}
	path, text := "", ""
	gjson.GetBytes(body, "input").ForEach(func(idx, item gjson.Result) bool {
		if role := item.Get("role").String(); role != "system" && role != "developer" {
			return true
		}
		content := item.Get("content")
		if content.Type == gjson.String && strings.Contains(content.String(), skills.Header) {
			path, text = "input."+idx.String()+".content", content.String()
			return false
		}
		content.ForEach(func(bidx, block gjson.Result) bool {
			v := block.Get("text")
			if v.Type == gjson.String && strings.Contains(v.String(), skills.Header) {
				path, text = "input."+idx.String()+".content."+bidx.String()+".text", v.String()
				return false
			}
			return true
		})
		return path == ""
	})
	return path, text
}

// responseInputIndex resolves a normalized slot to its containing wire item.
// Instructions are not input items and must never be part of a summary span.
func responseInputIndex(path string) (int, bool) {
	parts := strings.SplitN(path, ".", 3)
	if len(parts) < 2 || parts[0] != "input" {
		return 0, false
	}
	i, err := strconv.Atoi(parts[1])
	return i, err == nil && i >= 0
}

// A summary may remove a contiguous range of wholly represented wire items.
// The adapter stashes their original bytes, including opaque reasoning and
// complete call/output pairs. Refuse before the component runs if a span cuts
// through an item, contains unsupported multimodal data, or splits a call pair.
func responsesSummarySpanSafe(body []byte, slots []responseSlot, start, end int) bool {
	items := gjson.GetBytes(body, "input")
	if !items.IsArray() || start < 0 || end > len(slots) || end <= start {
		return false
	}
	first, ok := responseInputIndex(slots[start].path)
	if !ok {
		return false
	}
	last, ok := responseInputIndex(slots[end-1].path)
	if !ok || last < first || last >= len(items.Array()) {
		return false
	}
	covered := map[int]int{}
	for j, slot := range slots {
		idx, ok := responseInputIndex(slot.path)
		if !ok {
			continue // instructions
		}
		if idx >= first && idx <= last {
			if j < start || j >= end {
				return false // a boundary bisected a multi-block item
			}
			covered[idx]++
		} else if j >= start && j < end {
			return false // normalized order diverged from wire order
		}
	}
	representedBlocks := func(blocks []gjson.Result) int {
		if len(blocks) == 0 {
			return -1
		}
		for _, block := range blocks {
			if !block.IsObject() {
				return -1
			}
		}
		return len(blocks)
	}
	for i := first; i <= last; i++ {
		item := items.Array()[i]
		if covered[i] == 0 {
			return false
		}
		switch item.Get("type").String() {
		case "reasoning", "function_call", "custom_tool_call":
			if covered[i] != 1 {
				return false
			}
			continue // opaque bytes are stashed by the Responses adapter
		case "function_call_output", "custom_tool_call_output":
			output := item.Get("output")
			if output.Type == gjson.String && covered[i] == 1 {
				continue
			}
			if !output.IsArray() || representedBlocks(output.Array()) != covered[i] {
				return false
			}
			continue
		case "", "message":
		default:
			return false
		}
		if role := item.Get("role").String(); role != "user" && role != "assistant" && role != "system" && role != "developer" {
			return false
		}
		content := item.Get("content")
		if content.Type == gjson.String {
			if covered[i] != 1 {
				return false
			}
			continue
		}
		if !content.IsArray() || representedBlocks(content.Array()) != covered[i] {
			return false
		}
	}
	// Generic summarize aligns normalized call/result messages, but the wire
	// can also have a call whose result is outside the proposed span. Never
	// drop just one side of an exchange.
	calls, outputs := map[string]int{}, map[string]int{}
	for i, item := range items.Array() {
		id := item.Get("call_id").String()
		if id == "" {
			continue
		}
		switch item.Get("type").String() {
		case "function_call", "custom_tool_call":
			calls[id] = i
		case "function_call_output", "custom_tool_call_output":
			outputs[id] = i
		}
	}
	inside := func(i int) bool { return i >= first && i <= last }
	for id, i := range calls {
		j, ok := outputs[id]
		if inside(i) && (!ok || !inside(j)) || !inside(i) && ok && inside(j) {
			return false
		}
	}
	for id, i := range outputs {
		j, ok := calls[id]
		if inside(i) && (!ok || !inside(j)) || !inside(i) && ok && inside(j) {
			return false
		}
	}
	return true
}

func responsesSummaryStash(body []byte, slots []responseSlot, start, end int) ([]byte, error) {
	if !responsesSummarySpanSafe(body, slots, start, end) {
		return nil, fmt.Errorf("responses summary span is not wire-safe")
	}
	first, _ := responseInputIndex(slots[start].path)
	last, _ := responseInputIndex(slots[end-1].path)
	items := gjson.GetBytes(body, "input").Array()
	var b bytes.Buffer
	b.WriteByte('[')
	for i := first; i <= last; i++ {
		if i > first {
			b.WriteByte(',')
		}
		b.WriteString(items[i].Raw)
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// Rebuild a Responses input around the one summary a valid pipeline can emit
// (config rejects duplicate component names). A future count-changing
// component must extend this adapter with its own native wire representation.
// Preserve every opaque item
// outside the removed plain-text span and every retained item's original wire
// shape (including in-place text edits made by later components).
func rebuildResponsesCountChanged(body []byte, norm []bschemas.ChatMessage, slots []responseSlot, out []bschemas.ChatMessage) ([]byte, bool) {
	start := 0
	if len(slots) > 0 && slots[0].path == "instructions" {
		if len(out) == 0 {
			return body, false
		}
		b, _ := json.Marshal(out[0])
		if !bytes.Equal(b, slots[0].pre) {
			return body, false
		}
		start = 1
	}
	if len(out) > len(norm) || len(out) <= start {
		return body, false
	}
	// Only summarize currently changes count, and its wrapper identifies the
	// inserted message without relying on retained text staying unchanged.
	head := -1
	for i := start; i < len(out); i++ {
		text := schema.MessageText(out[i])
		if strings.HasPrefix(text, "=== History Summary ===") {
			if head >= 0 {
				return body, false
			}
			head = i
		}
	}
	if head < start || out[head].Role != bschemas.ChatMessageRoleUser {
		return body, false
	}
	tail := len(out) - head - 1
	spanEnd := len(slots) - tail
	if !responsesSummarySpanSafe(body, slots, head, spanEnd) {
		return body, false
	}
	items := gjson.GetBytes(body, "input").Array()
	first, _ := responseInputIndex(slots[head].path)
	last, _ := responseInputIndex(slots[spanEnd-1].path)
	edits := map[int][]byte{}
	writeSurvivor := func(outIdx, origIdx int) bool {
		if out[outIdx].Role != norm[origIdx].Role {
			return false
		}
		itemIdx, ok := responseInputIndex(slots[origIdx].path)
		if !ok || itemIdx >= len(items) || itemIdx >= first && itemIdx <= last {
			return false
		}
		if now := schema.MessageText(out[outIdx]); now != slots[origIdx].text {
			if slots[origIdx].opaque {
				return false
			}
			pathParts := strings.SplitN(slots[origIdx].path, ".", 3)
			if len(pathParts) != 3 {
				return false
			}
			item := edits[itemIdx]
			if item == nil {
				item = []byte(items[itemIdx].Raw)
			}
			var err error
			item, err = sjson.SetBytes(item, pathParts[2], now)
			if err != nil {
				return false
			}
			edits[itemIdx] = item
		}
		return true
	}
	for i := start; i < head; i++ {
		if !writeSurvivor(i, i) {
			return body, false
		}
	}
	for i := 0; i < tail; i++ {
		if !writeSurvivor(head+1+i, len(norm)-tail+i) {
			return body, false
		}
	}
	summary, err := json.Marshal(struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{"user", schema.MessageText(out[head])})
	if err != nil {
		return body, false
	}
	var parts [][]byte
	for i := range items {
		if i == first {
			parts = append(parts, summary)
		}
		if i >= first && i <= last {
			continue
		}
		item := edits[i]
		if item == nil {
			item = []byte(items[i].Raw)
		}
		parts = append(parts, item)
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, part := range parts {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(part)
	}
	buf.WriteByte(']')
	next, err := sjson.SetRawBytes(body, "input", buf.Bytes())
	return next, err == nil
}

func responseMessage(role string, text string) bschemas.ChatMessage {
	r := bschemas.ChatMessageRoleUser
	switch role {
	case "system", "developer":
		r = bschemas.ChatMessageRoleSystem
	case "assistant":
		r = bschemas.ChatMessageRoleAssistant
	case "tool":
		r = bschemas.ChatMessageRoleTool
	}
	m := bschemas.ChatMessage{Role: r}
	schema.SetMessageText(&m, text)
	return m
}

func normalizeResponses(body []byte) (out []bschemas.ChatMessage, slots []responseSlot) {
	if v := gjson.GetBytes(body, "instructions"); v.Type == gjson.String {
		m := responseMessage("system", v.String())
		pre, _ := json.Marshal(m)
		out, slots = append(out, m), append(slots, responseSlot{"instructions", v.String(), pre, false})
	}
	in := gjson.GetBytes(body, "input")
	if in.Type == gjson.String {
		m := responseMessage("user", in.String())
		pre, _ := json.Marshal(m)
		return append(out, m), append(slots, responseSlot{"input", in.String(), pre, false})
	}
	if !in.IsArray() {
		return out, slots
	}
	for i, item := range in.Array() {
		base := "input." + strconv.Itoa(i)
		typ := item.Get("type").String()
		if typ == "reasoning" {
			m := responseMessage("assistant", "")
			pre, _ := json.Marshal(m)
			out, slots = append(out, m), append(slots, responseSlot{base, "", pre, true})
			continue
		}
		if typ == "function_call" || typ == "custom_tool_call" {
			id, name := item.Get("call_id").String(), item.Get("name").String()
			fn := bschemas.ChatAssistantMessageToolCallFunction{Arguments: item.Get("arguments").Raw}
			if typ == "custom_tool_call" {
				fn.Arguments = item.Get("input").Raw
			}
			if name != "" {
				fn.Name = &name
			}
			call := bschemas.ChatAssistantMessageToolCall{Function: fn}
			if id != "" {
				call.ID = &id
			}
			m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
				ChatAssistantMessage: &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{call}}}
			pre, _ := json.Marshal(m)
			out, slots = append(out, m), append(slots, responseSlot{base, "", pre, true})
			continue
		}
		if typ == "function_call_output" || typ == "custom_tool_call_output" {
			output := item.Get("output")
			add := func(text, path string) {
				m := toolMessage(text, item.Get("call_id").String())
				pre, _ := json.Marshal(m)
				out, slots = append(out, m), append(slots, responseSlot{path, text, pre, false})
			}
			if output.Type == gjson.String {
				add(output.String(), base+".output")
			} else if output.IsArray() {
				for j, block := range output.Array() {
					path := base + ".output." + strconv.Itoa(j)
					if (block.Get("type").String() == "input_text" || block.Get("type").String() == "output_text") && block.Get("text").Type == gjson.String {
						add(block.Get("text").String(), base+".output."+strconv.Itoa(j)+".text")
					} else {
						text := "[non-text tool output block " + block.Get("type").String() + " preserved for expansion]"
						m := toolMessage(text, item.Get("call_id").String())
						pre, _ := json.Marshal(m)
						out, slots = append(out, m), append(slots, responseSlot{path: path, text: text, pre: pre, opaque: true})
					}
				}
			}
			continue
		}
		if typ != "message" && item.Get("role").String() == "" {
			continue // reasoning, function_call, item_reference: preserve, never reinterpret
		}
		role := item.Get("role").String()
		content := item.Get("content")
		if content.Type == gjson.String {
			m := responseMessage(role, content.String())
			pre, _ := json.Marshal(m)
			out, slots = append(out, m), append(slots, responseSlot{base + ".content", content.String(), pre, false})
			continue
		}
		if !content.IsArray() {
			continue
		}
		for j, block := range content.Array() {
			kind := block.Get("type").String()
			path := base + ".content." + strconv.Itoa(j)
			if (kind != "input_text" && kind != "output_text") || block.Get("text").Type != gjson.String {
				text := "[non-text " + kind + " block preserved for expansion]"
				m := responseMessage(role, text)
				pre, _ := json.Marshal(m)
				out, slots = append(out, m), append(slots, responseSlot{path: path, text: text, pre: pre, opaque: true})
				continue
			}
			m := responseMessage(role, block.Get("text").String())
			pre, _ := json.Marshal(m)
			out, slots = append(out, m), append(slots, responseSlot{path + ".text", block.Get("text").String(), pre, false})
		}
	}
	return out, slots
}

func bodyResponsesOpts(ctx context.Context, pipe *components.Pipeline, st store.Store, o Opts) Result {
	res := Result{Body: o.Body}
	res.Bypassed = o.Bypass
	client := o.Body // before the envelope rewrites; see BodyOpts
	// Envelope transforms must run before normalization: tools and instructions
	// are outside the component message view, and later text write-back uses
	// paths into this transformed body. Both transforms are deterministic and
	// preserve the original body on a parse or safety-check failure.
	var toolSchema bool
	var filteredTokens, filteredDecls int
	o.Body, toolSchema, filteredTokens, filteredDecls = transformEnvelope(o.Body, pipe, o.Bypass, responsesEnvelopeAdapter{})
	res.Body, res.Changed = o.Body, toolSchema || filteredDecls > 0
	res.FilteredDeclTokens, res.FilteredDecls = filteredTokens, filteredDecls
	norm, slots := normalizeResponses(o.Body)
	res.Messages = len(norm)
	if len(norm) == 0 {
		return res
	}
	sys, first := schema.SessionHead(norm)
	res.Session = session.Scoped(o.Tenant, explicitSession(o.Session, o.Body), sys, first)
	o.resolveThread(&res.Trace, res.Session)
	cacheAware := resolveCacheAware(o.CacheMode, bschemas.OpenAI, o.Body)
	// A previous_response_id request contains only this turn's delta, not a
	// cumulatively growing transcript. Boundary/TurnAt would interpret a smaller
	// delta as agent compaction and report a cached prefix we cannot see. Keep
	// cache awareness for provider pricing/lifetime, but leave the visible input
	// entirely eligible and its idle phase unknown.
	serverHeldHistory := gjson.GetBytes(o.Body, "previous_response_id").String() != ""
	maxCachedIdx, idleMs := -1, int64(-1)
	if cacheAware && !serverHeldHistory {
		nowMs := o.nowMs()
		if o.Tracker != nil {
			var prevAt int64
			maxCachedIdx, prevAt = o.Tracker.TurnAt(res.Session, len(norm), nowMs)
			maxCachedIdx--
			alias := session.Scoped(o.Tenant, "", sys, first)
			if at := aliasSeen(st, alias, nowMs); at > prevAt {
				prevAt = at
			}
			if prevAt > 0 && nowMs >= prevAt {
				idleMs = nowMs - prevAt
			}
		} else {
			maxCachedIdx = modes.Boundary(prevLen(st, res.Session), len(norm)) - 1
			defer putLen(st, res.Session, len(norm))
		}
	}
	res.CacheAware, res.MaxCachedIdx = cacheAware, maxCachedIdx
	if o.Bypass || pipe == nil {
		return res
	}
	mode := o.Mode
	if mode == "" {
		mode = components.ModeSync
	}
	ttl, ttlKind := CacheLifetime(bschemas.OpenAI, gjson.GetBytes(o.Body, "model").String(), o.Body)
	if !cacheAware {
		ttl, ttlKind = 0, CacheLifetimeUnknown
	}
	chat := &bschemas.BifrostChatRequest{Provider: bschemas.OpenAI, Input: append([]bschemas.ChatMessage(nil), norm...)}
	restore := planRestore(pipe, st, res.Session, "responses", client, gjson.GetBytes(client, "input").Array())
	c := &components.Ctx{Ctx: ctx, Session: res.Session, Thread: res.Thread.ID, Store: st, Model: o.Models,
		// OpenAI's 30m is a minimum, never proof of expiry. ColdCache stays
		// false even after a long idle gap, matching the Chat Completions path.
		// A previous_response_id points to upstream-held history that this
		// request cannot inspect or replace. Summarizing only the visible tail
		// would claim to compact a history that remains in the provider's state.
		DisallowCountChange: !gjson.GetBytes(o.Body, "input").IsArray() || serverHeldHistory,
		AllowSummarySpan: func(start, end int) bool {
			return responsesSummarySpanSafe(o.Body, slots, start, end)
		},
		SummaryStashPayload: func(start, end int, _ []bschemas.ChatMessage) ([]byte, error) {
			return responsesSummaryStash(o.Body, slots, start, end)
		},
		ToolSchema: toolSchema, FilteredDecls: filteredDecls,
		CtxWindow: o.Window, CtxWindowExact: o.WindowExact, CompactionPoint: o.CompactionPoint,
		CompactionPointSource: o.CompactionPointSource, ModelName: gjson.GetBytes(o.Body, "model").String(),
		Mode: mode, CacheAware: cacheAware, MaxCachedIdx: maxCachedIdx, IdleMs: idleMs,
		PrefixAsk:  o.PrefixAsk,
		CacheTTLMs: ttl.Milliseconds(), CacheTTLMinimum: ttlKind == CacheLifetimeMinimum,
		PrevBilledInput: prevBilledInput(st, res.Session), SelfRates: o.SelfRates, RatesFor: o.RatesFor,
		Restored: restore.restored()}
	res.Run = pipe.Run(chat, c)
	start := 0
	if cacheAware && maxCachedIdx >= 0 {
		start = min(maxCachedIdx+1, len(norm))
	}
	res.AttemptedTokens = schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: norm[start:]})
	res.FrozenTokens = schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: norm[:start]})
	if len(chat.Input) != len(norm) || summaryStructureChanged(norm, chat.Input) {
		if next, ok := rebuildResponsesCountChanged(o.Body, norm, slots, chat.Input); ok {
			res.Body, res.Changed = next, true
			res = finishRestore(res, restore, "responses", o.Body, gjson.GetBytes(o.Body, "input").Array())
		}
		return res
	}
	body := o.Body
	for i := range chat.Input {
		now, err := json.Marshal(chat.Input[i])
		if err != nil || bytes.Equal(now, slots[i].pre) {
			continue
		}
		text := schema.MessageText(chat.Input[i])
		if text == slots[i].text {
			continue
		}
		if slots[i].opaque {
			continue
		}
		next, err := sjson.SetBytes(body, slots[i].path, text)
		if err != nil {
			return Result{Body: o.Body, Trace: res.Trace}
		}
		body, res.Changed = next, true
	}
	res.Body = body
	return finishRestore(res, restore, "responses", o.Body, gjson.GetBytes(o.Body, "input").Array())
}
