package modelinfo

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A Table is an OPERATOR-SUPPLIED price list, consulted before the public LiteLLM
// map. It exists because the public map prices the public API, and a gateway does
// not have to charge that: IBM's litellm-gateway bills claude-sonnet-5 at $1.52/MTok in
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
type Table struct {
	entries []tableEntry
	// bareTail derives a qualified entry's own bare id (e.g. "claude-sonnet-5" for
	// "aws/claude-sonnet-5") so a request that omits the provider prefix still reaches
	// the specific entry instead of falling through to a wildcard family. See lookup.
	bareTail map[string]tableEntry
}

type tableEntry struct {
	match  string // normalized model id, or a substring of one
	prefix bool   // match is a prefix (trailing * in the file), not an exact id
	// qualified means the match looks like a model id rather than a bare word — it carries
	// a `/` or a `.`. Only those may match by containment; see lookup.
	qualified bool
	price     Price
	window    int
	// compactionPoint is the operator's compaction point in billed tokens, or 0.
	compactionPoint int
}

// tableFile is the on-disk shape.
type tableFile struct {
	// CacheReadFrac/CacheWriteFrac fill in the two cache tiers for entries that do
	// not state them. Defaults follow the Anthropic-family multiples the rest of
	// this package already assumes (1.25x a write; a read per model, see CacheReadFracFor).
	//
	// CacheReadFrac is a pointer: nil (key unset) means "per model", and a value, even 0.1,
	// is the operator's gateway multiple and wins for every entry.
	CacheReadFrac  *float64         `yaml:"cache_read_frac"`
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
	// CacheWrite1hFrac is the 1-hour cache-write rate as a multiple of In. 0 (unset) means
	// AnthropicCacheWrite1hFrac. A gateway that bills the 1-hour write at the input rate sets 1.0.
	CacheWrite1hFrac float64 `yaml:"cache_write_1h_frac"`
	// Window optionally overrides the context window for a model the public map
	// does not list either (a preview id), so fraction triggers still scale.
	Window int `yaml:"window"`
	// CompactionPoint optionally states, in BILLED input tokens, where the client compacts this model's
	// conversation (Claude Code on a GLM compacts near 167k of a 200k window). It is NOT derived from
	// window and does not change it. Used by internal/compactionpoint, for models its bootstrap table
	// does not know. 0 = unset.
	CompactionPoint int `yaml:"compaction_point"`
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
	if f.CacheReadFrac != nil && *f.CacheReadFrac <= 0 {
		return nil, fmt.Errorf("model price list: cache_read_frac must be above 0")
	}
	if f.CacheWriteFrac < 0 {
		return nil, fmt.Errorf("model price list: cache_write_frac must not be negative")
	}
	if f.CacheWriteFrac == 0 {
		f.CacheWriteFrac = AnthropicCacheWrite5mFrac
	}
	t := &Table{}
	seen := map[string]bool{}
	for i, m := range f.Models {
		match := strings.ToLower(strings.TrimSpace(m.Match))
		if match == "" {
			return nil, fmt.Errorf("model price list: entry %d has no match", i)
		}
		// Two entries for one match are two rates for one model, and lookup would pick
		// whichever sorted first. deriveBareTails relies on a match being unique.
		if seen[match] {
			return nil, fmt.Errorf("model price list: %q appears more than once", m.Match)
		}
		seen[match] = true
		prefix := strings.HasSuffix(match, "*")
		match = strings.TrimSuffix(match, "*")
		if m.Window > 0 && m.CompactionPoint > m.Window {
			return nil, fmt.Errorf("model price list: %q states compaction_point %d above its window %d", m.Match, m.CompactionPoint, m.Window)
		}
		if m.Window < 0 || m.CompactionPoint < 0 {
			return nil, fmt.Errorf("model price list: %q has a negative window or compaction_point", m.Match)
		}
		if m.In < 0 || m.Out < 0 || m.CacheRead < 0 || m.CacheWrite < 0 {
			return nil, fmt.Errorf("model price list: %q has a negative rate", m.Match)
		}
		if m.CacheWrite1hFrac < 0 {
			return nil, fmt.Errorf("model price list: %q: cache_write_1h_frac must not be negative", m.Match)
		}
		if m.In == 0 && m.Out == 0 {
			return nil, fmt.Errorf("model price list: %q has no rates; omit the entry rather than pricing it free", m.Match)
		}
		p := Price{
			Input: m.In / perMTok, Output: m.Out / perMTok,
			CacheRead: m.CacheRead / perMTok, CacheWrite: m.CacheWrite / perMTok,
			Write1hFrac: m.CacheWrite1hFrac,
		}
		// A blank tier is filled at lookup, when the model is known (Price.CacheReadFor). The
		// file-level multiples travel with the entry. A cache_read_frac the file leaves unset
		// stays 0, so the queried model decides.
		if f.CacheReadFrac != nil {
			p.ReadFrac = *f.CacheReadFrac
		}
		// The file-level cache_write_frac is documented in prices.example.yaml as "the
		// Anthropic-family multiples", so it is applied to the Anthropic family and not
		// to models whose provider charges no creation premium at all. The family is that of
		// the entry's own match, as before.
		p.WriteFrac = f.CacheWriteFrac
		if !chargesCacheWritePremium(match) {
			p.WriteFrac = 1.0
		}
		t.entries = append(t.entries, tableEntry{match: match, prefix: prefix,
			qualified: strings.ContainsAny(match, "/."), price: p, window: m.Window, compactionPoint: m.CompactionPoint})
	}
	// Longest match first, so `aws/claude-opus-4-8` wins over `aws/claude-opus-4*`
	// and the file's own order cannot make a lookup ambiguous.
	sort.SliceStable(t.entries, func(i, j int) bool {
		return len(t.entries[i].match) > len(t.entries[j].match)
	})
	t.bareTail = deriveBareTails(t.entries)
	return t, nil
}

