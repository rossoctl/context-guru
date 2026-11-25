package accwire

// The Params keys of a spend operation the host prices from its own Call (tool deferral's search turns, #1441).
const (
	// SpendParamPromptTokens is the prompt size the call ran on, when it is not the sum of its own input tiers. The
	// second pass of a search reads only the loaded definitions without the cache, but it runs on the whole prompt,
	// so the prompt picks its price tier.
	SpendParamPromptTokens = "prompt_tokens"
	// SpendParamEstimated is "true" when the usage is our estimate, not a count the provider billed.
	SpendParamEstimated = "estimated"
)
