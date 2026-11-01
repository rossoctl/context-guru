package modelinfo

import bschemas "github.com/maximhq/bifrost/core/schemas"

// HonoursCacheControl reports whether a provider accepts Anthropic-style `cache_control`
// breakpoints on the wire. It is the ONE list of such providers (#809). Four callers used to keep
// their own copy: the TTL tier, the prefix split, the keep-alive gate and cacheinject. One copy
// missed Bedrock Mantle, so cacheinject placed no breakpoint there while the tier said "5m".
//
// Bedrock Mantle serves Claude through the native Anthropic Messages surface
// (`/anthropic/v1/messages`), so the same body, and the same `cache_control`, applies.
func HonoursCacheControl(provider bschemas.ModelProvider) bool {
	switch provider {
	case bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex:
		return true
	}
	return false
}
