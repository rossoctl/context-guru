package config

import "testing"

// The dashboard form writes a YAML document by string surgery in the browser, with no YAML
// library. What makes that acceptable is that the SERVER validates strictly — LoadBytes with
// KnownFields plus a real pipeline build — so a document the form mangles is refused with the
// offending key named rather than stored.
//
// This test pins the other half of that contract: the documents the form actually produces
// must be accepted, and each key it writes must be one the components really have. A silently
// ignored key would be worse than a rejected one here, because extract_llm's own loader is
// non-strict: a misspelling in its block does nothing at all, and this is the component that
// spends money.
func TestDashboardFormDocumentsValidate(t *testing.T) {
	cases := map[string]string{
		"sweep only, the recommended first configuration": `pipeline: [format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit]
mode: sync
components:
  extract_llm:
    strategy: code
    per_output: false
    fire_on: pressure
    min_tokens: 2000
    llm_max_per_request: 2
    llm_max_per_session: 20
    aggressiveness: medium
    context: recent
    context_messages: 7
    cold_cache:
      enabled: true
      min_tokens: 1000
  extract:
    min_tokens: 400
`,
		"both paths, high aggressiveness, full context": `pipeline: [format, dedup, extract_llm, extract, cachesplit]
mode: sync
components:
  extract_llm:
    strategy: code
    per_output: true
    fire_on: size
    min_tokens: 1500
    llm_max_per_request: 3
    llm_max_per_session: 25
    aggressiveness: high
    context: full
    context_messages: 9
    cold_cache:
      enabled: true
      min_tokens: 800
  extract:
    min_tokens: 400
`,
		"component removed when both switches are off": `pipeline: [format, dedup, extract, cachesplit]
mode: sync
components:
  extract:
    min_tokens: 400
`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Validate([]byte(doc)); err != nil {
				t.Fatalf("the form produces a document the server rejects: %v", err)
			}
		})
	}
}

// The mirror image: a key the form might plausibly be edited to emit, that the component does
// NOT have, must be caught here — because extract_llm's own yaml.Unmarshal is not strict, so
// nothing downstream will complain.
func TestUnknownTopLevelKeyIsRejected(t *testing.T) {
	if err := Validate([]byte("pipeline: [format]\nmodel_source: incoming\n")); err == nil {
		t.Fatal("an unknown top-level key was accepted")
	}
}
