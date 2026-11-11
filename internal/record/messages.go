package record

// Anthropic wire format -> the corpus's message shape.
//
// WHY THIS FILE EXISTS, and it is the thing a schema test cannot tell you: the corpus does
// NOT store Anthropic's request/response JSON. It stores the OTel GenAI semconv message
// shape, which claude-record produces by translating. Measured over the existing 67 files:
//
//	output messages : 5,154, every one exactly {role, finish_reason, parts}, role=assistant
//	input messages  : 240,591, {role, parts} for system|user|assistant|tool
//	                  plus 1,952 system messages in the {role, content} string form
//	part types      : text (136,736) | tool_call (83,763) | tool_call_response (82,677)
//	tool_call       : {type, id, name, arguments} -- arguments a JSON STRING, 4,447/4,447
//	tool_call_resp  : {type, id, response}        -- response  a string, 50,666/50,666
//
// A recorder that wrote the raw Anthropic bodies instead would satisfy every attribute-level
// schema check (the value is a JSON string either way) and still be unreadable: the analysis
// side walks m["parts"] and matches type=="tool_call", so it would see zero assistant text
// and zero tool calls on every span while reporting no error at all. That is precisely the
// silently-empty-column failure the golden test was written to prevent, one level deeper
// than the golden test could see.
//
// The `arguments`/`response` fields are strings and not objects ON PURPOSE -- that is what
// the corpus has, and the scripts json.loads them a second time. Emitting an object there
// would break them.
//
// THIS IS A PORT OF A KNOWN SPEC, not reverse-engineering. The rules below mirror
// claude-record's converter.py (convert_messages_to_otel / convert_response_to_otel /
// build_llm_attributes) function by function, because THAT is the definition of the corpus
// format and any difference is a divergence rather than an improvement. Where its behaviour
// is surprising, it is reproduced anyway and the surprise is noted. Deviating "sensibly"
// here would produce files that look right and do not compare.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Part is one piece of a message: text, a tool call, or a tool result.
//
// Fields are omitempty so each part serialises to exactly the key set the corpus uses for
// its type -- a text part has no `id`, a tool_call has no `response`. An always-present
// empty key would be a new shape for every consumer to skip.
type Part struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`   // text
	ID        string `json:"id,omitempty"`        // tool_call, tool_call_response
	Name      string `json:"name,omitempty"`      // tool_call
	Arguments string `json:"arguments,omitempty"` // tool_call: JSON, as a STRING
	Response  string `json:"response,omitempty"`  // tool_call_response: a STRING
}

// Part type values, spelled as the corpus spells them. Note tool_call, NOT Anthropic's
// tool_use: the translation below is the whole point of this file.
const (
	PartText         = "text"
	PartToolCall     = "tool_call"
	PartToolResponse = "tool_call_response"
)

// Message is one entry in gen_ai.input.messages / gen_ai.output.messages.
//
// Content and Parts are both present because the corpus uses both: 1,952 system messages
// carry a plain `content` string rather than parts. Round-tripping that difference keeps a
// recorded file byte-comparable with a collected one instead of merely equivalent.
type Message struct {
	Role         string `json:"role"`
	FinishReason string `json:"finish_reason,omitempty"` // output messages only
	Content      string `json:"content,omitempty"`       // the string-form system message
	Parts        []Part `json:"parts,omitempty"`
}

// --- Anthropic wire shapes, only the fields needed to translate. ---

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type anthropicMessage struct {
	Role string `json:"role"`
	// A message's content is either a plain string or a list of blocks. Anthropic permits
	// both and Claude Code emits both, so this is decoded as raw and dispatched on the first
	// byte rather than guessing.
	Content json.RawMessage `json:"content"`
}

type anthropicRequest struct {
	Model    string             `json:"model"`
	System   json.RawMessage    `json:"system"` // string or list of blocks
	Messages []anthropicMessage `json:"messages"`
	Tools    json.RawMessage    `json:"tools"`
	Stream   bool               `json:"stream"`

	MaxTokens   int      `json:"max_tokens"`
	Temperature *float64 `json:"temperature"` // pointer: 0 and absent are different requests

	// output_config is where the effort and the structured-output format live. Anthropic has
	// no top-level `effort` field, which is why an earlier guess at `thinking` was wrong.
	OutputConfig *struct {
		Effort string `json:"effort"`
		Format *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
}

type anthropicResponse struct {
	Role       string           `json:"role"`
	StopReason string           `json:"stop_reason"`
	Content    []anthropicBlock `json:"content"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// InputMessages translates an Anthropic request body into the corpus's input-message list.
