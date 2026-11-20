package proxy

import (
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/components"
)

// #1243: the rescue of a 1-hour-on-wait ping.

var ohT0 = time.Unix(1_700_000_000, 0)

func ohPing(read int) oneHourPing {
	return oneHourPing{window: "ka:main:2023-11-14T22:13:20Z", at: ohT0.Add(240 * time.Second), read: read}
}

func rescueOf(res accountant.Result) *accountant.CacheEffect {
	for i, e := range res.CacheEffects {
		if e.Kind == accountant.EffectRescue {
			return &res.CacheEffects[i]
		}
	}
	return nil
}

// A return more than 5 minutes after the previous real request, inside the ping's hour, books the rescue
// of the tokens both read: a 5-minute rebuild (1.25x) became a read (0.1x).
func TestOneHourPingReturnAfterFiveMinutesBooksTheRescue(t *testing.T) {
	back := ohT0.Add(20 * time.Minute)
	res := oneHourRescue(accountant.Result{}, ohPing(49000), ohT0, true, back, 48000)
	e := rescueOf(res)
	if e == nil {
		t.Fatalf("no rescue booked: %+v", res.CacheEffects)
	}
	if e.ChangeID != "ka:main:2023-11-14T22:13:20Z" {
		t.Errorf("the rescue is on %q, not on the ping's window", e.ChangeID)
	}
	if e.Saved.CacheWrite5m != 48000 || e.Caused.CacheRead != 48000 {
		t.Errorf("saved %+v caused %+v; want 48000 tokens (the smaller read) moved from a 5m write to a read", e.Saved, e.Caused)
	}
	// Priced at 1.25 write and 0.10 read per unit: a saving.
	if gain := 1.25*float64(e.Saved.CacheWrite5m) - 0.10*float64(e.Caused.CacheRead); gain <= 0 {
		t.Errorf("gain %.2f", gain)
	}
}

// A return inside 5 minutes was hot without the ping: no rescue.
func TestOneHourPingReturnInsideFiveMinutesBooksNoRescue(t *testing.T) {
	res := oneHourRescue(accountant.Result{}, ohPing(49000), ohT0, true, ohT0.Add(4*time.Minute), 48000)
	if rescueOf(res) != nil {
		t.Fatal("a rescue was booked for a return inside the 5-minute lifetime")
	}
}

func TestOneHourRescueNeedsEvidence(t *testing.T) {
	back := ohT0.Add(20 * time.Minute)
	if rescueOf(oneHourRescue(accountant.Result{}, ohPing(49000), ohT0, true, back, 0)) != nil {
		t.Error("rescue without a read")
	}
	if rescueOf(oneHourRescue(accountant.Result{}, ohPing(49000), time.Time{}, false, back, 48000)) != nil {
		t.Error("rescue with no previous request to measure the gap from")
	}
	if rescueOf(oneHourRescue(accountant.Result{}, ohPing(49000), ohT0, true, ohT0.Add(2*time.Hour), 48000)) != nil {
		t.Error("rescue for a return more than an hour after the ping")
	}
	if rescueOf(oneHourRescue(accountant.Result{}, ohPing(0), ohT0, true, back, 48000)) != nil {
		t.Error("rescue for a ping that read nothing")
	}
	// The library's own rescue is not doubled.
	lib := accountant.Result{CacheEffects: []accountant.CacheEffect{{ID: "ce:1", Kind: accountant.EffectRescue}}}
	if got := oneHourRescue(lib, ohPing(49000), ohT0, true, back, 48000); len(got.CacheEffects) != 1 {
		t.Errorf("a second rescue was added: %+v", got.CacheEffects)
	}
}

// PingSavings keeps the 1-hour ping for the thread's return; a plain ping keeps none; drop clears it.
func TestPingSavingsRemembersTheOneHourPing(t *testing.T) {
	st := NewPingStore()
	report := func(oneHour bool) components.KeepAliveReport {
		return components.KeepAliveReport{Session: "s", Thread: "", Model: "m", CacheRead: 49000, Status: 200,
			TS: ohT0.Add(241 * time.Second).UnixMilli(), DurationMs: 1000, Window: ohT0, OneHour: oneHour}
	}
	if _, ok := PingSavings(report(false), st); !ok {
		t.Fatal("a plain ping was not priced")
	}
	key := accPingKey("s", "main")
	if _, ok := st.peekOneHour(key); ok {
		t.Fatal("a plain ping was kept as a 1-hour ping")
	}
	if _, ok := PingSavings(report(true), st); !ok {
		t.Fatal("a 1-hour ping was not priced")
	}
	p, ok := st.peekOneHour(key)
	if !ok || p.read != 49000 || p.window == "" {
		t.Fatalf("1-hour ping not kept: %+v %v", p, ok)
	}
	st.drop(key, p.window)
	if _, ok := st.peekOneHour(key); ok {
		t.Error("the 1-hour ping stayed after its window was dropped")
	}
}

// The start store gives each return the previous real request's start.
func TestStartStoreSwap(t *testing.T) {
	s := newStartStore()
	if _, ok := s.swap("k", ohT0); ok {
		t.Fatal("a first swap found a previous start")
	}
	if prev, ok := s.swap("k", ohT0.Add(time.Minute)); !ok || !prev.Equal(ohT0) {
		t.Errorf("prev = %v %v", prev, ok)
	}
}

// The capture path: the first return after a 1-hour ping books the rescue; the previous start is the
// one the capture noted.
func TestCaptureBooksTheOneHourRescueOnReturn(t *testing.T) {
	st := NewPingStore()
	sav := newSavingsTracker()
	key := accPingKey("s", "main")
	st.putOneHour(key, ohPing(49000))
	mk := func(at time.Time) *capture {
		return &capture{savings: sav, pings: st, accThread: "main", accMeta: accountant.RequestMeta{Session: "s", At: at}}
	}
	// The real request before the wait.
	if res := mk(ohT0).accOneHourRescue(accountant.Result{}, Usage{CacheRead: 48000}); rescueOf(res) != nil {
		t.Fatal("a rescue on a thread's first request")
	}
	// The return, 20 minutes later.
	res := mk(ohT0.Add(20*time.Minute)).accOneHourRescue(accountant.Result{}, Usage{CacheRead: 48000})
	if e := rescueOf(res); e == nil || e.Saved.CacheWrite5m != 48000 {
		t.Fatalf("no rescue on the return: %+v", res.CacheEffects)
	}
}
