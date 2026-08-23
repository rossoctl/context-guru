package dash

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// The KV-cache tab's four read routes.
//
// All in API.routes(), which is the table Mount walks AND the table both scoping tests walk —
// TestEveryMountedRouteDeclaresItsScope and TestNoRouteServesContentTextFromAnUntrustedAddress.
// A route mounted beside that table would be a route whose scope nothing checks, which is
// exactly how three unauthenticated routes and then /api/prompt shipped.
//
// Every one is scopeTenant and every one returns NUMBERS, ENUM LABELS AND IDS ONLY: no prompt
// text, no transcript, no tool schema. Session ids are returned — they are the caller's own,
// the same as /api/sessions — but nothing behind them. If any field here ever grows text it
// must apply a.trusted(r) exactly as a.prompt does.
//
// # Why they are all GET
//
// The pricing experiment is a set of inputs, and the obvious shape for it is a POST with a
// JSON body. It is a GET with query parameters instead, for two reasons that are worth more
// than the tidiness: every other route in this package is a GET, so the two scoping tests
// probe the table with a GET and a POST route would be a route neither of them could check;
// and a simulation is a VIEW, so its whole input belongs in a URL that can be bookmarked,
// pasted into an issue, and reproduced by whoever reads it.
func (a *API) kvCacheRoutes() []route {
	return []route{
		{"GET /api/kvcache", scopeTenant, a.kvCache},
		{"GET /api/kvcache/rows", scopeTenant, a.kvCacheRows},
		{"GET /api/kvcache/simulate", scopeTenant, a.kvCacheSimulate},
		{"GET /api/kvcache/pricing", scopeTenant, a.kvCachePricing},
	}
}

// kvCache serves the analysis: the summary cards, the histograms, the grouped views, the
// survival curve, the coverage statement and the price list.
func (a *API) kvCache(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	out, err := a.rec.DB().KVCacheAnalyze(f, kvCacheOptionsFrom(r), a.pricer, kvCacheConfigFrom(r))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, out)
}

// kvCacheRows serves one page of the sortable detail table.
func (a *API) kvCacheRows(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	out, err := a.rec.DB().KVCacheRows(f, kvCacheOptionsFrom(r))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, out)
}

// kvCacheSimulate replays the window under the requested strategies.
func (a *API) kvCacheSimulate(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	out, err := a.rec.DB().KVCacheSimulate(f, kvCacheOptionsFrom(r), a.pricer, kvCacheConfigFrom(r))
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, out)
}

