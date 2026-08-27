package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// TestOptionsServesTheFieldContractTheSettingsPageDraws pins the JSON shape of
// /api/options.component_fields, because that payload IS the settings form: the page has no
// hand-written list of knobs any more, it renders whatever this says. A rename here is a page
// that silently stops drawing a control, which is the failure the descriptors exist to end —
// the hand-written form reached 18 keys of about a hundred and nothing noticed.
func TestOptionsServesTheFieldContractTheSettingsPageDraws(t *testing.T) {
	f := ctlFixture(t)
	w, _ := f.signUp(t, "a@ibm.com", "l")
	jar := w.Result().Cookies()
	w, _ = f.do(t, "GET", "/api/options", "", jar)
	if w.Code != http.StatusOK {
		t.Fatalf("options = %d %s", w.Code, w.Body)
	}
	var got struct {
		Components      []string `json:"components"`
		ComponentFields map[string][]struct {
			Key     string   `json:"key"`
			Type    string   `json:"type"`
			Default any      `json:"default"`
			Options []string `json:"options"`
			Hint    string   `json:"hint"`
			Secret  bool     `json:"secret"`
			Min     int      `json:"min"`
		} `json:"component_fields"`
		Recommended map[string]map[string]any `json:"recommended"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the options payload does not match the documented shape: %v", err)
	}
	// Every name the page offers has an entry — empty for a component that takes none.
	for _, name := range got.Components {
		if _, ok := got.ComponentFields[name]; !ok {
			t.Errorf("component %q is offered with no field list, so the page can only "+
				"draw a checkbox for it", name)
		}
	}
	if len(got.ComponentFields) < 10 {
		t.Fatalf("only %d components declare fields", len(got.ComponentFields))
	}
	seenTypes := map[string]bool{}
	for name, fs := range got.ComponentFields {
		for _, fd := range fs {
			if fd.Key == "" || fd.Type == "" {
				t.Errorf("%s has a field with no key or no type: %+v", name, fd)
			}
			if fd.Hint == "" {
				t.Errorf("%s.%s has no hint, so the page would draw a bare number box", name, fd.Key)
			}
			if fd.Type == components.FieldEnum && len(fd.Options) == 0 {
				t.Errorf("%s.%s is an enum with no options", name, fd.Key)
			}
			seenTypes[fd.Type] = true
		}
	}
	for _, want := range []string{components.FieldBool, components.FieldInt, components.FieldFloat,
		components.FieldEnum, components.FieldString, components.FieldStrings} {
		if !seenTypes[want] {
			t.Errorf("no field of type %q is served, so the page has nothing to render it from", want)
		}
	}

	// The specifics the page's controls depend on, and every one of them is a bug that
	// shipped: the strategy list that omitted an accepted value, the credential that must
	// be write-only, the size floor whose 0 is a removed brake, and the cap whose 0 is a
	// legitimate "unlimited".
	x := map[string]struct {
		Type    string
		Options []string
		Secret  bool
		Min     int
	}{}
	for _, fd := range got.ComponentFields["extract_llm"] {
		x[fd.Key] = struct {
			Type    string
			Options []string
			Secret  bool
			Min     int
		}{fd.Type, fd.Options, fd.Secret, fd.Min}
	}
	if !hasOption(x["strategy"].Options, "deterministic") {
		t.Errorf("strategy options %v omit deterministic, which the engine accepts — the "+
			"omission is what silently rewrote a stored LLM-free config to `code`", x["strategy"].Options)
	}
	if !x["model.api_key"].Secret {
		t.Error("model.api_key is not marked secret, so the page would echo a credential back")
	}
	if x["min_tokens"].Min != 1 {
		t.Error("min_tokens accepts 0, which removes the only content gate fire_on: size has")
	}
	if x["llm_max_per_session"].Min != 0 {
		t.Error("llm_max_per_session must accept 0 — the component reads it as unlimited")
	}
	// A nested key must be served as ONE dotted field, which is the whole point of the path. It
	// used to be asserted on cold_cache.min_tokens; the cold sweep is its own component now, so it
	// is asserted on the same nesting that remains on this one.
	if x["trigger.min_request_tokens"].Type != components.FieldInt {
		t.Error("a nested key is not served as one dotted field, which is the whole point of the path")
	}
	// And the sweep's own fields must reach the page at all, or the split moved a component out of
	// the operator's reach rather than out of extract_llm.
	sweep := map[string]string{}
	for _, fd := range got.ComponentFields["extract_llm_sweep"] {
		sweep[fd.Key] = fd.Type
	}
	if len(sweep) == 0 {
		t.Fatalf("extract_llm_sweep serves no fields, so the settings page cannot configure it")
	}
	if sweep["min_tokens"] != components.FieldInt {
		t.Error("the sweep's own floor is not on the page, and it is the knob that decides whether it fires")
	}
	if sweep["model.model"] != components.FieldString {
		t.Error("the sweep's nested model key is not served as one dotted field")
	}
	// The compaction knobs must NOT be there: the component refuses them, so a field would put a
	// control on the page whose only behaviour is to fail the save.
	for _, banned := range []string{"strategy", "aggressiveness", "rewrite", "max_chars"} {
		if _, present := sweep[banned]; present {
			t.Errorf("extract_llm_sweep declares %q, which its constructor rejects", banned)
		}
	}
	// The recommended prefill is a separate layer from a component's own defaults, served
	// so the page does not carry a second copy of the policy.
	if got.Recommended["extract_llm"]["model.model"] == nil {
		t.Errorf("no recommended prefill for extract_llm: %v", got.Recommended)
	}
}

func hasOption(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
