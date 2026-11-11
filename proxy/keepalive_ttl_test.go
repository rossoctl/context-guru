package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/keepalive"
)

const ttlBody1h = `{"model":"claude-sonnet-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`

var bill1h = Usage{CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200}

// recordUsage registers one finished request with an explicit usage, after a seeded first turn.
func recordUsage(t *testing.T, k *keeper, pol CachePolicy, body string, at time.Time, u Usage) {
	t.Helper()
	tn := &Tenancy{ID: "t1", Cache: pol}
	for _, d := range []time.Duration{-time.Second, 0} {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		k.record(tn, "sess-1", "", at.Add(d), []byte(body), upstream{base: "http://up", path: "/v1/messages"},
			r, bschemas.Anthropic, "/v1/messages", http.StatusOK, u, true)
	}
}

// An armed override enables a session on an account whose default is off. Its 60-290 s band cannot
// express an hour, so a 1-hour thread keeps the 1-hour timing: no ping at 270 s, one at 59 min. Its
// ping count applies downward: K=4 against the default of 1 still sends 1; K=1 against an account
// K of 3 sends 1 as well.
func TestOverrideOnAOneHourThreadUsesTheOneHourTimingAndBoundsTheCount(t *testing.T) {
	k, _, clock := testKeeper(t, Limits{})
	start := clock.now()
	armOn(t, k, "sess-1", 120*time.Second, 4, start.Add(4*time.Hour))
	recordUsage(t, k, CachePolicy{}, ttlBody1h, start, bill1h)
	if n := k.Sweep(start.Add(270 * time.Second)); n != 0 {
		t.Fatalf("a 1h thread was pinged on the override's 5-minute timing (%d)", n)
	}
	if n := k.Sweep(start.Add(59 * time.Minute)); n != 1 {
		t.Fatalf("no ping at 59 min (%d)", n)
	}
	if n := k.Sweep(start.Add(2 * time.Hour)); n != 0 {
		t.Fatalf("a second ping: K=4 must not raise the account's 1h count of 1 (%d)", n)
	}

	k2, _, clock2 := testKeeper(t, Limits{})
	start = clock2.now()
	armOn(t, k2, "sess-1", 120*time.Second, 1, start.Add(4*time.Hour))
	acct := kaPolicy()
	acct.MaxPings1h = 3
	recordUsage(t, k2, acct, ttlBody1h, start, bill1h)
	if n := k2.Sweep(start.Add(59 * time.Minute)); n != 1 {
		t.Fatalf("no first ping (%d)", n)
	}
	if n := k2.Sweep(start.Add(118 * time.Minute)); n != 0 {
		t.Fatalf("override K=1 did not bound the 1h thread below the account's 3 (%d)", n)
	}
}

// The ctl API states the 1-hour hold beside the 5-minute one.
func TestArmedStateReportsTheOneHourHold(t *testing.T) {
	if got := int(keepalive.MaxHold1h / time.Minute); got != 236 {
		t.Errorf("max 1h hold = %d min, want 236", got)
	}
}

// A manual ping on a 1-hour thread, end to end through the endpoint: the held thread lives an hour,
// the rewrite it avoids is priced at the 1-hour write rate (2.0x input, not the 5-minute 1.25x), and
// the cache is reported warm for an hour from the ping.
func TestManualPingOnAOneHourThreadIsPricedAndHeldAtTheHourTier(t *testing.T) {
	m := newManualHost(t)
	at := m.clock.now().Add(-20 * time.Minute)
	tn := &Tenancy{ID: "", Cache: CachePolicy{}, Pipe: components.NewPipeline(nil, m.sink)}
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer sk-caller-secret")
		r.Header.Set(ManualHoldHeader, "1")
		m.h.keeper.record(tn, "sess-1h", "", at.Add(time.Duration(i)*time.Second), []byte(ttlBody1h),
			upstream{base: "http://up", path: "/v1/messages"}, r, bschemas.Anthropic, "/v1/messages",
			http.StatusOK, bill1h, true)
	}
	m.rec.Record(&dash.Event{TS: at.UnixMilli(), SessionID: "sess-1h", Model: "aws/claude-sonnet-5",
		Provider: "anthropic", Status: 200, CacheRead: 48576, CacheWrite: 1200, CacheWrite1h: 1200,
		CacheTTL: "ephemeral_1h", UpstreamMs: 1000, TokenAccounting: dash.AccountingComplete})
	waitForRows(t, m.rec, m.rowsSoFar()+1)
	rowsWritten[m.rec]++

	// 20 minutes after the last request: a 300 s hold would be gone, an hour-tier hold is not.
	if st := m.h.KeepAlivePingStates("", true, "sess-1h")[""]; !st.Pingable {
		t.Fatalf("a 1h thread 20 min after its last request is not pingable: %+v", st)
	}
	w, out := m.post(`{"session":"sess-1h"}`)
	if w.Code != http.StatusOK || out["sent"] != true {
		t.Fatalf("press = %d %v, want 200 sent", w.Code, out)
	}
	// fixedPricer: input 2e-6 (x2.0 on the 1h tier = 4e-6), write 2.5e-6 (the 5-minute rate).
	want := 49776 * 2e-6 * 2.0
	if got := out["rewrite_usd"].(float64); got < want*0.999 || got > want*1.001 {
		t.Errorf("rewrite_usd = %v, want %v (prefix x 2.0 x input)", got, want)
	}
	if got, want := int64(out["warm_until_ms"].(float64)), m.clock.now().Add(time.Hour).UnixMilli(); got != want {
		t.Errorf("warm_until_ms = %d, want ping start + 1 h %d", got, want)
	}
}

// An active campaign decides the ping count on five-minute economics. On a 1-hour thread it only
// bounds the 1-hour count from above, as an override does; it never raises it, and it never sets the
// 1-hour timing.
func TestCampaignBoundsAOneHourThreadsPingCountDownward(t *testing.T) {
	acct := kaPolicy()
	acct.MaxPings1h = 3
	got := predDecision{k: 1, idleS: 280, active: true}.apply(acct)
	if got.MaxPings != 1 || got.MaxPings1h != 1 {
		t.Errorf("campaign k=1: MaxPings=%d MaxPings1h=%d, want 1 and 1", got.MaxPings, got.MaxPings1h)
	}
	got = predDecision{k: 3, idleS: 280, active: true}.apply(kaPolicy())
	if got.MaxPings1h != 1 {
		t.Errorf("campaign k=3 raised the default 1h count of 1 to %d", got.MaxPings1h)
	}
	if got.Idle1h != 0 {
		t.Errorf("a campaign set the 1h timing to %v; it must stay the account's", got.Idle1h)
	}
}
