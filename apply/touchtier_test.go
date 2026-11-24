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
		if got := LifetimeAtTier(tc.provider, tc.model, tc.oneHour); got != tc.want {
			t.Errorf("LifetimeAtTier(%s, %s, %v) = %v, want %v", tc.provider, tc.model, tc.oneHour, got, tc.want)
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
