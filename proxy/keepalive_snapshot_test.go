package proxy

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/dash"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"net/http"
	"net/http/httptest"
	"strings"

	_ "modernc.org/sqlite" // the same pure-Go driver the dashboard's own store uses
)

// Replay of the production dashboard snapshot through the SHIPPED keep-alive.
//
// The point of doing it this way rather than in a separate model: the projected dollars in
// the pull request have to come out of the code that will run, not out of a Python script
// that agrees with it today. So this drives the real `keeper` — the real `due()` timing, the
// real K, the real per-session spend cap, the real thinking gate, the real record/arrive
// lifecycle — over the real request timeline, and prices every ping through the same Pricer
// the request path uses.
//
// Skipped unless CG_SNAPSHOT_DB names a snapshot, so it never runs in CI and never touches a
// live database. Open it READ-ONLY.
//
//	CG_SNAPSHOT_DB=/tmp/cgwork/cg-snap.db \
//	MODEL_PRICES=/etc/context-guru/prices.yaml \
//	CGO_ENABLED=1 go test -run TestKeepAliveOnProductionSnapshot -v ./proxy/
//
// What it does NOT model, stated because a projection that hides its assumptions is worth
// nothing:
//
//   - Tenant rate and concurrency limits. AcquireSpare reads the wall clock, which a
//     simulation on a synthetic clock cannot drive, so this runs with limits off and
//     therefore reports an UPPER bound on pings sent. In production a ping that finds no
//     slack is skipped, which removes both its cost and its conversion.
//   - The provider's content-keyed cache. Another session sending a byte-identical prefix
//     would refresh the same entry for free, so some conversions credited here would have
//     happened anyway. Same confound as the live credit, and in the same direction.
//   - Request bodies. The snapshot stores token counts, not bodies, so the replay hands the
//     keeper a synthetic body of the right shape. The gates that read the body (thinking, the
//     size bound) are driven from the recorded columns instead.
type snapRow struct {
	id, ts                  int64
	session, tenant, model  string
	agent, provider, reason string
	cacheRead, cacheWrite   int64
	costUSD                 float64
	thinking                string
}

func TestKeepAliveOnProductionSnapshot(t *testing.T) {
	path := os.Getenv("CG_SNAPSHOT_DB")
	if path == "" {
		t.Skip("set CG_SNAPSHOT_DB to replay a production snapshot")
	}
	prices := os.Getenv("MODEL_PRICES")
	if prices == "" {
		prices = "/etc/context-guru/prices.yaml"
	}
	table, err := modelinfo.LoadTable(prices)
	if err != nil {
		t.Fatalf("prices: %v", err)
	}
	rows := loadSnapshot(t, path)
	t.Logf("snapshot: %d requests, %s .. %s", len(rows),
		time.UnixMilli(rows[0].ts).UTC().Format(time.RFC3339),
		time.UnixMilli(rows[len(rows)-1].ts).UTC().Format(time.RFC3339))

	// Every arm the PR quotes, from the same decision function. The blanket arms set
	// MinPrefixTokens 0 and are run with the first-request gate lifted, so the gate's effect is
	// visible as the difference between two rows of the same table rather than asserted.
	for _, arm := range []struct {
		idle, maxPings, minPrefix int
		blanket                   bool
	}{
		{280, 2, 20000, false}, // SHIPPED
		{280, 2, 0, true},
		{280, 1, 20000, false},
		{280, 3, 20000, false},
		{240, 2, 20000, false},
		{280, 12, 20000, false},
		{280, 2, 50000, false},
	} {
		arm := arm
		name := fmt.Sprintf("X=%ds_K=%d_prefix>=%dk", arm.idle, arm.maxPings, arm.minPrefix/1000)
		if arm.blanket {
			name = fmt.Sprintf("X=%ds_K=%d_BLANKET", arm.idle, arm.maxPings)
		}
		t.Run(name, func(t *testing.T) {
			r := replay(t, rows, table, CachePolicy{
				KeepAlive: true, Idle: time.Duration(arm.idle) * time.Second,
				MaxPings: arm.maxPings, MaxUSDPerPing: 0.25,
				MinPrefixTokens: arm.minPrefix,
			}, arm.blanket)
			t.Logf("pings %d  ping $%.2f  converted %d  saved $%.2f  NET $%+.2f (%.2f%% of $%.2f)",
				r.pings, r.pingUSD, r.converted, r.savedUSD, r.savedUSD-r.pingUSD,
				(r.savedUSD-r.pingUSD)/r.totalUSD*100, r.totalUSD)
			t.Logf("  addressable misses %d worth $%.2f | wasted pings (gap would have hit) %d worth $%.2f"+
				" | skipped by a gate %d | live sessions: peak %d of %d bound, %d at end",
				r.addressable, r.addressableUSD, r.wastedPings, r.wastedUSD, r.skipped,
				r.peakLive, maxKeepAliveSessions, r.liveAtEnd)
			t.Logf("  per session: %d pinged | %d net positive | %d net NEGATIVE costing $%.2f in total"+
				" | worst single session $%.4f",
				r.sessions, r.sessionsWinning, r.sessionsLosing, -r.losingUSD, r.worstSessionUSD)
			if r.wroteInsteadOfRead != 0 {
				t.Errorf("%d pings created an entry instead of refreshing one", r.wroteInsteadOfRead)
			}
		})
	}
}

