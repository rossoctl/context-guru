package providerusage

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want Tiers
		ok   bool
	}{
		"anthropic": {`{"input_tokens":3,"cache_read_input_tokens":10,"cache_creation_input_tokens":20,"output_tokens":4}`,
			Tiers{FreshInput: 3, CacheRead: 10, CacheWrite: 20, Output: 4}, true},
		"anthropic 1h": {`{"input_tokens":3,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_1h_input_tokens":15},"output_tokens":4}`,
			Tiers{FreshInput: 3, CacheWrite: 20, CacheWrite1h: 15, Output: 4}, true},
		"responses": {`{"input_tokens":100,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":30},"output_tokens":5}`,
			Tiers{FreshInput: 10, CacheRead: 60, CacheWrite: 30, Output: 5}, true},
		"responses clamps": {`{"input_tokens":10,"input_tokens_details":{"cached_tokens":60},"output_tokens":5}`,
			Tiers{CacheRead: 60, Output: 5}, true},
		"chat": {`{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80}}`,
			Tiers{FreshInput: 20, CacheRead: 80, Output: 7}, true},
		"chat clamps": {`{"prompt_tokens":10,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80}}`,
			Tiers{CacheRead: 80, Output: 7}, true},
		"output only": {`{"output_tokens":42}`, Tiers{Output: 42}, true},
		"unknown":     {`{"inputTokens":3}`, Tiers{}, false},
		"empty":       {`{}`, Tiers{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := Parse(gjson.Parse(tc.in))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Parse = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPresentIsTrueOnlyForAnObjectWithAField(t *testing.T) {
	for in, want := range map[string]bool{
		`{"prompt_tokens":1}`: true,
		`{"x":null}`:          true,
		`{}`:                  false,
		`null`:                false,
		`[1]`:                 false,
		`3`:                   false,
		`"usage"`:             false,
	} {
		if got := Present(gjson.Parse(in)); got != want {
			t.Errorf("Present(%s) = %v, want %v", in, got, want)
		}
	}
	if Present(gjson.Get(`{}`, "usage")) {
		t.Error("Present(missing path) = true, want false")
	}
}
