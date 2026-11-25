package keepalive

import (
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/store"
)

// #1380. The 1-hour fact is read from the store. This drives one free 1-hour ping and asks the 3 readers of it
// (the dashboard clock, the manual-ping refusal, the cold check 13 minutes after the first request) for each kind
// of store the proxy can have: a Memory store (every storage level that keeps state), store.Nop (the store is
// switched off, so tn.Store is Nop, never nil in the proxy) and a nil store (a host with none).
//
// With a store that keeps nothing, the keeper answers as it did on main: the dashboard shows the hour and the
// manual ping is refused (its own fallback, storeOffAt). The cold check cannot read what was never stored, so it
// reads cold there, on main and now (#1350 is not fixable with no store).
func TestTheThreeReadersAtEachStoreKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   func() store.Store
	}{
		{"memory", func() store.Store { return store.NewMemory(store.Options{}) }},
		{"nop", func() store.Store { return store.Nop{} }},
		{"nil", func() store.Store { return nil }},
	} {
		// Every storage level answers the same: the dashboard shows the hour, the manual ping is refused, and the
		// cold check reads the entry WARM 13 minutes after the first request (9 minutes after the ping), so no
		// cold-only cut applies (#1350). With no store the cold check reads the keeper's record of the ping
		// (apply.Opts.FreePing); with a store it reads the stored clock and never asks the keeper.
		t.Run(tc.name, func(t *testing.T) {
			r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
			r.st = tc.st()
			proveRoute(r.k)
			t0 := r.clock.now()
			r.request(t0)
			r.record(t0, "end_turn", onWaitPolicy())
			r.clock.advance(240 * time.Second)
			r.k.Sweep(r.clock.now())
			waitPings(t, r.k, 1)
			_, dash := touchOf(r.k, sess1)
			refused := r.k.OneHourTouchLive(sess1)
			cold, _ := r.request(t0.Add(13 * time.Minute))
			t.Logf("%s: dashboard shows the hour=%v, manual ping refused=%v, cold check warm=%v", tc.name, dash, refused, !cold)
			if !dash || !refused || cold {
				t.Errorf("%s: dashboard=%v refused=%v warm=%v, want true true true", tc.name, dash, refused, !cold)
			}
		})
		// And the hour ends: 70 minutes after the first request (66 after the ping) the entry has really expired,
		// and the cold check says so at every level.
		t.Run(tc.name+" after the hour", func(t *testing.T) {
			r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
			r.st = tc.st()
			proveRoute(r.k)
			t0 := r.clock.now()
			r.request(t0)
			r.record(t0, "end_turn", onWaitPolicy())
			r.clock.advance(240 * time.Second)
			r.k.Sweep(r.clock.now())
			waitPings(t, r.k, 1)
			if cold, _ := r.request(t0.Add(70 * time.Minute)); !cold {
				t.Errorf("%s: the cold check still reads warm 66 minutes after the ping", tc.name)
			}
		})
	}
}

// When the stored record has expired (a store lifetime shorter than the hour, set by store.ttl_seconds), the 1-hour
// fact is gone from the store: the dashboard and the manual refusal say so, as the cold check does.
func TestAnExpiredStoreRecordShowsNoHour(t *testing.T) {
	r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
	mem := store.NewMemory(store.Options{TTLSeconds: 600})
	mem.SetClock(r.clock.now)
	r.st = mem
	proveRoute(r.k)
	t0 := r.clock.now()
	r.request(t0)
	r.record(t0, "end_turn", onWaitPolicy())
	r.clock.advance(240 * time.Second)
	r.k.Sweep(r.clock.now())
	waitPings(t, r.k, 1)
	if _, ok := touchOf(r.k, sess1); !ok {
		t.Fatal("fixture: no free 1-hour touch")
	}
	r.clock.advance(11 * time.Minute) // 11 minutes without a read or write: the 10-minute record is gone
	_, dash := touchOf(r.k, sess1)
	refused := r.k.OneHourTouchLive(sess1)
	t.Logf("expired record: dashboard shows the hour=%v, manual ping refused=%v", dash, refused)
	if dash || refused {
		t.Errorf("an expired record still shows an hour: dashboard=%v refused=%v", dash, refused)
	}
}

