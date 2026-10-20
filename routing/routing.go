// Package routing is the model routing table: which model a feature uses, and which credential
// and endpoint it reaches that model with.
//
// A row names a FEATURE (for example "classifier") and three environment variables — the model
// id, the key and the base URL. The file holds env var NAMES, never values, so a leaked file or a
// database dump holds no secret. config/upstreams.go (key_env) follows the same rule.
//
// The core never sees the key or the URL. The host hands Build a Pinner; the Pinner takes the
// endpoint and the key, holds them (the proxy pins them on its Dispatcher) and returns a
// dispatch.Ref. The Table keeps only that Ref, the model id and the prices. A feature asks
// Lookup("x") and gets a Route it can send through dispatch, or "none" — and "none" means the
// feature keeps its default behaviour.
//
// Fail open: a row with a missing env var, a bad base URL or a bad dialect is INACTIVE and logged
// once. It never stops the proxy.
package routing

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
)

// Row is one line of the routing file.
type Row struct {
	// Feature is the feature that uses the row. One row per feature.
	Feature string `yaml:"feature"`
	// The three below name environment variables. They are read once, at boot.
	ModelNameEnv string `yaml:"model_name_env"`
	KeyEnv       string `yaml:"key_env"`
	BaseURLEnv   string `yaml:"base_url_env"`
	// Dialect is the wire format of the endpoint: anthropic | openai | bob (config.Dialect*).
	Dialect string `yaml:"dialect"`
	// Prices are USD per MILLION tokens. All optional: a missing price is $0 for the routed
	// model, which is how a free or flat-rate endpoint is priced.
	PriceInput      float64 `yaml:"price_input"`
	PriceOutput     float64 `yaml:"price_output"`
	PriceCacheRead  float64 `yaml:"price_cache_read"`
	PriceCacheWrite float64 `yaml:"price_cache_write"`
}

type file struct {
	Routes []Row `yaml:"routes"`
}

// Load reads the routing file. Decoding is strict: an unknown field is an error, because a typo
// in a price or an env name would otherwise change what the row does without a word.
func Load(path string) ([]Row, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse is Load over bytes.
func Parse(b []byte) ([]Row, error) {
	var f file
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("routing: %w", err)
	}
	return f.Routes, nil
}

// Endpoint is what the host needs to hold for one active row. It carries the key, so it goes to
// the Pinner and nowhere else.
type Endpoint struct {
	BaseURL string
	Key     string
	Dialect string
}

// Pinner is the host's half: it holds the endpoint and credential and returns the Ref.
type Pinner func(Endpoint) dispatch.Ref

// Route is what a feature gets. It holds no key and no URL.
type Route struct {
	Feature string
	Model   string
	Ref     dispatch.Ref
	Dialect string
	// Path is the request path for the dialect, e.g. /inference/v1/chat/completions for bob.
	Path string
	// Price is per token, ready for modelinfo.Price.Cost. Zero when the row gives none.
	Price dispatch.Price
}

// PathFor is the path a dialect is called on.
func PathFor(dialect string) string {
	switch dialect {
	case config.DialectAnthropic:
		return "/v1/messages"
	case config.DialectBob:
		return "/inference/v1/chat/completions"
	default:
		return "/v1/chat/completions"
	}
}

// Outcome is how one call of a feature ended.
type Outcome uint8

const (
	// Routed: the routed model answered and the answer went to the caller.
	Routed Outcome = iota
	// FellBack: the feature matched, the route failed, the request went the normal way.
	FellBack
	// NotDetected: the feature looked at a request and it was not one of its own.
	NotDetected
)

// FeatureStats is the /stats view of one row. It has no key. The base URL is reduced to its host.
type FeatureStats struct {
	Feature     string  `json:"feature"`
	Active      bool    `json:"active"`
	Reason      string  `json:"reason,omitempty"` // why a row is inactive
	Model       string  `json:"model,omitempty"`
	BaseURLHost string  `json:"base_url_host,omitempty"`
	Dialect     string  `json:"dialect,omitempty"`
	Calls       int64   `json:"calls"` // requests that matched the feature
	Routed      int64   `json:"routed"`
	FellBack    int64   `json:"fell_back"`
	NotDetected int64   `json:"not_detected"`
	SavedUSD    float64 `json:"saved_usd"`
}

type entry struct {
	route  Route
	active bool
	mu     sync.Mutex
	st     FeatureStats
}

// Table is the loaded routing table. The zero value and nil are an empty table.
type Table struct {
	order []string
	by    map[string]*entry
}

