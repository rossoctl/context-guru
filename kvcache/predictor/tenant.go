package predictor

import (
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// concurrencyWindow is the recency band two sessions must both fall inside to count as
// "concurrent" for FeatureConcurrentSessions.
//
// ponytail: a fixed constant rather than a per-tenant or per-model adaptive window. It is a
// heuristic ceiling, not a measured one — kvcache.Horizon5m is the same band the rest of this
// domain already treats as "still around", chosen for consistency rather than fit. Upgrade
// path: replace with a per-cell median gap if a real model ever needs it.
const concurrencyWindow = kvcache.Horizon5m

// tenantEntry is what TenantStats keeps per tenant.
type tenantEntry struct {
	firstRequestID int64
	firstTS        int64
	totalRequests  int
	// sessions maps a session id to its own last-seen ts and the set of distinct models
	// observed under it — the second half is FeatureModelSwitchedInSession's whole state.
	sessions map[string]*sessionEntry
}

type sessionEntry struct {
	lastTS int64
	models map[string]bool
}

// TenantStats is a running, leak-free accumulator of per-tenant activity: how many distinct
// sessions a tenant has opened so far, whether this is its very first request, how many of
// its OTHER sessions are active right now, and its request rate — every one of them a
// PRESENT-TENSE fact (knowable at Now, the same footing kvcache.Observation.Turn already
// stands on) rather than a prediction.
//
// Structurally leak-free the same way kvcache.History is: Replay calls Observe exactly once
// per request, in wall-clock (ts, id) order, immediately BEFORE building that request's own
// features — so a read always reflects requests up to and including the current one, and
// never a later one. Unlike History, Observe runs on EVERY request, not just on a gap
// closing, because "how many sessions has this tenant opened" is a fact about arrivals, not
// about idle spans.
type TenantStats struct {
	tenants map[string]*tenantEntry
}

// NewTenantStats builds an empty accumulator.
func NewTenantStats() *TenantStats { return &TenantStats{tenants: map[string]*tenantEntry{}} }

// Observe records one request's arrival. Call exactly once per request, in the same
// wall-clock order Replay walks in, before that request's own features are computed.
func (s *TenantStats) Observe(tenant, session, model string, requestID, ts int64) {
	if s == nil {
		return
	}
	e := s.tenants[tenant]
	if e == nil {
		e = &tenantEntry{firstRequestID: requestID, firstTS: ts, sessions: map[string]*sessionEntry{}}
		s.tenants[tenant] = e
	}
	e.totalRequests++
	se := e.sessions[session]
	if se == nil {
		se = &sessionEntry{models: map[string]bool{}}
		e.sessions[session] = se
	}
	se.lastTS = ts
	se.models[model] = true
}

// ColdStart reports whether requestID was the FIRST request this tenant has ever made, as of
// the most recent Observe call for it.
func (s *TenantStats) ColdStart(tenant string, requestID int64) bool {
	if s == nil {
		return false
	}
	e := s.tenants[tenant]
	return e != nil && e.firstRequestID == requestID
}

// DistinctSessions is how many distinct sessions this tenant has opened so far, including the
// one the most recent Observe call belonged to.
func (s *TenantStats) DistinctSessions(tenant string) (int, bool) {
	if s == nil {
		return 0, false
	}
	e := s.tenants[tenant]
	if e == nil {
		return 0, false
	}
	return len(e.sessions), true
}

// RequestRate is this tenant's requests-per-hour so far: total requests observed, divided by
// the wall-clock span since its first one. Absent on a tenant's very first request — a rate
// over a zero-length span is not a rate, it is a division this function refuses to fake.
func (s *TenantStats) RequestRate(tenant string, now int64) (float64, bool) {
	if s == nil {
		return 0, false
	}
	e := s.tenants[tenant]
	if e == nil || now <= e.firstTS {
		return 0, false
	}
	elapsedHours := float64(now-e.firstTS) / float64(time.Hour.Milliseconds())
	return float64(e.totalRequests) / elapsedHours, true
}

// ConcurrentSessions is how many of this tenant's OTHER sessions were last active within
// concurrencyWindow of now — an approximation of "sessions open at the same time" from
// arrival timestamps alone, since nothing in this store records a session's true close.
func (s *TenantStats) ConcurrentSessions(tenant, session string, now int64) int {
	if s == nil {
		return 0
	}
	e := s.tenants[tenant]
	if e == nil {
		return 0
	}
	n := 0
	for sid, se := range e.sessions {
		if sid == session {
			continue
		}
		if time.Duration(now-se.lastTS)*time.Millisecond <= concurrencyWindow {
			n++
		}
	}
	return n
}

// ModelSwitchedInSession reports whether the given session (for this tenant) has now used
// more than one distinct model, as of the most recent Observe call naming it. This is a
// SESSION-scoped fact, deliberately not gated by kvcache.Conversation's own model-in-key
// convention: Conversation.Model exists to stop the SIMULATOR crediting cache reuse across a
// model switch (see Conversation's own doc comment), but detecting the switch itself has to
// look ACROSS the models a client-supplied session id carries, which is exactly what
// TenantStats.sessions here tracks and Conversation deliberately does not.
func (s *TenantStats) ModelSwitchedInSession(tenant, session string) bool {
	if s == nil {
		return false
	}
	e := s.tenants[tenant]
	if e == nil {
		return false
	}
	se := e.sessions[session]
	return se != nil && len(se.models) > 1
}