// With no store the cold check follows the same events as with a store (#1350): after a free 1-hour ping, an
// accepted request, a failed request and a route switch-off each leave the cold check where the stored clock
// leaves it. One verdict per event, the same at every storage level.
func TestTheColdCheckFollowsTheSameEventsAtEveryStoreKind(t *testing.T) {
	events := map[string]func(r *coldRig, t0 time.Time){
		"a failed request": func(r *coldRig, t0 time.Time) {
			r.request(t0.Add(6 * time.Minute))
			r.recordStatus(t0.Add(6*time.Minute), 500)
		},
		"an accepted request": func(r *coldRig, t0 time.Time) {
			r.request(t0.Add(6 * time.Minute))
			r.recordStatus(t0.Add(6*time.Minute), 200)
		},
		"a route switch-off": func(r *coldRig, t0 time.Time) {
			r.k.oneHourSwitchOff(oneHourKeyOf(), bschemas.Anthropic, "/v1/messages", "", "aws/claude-sonnet-5", "test")
		},
		"nothing": func(r *coldRig, t0 time.Time) {},
	}
	for name, event := range events {
		verdict := func(st store.Store) bool {
			r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
			r.st = st
			proveRoute(r.k)
			t0 := r.clock.now()
			r.request(t0)
			r.record(t0, "end_turn", onWaitPolicy())
			r.clock.advance(240 * time.Second)
			r.k.Sweep(r.clock.now())
			waitPings(t, r.k, 1)
			event(r, t0)
			cold, _ := r.request(t0.Add(13 * time.Minute))
			return cold
		}
		withStore := verdict(store.NewMemory(store.Options{}))
		if got := verdict(store.Nop{}); got != withStore {
			t.Errorf("after %s: cold=%v with Nop, %v with a store", name, got, withStore)
		}
		if got := verdict(nil); got != withStore {
			t.Errorf("after %s: cold=%v with no store, %v with a store", name, got, withStore)
		}
	}
}

// The same conversation arrives under two session ids: the first request carries the client's session header, the
// return does not (3 of 13 production tenants send both shapes, apply.go). With the store off the cold check must
// still read the entry warm. It asks by the alias, which the body fixes, not by the session id.
func TestTheStoreOffPingIsFoundWhateverSessionIdTheReturnCarries(t *testing.T) {
	for _, st := range []store.Store{store.Nop{}, nil} {
		r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
		r.st = st
		r.session = "client-session-S" // request A carries the header
		proveRoute(r.k)
		t0 := r.clock.now()
		r.request(t0)
		r.session = ""                 // request C, the same conversation without the header: apply derives another session id,
		r.request(t0.Add(time.Second)) // and this id is the one whose tracker the return B will find
		r.record(t0, "end_turn", onWaitPolicy())
		r.clock.advance(240 * time.Second)
		r.k.Sweep(r.clock.now())
		waitPings(t, r.k, 1)
		// Request B, the same conversation, again without the header: its tracker holds C, so only the keeper's
		// ping keeps it warm. The keeper's thread is keyed by another session id than B's.
		if cold, _ := r.request(t0.Add(13 * time.Minute)); cold {
			t.Errorf("%T: a return without the session header read cold inside the free ping's hour", st)
		}
	}
}

// A bypassed request takes no clock step, so with a store it ends nothing (the stored `:1h` stays). With the store
// off it must not end the keeper's record either, or the two storage levels disagree and a cold-only cut applies.
func TestABypassedAcceptedRequestDoesNotEndTheStoreOffPing(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   store.Store
	}{{"memory", store.NewMemory(store.Options{})}, {"nop", store.Nop{}}, {"nil", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newColdRig(t, Usage{CacheRead: 49500, CacheWrite: 0, Output: 1})
			r.st = tc.st
			proveRoute(r.k)
			t0 := r.clock.now()
			r.request(t0)
			r.record(t0, "end_turn", onWaitPolicy())
			r.clock.advance(240 * time.Second)
			r.k.Sweep(r.clock.now())
			waitPings(t, r.k, 1)
			// The bypassed request: accepted, and apply wrote no clock (TouchAtMs 0).
			r.k.Record(Record{Key: sess1, StartedAt: t0.Add(6 * time.Minute), Body: []byte(kaBody), Provider: bschemas.Anthropic,
				Route: "/v1/messages", Status: 200, UsageOK: true, Policy: onWaitPolicy(),
				Usage: Usage{CacheRead: 48576, CacheWrite: 1200, StopReason: "end_turn"}, Header: kaHeader(),
				Hold: fakeOf(r.k).hold(sess1), Store: tc.st, TouchAlias: r.alias})
			if cold, _ := r.request(t0.Add(13 * time.Minute)); cold {
				t.Errorf("%s: a bypassed request ended the ping's hour", tc.name)
			}
		})
	}
}
