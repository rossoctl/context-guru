package modelinfo

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func TestHonoursCacheControl(t *testing.T) {
	for _, p := range []bschemas.ModelProvider{bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex} {
		if !HonoursCacheControl(p) {
			t.Errorf("%q must honour cache_control", p)
		}
	}
	for _, p := range []bschemas.ModelProvider{bschemas.OpenAI, bschemas.Gemini, bschemas.Azure, "", "unknown"} {
		if HonoursCacheControl(p) {
			t.Errorf("%q must not honour cache_control", p)
		}
	}
}
