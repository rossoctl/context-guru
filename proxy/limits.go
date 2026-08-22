package proxy

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"
)

// Per-tenant limits, and the shared-resource ceilings behind them.
//
// A hosted proxy has three scarce things a single tenant can exhaust for everyone
// else, and each needs a different bound:
//
//   - REQUEST RATE — cheap to send, and an agent in a retry loop can send thousands.
//     Bounded per tenant so one runaway does not crowd out everyone's traffic.
//   - CONCURRENCY — each in-flight request buffers a body (up to 32 MiB) and holds a
//     goroutine, so concurrency is really a memory bound.
//   - MONEY — the upstream credential is the OPERATOR's, so an unbounded tenant spends
//     the organisation's budget. This one is not a nice-to-have: it is the direct
//     consequence of not asking users for their own keys.
//
// All three fail with a status and a message that says which limit was hit and what to
// do, because "429" with no explanation is indistinguishable from the service being
// broken, and the user cannot see our logs.

// Limits configures the per-tenant bounds. Zero values disable a bound rather than
// defaulting to something small — a limit nobody asked for that silently throttles an
// agent is worse than no limit, because it presents as the tool being slow.
type Limits struct {
	// RequestsPerMinute bounds one tenant's request rate. 0 = unlimited.
	RequestsPerMinute int
	// Concurrent bounds one tenant's in-flight requests. 0 = unlimited.
	Concurrent int
	// CheapModelConcurrent bounds cheap-model (compaction) calls across the WHOLE
	// process. Not per tenant: the point is to stop one tenant's extract_llm traffic
	// from making every other tenant's agent wait on a shared, rate-limited backend.
	// 0 = unlimited.
	CheapModelConcurrent int
}

// tenantLimiter holds one tenant's live counters.
type tenantLimiter struct {
	mu sync.Mutex
	// A fixed-window counter, not a token bucket. Deliberate: the window resets on a
	// wall-clock minute so the error message can say exactly when the tenant may
	// retry, which a leaky bucket cannot express. The cost is that a tenant can burst
	// through two windows back to back; for protecting a shared box from a runaway
	// agent that is entirely adequate, and a bucket is upgrade-shaped if it is not.
	windowStart time.Time
	count       int

	// inFlight is a counting semaphore over concurrency.
	inFlight chan struct{}
}

// Limiter enforces per-tenant rate and concurrency limits.
type Limiter struct {
	lim Limits

	// perTenant is true when Acquire's key is a registry tenant id, so a refusal may be
	// labelled with it. False for the registration limiter, whose keys are client IPs —
	// attacker-supplied, and therefore something that must never become a metric label.
	perTenant bool

	mu       sync.Mutex
	tenants  *lru[*tenantLimiter]
	cheapSem chan struct{}
	// actions is the per-hour counter for deliberate acts (AllowAction), separate from
	// tenants so a settings click cannot evict a live request counter.
	actions *lru[*hourly]
}

// maxLimiterKeys bounds the live counters a limiter holds. Reached only by a
// long-lived process accumulating entries for keys that stopped appearing — except
// for the registration limiter, whose keys are client IPs and therefore ARE
// attacker-supplied, which is why the bound evicts the least-recently-used entry
// instead of clearing the map.
const maxLimiterKeys = 10000

// NewLimiter builds a limiter. A nil result is a working no-op, so callers never
// have to nil-check at the call site.
func NewLimiter(l Limits) *Limiter { return newLimiter(l, true) }

// newAnonLimiter builds a limiter whose keys are NOT tenant ids (the registration
// limiter keys on client IP). Its refusals are counted process-wide only.
func newAnonLimiter(l Limits) *Limiter { return newLimiter(l, false) }

func newLimiter(l Limits, perTenant bool) *Limiter {
	lm := &Limiter{lim: l, perTenant: perTenant,
		tenants: newLRU[*tenantLimiter](maxLimiterKeys, nil)}
	if l.CheapModelConcurrent > 0 {
		lm.cheapSem = make(chan struct{}, l.CheapModelConcurrent)
	}
	return lm
}

// refused counts a refusal. The key is dropped unless it is a tenant id — see perTenant.
func (l *Limiter) refused(reason refusalReason, key string) {
	if !l.perTenant {
		key = ""
	}
	recordRefusal(reason, key)
}

