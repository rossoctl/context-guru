package compactionpoint

import (
	"math"
	"strconv"
	"strings"
)

// Bob is the auto-compact setting of Bob Shell (#963). A host fills it once, at start-up
// (proxy.ReadBobCompact). Read is true only when the host did the read (local mode, switch not off).
// The zero value changes nothing.
//
// VERIFIED FROM THE SHIPPED CLIENT, Bob Shell 1.0.3 (bundle/bob.js, read only, not from memory):
//
//   - KEY: `model.compressionThreshold` in settings.json (schema default 0.5, "the fraction of
//     context usage at which to trigger context compression", restart required). Files: the user
//     file ~/.bob/settings.json, the project file .bob/settings.json, and the system file
//     (BOB_SHELL_CLI_SYSTEM_SETTINGS_PATH, else /Library/Application Support/Bob-Shell/settings.json
//     on macOS, /etc/bob-shell/settings.json elsewhere). The merge order is system defaults, user,
//     project (if the folder is trusted), system; the last one wins. There is no env override.
//   - DECISION (class Eue, method compress): the client compacts before a turn when
//     `getLastPromptTokenCount() >= T x ex(model)`, with T = `config.getCompressionThreshold() ?? 0.5`.
//     getCompressionThreshold returns the user's value when it is truthy (so 0 means "not set"),
//     else a server-side experiment flag (CONTEXT_COMPRESSION_THRESHOLD) when the flag is non-zero,
//     else undefined, which gives the 0.5 above.
//   - RULER: lastPromptTokenCount is set from `usageMetadata.promptTokenCount` of the last
//     response, i.e. the PROVIDER-REPORTED prompt tokens. That is the same ruler as billed input
//     (ClientToBilled = 1.0 is right here, not a guess). After a compaction the client seeds it with
//     an estimate, JSON length / 4, until the next response. ex(model) is the model's context
//     window from Bob's OWN gateway catalog, GET /inference/v1/model/info (or /v1/model/info),
//     field data[].model_info.max_input_tokens, 200,000 when the field or the model is missing
//     (getLiteLLMModels and the `contextWindow: a.max_input_tokens||2e5` line). So T is a fraction
//     of the WHOLE window of THAT catalog, with no reserve.
//
// WHICH WINDOW (review round 1). Bob's catalog window can differ from ours (ours is 1,000,000 for
// claude-opus-5; Bob's catalog may say 200,000). A C built on a window larger than Bob's is too
// high, the dangerous direction for this package. So Resolve takes min(our window, Bob's window):
// Bob's window is the max_input_tokens the proxy saw in the model/info response it passed through
// (BobWindow, a number for each model id); when the proxy has not seen it, Bob's own fallback
// 200,000 is used. A lower C is the conservative side.
//
// NOT READ: the project file (the proxy does not know the directory), the old flat key
// `chatCompression` (the client migrates it), and the server-side experiment flag. If that flag is
// set for a user who changed nothing, the real default is not 0.5 and C is off. The PR says so.
type Bob struct {
	Read bool
	// Threshold is the raw model.compressionThreshold from the winning file, "" when not set.
	Threshold string
}

// BobFallbackWindow is the window Bob uses when its catalog has no max_input_tokens for a model.
const BobFallbackWindow = 200_000

// BobDefaultThreshold is the client's default, `fla = .5` in the client.
const BobDefaultThreshold = 0.5

// Resolve gives C for Bob: the user's fraction of the window when set, else the client default.
// ok is false when nothing was read or the window is unknown: the caller then uses the table.
//
// window is OUR window for the model (0 = unknown). bobWindow is Bob's own max_input_tokens for the
// model, seen in its model/info response (0 = not seen). The window used is
// min(window, bobWindow), or min(window, BobFallbackWindow) when bobWindow is not seen.
func (b Bob) Resolve(window, bobWindow int) (Point, bool) {
	if !b.Read {
		return Point{}, false
	}
	bw := bobWindow
	if bw <= 0 {
		bw = BobFallbackWindow
	}
	eff := bw
	if window > 0 && window < bw {
		eff = window
	}
	if window <= 0 && bobWindow <= 0 {
		return Point{}, false // nothing known about this model: the table
	}
	t, src := BobDefaultThreshold, ClientDefault
	which := "Bob's window seen in its model/info response (or ours, if smaller)"
	if bobWindow <= 0 {
		which = "min(our window, 200,000), because the proxy has not seen Bob's own window for this model and 200,000 is Bob's fallback"
	}
	note := "Bob Shell's own default model.compressionThreshold 0.5 of the window " + which + " (client compress(): " +
		"promptTokenCount >= T x window). The ruler is the provider-reported prompt tokens, so the factor to billed tokens is 1.0."
	if raw := strings.TrimSpace(b.Threshold); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		// The client ignores a falsy value (0) and takes any other number. NaN, a negative or a
		// non-number cannot be a threshold: fail open to the default.
		if err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 {
			if f > 1 {
				f = 1 // above 1 the client never compacts: the provider's limit is the window
			}
			t, src = f, Configured
			note = "the user's own Bob model.compressionThreshold, a fraction of the window (" + which + ") " +
				"(client compress(): promptTokenCount >= T x window). The ruler is the provider-reported prompt tokens."
		}
	}
	abs := int(math.Floor(t * float64(eff)))
	if abs <= 0 {
		return Point{}, false
	}
	p := Point{Abs: abs, Source: src, Note: note}
	if window > 0 {
		p.Frac = float64(abs) / float64(window)
	}
	return p, true
}

