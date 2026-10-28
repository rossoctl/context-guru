package keepalive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dispatch"
)

// A manual ping is the user pressing a button: ONE ping of the ordinary shape, sent now, on one
// held thread. It goes through the same Dispatcher, the same body rewrite and the same pricing as
// the scheduled ones, so what it costs and what it reads are what a scheduled ping would have.
//
// It is not one of the thread's K scheduled pings. It does not count against them, it never
// stops the automatic schedule, and it leaves the entry's feedback fields (refreshed, missed)
// alone, so the credit a later real request gets for the scheduled pings is never doubled by the
// button. What it does share with a scheduled ping is the one real effect: a ping that READS the
// cache refreshes the provider's entry, so the automatic clock restarts from it.

var (
	// ErrNotHeld means the keeper holds nothing to ping this thread with: an unknown thread,
	// another tenant's, one the keeper has stopped, or a thread of an account whose keep-alive is
	// off (no body and no credential are kept then, and pinging needs both).
	ErrNotHeld = errors.New("keepalive: nothing held for this thread")
	// ErrBusy means a real request is open on the thread. It refreshes the entry itself, and a
	// second request beside it would only crowd the tenant's budget.
	ErrBusy = errors.New("keepalive: a request is in flight on this thread")
	// ErrLimited means the tenant has no spare rate or concurrency for a ping right now.
	ErrLimited = errors.New("keepalive: no spare capacity for a ping")
	// ErrTooCostly means one ping on this thread would cost more than the policy's per-ping
	// ceiling, the same guard the scheduled path applies (Policy.Ceiling).
	ErrTooCostly = errors.New("keepalive: a ping on this thread costs more than the per-ping ceiling")
)

// TooSoonError is the per-thread rate limit: the thread was manually pinged less than MinGap ago.
type TooSoonError struct{ RetryAfter time.Duration }

func (e *TooSoonError) Error() string {
	return fmt.Sprintf("keepalive: this thread was pinged by hand a moment ago; try again in %s", e.RetryAfter.Round(time.Second))
}

// UpstreamError is a ping the provider answered with a failure, or answered with nothing usable.
type UpstreamError struct{ Status int }

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("keepalive: the provider answered the ping with status %d", e.Status)
}

// ManualRequest asks for one ping on Key's thread.
type ManualRequest struct {
	Key Key
	// MinGap is the least time between two manual pings of this thread; zero is no limit.
	MinGap time.Duration
	// RewriteUSD is what writing the thread's current context into a cold cache would cost: the
	// credit when this ping keeps it warm. The host prices it (it knows the TTL tier the cache
	// was written at); the keeper only carries it onto the report, and zeroes it when the ping
	// read nothing, because then it avoided nothing.
	RewriteUSD float64
}

// ManualResult is what one manual ping did.
type ManualResult struct {
	Status    int
	Usage     Usage
	StartedAt time.Time
	// CostUSD is the ping's own cost; Priced is false when the model has no rate.
	CostUSD float64
	Priced  bool
	// AvoidedUSD is the rewrite the ping saved: ManualRequest.RewriteUSD if it read the cache, else 0.
	AvoidedUSD float64
}

