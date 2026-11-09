package schema

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