// Build validates rows against the environment and pins every active one. getenv nil means
// os.Getenv. pin may be nil only when no row is meant to be active.
func Build(rows []Row, getenv func(string) string, pin Pinner, lg *slog.Logger) *Table {
	if getenv == nil {
		getenv = os.Getenv
	}
	if lg == nil {
		lg = slog.Default()
	}
	t := &Table{by: map[string]*entry{}}
	for _, r := range rows {
		r.Feature = strings.TrimSpace(r.Feature)
		r.Dialect = strings.ToLower(strings.TrimSpace(r.Dialect))
		e := &entry{st: FeatureStats{Feature: r.Feature, Dialect: r.Dialect}}
		if r.Feature == "" {
			lg.Warn("routing: row has no feature; ignored")
			continue
		}
		if _, dup := t.by[r.Feature]; dup {
			// One row per feature. The first row wins and stays as it was; the second is dropped.
			lg.Warn("routing: second row for a feature; ignored", "feature", r.Feature)
			continue
		}
		t.order = append(t.order, r.Feature)
		t.by[r.Feature] = e
		model, key, base, reason := resolve(r, getenv)
		if reason != "" {
			e.st.Reason = reason
			lg.Warn("routing: row inactive", "feature", r.Feature, "reason", reason)
			continue
		}
		if pin == nil {
			e.st.Reason = "no pinner"
			continue
		}
		ref := pin(Endpoint{BaseURL: base, Key: key, Dialect: r.Dialect})
		e.active = true
		e.route = Route{Feature: r.Feature, Model: model, Ref: ref, Dialect: r.Dialect,
			Path: PathFor(r.Dialect), Price: dispatch.Price{
				Input: r.PriceInput / 1e6, Output: r.PriceOutput / 1e6,
				CacheRead: r.PriceCacheRead / 1e6, CacheWrite: r.PriceCacheWrite / 1e6}}
		u, _ := url.Parse(base)
		e.st.Active, e.st.Model, e.st.BaseURLHost = true, model, u.Host
		lg.Info("routing: row active", "feature", r.Feature, "model", model, "host", u.Host,
			"dialect", r.Dialect)
	}
	return t
}

// resolve checks one row. A non-empty reason means inactive. The reason names the env var, never
// its value.
func resolve(r Row, getenv func(string) string) (model, key, base, reason string) {
	switch r.Dialect {
	case config.DialectAnthropic, config.DialectOpenAI, config.DialectBob:
	default:
		return "", "", "", fmt.Sprintf("unknown dialect %q", r.Dialect)
	}
	for _, p := range []float64{r.PriceInput, r.PriceOutput, r.PriceCacheRead, r.PriceCacheWrite} {
		if p < 0 {
			return "", "", "", "negative price"
		}
	}
	for _, n := range []struct{ field, name string }{
		{"model_name_env", r.ModelNameEnv}, {"key_env", r.KeyEnv}, {"base_url_env", r.BaseURLEnv}} {
		if strings.TrimSpace(n.name) == "" {
			return "", "", "", n.field + " is empty"
		}
	}
	if model = strings.TrimSpace(getenv(strings.TrimSpace(r.ModelNameEnv))); model == "" {
		return "", "", "", "env " + r.ModelNameEnv + " is unset"
	}
	if key = strings.TrimSpace(getenv(strings.TrimSpace(r.KeyEnv))); key == "" {
		return "", "", "", "env " + r.KeyEnv + " is unset"
	}
	raw := strings.TrimSpace(getenv(strings.TrimSpace(r.BaseURLEnv)))
	if raw == "" {
		return "", "", "", "env " + r.BaseURLEnv + " is unset"
	}
	base, err := CheckBaseURL(raw)
	if err != nil {
		return "", "", "", "env " + r.BaseURLEnv + ": " + err.Error()
	}
	return model, key, base, ""
}

// CheckBaseURL accepts an absolute https URL, or http for localhost and 127.0.0.1 only. It
// refuses userinfo (a credential), a query and a fragment. A path is kept: a gateway may live
// under one. The error never repeats the value, which may be sensitive.
func CheckBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("does not parse")
	}
	if u.Host == "" {
		return "", fmt.Errorf("not an absolute URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
			return "", fmt.Errorf("http is allowed for localhost only")
		}
	default:
		return "", fmt.Errorf("scheme must be https")
	}
	if u.User != nil {
		return "", fmt.Errorf("carries userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("has a query or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

// Lookup answers "is there a model for this feature?". No row, or an inactive row, is false.
// Safe on a nil Table.
func (t *Table) Lookup(feature string) (Route, bool) {
	if t == nil {
		return Route{}, false
	}
	e := t.by[feature]
	if e == nil || !e.active {
		return Route{}, false
	}
	return e.route, true
}

// Record counts one call. savedUSD is added only for Routed. Safe on a nil Table.
func (t *Table) Record(feature string, o Outcome, savedUSD float64) {
	if t == nil {
		return
	}
	e := t.by[feature]
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch o {
	case Routed:
		e.st.Calls++
		e.st.Routed++
		e.st.SavedUSD += savedUSD
	case FellBack:
		e.st.Calls++
		e.st.FellBack++
	case NotDetected:
		e.st.NotDetected++
	}
}

// Stats is one entry per row in file order, or nil for an empty table.
func (t *Table) Stats() []FeatureStats {
	if t == nil || len(t.order) == 0 {
		return nil
	}
	out := make([]FeatureStats, 0, len(t.order))
	for _, f := range t.order {
		e := t.by[f]
		e.mu.Lock()
		out = append(out, e.st)
		e.mu.Unlock()
	}
	return out
}
