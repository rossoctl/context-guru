// Package respfacts reads the facts a provider response states, from its bytes: the billed token
// tiers and why they could not be read (#200), the stop reason, the tool calls and their wait
// parameters, and the subagent spawns. One rule, one place, so every host reads a response the
// same way. It reads wire format only. It holds no transport and no stored customer data.
package respfacts
