package tooldefer

import (
	"sync"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// Net saving, not gross.
//
// Gross is what the deferred definitions would have cost: their weight, on every request, at the
// tier that request was billed at (a cache read, a cache write, or fresh input).
//
// Net subtracts everything deferral itself causes:
//   - the names list, at the same tier (it sits in the cached prefix);
//   - each search turn: the model's output for the search call, and the second pass inside the API,
//     which reads the search result and the loaded definitions once WITHOUT the cache;
//   - each loaded definition: it now rides in the transcript, so the next request writes it to the
//     cache once (at the write price), and later requests pay it at the tier they are billed at
//     (counted by keeping it in the request weight: see Request).
//
// The token weights come from our own tokenizer (o200k), which under-counts Claude's. They are
// scaled by tokens.BilledDeltaFactor, the measured ratio for a removed span, before pricing.

// Tier is how a request was billed for the bytes at the head of its prefix.
type Tier int

const (
	TierFresh Tier = iota
	TierRead
	TierWrite
	TierWrite1h
)

// TierOf is the tier the head of the prefix was billed at, from the response usage. The same rule
// as the dashboard's declaration-filter saving: any cache read means the head was read.
func TierOf(cacheRead, cacheWrite, cacheWrite1h int64) Tier {
	switch {
	case cacheRead > 0:
		return TierRead
	case cacheWrite1h > 0:
		return TierWrite1h
	case cacheWrite > 0:
		return TierWrite
	}
	return TierFresh
}

func rate(p modelinfo.Price, t Tier) float64 {
	switch t {
	case TierRead:
		return p.CacheRead
	case TierWrite:
		return p.CacheWrite
	case TierWrite1h:
		return p.CacheWrite1h()
	}
	return p.Input
}

// Request is one request's deferral facts, in OUR tokenizer's units.
type Request struct {
	// Tenant is the account ("" on a single-tenant proxy): the ledger keeps a total per account.
	Tenant string
	Model  string
	// DeferredTokens is the weight of the definitions deferred on this request, minus those of
	// tools a search has already loaded in this transcript (those are paid inline again).
	DeferredTokens int
	// ListTokens is the names list's weight.
	ListTokens int
	// Searches is how many server-side searches this response ran.
	Searches int
	// SearchOutput is the output tokens of the search calls (provider units; from usage when known,
	// else an estimate of 30 per search).
	SearchOutput int64
	// LoadedTokens is the weight of the definitions the searches in this response loaded: the
	// second pass reads them uncached once, and the next request writes them to the cache once.
	LoadedTokens int
	// WrongGuesses is how many tools this request deferred that a search in its response then
	// loaded. DroppedSearches is how many searches of this response have no result: the API does
	// not run a search sent together with another tool call, and the model searches again.
	WrongGuesses, DroppedSearches int
	// Tier is how this request was billed.
	Tier Tier
}

// WrongGuesses is how many wrong guesses one response shows (#866, N8): tools that are in this
// request's deferred set (deferred, the names we sent with defer_loading), that a search in the
// response loaded (loaded), and that no earlier message of the request's transcript had loaded
// (original, the client's body). Each tool counts once, however many results load it.
//
// A tool Claude Code deferred is not in deferred: not our guess. A tool loaded earlier in the
// thread was counted at its first load: a search again is not a new wrong guess. /stats and the
// dashboard both count with this function, so their numbers agree.
func WrongGuesses(deferred []string, original []byte, loaded []string) int {
	if len(deferred) == 0 || len(loaded) == 0 {
		return 0
	}
	ours := make(map[string]bool, len(deferred))
	for _, n := range deferred {
		ours[n] = true
	}
	before := Loaded(original)
	n := 0
	for _, name := range loaded {
		if ours[name] && !before[name] {
			n++
			delete(ours, name) // once per tool
		}
	}
	return n
}

// Money is one request's (or a total's) deferral figures in USD.
type Money struct {
	Gross float64 `json:"gross_usd"`
	Cost  float64 `json:"cost_usd"`
	Net   float64 `json:"net_usd"`
	// SearchCost is the part of Cost that the request body does not show: the search calls' output, the
	// second pass over the loaded definitions, and their write to the cache. The accountant prices a
	// request from its bodies, and the names list is in them, so it sees the rest of Cost on its own
	// (accountant R56). SearchCost is what a host must still subtract from the accountant's figure.
	SearchCost float64 `json:"-"`
}

// searchOutputEstimate is the output tokens one search call costs when usage does not say.
const searchOutputEstimate = 30

// Price prices one request. ok=false when the model has no known price.
func Price(r Request, p modelinfo.Price) (Money, bool) {
	if p.Zero() {
		return Money{}, false
	}
	f, _ := tokens.BilledDeltaFactor(r.Model)
	billed := func(n int) float64 { return float64(n) * f }
	gross := billed(r.DeferredTokens) * rate(p, r.Tier)
	cost := billed(r.ListTokens) * rate(p, r.Tier)
	var search float64 // the cost the bodies do not show; exactly 0 when no search ran
	if r.Searches > 0 {
		out := r.SearchOutput
		if out == 0 {
			out = int64(r.Searches * searchOutputEstimate)
		}
		search += float64(out) * p.Output
		search += billed(r.LoadedTokens) * p.Input
		// The next request carries the loaded definitions in its transcript and writes them to
		// the cache, once per load (#866, N7). The later reads are in that request's own weight.
		w := TierWrite
		if r.Tier == TierWrite1h {
			w = TierWrite1h
		}
		search += billed(r.LoadedTokens) * rate(p, w)
		cost += search
	}
	return Money{Gross: gross, Cost: cost, Net: gross - cost, SearchCost: search}, true
}

// Ledger adds requests up for /stats, in total and per account. Safe for concurrent use.
type Ledger struct {
	mu       sync.Mutex
	total    Stats
	accounts map[string]*Stats
}

// maxLedgerAccounts bounds the per-account totals. Past it, a new account counts in the total only.
const maxLedgerAccounts = 10000

// Add folds one request in.
func (l *Ledger) Add(r Request, p modelinfo.Price) {
	m, ok := Price(r, p)
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[r.Tenant]
	if a == nil && len(l.accounts) < maxLedgerAccounts {
		if l.accounts == nil {
			l.accounts = map[string]*Stats{}
		}
		a = &Stats{}
		l.accounts[r.Tenant] = a
	}
	l.total.add(r, m, ok)
	if a != nil {
		a.add(r, m, ok)
	}
}

// Stats is the ledger as /stats reports it.
type Stats struct {
	Money
	Requests        int64 `json:"requests"`
	Searches        int64 `json:"searches"`
	DeferredTokens  int64 `json:"deferred_tokens"`
	Unpriced        int64 `json:"unpriced_requests"`
	WrongGuesses    int64 `json:"wrong_guesses"`
	DroppedSearches int64 `json:"dropped_searches"`
}

func (s *Stats) add(r Request, m Money, priced bool) {
	s.Requests++
	s.Searches += int64(r.Searches)
	s.DeferredTokens += int64(r.DeferredTokens)
	s.WrongGuesses += int64(r.WrongGuesses)
	s.DroppedSearches += int64(r.DroppedSearches)
	if !priced {
		s.Unpriced++
		return
	}
	s.Gross += m.Gross
	s.Cost += m.Cost
	s.Net += m.Net
}

// Snapshot returns the totals.
func (l *Ledger) Snapshot() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Accounts returns the totals per account ("" is a single-tenant proxy's one account).
func (l *Ledger) Accounts() map[string]Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Stats, len(l.accounts))
	for k, v := range l.accounts {
		out[k] = *v
	}
	return out
}