type replayResult struct {
	pings, converted, addressable, wastedPings, skipped int64
	liveAtEnd, peakLive                                 int
	wroteInsteadOfRead                                  int64
	pingUSD, savedUSD, addressableUSD, wastedUSD        float64
	totalUSD                                            float64
	// Per-session outcomes, because the aggregate hides the only thing a user cares about:
	// whether THEIR sessions pay. A policy that nets positive service-wide while costing the
	// median session money is not a policy anybody should be opted into.
	sessions                        int
	sessionsLosing, sessionsWinning int
	worstSessionUSD                 float64
	worstSessionID                  string
	losingUSD                       float64
}

// replay drives the shipped keeper over the snapshot's timeline.
func replay(t *testing.T, rows []snapRow, table *Table2, pol CachePolicy, blanket bool) replayResult {
	t.Helper()
	var out replayResult
	// A recorder, because retention requires an audit sink. The zero value discards its events,
	// which is what this replay wants: the decision function is under test, not the writer.
	h := &Handler{opts: Options{Prices: table}, limiter: NewLimiter(Limits{}), rec: &dash.Recorder{}}
	k := newKeeper(h)
	// The blanket arms lift the first-request gate by pre-seeding every session's turn counter,
	// which is the only way to compare "gated" against "blanket" through the SAME code path.
	seedTurn := func(key string) {
		if blanket {
			k.mu.Lock()
			k.turns[key]++
			k.mu.Unlock()
		}
	}
	// Inline dispatch: a five-day replay must be deterministic, and a goroutine per ping would
	// need synchronising against the clock the test is driving.
	k.dispatch = k.fire

	simNow := time.UnixMilli(rows[0].ts).UTC()
	k.now = func() time.Time { return simNow }

	// The provider, simulated: an entry lives 5 minutes from the last touch (a real request or
	// a ping), which is the documented rule the whole mechanism rests on.
	type live struct {
		lastTouch time.Time
		prefix    int64
	}
	entries := map[string]*live{}
	entryModel := map[string]string{}
	// Per-session ledger: what pings cost that session against what they saved it.
	type ledger struct{ ping, saved float64 }
	perSession := map[string]*ledger{}
	sessionOf := func(id string) *ledger {
		l := perSession[id]
		if l == nil {
			l = &ledger{}
			perSession[id] = l
		}
		return l
	}

	// prefixOf is what a ping would re-read, and therefore what it costs: the size of the
	// entry the previous request established.
	k.send = func(j pingJob, body []byte) (Usage, int, error) {
		e := entries[j.session]
		if e == nil {
			return Usage{}, http.StatusOK, nil
		}
		if simNow.Sub(e.lastTouch) > 5*time.Minute {
			// The entry had already lapsed: this ping CREATES one. It is the failure mode the
			// shipped code treats as a bug, and by construction it cannot happen at X < 300s.
			out.wroteInsteadOfRead++
			e.lastTouch = simNow
			return Usage{CacheWrite: e.prefix, Output: 1}, http.StatusOK, nil
		}
		e.lastTouch = simNow // the read refreshed it, for no additional cost
		return Usage{CacheRead: e.prefix, Output: 1}, http.StatusOK, nil
	}
	// The ping's cost, attributed to the session that caused it, read out of the entry the
	// shipped record1 just updated. Wrapping the sender is the only place both facts are in
	// hand at once.
	rawSend := k.send
	k.send = func(j pingJob, body []byte) (Usage, int, error) {
		u, st, err := rawSend(j, body)
		if price, ok := table.Price(nil, entryModel[j.session]); ok {
			sessionOf(j.session).ping += float64(u.CacheRead)*price.CacheRead +
				float64(u.CacheWrite)*price.CacheWrite + float64(u.Output)*price.Output
		}
		return u, st, err
	}

	// One tenancy per tenant id: the keeper keys its map on (tenant, session), so a replay that
	// records under "" and arrives under the real id never matches — which is exactly the bug
	// this line fixes, and it reported zero conversions rather than an error.
	tenancies := map[string]*Tenancy{}
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer replay")
	up := upstream{base: "http://replay", path: "/v1/messages"}

	for i := range rows {
		r := &rows[i]
		out.totalUSD += r.costUSD
		at := time.UnixMilli(r.ts).UTC()
		// Advance the clock to this request, sweeping exactly as the ticker would.
		for simNow.Before(at) {
			next := simNow.Add(keepAliveTick)
			if next.After(at) {
				next = at
			}
			simNow = next
			k.sweep(simNow)
		}
		simNow = at

		if n := k.Stats().Live; n > out.peakLive {
			out.peakLive = n
		}
		pings, _ := k.arrive(r.tenant, r.session)
		e := entries[r.session]
		addressable := r.reason == "ttl_expiry" && r.cacheWrite > 0
		if addressable {
			out.addressable++
			out.addressableUSD += r.costUSD
		}
		price, priced := table.Price(nil, r.model)
		switch {
		case addressable && pings > 0 && e != nil && at.Sub(e.lastTouch) <= 5*time.Minute:
			// The entry was still alive because of our pings, so this request reads its prefix
			// instead of re-creating it. What it saves is the re-write penalty on the tokens it
			// actually wrote.
			out.converted++
			if priced {
				v := float64(r.cacheWrite) * (price.CacheWrite - price.CacheRead)
				out.savedUSD += v
				sessionOf(r.session).saved += v
			}
		case pings > 0 && r.reason == "hit":
			// Pings spent on a gap that would have hit anyway. Not a saving, and the honest
			// place to count them is here rather than nowhere.
			out.wastedPings += int64(pings)
		}
		// This request refreshes or establishes the entry.
		if e == nil {
			e = &live{}
			entries[r.session] = e
		}
		e.lastTouch, e.prefix = at, r.cacheRead+r.cacheWrite
		entryModel[r.session] = r.model
		if e.prefix == 0 {
			e.prefix = 1 // nothing cached; a ping on it reads nothing and costs nothing
		}

		// Hand the finished request to the keeper, with the gates it reads driven from the
		// recorded columns.
		body := snapBody(r)
		usageOK := r.cacheRead > 0 || r.cacheWrite > 0
		tn := tenancies[r.tenant]
		if tn == nil {
			tn = &Tenancy{ID: r.tenant, Cache: pol}
			tenancies[r.tenant] = tn
		}
		seedTurn(kaKey(r.tenant, r.session))
		k.record(tn, r.session, at, body, up, req, providerOf(r.provider), "/v1/messages",
			http.StatusOK, Usage{CacheRead: r.cacheRead, CacheWrite: r.cacheWrite}, usageOK)
	}
	// Cost is accumulated by the shipped record1, from the shipped Pricer.
	out.pings = k.pings.Load()
	out.skipped = k.skipped.Load()
	out.pingUSD = k.Stats().SpentUSD
	out.liveAtEnd = k.Stats().Live
	if out.wastedPings > 0 {
		out.wastedUSD = out.pingUSD * float64(out.wastedPings) / float64(max64(out.pings, 1))
	}
	for id, l := range perSession {
		if l.ping == 0 && l.saved == 0 {
			continue
		}
		out.sessions++
		switch net := l.saved - l.ping; {
		case net > 0:
			out.sessionsWinning++
		case net < 0:
			out.sessionsLosing++
			out.losingUSD += net
			if net < out.worstSessionUSD {
				out.worstSessionUSD, out.worstSessionID = net, id
			}
		}
	}
	return out
}