//
// The system prompt becomes a leading role=system message, which is how the corpus carries
// it: there is no separate system attribute, and an analysis asking "was a system prompt
// present" looks for that message.
func InputMessages(requestBody []byte) ([]Message, error) {
	var req anthropicRequest
	if err := json.Unmarshal(requestBody, &req); err != nil {
		return nil, fmt.Errorf("record: parse request body: %w", err)
	}
	msgs := make([]Message, 0, len(req.Messages)+1)

	if sys, ok := systemMessage(req.System); ok {
		msgs = append(msgs, sys)
	}
	for _, m := range req.Messages {
		msgs = append(msgs, convertMessage(m)...)
	}
	return msgs, nil
}

// convertMessage mirrors converter.py's convert_messages_to_otel, whose per-role behaviour is
// not uniform and must not be "tidied":
//
//   - A user message with a LIST content becomes ONE MESSAGE PER BLOCK, not one message with
//     several parts -- and a tool_result block becomes a message with role "tool", not "user".
//     That is why the corpus has 82,677 role=tool messages: Anthropic has no tool role, so
//     every one of them was synthesised here.
//   - An assistant message becomes exactly one message carrying all its parts.
//   - Any other role falls through to a single text part built from the stringified content.
//
// So one Anthropic message can yield zero, one, or many corpus messages. A recorder that
// emitted one-for-one would produce a different message count on every turn that carries
// tool results, which is most of them.
func convertMessage(m anthropicMessage) []Message {
	switch m.Role {
	case "user":
		if s, ok := asString(m.Content); ok {
			if s == "" {
				return nil
			}
			return []Message{{Role: "user", Parts: []Part{{Type: PartText, Content: s}}}}
		}
		var out []Message
		for _, b := range decodeBlocks(m.Content) {
			switch b.Type {
			case "text":
				// Emitted even when empty: converter.py appends unconditionally here, and the
				// message count is something analyses compare.
				out = append(out, Message{Role: "user",
					Parts: []Part{{Type: PartText, Content: b.Text}}})
			case "tool_result":
				out = append(out, Message{Role: "tool",
					Parts: []Part{{Type: PartToolResponse, ID: b.ToolUseID,
						Response: toolResultText(b.Content)}}})
			}
		}
		return out

	case "assistant":
		var parts []Part
		if s, ok := asString(m.Content); ok {
			if s != "" {
				parts = append(parts, Part{Type: PartText, Content: s})
			}
		} else {
			for _, b := range decodeBlocks(m.Content) {
				switch b.Type {
				case "text":
					parts = append(parts, Part{Type: PartText, Content: b.Text})
				case "tool_use":
					parts = append(parts, Part{Type: PartToolCall, ID: b.ID,
						Name: b.Name, Arguments: jsonArgs(b.Input)})
				}
			}
		}
		// An assistant message is emitted even with no parts, matching converter.py. A
		// thinking-only turn therefore appears as an empty-parts assistant message rather
		// than vanishing, which is the honest record of what was sent.
		return []Message{{Role: "assistant", Parts: parts}}

	default:
		if s, ok := asString(m.Content); ok {
			if s == "" {
				return nil
			}
			return []Message{{Role: m.Role, Parts: []Part{{Type: PartText, Content: s}}}}
		}
		if len(m.Content) == 0 || string(m.Content) == "null" {
			return nil
		}
		return []Message{{Role: m.Role,
			Parts: []Part{{Type: PartText, Content: string(m.Content)}}}}
	}
}

