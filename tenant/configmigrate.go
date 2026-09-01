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
// happened to read the log line rather than the dashboard (see dash/overview.go for the follow-up
// fix that makes a build failure visible there instead of only in the journal).
//
// This closes that gap the way it should have shipped with #118: not by loosening the refusal
// (the refusal is right — a silently-reinterpreted `cold_cache` is "the most expensive possible
// misreading of this config", per that same comment), but by performing the EXACT mechanical
// translation #118's own migration guidance already names, in code, so it happens once,
// automatically, and is provably correct before anything is written back.
//
// A real YAML decode-modify-encode round trip, not a text-level rewrite: a first draft did this
// with regexes and every one of its bugs traced back to the same cause — a regex has no idea
// what it is looking at, so a trailing comma, a shared `trigger:` block another component also
// owns, a comment line, or a key that sorts into a different position all broke it in a
// different way, and some of those broke it SILENTLY (a config.Validate pass is not proof the
// document still says what the account meant — it is only proof the document parses and
// builds). config/form.go already decodes, edits, and re-encodes every settings-page save this
// same way (see marshalConfig's own yaml.NewEncoder(&buf); enc.SetIndent(2)), so this is not a
// new pattern in the codebase, just the first migration to use it instead of hand-rolled text
// surgery.
//
// NOTHING IS DELETED and nothing is guessed: per_output is dropped outright (the sweep "now IS
// the warm/tail pass, so there is nothing to switch off" — its presence changed nothing to begin
// with), and cold_cache's settings are carried onto a new extract_llm_sweep entry — in both the
// components map and the pipeline list, in the position config.go's own "housellm" preset uses
// — rather than discarded. Every rewritten document is round-tripped through the caller's own
// validator before it is ever written, and a tenant whose config does not match the exact shape
// this expects is left untouched and logged, never guessed at.

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"
)