// snapBody is a synthetic request body with the shape the keeper's gates read: the model, and
// the thinking block the snapshot recorded. The snapshot stores no bodies, so this is the
// honest substitute — every gate that consults the body is driven by a recorded column.
func snapBody(r *snapRow) []byte {
	think := ""
	switch r.thinking {
	case "enabled":
		think = `,"thinking":{"type":"enabled","budget_tokens":32000}`
	case "adaptive", "disabled":
		think = `,"thinking":{"type":"` + r.thinking + `"}`
	}
	return []byte(`{"model":"` + r.model + `","max_tokens":32000,"stream":true` + think +
		`,"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]` +
		`,"messages":[{"role":"user","content":[{"type":"text","text":"x",` +
		`"cache_control":{"type":"ephemeral"}}]}]}`)
}

func providerOf(s string) bschemas.ModelProvider {
	if s == "" {
		return bschemas.Anthropic
	}
	return bschemas.ModelProvider(s)
}

// Table2 is modelinfo's price table under a local name, so the signature above reads without
// dragging the import into every line.
type Table2 = modelinfo.Table

func loadSnapshot(t *testing.T, path string) []snapRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer db.Close()
	q, err := db.Query(`SELECT id, ts, session_id, tenant_id, model, agent, provider,
		cache_miss_reason, cache_read, cache_write, cost_usd, thinking_mode
		FROM requests WHERE session_id <> '' ORDER BY ts, id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer q.Close()
	var out []snapRow
	for q.Next() {
		var r snapRow
		if err := q.Scan(&r.id, &r.ts, &r.session, &r.tenant, &r.model, &r.agent, &r.provider,
			&r.reason, &r.cacheRead, &r.cacheWrite, &r.costUSD, &r.thinking); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("snapshot has no rows")
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ts != out[j].ts {
			return out[i].ts < out[j].ts
		}
		return out[i].id < out[j].id
	})
	return out
}