func (l *Limiter) forTenant(id string) *tenantLimiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tenants.get(id)
	if !ok {
		t = &tenantLimiter{windowStart: time.Now().Truncate(time.Minute)}
		if l.lim.Concurrent > 0 {
			t.inFlight = make(chan struct{}, l.lim.Concurrent)
		}
		l.tenants.put(id, t)
	}
	return t
}

// Acquire checks the rate limit and takes a concurrency slot. The returned release
// function must always be called; it is a no-op when nothing was taken, so
// `defer release()` is correct even on the error path.
func (l *Limiter) Acquire(tenantID string) (release func(), err error) {
	if l == nil || (l.lim.RequestsPerMinute <= 0 && l.lim.Concurrent <= 0) {
		return func() {}, nil
	}
	t := l.forTenant(tenantID)

	if l.lim.RequestsPerMinute > 0 {
		t.mu.Lock()
		now := time.Now()
		if w := now.Truncate(time.Minute); w.After(t.windowStart) {
			t.windowStart, t.count = w, 0
		}
		if t.count >= l.lim.RequestsPerMinute {
			retry := t.windowStart.Add(time.Minute)
			t.mu.Unlock()
			// Counted here rather than where the 429 is written: only this branch knows
			// WHICH 429 this is, and rate limit and concurrency have different fixes.
			// failAuth deliberately does not count 429s, so this is exactly once.
			l.refused(refuseRateLimit, tenantID)
			return func() {}, statusError{http.StatusTooManyRequests, fmt.Sprintf(
				"rate limit: %d requests/minute for this account; retry in %ds",
				l.lim.RequestsPerMinute, int(time.Until(retry).Seconds())+1)}
		}
		t.count++
		t.mu.Unlock()
	}

	if t.inFlight != nil {
		select {
		case t.inFlight <- struct{}{}:
			return func() { <-t.inFlight }, nil
		default:
			// Refuse rather than queue. A queued agent request looks like a hung agent,
			// and an explicit 429 it can retry is more useful than a stall — especially
			// since the agent's own deadline is shorter than our patience.
			l.refused(refuseConcurrency, tenantID)
			return func() {}, statusError{http.StatusTooManyRequests, fmt.Sprintf(
				"concurrency limit: %d requests in flight for this account", l.lim.Concurrent)}
		}
	}
	return func() {}, nil
}

// AcquireSpare is Acquire for traffic that is OURS rather than the agent's: it succeeds only
// out of the tenant's slack, leaving reserveFrac of both bounds untouched.
//
// The idle keep-alive needs this. A ping is a real request against the tenant's rate and
// concurrency budget, and the requirement is that it never crowd out a request an agent is
// waiting on. Refusing rather than queueing is half of that — a queued ping sits in front of
// real traffic — but with a fixed-window counter refusing at the limit is not enough either:
// a ping that takes the last request of the minute has already displaced whatever arrives
// next. So a ping is only allowed while the tenant is comfortably inside its bounds, and is
// simply skipped otherwise. Skipping costs one idle span's refresh; displacing costs an agent
// a 429 it did not earn.
//
// reserveFrac <= 0 makes this identical to Acquire.
func (l *Limiter) AcquireSpare(tenantID string, reserveFrac float64) (release func(), err error) {
	if l == nil || (l.lim.RequestsPerMinute <= 0 && l.lim.Concurrent <= 0) {
		return func() {}, nil
	}
	t := l.forTenant(tenantID)

	// CONCURRENCY FIRST, then the rate window. The reverse order charged the tenant's
	// per-minute budget for pings it then refused on concurrency: ten refused pings consumed
	// ten of a hundred requests a minute, and it fired precisely when the tenant was busiest
	// and the refusals most likely. The rate counter is only incremented once the request is
	// certain to be sent.
	release = func() {}
	if t.inFlight != nil {
		// The slot is taken under the tenant's lock together with the headroom test, so two
		// pings cannot both read "there is room" and then both take the last spare slot. The
		// earlier version tested len() outside any lock and raced pings against each other as
		// well as against real traffic.
		t.mu.Lock()
		spare := reserved(cap(t.inFlight), reserveFrac)
		if len(t.inFlight) >= spare {
			t.mu.Unlock()
			// NOT counted as a refusal: nobody was refused. A skipped ping is our own decision
			// not to spend a tenant's budget, and publishing it as a rate-limit event would make
			// a healthy account look throttled on its own dashboard.
			return func() {}, errNoSpare
		}
		select {
		case t.inFlight <- struct{}{}:
			release = func() { <-t.inFlight }
		default:
			t.mu.Unlock()
			return func() {}, errNoSpare
		}
		t.mu.Unlock()
	}

	if l.lim.RequestsPerMinute > 0 {
		limit := reserved(l.lim.RequestsPerMinute, reserveFrac)
		t.mu.Lock()
		now := time.Now()
		if w := now.Truncate(time.Minute); w.After(t.windowStart) {
			t.windowStart, t.count = w, 0
		}
		if t.count >= limit {
			t.mu.Unlock()
			release() // give the concurrency slot straight back; this ping is not being sent
			return func() {}, errNoSpare
		}
		t.count++
		t.mu.Unlock()
	}
	return release, nil
}

