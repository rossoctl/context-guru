package modelinfo

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A Table is an OPERATOR-SUPPLIED price list, consulted before the public LiteLLM
// map. It exists because the public map prices the public API, and a gateway does
// not have to charge that: IBM's ete-litellm bills claude-sonnet-5 at $1.52/MTok in
// where anthropic.com bills $3.00, so every dollar figure this dashboard produced
// for that gateway was ~2x too high. A price list is also the only way to price a
// model the public map has never heard of — a preview deployment, an internal
// route name, or a server-resolved TIER like Bob's `premium`, which is not a model
// id at all and left every Bob request's cost reading "unknown".
//
// Rates in the file are USD per MILLION tokens, because that is the unit every
// vendor's price page and every gateway's admin UI uses; storing per-token floats
// in a hand-edited file invites a factor-of-a-thousand typo that nothing would
// catch. They are converted once, here.
//
// Nothing in a Table is a secret: these are list prices. It is a plain file, and a
// missing or malformed one is a startup error rather than a silent fallback — a
// price list that failed to load looks exactly like "this model is free".
type Table struct{ entries []tableEntry }

type tableEntry struct {
	match  string // normalized model id, or a substring of one
	prefix bool   // match is a prefix (trailing * in the file), not an exact id
	price  Price
	window int
}

// tableFile is the on-disk shape.
type tableFile struct {
	// CacheReadFrac/CacheWriteFrac fill in the two cache tiers for entries that do
	// not state them. Defaults follow the Anthropic-family multiples the rest of
	// this package already assumes (0.1x a read, 1.25x a write).
	CacheReadFrac  float64          `yaml:"cache_read_frac"`
	CacheWriteFrac float64          `yaml:"cache_write_frac"`
	Models         []tableFileModel `yaml:"models"`
}

type tableFileModel struct {
	// Match is a gateway model id, optionally ending in `*` to match a family.
	Match string `yaml:"match"`
	// In/Out/CacheRead/CacheWrite are USD per MILLION tokens.
	In         float64 `yaml:"in"`
	Out        float64 `yaml:"out"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
	// Window optionally overrides the context window for a model the public map
	// does not list either (a preview id), so fraction triggers still scale.
	Window int `yaml:"window"`
	// Note is documentation for whoever edits the file — in particular, which
	// entries are an operator ESTIMATE (a tier) rather than a published rate.
	Note string `yaml:"note"`
}

const perMTok = 1e6

// LoadTable reads a price list. An empty path returns a nil *Table, which prices
// nothing and is safe to put in a Chain.
func LoadTable(path string) (*Table, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTable(b)
}

// ParseTable builds a Table from the YAML document LoadTable reads.
func ParseTable(b []byte) (*Table, error) {
	var f tableFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // a typo'd key is a wrong price, not a comment
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("model price list: %w", err)
	}
	if f.CacheReadFrac == 0 {
		f.CacheReadFrac = 0.1
	}
	if f.CacheWriteFrac == 0 {
		f.CacheWriteFrac = 1.25
	}
	t := &Table{}
	for i, m := range f.Models {
		match := strings.ToLower(strings.TrimSpace(m.Match))
		if match == "" {
			return nil, fmt.Errorf("model price list: entry %d has no match", i)
		}
		prefix := strings.HasSuffix(match, "*")
		match = strings.TrimSuffix(match, "*")
		if m.In < 0 || m.Out < 0 || m.CacheRead < 0 || m.CacheWrite < 0 {
			return nil, fmt.Errorf("model price list: %q has a negative rate", m.Match)
		}
		if m.In == 0 && m.Out == 0 {
			return nil, fmt.Errorf("model price list: %q has no rates; omit the entry rather than pricing it free", m.Match)
		}
		p := Price{
			Input: m.In / perMTok, Output: m.Out / perMTok,
			CacheRead: m.CacheRead / perMTok, CacheWrite: m.CacheWrite / perMTok,
		}
		if p.CacheRead == 0 {
			p.CacheRead = p.Input * f.CacheReadFrac
		}
		if p.CacheWrite == 0 {
			p.CacheWrite = p.Input * f.CacheWriteFrac
		}
		t.entries = append(t.entries, tableEntry{match: match, prefix: prefix, price: p, window: m.Window})
	}
	// Longest match first, so `aws/claude-opus-4-8` wins over `aws/claude-opus-4*`
	// and the file's own order cannot make a lookup ambiguous.
	sort.SliceStable(t.entries, func(i, j int) bool {
		return len(t.entries[i].match) > len(t.entries[j].match)
	})
	return t, nil
}

// Len is how many entries were loaded, for the startup log line.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.entries)
}

// lookup finds the most specific entry for a model id. The id is matched whole
// first (with and without its provider prefix), then as a family prefix, then —
// for a gateway that decorates the id (`bedrock/us.anthropic.claude-opus-5`) — by
// containment. Longest entry wins throughout, so this is deterministic; the
// LiteLLM map's own containment scan iterates a Go map and is not.
func (t *Table) lookup(model string) (tableEntry, bool) {
	if t == nil || len(t.entries) == 0 {
		return tableEntry{}, false
	}
	full, tail := normalize(model)
	for _, e := range t.entries {
		if e.match == full || e.match == tail {
			return e, true
		}
	}
	for _, e := range t.entries {
		if e.prefix && (strings.HasPrefix(full, e.match) || strings.HasPrefix(tail, e.match)) {
			return e, true
		}
	}
	for _, e := range t.entries {
		if strings.Contains(full, e.match) {
			return e, true
		}
	}
	return tableEntry{}, false
}

// Price returns the operator's rate for a model. ok=false means "not in the file",
// and the next source in the Chain answers.
func (t *Table) Price(_ context.Context, model string) (Price, bool) {
	e, ok := t.lookup(model)
	if !ok {
		return Price{}, false
	}
	return e.price, true
}

// Window returns an operator-supplied context window, for models the public map
// does not list. Entries without one return ok=false so the public map still wins.
func (t *Table) Window(_ context.Context, model string) (int, bool) {
	e, ok := t.lookup(model)
	if !ok || e.window == 0 {
		return 0, false
	}
	return e.window, true
}