// migrateDeprecatedExtractLLMConfig rewrites one tenant's config_yaml, moving per_output and
// cold_cache onto extract_llm_sweep exactly as #118's own migration guidance names. Returns the
// rewritten document and whether anything changed; an error means the document did not match
// the shape this can safely rewrite (extract_llm missing or not a map, cold_cache present but
// not exactly {enabled, min_tokens}, or no components/pipeline to add extract_llm_sweep to) —
// the caller's response to that is to leave the tenant alone and log it, not to guess further.
func migrateDeprecatedExtractLLMConfig(cfg string) (rewritten string, changed bool, err error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		return cfg, false, fmt.Errorf("could not parse as a YAML mapping: %w", err)
	}

	comps, _ := doc["components"].(map[string]any)
	extractLLM, _ := comps["extract_llm"].(map[string]any)
	_, hasPerOutput := extractLLM["per_output"]
	coldCacheRaw, hasColdCache := extractLLM["cold_cache"]
	if !hasPerOutput && !hasColdCache {
		return cfg, false, nil
	}
	if extractLLM == nil {
		// hasPerOutput/hasColdCache can only be true with a non-nil extractLLM, so reaching
		// here at all would itself be a bug — kept as a hard stop rather than a silent no-op.
		return cfg, false, fmt.Errorf("per_output or cold_cache present but extract_llm is not a mapping")
	}

	// addSweep stays false when cold_cache was already off: "cold_cache.enabled becomes the
	// component's presence in the pipeline" (per extract_llm.go's own migration note) means an
	// account that had already turned the sweep off needs nothing added back — just the now-
	// refused key removed, same as per_output.
	var sweepMinTokens int
	addSweep := false
	if hasColdCache {
		// Decoded through a TYPED struct, not read as `any` — coldCacheRaw is a
		// map[string]any at this point (the outer doc was decoded that way), and reading
		// `["enabled"].(bool)` off it directly is wrong for a real config: YAML 1.1 accepts
		// yes/Yes/YES/on/On/ON/y/Y as true (and their negatives as false), but decoding one of
		// those into `any` yields a plain string, not a bool, so the type assertion silently
		// reads it as false — exactly Bug 1's silent-loss shape again, just reached a
		// different way. Re-marshaling the sub-map and decoding it through the same typed
		// path config.LoadBytes itself would use fixes the bool words by construction, and
		// KnownFields(true) gives the "nothing but enabled/min_tokens" check for free, so it
		// replaces the extra-field bookkeeping a first draft of this had rather than adding
		// to it — an account with `max_calls` or `min_idle_seconds` set is still refused, now
		// because the decoder itself rejects the extra field.
		raw, err := yaml.Marshal(coldCacheRaw)
		if err != nil {
			return cfg, false, fmt.Errorf("could not re-encode cold_cache for a typed re-read: %w", err)
		}
		var cc struct {
			Enabled   bool `yaml:"enabled"`
			MinTokens *int `yaml:"min_tokens"`
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cc); err != nil {
			return cfg, false, fmt.Errorf("cold_cache present but not in the enabled+min_tokens-only "+
				"shape this migration knows how to translate: %w", err)
		}
		if cc.Enabled {
			if cc.MinTokens == nil {
				return cfg, false, fmt.Errorf("cold_cache enabled but min_tokens is unset")
			}
			addSweep, sweepMinTokens = true, *cc.MinTokens
		}
		// !cc.Enabled falls through with addSweep left false: disabled, and nothing else set
		// that would need translating (KnownFields already refused anything else) — drop the
		// whole block, add nothing, since the sweep it would have configured never ran.
	}

	if hasPerOutput {
		delete(extractLLM, "per_output")
	}
	if hasColdCache {
		delete(extractLLM, "cold_cache")
	}
	if addSweep {
		if comps["extract_llm_sweep"] != nil {
			return cfg, false, fmt.Errorf("cold_cache present but extract_llm_sweep already exists in components")
		}
		comps["extract_llm_sweep"] = map[string]any{"min_tokens": sweepMinTokens}

		pipeline, ok := doc["pipeline"].([]any)
		if !ok {
			return cfg, false, fmt.Errorf("cold_cache present but pipeline is missing or not a list")
		}
		idx := -1
		for i, name := range pipeline {
			if s, ok := name.(string); ok && s == "extract_llm" {
				idx = i
				break
			}
		}
		if idx < 0 {
			return cfg, false, fmt.Errorf("cold_cache present but extract_llm does not appear in the pipeline list")
		}
		grown := make([]any, 0, len(pipeline)+1)
		grown = append(grown, pipeline[:idx+1]...)
		grown = append(grown, "extract_llm_sweep")
		grown = append(grown, pipeline[idx+1:]...)
		doc["pipeline"] = grown
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return cfg, false, fmt.Errorf("could not re-encode the migrated document: %w", err)
	}
	if err := enc.Close(); err != nil {
		return cfg, false, fmt.Errorf("could not re-encode the migrated document: %w", err)
	}
	return buf.String(), true, nil
}

// fixDeprecatedExtractLLMConfigs runs the migration above against every stored tenant config
// that still carries either deprecated key, once, at Open. Cheap by construction: this only
// ever matches the handful of tenants configured before #118 shipped, and once each is fixed
// (or logged as unfixable) there is nothing left for a future call to find for THAT tenant — a
// tenant this cannot safely rewrite is re-checked on every future Open, which costs one cheap
// LIKE-filtered query and is the point: it stays visible rather than being forgotten after one
// failed attempt.
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
// Best-effort per tenant and never fatal to Open: a tenant this cannot safely rewrite, OR
// whose rewrite fails to save, keeps its current (fail-open, uncompacted) behavior and is
// logged loudly, which is a strict improvement over the silent version of that same outcome
// this is replacing. A save failure for one tenant does not stop the rest from being tried.
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
			// Logged, not returned: one account's write failing (a locked row, a disk error)
			// must not stop every other candidate in this batch from getting its own turn —
			// the same reason the two checks above use continue rather than return.
			slog.Error("tenant: could not save a migrated extract_llm config; "+
				"this account keeps failing to build a compaction pipeline until fixed by hand",
				"tenant", c.id, "err", err)
			continue
		}
		fixed++
	}
	if fixed > 0 {
		slog.Info("tenant: recovered compaction for accounts whose config used a key #118 removed",
			"accounts", fixed)
	}
	return nil
}