// errNoSpare says a tenant had no slack for a request of ours. Deliberately not a
// statusError: nothing is being returned to a caller, and this is not a failure.
var errNoSpare = errors.New("no spare capacity for a background request")

// reserved is how much of a bound background traffic may use: the bound minus its reserved
// share, rounded so the reservation is never less than one.
//
// Floored at ZERO, not at one. A floor of one meant that at Concurrent: 1 a ping was allowed to
// take the only slot and a real request arriving behind it got a 429 — the exact outcome
// "a ping must never crowd out a real request" forbids. A tenant whose whole budget is one
// in-flight request has no slack, and the honest answer is that it is never pinged.
func reserved(limit int, frac float64) int {
	if frac <= 0 {
		return limit
	}
	n := limit - int(math.Ceil(float64(limit)*frac))
	if n < 0 {
		n = 0
	}
	return n
}

// AcquireCheapModel takes a slot on the process-wide compaction-model semaphore,
// blocking until one is free or the caller's deadline passes. Blocking rather than
// refusing, because the alternative to waiting here is not compacting — and a request
// that skips compaction is correct, just more expensive. Returns false if the wait was
// abandoned, in which case the caller must proceed without compaction.
func (l *Limiter) AcquireCheapModel(done <-chan struct{}) (release func(), ok bool) {
	if l == nil || l.cheapSem == nil {
		return func() {}, true
	}
	select {
	case l.cheapSem <- struct{}{}:
		return func() { <-l.cheapSem }, true
	case <-done:
		return func() {}, false
	}
}

// SpendChecker reports a tenant's month-to-date cost, for display. Not a cap: each
// tenant spends their own provider credential, so there is no shared budget to guard.
type SpendChecker interface {
	// MonthToDateUSD returns what this tenant has spent since the start of the
	// current calendar month.
	MonthToDateUSD(tenantID string) (float64, error)
}

// hourly is one key's fixed-window counter for a deliberate ACT rather than for traffic.
type hourly struct {
	windowStart time.Time
	count       int
}

// AllowAction rate-limits a deliberate act — arming a keep-alive override is the one caller —
// on a per-hour fixed window, and returns an error naming when the caller may retry.
//
// Separate from Acquire because it is a different kind of limit on a different thing. Acquire
// bounds request THROUGHPUT per minute and hands back a concurrency slot; this bounds how often
// somebody may authorize a spend, has no concurrency half, and takes nothing that has to be
// released. Sharing the per-minute window would have made "30 arms an hour" unexpressible and
// would have charged a settings click against an agent's request budget.
//
// A fixed window for the same reason Acquire uses one: the refusal can say exactly when the
// window resets, which a leaky bucket cannot. It is also its own map, so an arm cannot evict a
// live tenantLimiter holding an in-flight semaphore.
func (l *Limiter) AllowAction(key, what string, perHour int) error {
	if l == nil || perHour <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.actions == nil {
		l.actions = newLRU[*hourly](maxLimiterKeys, nil)
	}
	h, ok := l.actions.get(key)
	if !ok {
		h = &hourly{}
		l.actions.put(key, h)
	}
	now := time.Now()
	if w := now.Truncate(time.Hour); w.After(h.windowStart) {
		h.windowStart, h.count = w, 0
	}
	if h.count >= perHour {
		retry := int(time.Until(h.windowStart.Add(time.Hour)).Seconds()) + 1
		return statusError{http.StatusTooManyRequests, fmt.Sprintf(
			"rate limit: %d %s per hour for this account; retry in %ds", perHour, what, retry)}
	}
	h.count++
	return nil
}