// PingNow sends one ping on r.Key's thread now. It fails open in the sense that matters here: any
// refusal sends nothing and changes nothing, and the scheduled pings are unaffected.
func (k *Keeper) PingNow(r ManualRequest) (ManualResult, error) {
	if k == nil || r.Key.Session == "" {
		return ManualResult{}, ErrNotHeld
	}
	id := r.Key.id()
	now := k.now()
	k.mu.Lock()
	e, ok := k.live[id]
	if !ok || e.stopped || e.manualExpired(now) {
		k.mu.Unlock()
		return ManualResult{}, ErrNotHeld
	}
	if k.inflight[id] > 0 {
		k.mu.Unlock()
		return ManualResult{}, ErrBusy
	}
	if last, ok := k.manualAt[id]; ok && r.MinGap > 0 && now.Sub(last) < r.MinGap {
		k.mu.Unlock()
		return ManualResult{}, &TooSoonError{RetryAfter: r.MinGap - now.Sub(last)}
	}
	// The scheduled path's cost guard applies to a press too: with no limiter configured the
	// per-thread gap is the only other brake, and a huge prefix would pass it every 30 s.
	if e.pingUSD > e.entryCeiling() {
		k.mu.Unlock()
		return ManualResult{}, ErrTooCostly
	}
	// Claimed under the lock, so two presses racing each other send one ping, not two.
	k.noteManualLocked(id, now, r.MinGap)
	// Copied out under the lock for the reason pingJob is: the entry may be retired, its body and
	// credential dropped, while this runs.
	raw := append([]byte(nil), e.body...)
	mask(raw)
	job := pingJob{e: e, raw: raw, hdr: e.hdr.Clone(), key: e.key, ref: e.ref, route: e.route,
		provider: e.provider, model: e.model, tenant: e.tenant, session: e.session,
		thread: e.thread, st: e.st, emitter: e.emitter,
		// The window this press serves starts at the last real request, read before the press
		// below moves e.startedAt.
		prevStartedAt: e.startedAt}
	endpoint, prefix, agent, preset := e.endpoint, e.prefix, e.agent, e.preset
	k.mu.Unlock()
	defer zero(raw)

	release, err := k.spare(r.Key.Tenant)
	if err != nil {
		k.unclaimManual(id, now)
		return ManualResult{}, ErrLimited
	}
	defer release()
	// A press is a ping too: it takes the thread's next step, at dispatch time (nextVaryLocked).
	k.mu.Lock()
	job.vary = k.nextVaryLocked(id)
	k.mu.Unlock()
	body, ok := pingBodyN(raw, job.route, job.vary)
	if !ok {
		k.unclaimManual(id, now)
		return ManualResult{}, ErrNotHeld
	}
	start := k.now()
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	resp, err := k.o.Dispatcher.Do(ctx, dispatch.Request{Purpose: dispatch.PurposeKeepAlivePing,
		Ref: job.ref, Provider: job.provider, Method: http.MethodPost, Path: job.route, Header: job.hdr,
		Body: body})
	cancel()
	zero(body)
	if err != nil {
		// A deadline or cancel may have left a billed request behind, so the gap stays claimed.
		// Any other transport error means nothing reached the provider: let them retry.
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			k.unclaimManual(id, now)
		}
		return ManualResult{}, err
	}
	u := PingUsage(resp.Body)
	res := ManualResult{Status: resp.Status, Usage: u, StartedAt: start}
	switch {
	case resp.Status == http.StatusTooManyRequests || resp.Status >= 500:
		return res, &UpstreamError{Status: resp.Status}
	case resp.Status >= 400:
		// Reported, not acted on. A scheduled ping stops its thread on a 4xx; a button press must not
		// switch off the whole automatic schedule over one answer (a proxy hiccup is enough to give a
		// 400). The route breaker and the stop are the scheduled path's business.
		return res, &UpstreamError{Status: resp.Status}
	case u.FreshInput == 0 && u.CacheRead == 0 && u.CacheWrite == 0:
		return res, &UpstreamError{Status: resp.Status}
	}
	res.CostUSD, res.Priced = k.pingCost(endpoint, job.model, prefix, u)
	if u.CacheRead > 0 {
		res.AvoidedUSD = r.RewriteUSD
		// A ping that read the entry refreshed it; one that wrote refreshed nothing that was there.
		// The same rule record1 applies, and the same two clocks it moves.
		apply.RecordCacheTouch(job.st, job.tenant, raw, job.provider, start.UnixMilli())
		k.mu.Lock()
		e.refreshedAt = start
		// An automatic entry's schedule restarts from a ping that read the cache. A manual-only hold does
		// NOT: its life is measured from the last real request, so a press can never extend how long a
		// credential is held (manualhold.go).
		if !e.manualOnly {
			e.startedAt = start
		}
		k.mu.Unlock()
	}
	k.mu.Lock()
	e.spent += res.CostUSD
	k.mu.Unlock()
	k.addSpent(res.CostUSD)
	if ka, ok := job.emitter.(components.KeepAliveEmitter); ok {
		ka.KeepAlivePing(components.KeepAliveReport{
			Tenant: job.tenant, Session: job.session, Thread: job.thread, Model: job.model,
			Provider: string(job.provider), Route: job.route, CacheRead: u.CacheRead,
			CacheWrite: u.CacheWrite, Output: u.Output, CostUSD: res.CostUSD, Status: resp.Status,
			DurationMs: float64(k.now().Sub(start).Microseconds()) / 1000.0, TS: k.now().UnixMilli(),
			Agent: agent, Preset: preset, StopReason: u.StopReason, FreshInput: u.FreshInput,
			CacheWrite1h: u.CacheWrite1h, MaxTokens: pingOutputBudget(job.route) + job.vary%pingVary,
			Manual: true, Strategy: ManualStrategyID, RewriteUSD: res.AvoidedUSD,
			Window: job.prevStartedAt,
		})
	}
	return res, nil
}

// ManualStrategyID is the keepalive_strategy_id a manual ping's row carries. It names no real
// strategy, so no strategy ledger matches it.
const ManualStrategyID = "manual"

// manualAtMax bounds the per-thread last-press map. It holds one time per thread pressed, so it
// stays tiny; the cap is only so a pathological caller cannot grow it.
const manualAtMax = 4096

func (k *Keeper) noteManualLocked(id string, at time.Time, gap time.Duration) {
	if k.manualAt == nil {
		k.manualAt = map[string]time.Time{}
	}
	if len(k.manualAt) >= manualAtMax {
		// Only expired claims go: dropping a live one would reset its gap.
		for key, t := range k.manualAt {
			if at.Sub(t) >= gap {
				delete(k.manualAt, key)
			}
		}
	}
	k.manualAt[id] = at
}

// unclaimManual releases a press that sent nothing, so a refusal does not cost the user the gap.
func (k *Keeper) unclaimManual(id string, at time.Time) {
	k.mu.Lock()
	if k.manualAt[id].Equal(at) {
		delete(k.manualAt, id)
	}
	k.mu.Unlock()
}
