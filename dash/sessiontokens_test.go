package dash

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The Sessions tab in tokens (#1023): each session's figure is the accountant's, split into the
// keep-alive rescue and the rest, with the pings' billed tokens apart. The two parts add up to the
// session's /api/stats tokens, tier by tier.
func TestSessionTokens_SplitTheKeepAliveAndKeepThePingsApart(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	accTokensFixture(t, db, now)
	rows, _, err := db.Sessions(Filter{Tenant: "t1"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*SessionRow{}
	for _, s := range rows {
		by[s.SessionID] = s
	}
	s1 := by["s1"]
	if s1 == nil || by["s2"] == nil {
		t.Fatalf("sessions %v", by)
	}
	o, err := db.Overview(Filter{Tenant: "t1", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	sum := s1.Tokens
	sum.add(s1.KeepAliveTokens)
	if sum.Saved != o.Tokens.Saved || sum.Caused != o.Tokens.Caused {
		t.Errorf("session %+v + keep-alive %+v, want the stats total %+v / %+v", s1.Tokens, s1.KeepAliveTokens,
			o.Tokens.Saved, o.Tokens.Caused)
	}
	if s1.KeepAliveTokens != o.Tokens.ByFeature["keepalive"] {
		t.Errorf("keep-alive %+v, want %+v", s1.KeepAliveTokens, o.Tokens.ByFeature["keepalive"])
	}
	if s1.KeepAlivePingTokens != o.KeepAlivePingTokens {
		t.Errorf("ping tokens %+v, want %+v", s1.KeepAlivePingTokens, o.KeepAlivePingTokens)
	}
	if s1.TokensUnmeasured != o.TokensUnmeasuredRequests || s1.TokensUnmeasured != 2 {
		t.Errorf("unmeasured %d, want %d (2)", s1.TokensUnmeasured, o.TokensUnmeasuredRequests)
	}
	// Four agent rows of 100 fresh, 5,000 read, 400 written (5-minute); the ping is not in it.
	if want := (TierTokens{Input: 400, CacheRead: 20_000, CacheWrite5m: 1600}); s1.UsedTokens != want {
		t.Errorf("used %+v, want %+v", s1.UsedTokens, want)
	}
	// s2 has one priced request and no ping.
	s2 := by["s2"]
	if s2.TokensUnmeasured != 0 || s2.KeepAlivePingTokens != (TierTokens{}) || s2.Tokens.Saved == (TierTokens{}) {
		t.Errorf("s2 %+v", s2.SessionTokens)
	}
	// A page of one session reads that session only.
	page, _, err := db.Sessions(Filter{Tenant: "t1"}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].SessionID != "s2" || page[0].SessionTokens != s2.SessionTokens {
		t.Errorf("page %+v", page)
	}
}

// The token read must not change the cost of the Sessions tab: it reads only the page's sessions,
// through a session index, whatever the window. Measured here against the list's own aggregate,
// which scans the window.
func TestSessionTokens_StaysCheapOnALargeWindow(t *testing.T) {
	db := openTestDB(t)
	// Measured at 20,000 rows without -race: the list 103 ms, the token read 2.5 ms of it.
	// The test runs at 8,000 so the suite stays quick under -race.
	const n = 8_000
	batch := make([]*Event, 0, 1000)
	for i := 0; i < n; i++ {
		e := tokEvent(int64(1_700_000_000_000+i*100), fmt.Sprintf("s%d", i/40))
		e.PriceMode(accTestPrice, true)
		batch = append(batch, e)
		if len(batch) == 1000 {
			if err := db.insertBatch(batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	f := Filter{Tenant: "t1"}
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i)
	}
	q, args := sessionTokensQuery(f, ids)
	rows, err := db.sql.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		plan += d + "\n"
	}
	rows.Close()
	if !strings.Contains(plan, "idx_requests_session") && !strings.Contains(plan, "idx_requests_tenant_session") {
		t.Errorf("the session token read does not use a session index:\n%s", plan)
	}
	best := func(fn func()) time.Duration {
		var m time.Duration
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			fn()
			if d := time.Since(t0); i == 0 || d < m {
				m = d
			}
		}
		return m
	}
	list := best(func() {
		if _, _, err := db.Sessions(f, 25, 0); err != nil {
			t.Fatal(err)
		}
	})
	tok := best(func() {
		if _, err := db.sessionTokens(f, ids); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("%d rows, 25 sessions a page: Sessions %v, of which the token read %v", n, list, tok)
	// RELATIVE, as in TestOverviewStaysFastOnALargeWindow: the token read is a small part of the list.
	if tok > list/4 {
		t.Errorf("the token read is %v of the list's %v", tok, list)
	}
}