// deriveBareTails indexes each qualified, non-family entry (e.g. "aws/claude-sonnet-5")
// by its own bare tail ("claude-sonnet-5") — the fix for #324, mirroring
// modelinfo.LiteLLM.fetch, which does the same for the public price map
// (`m[full]=w; if _,ok:=m[tail]; !ok { m[tail]=w }`). Without it, a request whose
// model id lacks the provider prefix the operator wrote the specific rate under fell
// straight past that entry to whichever wildcard family entry happened to
// prefix-match — silently, at the wrong rate.
//
// Two rules keep this from introducing a new silent-wrong-price bug of its own:
//
//   - An entry the operator wrote bare, on purpose (no `/`), always wins: this
//     function skips any tail already claimed by a literal entry, so lookup's own
//     first (exact) pass finds that entry before ever consulting this index.
//   - A tail claimed by TWO OR MORE differently-priced providers is ambiguous — a
//     bare request for it cannot be resolved without guessing which provider it
//     meant, and guessing is exactly the mechanism that caused #324. "Differently
//     priced" covers the window and compaction point the entry also serves, not only
//     the rate: Window, WindowExact and CompactionPoint read the same entry, and a
//     5x-too-high window reported as exact is as wrong as a wrong price. Such a tail is
//     not indexed at all here, so it falls through to the family/public-map passes
//     instead of silently picking one provider's rate for the other's traffic.
func deriveBareTails(entries []tableEntry) map[string]tableEntry {
	literal := make(map[string]bool, len(entries))
	for _, e := range entries {
		literal[e.match] = true
	}
	tails := map[string]tableEntry{}
	ambiguous := map[string]bool{}
	for _, e := range entries {
		if e.prefix || !e.qualified {
			continue // families and bare entries don't need a derived tail
		}
		i := strings.LastIndexByte(e.match, '/')
		if i < 0 {
			continue // no provider prefix to strip, e.g. a bare "gpt-5.6-sol" entry
		}
		tail := e.match[i+1:]
		if tail == "" || literal[tail] {
			continue // an explicit bare entry for this tail already wins on its own
		}
		if prev, ok := tails[tail]; ok && prev.match != e.match {
			// Two providers at one rate and one window are not a guess.
			if prev.price != e.price || prev.window != e.window || prev.compactionPoint != e.compactionPoint {
				ambiguous[tail] = true
			}
			continue
		}
		tails[tail] = e
	}
	if len(ambiguous) > 0 {
		names := make([]string, 0, len(ambiguous))
		for tail := range ambiguous {
			delete(tails, tail)
			names = append(names, tail)
		}
		sort.Strings(names)
		slog.Warn("model price list: bare id is ambiguous between two qualified entries at "+
			"different rates; not indexing it — a request for it falls through to the family/"+
			"public rate instead of guessing which provider's rate applies",
			"tails", names)
	}
	return tails
}