// asString reports whether raw is a JSON string, and its value.
func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func decodeBlocks(raw json.RawMessage) []anthropicBlock {
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

// jsonArgs renders a tool call's input as the JSON STRING the corpus stores. An absent input
// becomes "{}" because converter.py passes block.get("input", {}) through json.dumps.
func jsonArgs(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

// systemMessage renders the Anthropic `system` field, which is a string in some requests and
// a list of cacheable blocks in others.
//
// BOTH forms collapse to the {role, content} STRING shape, with block texts joined by "\n".
// This is the one place where matching the collector matters most and where the obvious
// implementation is wrong: a block list looks like it should become parts, and converter.py
// joins it into a single content string instead. That is why all 1,952 system messages in the
// corpus are the {role, content} form and not one of them uses parts. Emitting parts here
// would break every analysis that reads the system prompt as a string.
func systemMessage(raw json.RawMessage) (Message, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return Message{}, false
	}
	if str, ok := asString(raw); ok {
		if str == "" {
			return Message{}, false
		}
		return Message{Role: "system", Content: str}, true
	}
	var texts []string
	for _, b := range decodeBlocks(raw) {
		if b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return Message{}, false
	}
	return Message{Role: "system", Content: strings.Join(texts, "\n")}, true
}

// toolResultText flattens a tool result to the single string the corpus stores. Anthropic
// allows a string or a block list here; the corpus has a string in all 50,666 cases.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Not a shape we know. Keeping the raw JSON is better than dropping the result: it
		// stays greppable and a reader can see what arrived.
		return string(raw)
	}
	out := ""
	for _, b := range blocks {
		if b.Type == "text" {
			if out != "" {
				out += "\n"
			}
			out += b.Text
		}
	}
	return out
}

// OutputMessages translates a non-streaming Anthropic response into the corpus output-message
// list: always exactly one assistant message, mirroring convert_response_to_otel.
//
// Two details are copied rather than improved on. The finish_reason falls back to "stop" when
// the response carries no stop_reason (the corpus default, not part of the Anthropic
// vocabulary), and role is the literal "assistant" rather than whatever the body claims --
// converter.py hardcodes it, and a response claiming another role would otherwise produce a
// row no analysis expects.
func OutputMessages(responseBody []byte) ([]Message, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(responseBody, &resp); err != nil {
		return nil, fmt.Errorf("record: parse response body: %w", err)
	}
	finish := resp.StopReason
	if finish == "" {
		finish = "stop"
	}
	parts := make([]Part, 0, len(resp.Content))
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			parts = append(parts, Part{Type: PartText, Content: b.Text})
		case "tool_use":
			parts = append(parts, Part{Type: PartToolCall, ID: b.ID,
				Name: b.Name, Arguments: jsonArgs(b.Input)})
		}
		// thinking / redacted_thinking are skipped, as converter.py skips them: the corpus
		// part vocabulary is text | tool_call | tool_call_response, and adding a fourth type
		// would be a schema change no consumer knows about.
	}
	return []Message{{Role: "assistant", FinishReason: finish, Parts: parts}}, nil
}

// MarshalMessages renders a message list as the attribute value: a JSON string, because that
// is what the corpus stores and what the scripts json.loads.
func MarshalMessages(msgs []Message) (string, error) {
	b, err := json.Marshal(msgs)
	if err != nil {
		return "", fmt.Errorf("record: marshal messages: %w", err)
	}
	return string(b), nil
}

