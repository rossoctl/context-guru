package keepalive

import (
	"testing"
	"time"

	"github.com/rossoctl/context-guru/store"
)

// #1380. The 1-hour fact is read from the store. This drives one free 1-hour ping and asks the 3 readers of it
// (the dashboard clock, the manual-ping refusal, the cold check 13 minutes after the first request) for each kind
// of store the proxy can have: a Memory store (every storage level that keeps state), store.Nop (the store is
// switched off, so tn.Store is Nop, never nil in the proxy) and a nil store (a host with none).
func TestTheThreeReadersAtEachStoreKind(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		st                    store.Store
		dash, refused, notCld bool
	}{
		{"memory", store.NewMemory(store.Options{}), true, true, true},
		{"nop", store.Nop{}, false, false, false},
		{"nil", nil, false, false, false},
	} {
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
			_, dash := touchOf(r.k, sess1)
			refused := r.k.OneHourTouchLive(sess1)
			cold, _ := r.request(t0.Add(13 * time.Minute))
			t.Logf("%s: dashboard shows the hour=%v, manual ping refused=%v, cold check warm=%v", tc.name, dash, refused, !cold)
			if dash != tc.dash || refused != tc.refused || !cold != tc.notCld {
				t.Errorf("%s: dashboard=%v refused=%v warm=%v, want %v %v %v", tc.name, dash, refused, !cold, tc.dash, tc.refused, tc.notCld)
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
