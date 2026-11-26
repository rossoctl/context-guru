package dash

import (
	"testing"
	"time"
)

// The Cache strategy tab's tables in tokens (#1048). The session and live tables carry each
// session's keep-alive figures from the Sessions tab's own query, so the two tabs cannot disagree.
// The expiries' cost has its token form, by tier, on the session rows and on every behaviour band,
// and the bands of each kind add up to the same total, tier by tier.
func TestKeepAliveTables_TokensMatchTheSessionsTab(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	accTokensFixture(t, db, now)
	// One addressable expiry on s1: a ttl_expiry row that wrote, after an earlier request.
	exp := &Event{TS: now - 500, SessionID: "s1", Model: "m", TenantID: "t1", CacheMissReason: "ttl_expiry",
		FreshInput: 70, CacheRead: 0, CacheWrite: 6000, CacheWrite1h: 1000, OutputTokens: 5}
	exp.Price(accTestPrice, true)
	if err := db.insertBatch([]*Event{exp}); err != nil {
		t.Fatal(err)
	}
	f := Filter{Tenant: "t1"}
	page, _, err := db.Sessions(f, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]SessionTokens{}
	for _, s := range page {
		want[s.SessionID] = s.SessionTokens
	}
	same := func(where string, got KeepAliveSessionTokens, w SessionTokens) {
		t.Helper()
		if got.KeepAliveTokens != w.KeepAliveTokens || got.KeepAlivePingTokens != w.KeepAlivePingTokens ||
			got.TokensUnmeasured != w.TokensUnmeasured {
			t.Errorf("%s: %+v, the Sessions tab has %+v", where, got, w)
		}
	}
	expiry := TierTokens{Input: 70, CacheWrite5m: 5000, CacheWrite1h: 1000}

	rows, err := db.KeepAliveSessions(f, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionID != "s1" {
		t.Fatalf("sessions %+v", rows)
	}
	same("sessions table", rows[0].KeepAliveSessionTokens, want["s1"])
	if rows[0].ExpiryTokens != expiry {
		t.Errorf("expiry tokens %+v, want %+v", rows[0].ExpiryTokens, expiry)
	}
	// The fixture's rescue and pings are there, not zero.
	if rows[0].KeepAliveTokens.Saved == (TierTokens{}) || rows[0].KeepAlivePingTokens == (TierTokens{}) {
		t.Errorf("s1 has no keep-alive tokens: %+v", rows[0].KeepAliveSessionTokens)
	}

	live, err := db.KeepAliveLive(f, now, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Rows) == 0 {
		t.Fatal("no live rows")
	}
	for _, r := range live.Rows {
		same("live table "+r.SessionID, r.KeepAliveSessionTokens, want[r.SessionID])
	}

	b, err := db.KeepAliveBehaviour(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	sum := func(bands []Band) TierTokens {
		var s TierTokens
		for _, x := range bands {
			if x.Tokens == nil {
				t.Fatalf("band %q has no tokens", x.Label)
			}
			s.add(*x.Tokens)
		}
		return s
	}
	var daily TierTokens
	for _, d := range b.Daily {
		daily.add(d.Tokens)
	}
	for name, got := range map[string]TierTokens{"gap bands": sum(b.GapBands), "prefix bands": sum(b.PrefixBands),
		"hour bins": sum(b.HourBins), "daily": daily} {
		if got != expiry {
			t.Errorf("%s: %+v, want %+v", name, got, expiry)
		}
	}
}
