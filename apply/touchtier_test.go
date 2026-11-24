package apply

import (
	"context"
	"strconv"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

// #1350. The cold check counts the tier of the LAST touch: a free 1-hour keep-alive ping leaves the entry at
// 1 hour, and the next real request must not read it as expired after 5 minutes.

var touchTierBody = []byte(`{"model":"claude-sonnet-5","messages":[` +
	`{"role":"user","content":[{"type":"text","text":"fix the failing test","cache_control":{"type":"ephemeral"}}]},` +
	`{"role":"user","content":"more"}]}`)

type touchStep struct {
	at      time.Duration // after the first request
	ping    bool          // a keep-alive touch, not a real request
	oneHour bool          // the tier a ping left the entry at
}

// runTouches plays the steps and returns the ColdCache of every REAL request, in order.
func runTouches(t *testing.T, provider bschemas.ModelProvider, model string, steps []touchStep) []bool {
	t.Helper()
	var cold []bool
	pipe := components.NewPipeline([]components.Component{coldSpy{&cold}}, nil)
	st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
	body := touchTierBody
	if model != "" {
		body = []byte(`{"model":"` + model + `","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"fix the failing test","cache_control":{"type":"ephemeral"}}]},` +
			`{"role":"user","content":"more"}]}`)
	}
	base := time.Unix(1_700_000_000, 0)
	run := func(at time.Duration) {
		BodyOpts(context.Background(), pipe, st, Opts{Provider: provider, Body: body, Tracker: tr,
			Tenant: "t", Now: base.Add(at)})
		// The host's Record confirms an accepted request: it ends the carried 1-hour ping.
		ConfirmRealTouch(st, TouchAliasFor("t", body, provider))
	}
	run(0)
	cold = nil
	for _, s := range steps {
		if s.ping {
			RecordCacheTouchTier(st, "t", body, provider, base.Add(s.at).UnixMilli(), s.oneHour)
			continue
		}
		run(s.at)
	}
	return cold
}

func TestACheckAfterAFreeOneHourPingCountsAnHourFromIt(t *testing.T) {
	m := time.Minute
	for _, tc := range []struct {
		name  string
		steps []touchStep
		want  bool
	}{
		{"a 5-minute ping: cold 7 minutes on", []touchStep{{4 * m, true, false}, {11 * m, false, false}}, true},
		{"a free 1-hour ping: warm 9 minutes on", []touchStep{{4 * m, true, true}, {13 * m, false, false}}, false},
		{"a free 1-hour ping: warm 59 minutes on", []touchStep{{4 * m, true, true}, {63 * m, false, false}}, false},
		{"a free 1-hour ping: cold past the hour and the margin", []touchStep{{4 * m, true, true}, {66 * m, false, false}}, true},
		// The last touch counts, not the longest: a 5-minute ping after the 1-hour ping moves the entry back (#1258).
		{"a 5-minute ping after a 1-hour ping: cold again", []touchStep{{4 * m, true, true}, {6 * m, true, false}, {13 * m, false, false}}, true},
		{"a 1-hour ping after a 5-minute ping: warm", []touchStep{{4 * m, true, false}, {8 * m, true, true}, {17 * m, false, false}}, false},
		// A real request is a touch of its own: the tier ends there.
		{"a real request ends the 1-hour tier", []touchStep{{4 * m, true, true}, {13 * m, false, false}, {25 * m, false, false}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runTouches(t, bschemas.Anthropic, "", tc.steps)
			if len(got) == 0 || got[len(got)-1] != tc.want {
				t.Fatalf("ColdCache per real request = %v, want the last to be %v", got, tc.want)
			}
		})
	}
}

// A provider with a fixed lifetime takes it from the one table, whatever tier a touch names. GPT-5.6+ on
// OpenAI keeps 30 minutes: its cold gate never fires on a minimum, and a "1-hour" touch changes nothing.
func TestTheLifetimeComesFromTheOneTable(t *testing.T) {
	for _, tc := range []struct {
		provider bschemas.ModelProvider
		model    string
		oneHour  bool
		want     time.Duration
	}{
		{bschemas.Anthropic, "claude-sonnet-5", false, 5 * time.Minute},
		{bschemas.Anthropic, "claude-sonnet-5", true, time.Hour},
		{bschemas.Bedrock, "aws/claude-sonnet-5", true, time.Hour},
		{bschemas.OpenAI, "gpt-5.6", false, 30 * time.Minute},
		{bschemas.OpenAI, "gpt-5.6", true, 30 * time.Minute},
	} {
		if got := EntryLifetime(CacheFacts{Provider: tc.provider, Model: tc.model, LastOneHour: tc.oneHour}); got != tc.want {
			t.Errorf("EntryLifetime(%s, %s, %v) = %v, want %v", tc.provider, tc.model, tc.oneHour, got, tc.want)
		}
	}
	// And the cold check on that model: a 1-hour touch does not make it declare anything.
	got := runTouches(t, bschemas.OpenAI, "gpt-5.6", []touchStep{{4 * time.Minute, true, true}, {13 * time.Minute, false, false}, {200 * time.Minute, false, false}})
	for i, c := range got {
		if c {
			t.Errorf("request %d on a 30-minute minimum read cold: %v", i, got)
		}
	}
}

// The tier rides in the clock's own record, so a real request (plain value) clears it, and an older touch
// never moves it back.
func TestTheTierRecordIsMonotoneAndReadable(t *testing.T) {
	st := store.NewMemory(store.Options{})
	RecordCacheTouchAliasTier(st, "a", 2000, true)
	RecordCacheTouchAliasTier(st, "a", 1000, false) // older: ignored
	b, _ := st.Get(store.SeenPrefix + "a")
	if ms, oneHour := parseSeen(b); ms != 2000 || !oneHour {
		t.Fatalf("record = %q, want 2000 at 1 hour", b)
	}
	RecordCacheTouchAliasTier(st, "a", 3000, false) // a later 5-minute touch ends the tier
	b, _ = st.Get(store.SeenPrefix + "a")
	if ms, oneHour := parseSeen(b); ms != 3000 || oneHour {
		t.Fatalf("record = %q, want 3000 at 5 minutes", b)
	}
	if _, err := strconv.ParseInt(string(b), 10, 64); err != nil {
		t.Errorf("a 5-minute touch must stay a plain integer, got %q", b)
	}
	if prev, oneHour := aliasSeenTier(st, "a", 4000); prev != 3000 || oneHour {
		t.Errorf("aliasSeenTier = %d %v", prev, oneHour)
	}
	if b, _ = st.Get(store.SeenPrefix + "a"); string(b) != "4000" {
		t.Errorf("a real request left %q, want a plain 4000", b)
	}
}

// A sub-agent's real request on the same session has its own prefix, so its own clock. It must not end the
// main thread's 1-hour tier, and the main thread's return must still read warm.
func TestASubAgentRequestDoesNotEndTheMainThreadsOneHourTier(t *testing.T) {
	var cold []bool
	pipe := components.NewPipeline([]components.Component{coldSpy{&cold}}, nil)
	st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
	mainBody := touchTierBody
	subBody := []byte(`{"model":"claude-haiku-5","system":"you are a sub-agent that searches files","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"find the config","cache_control":{"type":"ephemeral"}}]}]}`)
	base := time.Unix(1_700_000_000, 0)
	run := func(body []byte, at time.Duration) {
		BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, Body: body, Tracker: tr,
			Tenant: "t", Session: "cc-abc", Now: base.Add(at)})
	}
	run(mainBody, 0)
	RecordCacheTouchTier(st, "t", mainBody, bschemas.Anthropic, base.Add(4*time.Minute).UnixMilli(), true)
	run(subBody, 6*time.Minute) // a sub-agent turn on the same session
	run(subBody, 9*time.Minute) // and another
	cold = nil
	run(mainBody, 13*time.Minute)
	if len(cold) != 1 || cold[0] {
		t.Fatalf("the main thread's return read cold after sub-agent turns on its session: %v", cold)
	}
}