// Codex is the user's own auto-compact setting of Codex CLI (#963). A host fills it once, at
// start-up (proxy.ReadCodexCompact). The zero value changes nothing.
//
// VERIFIED FROM THE SHIPPED CLIENT'S SOURCE, github.com/openai/codex tag rust-v0.160.0 (read only;
// `codex --version` on the laptop says 0.160.0):
//
//   - KEY: `model_auto_compact_token_limit` (integer tokens) at the top level of
//     $CODEX_HOME/config.toml (default ~/.codex; codex-rs/utils/home-dir/src/lib.rs). No env
//     override and no per-profile key exist in this version. Other layers exist (project config,
//     managed config, `-c` on the command line); only the user file is read.
//   - FORMULA (codex-rs/protocol/src/openai_models.rs ModelInfo::auto_compact_token_limit, and
//     codex-rs/core/src/session/context_window.rs context_window_token_status_with_config):
//     limit = min(configured limit or catalog limit, 0.9 x context_window); compact when
//     active_context_tokens >= limit (+ a fallback buffer only when the token-budget feature is on).
//   - RULER: active_context_tokens = the provider-reported total_tokens of the LAST response
//     (input + output) plus estimates for items added after it. That is the next request's billed
//     input within the size of the new items, so the factor to billed tokens is taken as 1.0.
//   - THE LIMIT CLAMP uses Codex's own context_window (272,000 for the models in the bundled 0.160.0
//     catalog, models-manager/models.json, one at 372,000). That value is the CLIENT's catalog,
//     not ours, so we clamp to 0.9 x OUR window only as a sanity bound.
//
// WHAT IS NOT DONE, ON PURPOSE. The default (no key set) is 0.9 x the client's catalog window,
// which depends on a per-model catalog that changes with each release and that the user can
// replace (model_catalog_json). Our window for the same model differs (872,000 vs 272,000 for
// gpt-6-sol). No default is invented: the request stays on the table. Two settings change the
// meaning of the limit, and with either set no C is derived (fail open):
// `model_auto_compact_token_limit_scope = "body_after_prefix"` (the limit then counts only growth
// after a prefix) and `model_post_turn_compact_threshold_percent` > 0 (a second, lower trigger).
type Codex struct {
	// Limit is the raw model_auto_compact_token_limit, "" when not set.
	Limit string
	// Scope and PostTurn are the raw values of the two settings above, "" when not set.
	Scope, PostTurn string
}

// Resolve gives C for Codex when the user set a limit. window is OUR window for the model (0 = unknown).
func (c Codex) Resolve(window int) (Point, bool) {
	if strings.TrimSpace(c.Limit) == "" {
		return Point{}, false
	}
	if s := strings.ToLower(strings.TrimSpace(c.Scope)); s != "" && s != "total" {
		return Point{}, false
	}
	if p := strings.TrimSpace(c.PostTurn); p != "" {
		// Numeric compare: the client reads 0 as off, and TOML may write 0.0 or 0 alike.
		if f, err := strconv.ParseFloat(p, 64); err != nil || f != 0 {
			return Point{}, false
		}
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(c.Limit), "_", ""), 10, 64)
	if err != nil || n <= 0 || n > 1<<40 {
		return Point{}, false
	}
	abs := int(n)
	// A SANITY BOUND only. The client clamps to 0.9 x ITS catalog window, which we do not know; the
	// user's limit is the value used. We cap at 0.9 x our window so a typo cannot exceed it.
	if window > 0 && abs > window*9/10 {
		abs = window * 9 / 10
	}
	p := Point{Abs: abs, Source: Configured,
		Note: "the user's own Codex model_auto_compact_token_limit, a count of provider-reported tokens " +
			"(codex-rs core/src/session/context_window.rs), taken as billed tokens by a factor of 1.0 that is not measured."}
	if window > 0 {
		p.Frac = float64(abs) / float64(window)
	}
	return p, true
}
