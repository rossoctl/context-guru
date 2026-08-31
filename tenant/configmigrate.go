package tenant

// One-time recovery for the tenant configs PR #118 broke.
//
// #118 moved extract_llm's `per_output` and `cold_cache` keys to the new `extract_llm_sweep`
// component and made config.LoadBytes REFUSE either one outright — "Breaking existing configs
// is deliberate... migrated by hand" per components/offload/extract_llm.go's own comment. That
// hand migration never happened for the accounts that were already running with either key set:
// on the very next request after the new binary shipped, buildTenantConfig started failing for
// them, and the proxy's own fail-open guarantee took over — every one of their requests kept
// being forwarded, just with NO compaction applied to any of them, silently, until an operator
// happened to read the log line rather than the dashboard (see dash/api.go for the follow-up
// fix that makes a build failure visible there instead of only in the journal).
//
// This closes that gap the way it should have shipped with #118: not by loosening the refusal
// (the refusal is right — a silently-reinterpreted `cold_cache` is "the most expensive possible
// misreading of this config", per that same comment), but by performing the EXACT mechanical
// translation #118's own migration guidance already names, in code, so it happens once,
// automatically, and is provably correct before anything is written back.
//
// NOTHING IS DELETED and nothing is guessed: per_output is dropped outright (the sweep "now IS
// the warm/tail pass, so there is nothing to switch off" — its presence changed nothing to begin
// with), and cold_cache's settings are carried onto a new extract_llm_sweep entry — in both the
// components map and the pipeline list, in the position config.go's own "housellm" preset uses
// — rather than discarded. Every rewritten document is round-tripped through config.Validate
// before it is ever written, and a tenant whose config does not match the exact shape this
// expects is left untouched and logged, never guessed at.

