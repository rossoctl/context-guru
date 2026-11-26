package accwire

// The Params keys of a spend operation the host prices from its own Call (tool deferral's search turns, #1441).
const (
	// SpendParamEstimated is "true" when the usage is our estimate, not a count the provider billed. It documents the
	// call on the wire only: accountant.Figure marks its figure estimated from the pricer, not from Params.
	SpendParamEstimated = "estimated"
)