// Len is how many entries were loaded, for the startup log line.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.entries)
}

// lookup finds the most specific entry for a model id.
//
// Precedence, most specific first: (1) an exact match, full id or literal bare id;
// (2) a bare id resolving to the one qualified entry it is the tail of (#324); then
// (3) a family `prefix*` and a bare id matched by containment, which compete in ONE
// pass ordered by match length, because entries are sorted longest-first.
//
// (3) used to be two separate passes, and that was its own bug: `gemini-2.5*` (a
// prefix entry) beat `gemini-2.5-pro` (an exact entry reached only by containment)
// for `gcp/gemini-2.5-pro-preview-05-06`, pricing a Pro deployment at Flash's rate —
// a 4x underprice on its cost, its baseline and every saving derived from them.
//
// Containment is restricted twice over, for the symmetric reason. A `*` entry only ever
// matches as a prefix, and a non-`*` entry is only matched by containment when it LOOKS
// like a qualified model id — when it contains a `/` or a `.`. Without that second rule
// Bob's tier names, which are 4-8 characters of ordinary English (`fast`, `premium`,
// `standard`), claimed every unrelated id that happened to contain them:
// `azure/gpt-5.2-fast` priced at Bob's `fast` estimate, ten times under, and reported
// `ok=true` so the public map was never consulted and the row read `complete` rather than
// `partial`. A confidently wrong price is worse than a missing one.
func (t *Table) lookup(model string) (tableEntry, bool) {
	if t == nil || len(t.entries) == 0 {
		return tableEntry{}, false
	}
	full, tail := Normalize(model)
	for _, e := range t.entries {
		if e.match == full || e.match == tail {
			return e, true
		}
	}
	// A bare id resolves to the one qualified entry it is the tail of (#324), before
	// any wildcard family is consulted — see deriveBareTails for precedence and the
	// ambiguous case.
	// Only for a request that carries no provider prefix. `vertex/claude-sonnet-5` shares the
	// tail of `aws/claude-sonnet-5` but is another provider's traffic; it must not take aws's rate.
	if full == tail {
		if e, ok := t.bareTail[tail]; ok {
			return e, true
		}
	}
	for _, e := range t.entries { // longest match first
		if e.prefix {
			if strings.HasPrefix(full, e.match) || strings.HasPrefix(tail, e.match) {
				return e, true
			}
			continue
		}
		if e.qualified && strings.Contains(full, e.match) {
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
	// The fill uses the QUERIED model, not the match pattern: a family entry such as
	// `claude-opus*`, or a containment hit such as `aws/claude-sonnet-5` on
	// `aws/claude-sonnet-5-5`, serves models with different read tiers.
	return e.price.Resolved(model), true
}

// Window returns an operator-supplied context window, for models the public map
// does not list. Entries without one return ok=false so the public map still wins.
// WindowExact: an operator's own table is authoritative by construction — they wrote a window
// for this deployment's models, which is a stronger claim than the public map's.
func (t *Table) WindowExact(ctx context.Context, model string) (int, bool, bool) {
	w, ok := t.Window(ctx, model)
	return w, ok, ok
}

// CompactionPoint returns the operator's compaction point for a model, in billed input tokens.
// ok=false when the entry states none, so the bootstrap table of internal/compactionpoint answers.
func (t *Table) CompactionPoint(_ context.Context, model string) (int, bool) {
	e, ok := t.lookup(model)
	if !ok || e.compactionPoint <= 0 {
		return 0, false
	}
	return e.compactionPoint, true
}

func (t *Table) Window(_ context.Context, model string) (int, bool) {
	e, ok := t.lookup(model)
	if !ok || e.window == 0 {
		return 0, false
	}
	return e.window, true
}
