package keepalive

import (
	"testing"

	"github.com/tidwall/gjson"
)

// WHICH MODEL EACH CALL USES (#491). The ping is built from the held forwarded body and changes only the
// output budget and `stream`: the model, and every other field, stay the request's. A glm model stays glm.
func TestThePingKeepsTheRequestsModelAndEveryOtherField(t *testing.T) {
	for _, model := range []string{"glm-5-test", "claude-opus-5", "gpt-x"} {
		body := []byte(`{"model":"` + model + `","max_tokens":4096,"stream":true,"system":"s",` +
			`"messages":[{"role":"user","content":"hi"}]}`)
		out, ok := pingBodyN(body, "/v1/messages", 3)
		if !ok {
			t.Fatalf("%s: no ping body", model)
		}
		if got := gjson.GetBytes(out, "model").String(); got != model {
			t.Errorf("the ping for %q went out with model %q", model, got)
		}
		for _, f := range []string{"system", "messages.0.content"} {
			if gjson.GetBytes(out, f).Raw != gjson.GetBytes(body, f).Raw {
				t.Errorf("%s: the ping changed %s", model, f)
			}
		}
	}
}
