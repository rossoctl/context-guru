// Package counterfactual prices context operations in tokens for each billing tier.
//
// Context operators report what they changed as facts (RequestOps, Operation). Compute turns
// those facts and the provider's cache state into tokens. The host turns tokens into money.
//
// The package is a standalone module. It imports only the standard library, so context-guru,
// Cortex and any other AI framework can use it (docs/counterfactual/design.md, R3, D7).
// boundary_test.go enforces that rule.
package counterfactual