// kvCachePricing serves the price list for the models in this window, plus the assumptions.
//
// A route of its own so the pricing panel can load and re-price without re-running the whole
// simulation, which is what makes editing a rate feel like editing a rate.
func (a *API) kvCachePricing(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	cfg := kvCacheConfigFrom(r)
	models, err := a.rec.DB().KVCacheModels(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	prefix, err := a.rec.DB().KVCacheMedianPrefix(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, kvCachePriceView(models, prefix, a.pricer, cfg))
}

// kvCacheOptionsFrom parses the page-specific narrowings and the table's paging.
//
// Every enum is validated against a closed set rather than passed through: these values reach
// a comparison, not a query, but a filter that silently accepts "afternnon" and returns an
// empty table is indistinguishable from a window with no traffic in it.
func kvCacheOptionsFrom(r *http.Request) KVCacheOptions {
	q := r.URL.Query()
	o := KVCacheOptions{
		Limit:  atoiDefault(q.Get("limit"), 50),
		Offset: atoiDefault(q.Get("offset"), 0),
		Sort:   q.Get("sort"),
		Dir:    q.Get("dir"),
	}
	if b := kvcache.Bucket(q.Get("bucket")); q.Get("bucket") != "" {
		for _, known := range kvcache.Buckets {
			if b == known {
				o.Bucket = string(b)
			}
		}
	}
	switch t := q.Get("ttl"); t {
	case "none", TTLUnrecorded, string(kvcache.TTL5m), string(kvcache.TTL1h):
		o.TTL = t
	}
	switch n := q.Get("has_next"); n {
	case "yes", "no":
		o.HasNext = n
	}
	if _, ok := kvCacheSortKeys[o.Sort]; !ok {
		o.Sort = ""
	}
	if o.Dir != "asc" {
		o.Dir = "desc"
	}
	return o
}

// kvCacheConfigFrom parses the pricing experiment, the cache semantics, the keep-alive
// schedule and the strategy selection.
//
// The whole simulation input is in the query string, so a result is a URL. Nothing here has a
// default of its own: every zero falls through to package kvcache's shipped default, which is
// the one place those numbers live.
func kvCacheConfigFrom(r *http.Request) KVCacheSimConfig {
	q := r.URL.Query()
	cfg := KVCacheSimConfig{
		Multipliers: kvcache.Multipliers{
			CacheRead: atof(q.Get("mult_read")),
			Write5m:   atof(q.Get("mult_w5m")),
			Write1h:   atof(q.Get("mult_w1h")),
		},
		Semantics: kvcache.Semantics{
			// The provider's documented behaviour is the default, so an absent parameter means
			// "as documented" and only an explicit 0 turns one off.
			HitRefreshesTTL:  boolParam(q.Get("hit_refresh"), true),
			PingRefreshesTTL: boolParam(q.Get("ping_refresh"), true),
			ZeroGeneration:   boolParam(q.Get("zero_gen"), false),
		},
		PingIdle:   secondsParam(q.Get("x")),
		PingIdle1h: secondsParam(q.Get("x1h")),
		MaxPings:   atoiDefault(q.Get("k"), 0),
		Baseline:   q.Get("baseline"),
		Custom: KVCacheCustom{
			P5m:        atof(q.Get("p5m")),
			P1h:        atof(q.Get("p1h")),
			MinPrefix:  atoi64(q.Get("min_prefix")),
			AlwaysPing: boolParam(q.Get("always_ping"), false),
		},
	}
	cfg.Strategies = listParam(q["strategy"])
	cfg.Overrides = rateOverrides(q)
	return cfg
}

// rateOverrides parses the per-model rate edits.
//
// One repeated `rate` parameter per model, colon-separated:
//
//	rate=<model>:<input>:<output>:<cache_read>:<write_5m>:<write_1h>
//
// Rates are USD per MILLION tokens, because that is the unit every vendor's price page and
// every gateway admin UI uses — the same decision, and the same reason, as
// internal/modelinfo.Table: a hand-edited per-token float invites a factor-of-a-thousand typo
// that nothing would catch. They are converted once, here.
//
// An EMPTY field means "not overridden", which is why Override's fields are pointers: a rate
// of zero is a legitimate thing to experiment with, and a sentinel could not say so.
//
// The model is everything before the LAST five colons, so a gateway route name with a slash or
// a dot in it needs no escaping.
func rateOverrides(q map[string][]string) map[string]kvcache.Override {
	out := map[string]kvcache.Override{}
	// The two ping-overhead parameters are provider facts rather than model rates, so they
	// apply to every model through the wildcard key.
	var all kvcache.Override
	if v := q["ping_in"]; len(v) > 0 && v[0] != "" {
		n := atoi64(v[0])
		all.PingInputTokens = &n
	}
	if v := q["ping_out"]; len(v) > 0 && v[0] != "" {
		n := atoi64(v[0])
		all.PingOutputTokens = &n
	}
	if !all.Empty() {
		out[kvcache.OverrideAll] = all
	}
	for _, spec := range q["rate"] {
		model, ov, ok := parseRateOverride(spec)
		if !ok {
			continue
		}
		out[model] = ov
	}
	return out
}

// perMTok is the divisor from the file/UI unit to the per-token unit this package works in.
const perMTok = 1e6

// parseRateOverride reads one `rate=` specification. A malformed one is DROPPED rather than
// half-applied: a partially parsed price is a wrong price, and the panel shows the rates it
// got back, so an ignored edit is visible on screen.
func parseRateOverride(spec string) (string, kvcache.Override, bool) {
	parts := strings.Split(spec, ":")
	if len(parts) < 6 {
		return "", kvcache.Override{}, false
	}
	// The last five fields are the rates; everything before them is the model name, rejoined.
	cut := len(parts) - 5
	model := strings.Join(parts[:cut], ":")
	if model == "" {
		return "", kvcache.Override{}, false
	}
	var ov kvcache.Override
	fields := []**float64{&ov.Input, &ov.Output, &ov.CacheRead, &ov.Write5m, &ov.Write1h}
	for i, raw := range parts[cut:] {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			return "", kvcache.Override{}, false
		}
		perToken := v / perMTok
		*fields[i] = &perToken
	}
	if ov.Empty() {
		return "", kvcache.Override{}, false
	}
	return model, ov, true
}

// listParam accepts a repeated parameter, a comma-joined one, or both.
func listParam(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// boolParam reads 1/0, true/false, on/off, with a default for an absent parameter. The
// default matters: the cache semantics default to the provider's DOCUMENTED behaviour, so an
// absent parameter must not read as false.
func boolParam(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// secondsParam reads a duration given in seconds. 0 (and anything unparseable) falls through
// to package kvcache's own default rather than to a zero interval, which would mean "never
// ping" — a silently different policy from the one the caller asked for.
func secondsParam(v string) time.Duration {
	f := atof(v)
	if f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// atof parses a float, returning 0 for anything unparseable so the caller's own default
// applies.
func atof(v string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}