import (
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// coldCacheBlockRe matches extract_llm's cold_cache block IF it is exactly the two-field shape
// every affected account shared (enabled, min_tokens) — group 1 is the inner fields' own
// indentation, checked against what follows the match (see coldCacheExactlyTwoFields), and
// group 2 is the min_tokens value, the one field of it any tenant here ever set. Go's regexp
// (RE2) has no lookahead, so "nothing else in this block" cannot be expressed in the pattern
// itself — a config with a third cold_cache field would otherwise match only its FIRST two
// lines and silently leave the third dangling in the rewritten document, which is exactly the
// unrecognized-shape case this whole file exists to refuse rather than mishandle.
var coldCacheBlockRe = regexp.MustCompile(`(?m)^\s*cold_cache:\n(\s*)enabled:\s*true\n\s*min_tokens:\s*(\d+)\n`)

// coldCacheExactlyTwoFields reports whether the text immediately following a coldCacheBlockRe
// match continues the SAME block with a third field (same indentation as enabled/min_tokens)
// rather than ending it. innerIndent is coldCacheBlockRe's captured group 1.
func coldCacheExactlyTwoFields(rest, innerIndent string) bool {
	if !strings.HasPrefix(rest, innerIndent) {
		return true // de-indented (or EOF): the block ended after min_tokens.
	}
	afterIndent := rest[len(innerIndent):]
	// A line that starts right back at column 0 of the inner indent, but is ITSELF further
	// indented or blank, is not a sibling field — only literal same-level content is.
	return afterIndent == "" || afterIndent[0] == ' ' || afterIndent[0] == '\t' || afterIndent[0] == '\n'
}

// perOutputLineRe matches extract_llm's per_output line, however it is indented.
var perOutputLineRe = regexp.MustCompile(`(?m)^\s*per_output:\s*(?:true|false)\n`)

// extractLLMTriggerRe anchors the new extract_llm_sweep entry immediately after extract_llm's
// own trigger block, matching config.go's "housellm" preset's own ordering ("It sits immediately
// after extract_llm so the two work disjoint regions of the same turn").
var extractLLMTriggerRe = regexp.MustCompile(`(?m)(trigger:\n\s*min_request_tokens:\s*\d+\n)`)

// flowPipelineRe matches a one-line `pipeline: [a, b, c]` list.
var flowPipelineRe = regexp.MustCompile(`pipeline:\s*\[([^\]]*)\]`)

// blockPipelineExtractLLMRe matches extract_llm's own line in a block-style (`- x` per line)
// pipeline list, capturing its indentation so the inserted line matches it exactly.
var blockPipelineExtractLLMRe = regexp.MustCompile(`(?m)^(\s*)- extract_llm\n`)

// migrateDeprecatedExtractLLMConfig rewrites one tenant's config_yaml, moving per_output and
// cold_cache onto extract_llm_sweep exactly as #118's own migration guidance names. Returns the
// rewritten document and whether anything changed; an error means the document did not match
// the shape this can safely rewrite (e.g. cold_cache present but not in the exact
// enabled/min_tokens-only shape every affected account happened to share) — the caller's
// response to that is to leave the tenant alone and log it, not to guess further.
func migrateDeprecatedExtractLLMConfig(cfg string) (rewritten string, changed bool, err error) {
	hasPerOutput := perOutputLineRe.MatchString(cfg)
	hasColdCache := strings.Contains(cfg, "cold_cache:")
	if !hasPerOutput && !hasColdCache {
		return cfg, false, nil
	}

	var sweepMinTokens string
	if hasColdCache {
		idx := coldCacheBlockRe.FindStringSubmatchIndex(cfg)
		if idx == nil {
			return cfg, false, fmt.Errorf("cold_cache present but not in the enabled+min_tokens-only shape this migration knows how to translate")
		}
		innerIndent := cfg[idx[2]:idx[3]]
		if !coldCacheExactlyTwoFields(cfg[idx[1]:], innerIndent) {
			return cfg, false, fmt.Errorf("cold_cache present with a field beyond enabled/min_tokens; " +
				"this migration only knows the shape every affected account shared")
		}
		sweepMinTokens = cfg[idx[4]:idx[5]]
	}

	out := cfg
	if hasPerOutput {
		out = perOutputLineRe.ReplaceAllString(out, "")
	}
	if hasColdCache {
		out = coldCacheBlockRe.ReplaceAllString(out, "")
		sweepBlock := fmt.Sprintf("  extract_llm_sweep:\n    min_tokens: %s\n", sweepMinTokens)
		if !extractLLMTriggerRe.MatchString(out) {
			return cfg, false, fmt.Errorf("cold_cache present but extract_llm has no trigger block to anchor extract_llm_sweep after")
		}
		out = extractLLMTriggerRe.ReplaceAllString(out, "$1"+sweepBlock)

		switch {
		case flowPipelineRe.MatchString(out):
			out = flowPipelineRe.ReplaceAllStringFunc(out, func(s string) string {
				return strings.Replace(s, "extract_llm,", "extract_llm, extract_llm_sweep,", 1)
			})
		case blockPipelineExtractLLMRe.MatchString(out):
			out = blockPipelineExtractLLMRe.ReplaceAllString(out, "${1}- extract_llm\n${1}- extract_llm_sweep\n")
		default:
			return cfg, false, fmt.Errorf("cold_cache present but extract_llm does not appear in the pipeline list in a recognized form")
		}
	}
	return out, true, nil
}

// fixDeprecatedExtractLLMConfigs runs the migration above against every stored tenant config
// that still carries either deprecated key, once, at Open. Cheap by construction: this only
// ever matches the handful of tenants configured before #118 shipped, and once each is fixed
// (or logged as unfixable) there is nothing left for a future call to find — no meta marker is
// needed the way dash's larger migrations need one, because the predicate itself is already
// this narrow.
//
// validate proves a rewritten document actually builds before it is ever written back — the
// same Options.Validate a caller already supplies so a user's OWN settings-page save gets
// rejected instead of silently stored broken (see Patch). Reused here rather than a second
// field: it is a parameter, not an import of the `config` package, because `config`'s own
// tests import `tenant` for a settings-form fixture, and `tenant` importing `config` back
// would be a cycle. nil — every test that opens a bare Registry, and any deployment that
// never set Options.Validate — skips this migration entirely rather than validating with
// something that would rubber-stamp anything.
//
// Best-effort per tenant and never fatal to Open: a tenant this cannot safely rewrite keeps
// its current (fail-open, uncompacted) behavior and is logged loudly, which is a strict
// improvement over the silent version of that same outcome this is replacing.
func fixDeprecatedExtractLLMConfigs(db *sql.DB, validate func([]byte) error) error {
	if validate == nil {
		return nil
	}
	rows, err := db.Query(`SELECT id, config_yaml FROM tenants
		WHERE config_yaml LIKE '%per_output%' OR config_yaml LIKE '%cold_cache%'`)
	if err != nil {
		return err
	}
	type pending struct{ id, cfg string }
	var candidates []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.cfg); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	var fixed int
	for _, c := range candidates {
		newCfg, changed, err := migrateDeprecatedExtractLLMConfig(c.cfg)
		if err != nil {
			slog.Error("tenant: could not migrate a deprecated extract_llm config; "+
				"this account keeps failing to build a compaction pipeline until fixed by hand",
				"tenant", c.id, "err", err)
			continue
		}
		if !changed {
			continue
		}
		// The one place this whole file exists to prevent: never write a document back that
		// does not provably build. The caller's validate runs the exact LoadBytes+Build path
		// buildTenantConfig does in production.
		if verr := validate([]byte(newCfg)); verr != nil {
			slog.Error("tenant: migrated extract_llm config failed its own validation; "+
				"leaving the stored config untouched rather than writing something unproven",
				"tenant", c.id, "err", verr)
			continue
		}
		if _, err := db.Exec(`UPDATE tenants SET config_yaml = ? WHERE id = ?`, newCfg, c.id); err != nil {
			return fmt.Errorf("tenant %s: %w", c.id, err)
		}
		fixed++
	}
	if fixed > 0 {
		slog.Info("tenant: recovered compaction for accounts whose config used a key #118 removed",
			"accounts", fixed)
	}
	return nil
}
