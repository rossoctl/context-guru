package apply_test

// sweepVariants is the table TestSweepCapture walks. Each entry is a whole config document
// so a variant can differ in the pipeline as well as in one component's knobs.
//
// The question the table is built to answer: on a prompt-caching backend, ~86% of a real
// Claude Code request is frozen by cache safety, so the only tokens any component may touch
// are the uncached tail. Those tail tokens are billed as cache_CREATION (1.25x fresh), not
// cache_read (0.1x fresh) — so removing one is worth 12.5x what savedTokenValue() credits it.
// These variants measure whether extraction on the warm tail pays once that is true.
var sweepVariants = []sweepVariant{
	{name: "det-only", yaml: `
pipeline: [format, toon, dedup, cmdfilter, extract, cachesplit]
components:
  extract:
    min_tokens: 400
mode: sync
`},
	// Osher's production document, verbatim.
	{name: "prod-osher", yaml: `
pipeline: [format, toon, dedup, cmdfilter, extract_llm, extract, cachesplit]
components:
  extract:
    min_tokens: 400
  extract_llm:
    aggressiveness: medium
    allow_on_caching_backend: false
    cold_cache: {enabled: true, min_tokens: 1000}
    context: recent
    context_messages: 7
    fire_on: pressure
    llm_every_n_requests: 1
    llm_max_per_request: 20
    llm_max_per_session: 80
    min_tokens: 1000
    model: {model: claude-haiku-4-5, source: incoming}
    per_output: false
    strategy: code
    trigger: {min_request_tokens: 3000}
mode: sync
`},
	// The hypothesis: let it act on warm cached turns, fire on size, and let it see the
	// file reads that AUTO would skip.
	{name: "warm-tail", yaml: `
pipeline: [format, toon, dedup, cmdfilter, extract_llm, extract, cachesplit]
components:
  extract:
    min_tokens: 400
  extract_llm:
    aggressiveness: medium
    allow_on_caching_backend: true
    skip_file_reads: false
    cold_cache: {enabled: true, min_tokens: 1000}
    context: recent
    context_messages: 7
    fire_on: size
    min_tokens: 1500
    llm_max_per_request: 3
    llm_max_per_session: 40
    model: {model: claude-haiku-4-5, source: incoming}
    per_output: true
    strategy: code
mode: sync
`},
	{name: "warm-tail-800", yaml: `
pipeline: [format, toon, dedup, cmdfilter, extract_llm, extract, cachesplit]
components:
  extract:
    min_tokens: 400
  extract_llm:
    aggressiveness: medium
    allow_on_caching_backend: true
    skip_file_reads: false
    cold_cache: {enabled: true, min_tokens: 1000}
    context: recent
    fire_on: size
    min_tokens: 800
    llm_max_per_request: 4
    llm_max_per_session: 60
    model: {model: claude-haiku-4-5, source: incoming}
    per_output: true
    strategy: code
mode: sync
`},
	{name: "warm-tail-high", yaml: `
pipeline: [format, toon, dedup, cmdfilter, extract_llm, extract, cachesplit]
components:
  extract:
    min_tokens: 400
  extract_llm:
    aggressiveness: high
    allow_on_caching_backend: true
    skip_file_reads: false
    cold_cache: {enabled: true, min_tokens: 1000}
    context: recent
    fire_on: size
    min_tokens: 1500
    llm_max_per_request: 3
    llm_max_per_session: 40
    model: {model: claude-haiku-4-5, source: incoming}
    per_output: true
    strategy: code
mode: sync
`},
	// Deterministic-only extraction on the same trigger: the free comparison arm. If this
	// gets close to the LLM arms, the LLM calls are not buying much.
	{name: "det-strategy", yaml: `
pipeline: [format, toon, dedup, cmdfilter, extract_llm, extract, cachesplit]
components:
  extract:
    min_tokens: 400
  extract_llm:
    allow_on_caching_backend: true
    skip_file_reads: false
    cold_cache: {enabled: true, min_tokens: 1000}
    fire_on: size
    min_tokens: 1500
    strategy: deterministic
    per_output: true
mode: sync
`},
}