// A reader that does not know the tier suffix (an older binary on the same store after a downgrade) parses
// the value as an integer, fails, and reads 0: "no previous touch", which is never cold. It then writes a plain
// value. The new reader never fails on the old format.
func TestTheTierSuffixFailsOpenForAnOlderReader(t *testing.T) {
	if _, err := strconv.ParseInt("1700000000000:1h", 10, 64); err == nil {
		t.Fatal("fixture: the suffixed value must not parse as a plain integer")
	}
	if ms, oneHour := parseSeen([]byte("1700000000000")); ms != 1700000000000 || oneHour {
		t.Errorf("a plain value read as %d %v", ms, oneHour)
	}
	if ms, oneHour := parseSeen([]byte("garbage:1h")); ms != 0 || oneHour {
		t.Errorf("an unreadable value read as %d %v, want 0 (unknown, never cold)", ms, oneHour)
	}
	if cacheIsCold(0, 1700000000000, 5*time.Minute) {
		t.Error("an unknown last touch read cold")
	}
	// The carried shape ("<ms>^<pingMs>") fails open for an older reader the same way. The older reader is
	// `prev, _ = strconv.ParseInt(value)`: an error leaves 0, which is "no previous touch".
	for _, v := range []string{"1700000000000:1h", "1700000000000^1699999990000"} {
		legacy, err := strconv.ParseInt(v, 10, 64)
		if err == nil || legacy != 0 {
			t.Errorf("an older reader read %q as %d (err %v), want 0 with an error", v, legacy, err)
		}
		if cacheIsCold(legacy, 1700000000000+int64(time.Hour/time.Millisecond), 5*time.Minute) {
			t.Errorf("an older reader would read %q cold", v)
		}
	}
}