// ToolDefinitions renders gen_ai.tool.definitions, present on 5,037 of the 5,680 corpus chat
// spans and the thing that makes a tool_call interpretable: the call names a tool whose
// parameter schema lives only here.
//
// The array is RESHAPED, not passed through. converter.py rewrites each Anthropic tool
// {name, description, input_schema} into {type:"function", name, description, parameters}, so
// the corpus stores the OpenAI-flavoured spelling. Forwarding the Anthropic shape would put
// `input_schema` where every consumer reads `parameters`.
func ToolDefinitions(requestBody []byte) (string, error) {
	var req anthropicRequest
	if err := json.Unmarshal(requestBody, &req); err != nil {
		return "", fmt.Errorf("record: parse request body: %w", err)
	}
	if len(req.Tools) == 0 || string(req.Tools) == "null" {
		return "", nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(req.Tools, &tools); err != nil {
		return "", fmt.Errorf("record: parse tools: %w", err)
	}
	// An EMPTY tools array counts as absent, because converter.py gates on `if tools:` and a
	// Python empty list is falsy -- so it writes no gen_ai.tool.definitions at all. 34 of the
	// 183 measured request bodies send `tools: []`, and emitting "[]" for them would put an
	// attribute on 183 spans where the collector puts one on 149. Found by the differential
	// test; it is exactly the kind of difference no schema check can see.
	if len(tools) == 0 {
		return "", nil
	}
	defs := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		params := t.InputSchema
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage("{}")
		}
		defs = append(defs, toolDef{Type: "function", Name: t.Name,
			Description: t.Description, Parameters: params})
	}
	b, err := json.Marshal(defs)
	if err != nil {
		return "", fmt.Errorf("record: marshal tool definitions: %w", err)
	}
	return string(b), nil
}

// toolDef is the corpus tool-definition shape. Description has no omitempty: converter.py
// writes it unconditionally via tool.get("description", ""), so a tool without one still has
// the key.
type toolDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// UsageFromResponse pulls the token split out of a response body. This is the mooncake
// unblock arriving in one place: Anthropic reports cache_read_input_tokens and
// cache_creation_input_tokens on every response, and `input_tokens` is the FRESH prefix only
// -- the under-reporting that claude-record inherits as issue #17.
func UsageFromResponse(responseBody []byte) (fresh, cacheRead, cacheWrite, output int, stopReason string, err error) {
	var resp anthropicResponse
	if e := json.Unmarshal(responseBody, &resp); e != nil {
		return 0, 0, 0, 0, "", fmt.Errorf("record: parse response usage: %w", e)
	}
	u := resp.Usage
	return u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, u.OutputTokens, resp.StopReason, nil
}

// RequestMeta pulls the request-side span attributes that are not messages.
//
// effort comes from req.output_config.effort VERBATIM -- it is not derived. An earlier version
// of this function inferred it from the thinking budget on three tiers (low/medium/high),
// which was wrong in a way worth recording: the corpus contains only "high" (4,965) and
// "xhigh" (698), max_tokens is a constant 64000 on every span and so carries no signal, and
// "low"/"medium" appear nowhere at all. A derived value would have invented data the corpus
// has never held. Claude Code sends the effort it was configured with; this passes it through.
//
// OutputType mirrors the same block: "json" when output_config.format is present, "text" when
// output_config exists without one, and EMPTY when there is no output_config at all -- the
// three-way distinction converter.py makes, where absent means "this request did not say".
func RequestMeta(requestBody []byte) (Meta, error) {
	var req anthropicRequest
	if err := json.Unmarshal(requestBody, &req); err != nil {
		return Meta{}, fmt.Errorf("record: parse request meta: %w", err)
	}
	meta := Meta{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
		Temperature: req.Temperature,
	}
	if req.OutputConfig != nil {
		meta.Effort = req.OutputConfig.Effort
		if req.OutputConfig.Format != nil {
			meta.OutputType = "json"
			if len(req.OutputConfig.Format.Schema) > 0 &&
				string(req.OutputConfig.Format.Schema) != "null" {
				meta.OutputSchema = string(req.OutputConfig.Format.Schema)
			}
		} else {
			meta.OutputType = "text"
		}
	}
	return meta, nil
}

// Meta is the non-message request side of a span. A struct rather than six return values
// because the list was already at the length where callers start transposing arguments.
type Meta struct {
	Model        string
	MaxTokens    int
	Temperature  *float64
	Effort       string
	OutputType   string
	OutputSchema string
	Stream       bool
}
