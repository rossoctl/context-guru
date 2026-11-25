package schema

import "github.com/tidwall/gjson"

// The wire dialects a request body can be in. The provider alone does not say: OpenAI speaks two
// (Chat Completions and Responses), and they differ in where the transcript lives (`messages` or
// `input`), how a tool is declared and how a tool call is written.
const (
	WireAnthropic = "anthropic"
	WireOpenAI    = "openai" // Chat Completions
	WireResponses = "responses"
)

// WireFor returns the wire dialect of a request. This is the one place that decides it; every
// host and library package asks here instead of testing the route or the API name itself.
//
// api is the API name a route carries ("responses", or "" for the provider's default) or the
// upstream path ("/v1/responses"). Both name the Responses dialect.
func WireFor(provider, api string) string {
	if api == "responses" || api == "/v1/responses" {
		return WireResponses
	}
	return provider
}

// IsResponses reports whether wire is the Responses dialect.
func IsResponses(wire string) bool { return wire == WireResponses }

// ServerHeldHistory says whether an OpenAI Responses body leaves part of its prompt on the provider's side, so
// the host cannot see the prefix the provider reads. It is true for:
//
//   - previous_response_id (the earlier turns of a response chain);
//   - conversation (a stored conversation's items, a string id or an object);
//   - an input item of type item_reference (an item the provider expands from its own store).
//
// The accountant, apply and expand all use this one rule.
func ServerHeldHistory(body []byte) bool {
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return true
	}
	switch c := gjson.GetBytes(body, "conversation"); {
	case c.Type == gjson.String && c.String() != "", c.IsObject():
		return true
	}
	held := false
	gjson.GetBytes(body, "input").ForEach(func(_, it gjson.Result) bool {
		held = it.Get("type").String() == "item_reference"
		return !held
	})
	return held
}