func TestEndTouchTierKeepsTheTimeAndLeavesALaterTouchAlone(t *testing.T) {
	st := store.NewMemory(store.Options{})
	RecordCacheTouchAliasTier(st, "a", 2000, true)
	EndTouchTier(st, "a", 1000) // another touch: nothing
	if b, _ := st.Get(store.SeenPrefix + "a"); string(b) != "2000:1h" {
		t.Fatalf("a different touch's end changed the record: %q", b)
	}
	EndTouchTier(st, "a", 2000)
	if b, _ := st.Get(store.SeenPrefix + "a"); string(b) != "2000" {
		t.Fatalf("record = %q, want a plain 2000", b)
	}
}

func TestEntryLifetimeIsTheOneDecision(t *testing.T) {
	a, m := bschemas.Anthropic, "claude-sonnet-5"
	for _, tc := range []struct {
		name string
		f    CacheFacts
		want time.Duration
	}{
		{"base", CacheFacts{Provider: a, Model: m}, 5 * time.Minute},
		{"last touch at 1h", CacheFacts{Provider: a, Model: m, LastOneHour: true}, time.Hour},
		{"billed at 1h", CacheFacts{Provider: a, Model: m, BilledOneHour: true}, time.Hour},
		{"a guard's floor is never undercut", CacheFacts{Provider: a, Model: m, Floor: time.Hour}, time.Hour},
		{"OpenAI GPT-5.6 keeps its minimum", CacheFacts{Provider: bschemas.OpenAI, Model: "gpt-5.6", LastOneHour: true}, 30 * time.Minute},
	} {
		if got := EntryLifetime(tc.f); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if IsCold(CacheFacts{Provider: a, Model: m, LastOneHour: true}, 1000, 1000+int64(30*time.Minute/time.Millisecond)) {
		t.Error("30 minutes after a 1-hour touch read cold")
	}
}

// The miss attribution reads the lifetime through BilledLifetimeMs: the proxy passes two facts, the tier the last
// write was billed at and whether the last touch was a live free 1-hour ping, and apply decides. Either one says 1
// hour. A minimum (OpenAI GPT-5.6) is not an exact lifetime: 0 for all four.
func TestBilledLifetimeMsDecidesFromTheTwoFacts(t *testing.T) {
	const m5, h1 = int64(300_000), int64(3_600_000)
	for _, tc := range []struct {
		provider bschemas.ModelProvider
		model    string
		want     [4]int64 // billed false/refreshed false, false/true, true/false, true/true
	}{
		{bschemas.Anthropic, "claude-sonnet-5", [4]int64{m5, h1, h1, h1}},
		{bschemas.Bedrock, "aws/claude-sonnet-5", [4]int64{m5, h1, h1, h1}},
		{bschemas.OpenAI, "gpt-5.6", [4]int64{0, 0, 0, 0}},
	} {
		for i, w := range tc.want {
			billed, refreshed := i >= 2, i%2 == 1
			if got := BilledLifetimeMs(tc.provider, tc.model, billed, refreshed); got != w {
				t.Errorf("BilledLifetimeMs(%s, %s, billed=%v, refreshed=%v) = %d, want %d", tc.provider, tc.model, billed, refreshed, got, w)
			}
		}
	}
}

// A real request writes its own clock but carries the 1-hour ping that stood before it, until the host confirms the
// request was accepted. Chained unconfirmed requests keep carrying it. A new touch (a ping) replaces it.
func TestARealRequestCarriesTheOneHourPingUntilConfirmed(t *testing.T) {
	st := store.NewMemory(store.Options{})
	get := func() string { b, _ := st.Get(store.SeenPrefix + "a"); return string(b) }
	RecordCacheTouchAliasTier(st, "a", 2000, true)
	aliasSeenState(st, "a", 3000)
	if got := get(); got != "3000^2000" {
		t.Fatalf("record = %q, want 3000^2000", got)
	}
	aliasSeenState(st, "a", 4000)
	if got := get(); got != "4000^2000" {
		t.Fatalf("a second unconfirmed request: %q, want 4000^2000", got)
	}
	ConfirmRealTouch(st, "a")
	if got := get(); got != "4000" {
		t.Fatalf("after the confirm: %q, want a plain 4000", got)
	}
	ConfirmRealTouch(st, "a") // nothing carried: nothing changes
	if got := get(); got != "4000" {
		t.Fatalf("a second confirm changed the record: %q", got)
	}
	aliasSeenState(st, "a", 5000)
	RecordCacheTouchAliasTier(st, "a", 6000, false) // a later ping is the last touch, with no carry
	if got := get(); got != "6000" {
		t.Fatalf("a later 5-minute touch: %q, want 6000", got)
	}
	// An older binary parses none of the suffixed shapes and reads 0: never cold.
	if v := parseSeenState([]byte("3000^2000")); v.at != 3000 || v.carriedAt != 2000 {
		t.Errorf("parse = %+v", v)
	}
	if _, err := strconv.ParseInt("3000^2000", 10, 64); err == nil {
		t.Error("fixture: the carried value must not parse as a plain integer")
	}
	if v := parseSeenState([]byte("3000^x")); v != (seenState{}) {
		t.Errorf("an unreadable carry read as %+v", v)
	}
}

func TestEndTouchTierEndsACarriedPingToo(t *testing.T) {
	st := store.NewMemory(store.Options{})
	get := func() string { b, _ := st.Get(store.SeenPrefix + "a"); return string(b) }
	RecordCacheTouchAliasTier(st, "a", 2000, true)
	aliasSeenState(st, "a", 3000) // a real request: "3000^2000"
	EndTouchTier(st, "a", 1999)   // another ping: nothing
	if got := get(); got != "3000^2000" {
		t.Fatalf("a different ping's end changed the record: %q", got)
	}
	EndTouchTier(st, "a", 2000)
	if got := get(); got != "3000" {
		t.Fatalf("record = %q, want a plain 3000", got)
	}
}

// A 1-hour tier that a LATER ping wrote (another thread's, on the same alias) is not the confirmed request's to
// end. The ping writes ":1h" after the request started; the confirm leaves it, and a return at +13 minutes is warm.
func TestConfirmRealTouchLeavesALaterPingsTier(t *testing.T) {
	var cold []bool
	pipe := components.NewPipeline([]components.Component{coldSpy{&cold}}, nil)
	st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
	base := time.Unix(1_700_000_000, 0)
	alias := TouchAliasFor("t", touchTierBody, bschemas.Anthropic)
	do := func(at time.Duration) {
		BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, Body: touchTierBody, Tracker: tr,
			Tenant: "t", Now: base.Add(at)})
	}
	do(0)
	ConfirmRealTouch(st, alias)
	do(5 * time.Minute)                                                                                         // a real request starts
	RecordCacheTouchTier(st, "t", touchTierBody, bschemas.Anthropic, base.Add(6*time.Minute).UnixMilli(), true) // a later ping
	ConfirmRealTouch(st, alias)                                                                                 // the request is accepted
	if b, _ := st.Get(store.SeenPrefix + alias); string(b) != strconv.FormatInt(base.Add(6*time.Minute).UnixMilli(), 10)+":1h" {
		t.Fatalf("the confirm changed a later ping's tier: %q", b)
	}
	cold = nil
	do(13 * time.Minute)
	if len(cold) != 1 || cold[0] {
		t.Fatalf("a return 7 minutes after the later ping read cold: %v", cold)
	}
}

