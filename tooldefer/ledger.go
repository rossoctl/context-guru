package tooldefer

import (
	"math"
	"sync"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// The ledger counts deferral's requests, searches and tokens, and adds up its dollars.
//
// It does not price anything. One figure prices tool deferral: the accountant's component figure
// (dash AccountantFigure). That figure holds the gross (the definitions left out), the names list,
// the tier re-rate, and the search-turn costs (SearchSpend, booked as spend operations). The proxy
// copies that figure into the ledger when the request is priced (Ledger.AddMoney).
//
// The token weights come from our own tokenizer (o200k), which under-counts Claude's. They are
// scaled by tokens.BilledDeltaFactor, the measured ratio for a removed span, before they go to the
// accountant as a spend.

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
	// PromptTokens is the billed prompt size of the request sent.
	//
	// Deprecated: nothing reads it. The accountant prices deferral (#1441). Kept so a library user's code still builds.
	PromptTokens int64
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

// Money is one request's (or a total's) deferral figures in USD, mapped from the accountant's figure for the
// tooldefer component: Net is the figure, Gross is its saved side (the definitions left out, with their share of
// the tier re-rate), Cost is Gross less Net (the names list, the search turns, and any break deferral caused).
type Money struct {
	Gross float64 `json:"gross_usd"`
	Cost  float64 `json:"cost_usd"`
	Net   float64 `json:"net_usd"`
}

// searchOutputEstimate is the output tokens one search call costs when usage does not say.
const searchOutputEstimate = 30

// SearchSpend is what the search turns of one request cost, as token counts. The host books each part as an
// accountant spend operation on the tooldefer operator. A request that ran no search has none.
type SearchSpend struct {
	// Output is the output tokens of the search calls. OutputEstimated is true when usage did not say.
	Output          int64
	OutputEstimated bool
	// Loaded is the billed weight of the definitions the searches loaded. The API's second pass reads them once
	// WITHOUT the cache (Input, an estimate), and the next request writes them to the cache once (Write, an
	// estimate: that request has not happened yet).
	Loaded int64
	// Write1h is true when the next request's write is at the 1-hour price.
	Write1h bool
}

// SearchSpend returns the search-turn costs of r. ok is false when no search ran.
func (r Request) SearchSpend() (s SearchSpend, ok bool) {
	if r.Searches <= 0 {
		return SearchSpend{}, false
	}
	f, _ := tokens.BilledDeltaFactor(r.Model)
	s.Output = r.SearchOutput
	if s.Output == 0 {
		s.Output, s.OutputEstimated = int64(r.Searches*searchOutputEstimate), true
	}
	s.Loaded = int64(math.Round(float64(r.LoadedTokens) * f))
	// The next request carries the loaded definitions in its transcript and writes them to the cache, once per
	// load (#866, N7). The later reads are in that request's own weight.
	s.Write1h = r.Tier == TierWrite1h
	return s, true
}

// Ledger adds requests up for /stats, in total and per account. Safe for concurrent use.
type Ledger struct {
	mu       sync.Mutex
	total    Stats
	accounts map[string]*Stats
}

// maxLedgerAccounts bounds the per-account totals. Past it, a new account counts in the total only.
const maxLedgerAccounts = 10000

// Add folds one request's counts in. Its dollars follow with AddMoney, once the accountant has priced it.
func (l *Ledger) Add(r Request) {
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
	l.total.add(r)
	if a != nil {
		a.add(r)
	}
}

// AddMoney folds one request's dollars in: the accountant's figure for it. priced is false when the request has
// no figure (an unpriced model, or a request the accountant did not price): its dollars read 0, not free, and it
// counts in Unpriced.
func (l *Ledger) AddMoney(tenant string, m Money, priced bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total.addMoney(m, priced)
	if a := l.accounts[tenant]; a != nil {
		a.addMoney(m, priced)
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

func (s *Stats) add(r Request) {
	s.Requests++
	s.Searches += int64(r.Searches)
	s.DeferredTokens += int64(r.DeferredTokens)
	s.WrongGuesses += int64(r.WrongGuesses)
	s.DroppedSearches += int64(r.DroppedSearches)
}

func (s *Stats) addMoney(m Money, priced bool) {
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