// The carry follows the same rules as the verdict: it ends at the hour plus the cold margin, and it does not
// outlive a request that moved the entry without writing the alias clock (a bypassed one).
func TestTheCarriedPingHasTheColdMarginAndTheLatestClockGuard(t *testing.T) {
	m := time.Minute
	run := func(returnAt time.Duration, bypassAt time.Duration) bool {
		var cold []bool
		pipe := components.NewPipeline([]components.Component{coldSpy{&cold}}, nil)
		st, tr := store.NewMemory(store.Options{}), modes.NewTracker(0)
		base := time.Unix(1_700_000_000, 0)
		do := func(at time.Duration, bypass bool) {
			BodyOpts(context.Background(), pipe, st, Opts{Provider: bschemas.Anthropic, Body: touchTierBody, Tracker: tr,
				Tenant: "t", Now: base.Add(at), Bypass: bypass})
		}
		do(0, false)
		ConfirmRealTouch(st, TouchAliasFor("t", touchTierBody, bschemas.Anthropic))
		RecordCacheTouchTier(st, "t", touchTierBody, bschemas.Anthropic, base.Add(4*m).UnixMilli(), true)
		do(6*m, false) // fails: never confirmed, so the ping's hour is carried
		if bypassAt > 0 {
			do(bypassAt, true)
		}
		cold = nil
		do(returnAt, false)
		return cold[len(cold)-1]
	}
	if run(4*m+60*m+30*time.Second, 0) {
		t.Error("30 seconds past the hour, inside the margin, read cold")
	}
	if !run(4*m+60*m+70*time.Second, 0) {
		t.Error("past the hour and the margin read warm")
	}
	if !run(16*m, 8*m) {
		t.Error("a bypassed request moved the entry to 5 minutes at 8 minutes; the carry still said warm at 16")
	}
}
