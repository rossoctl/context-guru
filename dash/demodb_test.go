package dash

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWriteDemoDB writes the golden fixture to $CG_DEMO_DB so a dashboard can be started on
// synthetic data (screenshots for a public change). It does nothing unless the variable is set.
func TestWriteDemoDB(t *testing.T) {
	path := os.Getenv("CG_DEMO_DB")
	if path == "" {
		t.Skip("set CG_DEMO_DB to write the synthetic fixture database")
	}
	goldenAPIAt(t, path)
}

// TestWriteRichDemoDB writes a larger synthetic database to $CG_DEMO_DB_RICH: about 30 days of
// history ending a few hours before now, ~300 sessions over three tenants and three models, cache
// hits and TTL expiries, keep-alive pings (some rescuing a miss), component savings, a tool
// inventory with unused servers and skills, and request content for a few requests. Every value is
// invented from a fixed seed; timestamps are relative to time.Now(). It does nothing unless the
// variable is set:
//
//	CG_DEMO_DB_RICH=/path/rich.db go test ./dash -run TestWriteRichDemoDB -count=1
func TestWriteRichDemoDB(t *testing.T) {
	path := os.Getenv("CG_DEMO_DB_RICH")
	if path == "" {
		t.Skip("set CG_DEMO_DB_RICH to write the rich synthetic database")
	}
	_, rec := newTestAPI(t, Options{CaptureContent: true, DBPath: path})
	rng := rand.New(rand.NewSource(7))
	const minute, hour = int64(60_000), int64(3_600_000)
	end := time.Now().UnixMilli() - 4*hour
	start := end - 30*24*hour

	type price struct{ in, out float64 } // dollars per token; cache read 0.1x, write 1.25x
	prices := map[string]price{
		"claude-opus-5":    {3.8e-6, 19e-6},
		"claude-sonnet-5":  {3e-6, 15e-6},
		"claude-haiku-4-5": {0.76e-6, 3.8e-6},
	}
	models := []string{"claude-opus-5", "claude-sonnet-5", "claude-sonnet-5", "claude-haiku-4-5"}
	// Rows belong to three tenants. For a hosted dashboard, CG_DEMO_TENANTS=<id1>,<id2>,<id3> names
	// the real account ids of the control database; otherwise they are team-red/blue/green.
	tenants := []string{"team-red", "team-blue", "team-green"}
	if ids := strings.Split(os.Getenv("CG_DEMO_TENANTS"), ","); len(ids) == 3 {
		tenants = ids
	}
	pinging := map[string]bool{tenants[0]: true, tenants[1]: true}
	comps := []struct{ name, kind string }{{"cmdfilter", "offload"}, {"dedup", "reformat"}, {"collapse", "reformat"}, {"extract", "offload"}}

	servers := []string{"github", "jira", "slack", "postgres", "figma", "notion", "pagerduty", "sentry"}
	skills := []string{"brainstorming", "code-review", "debugging", "release-notes", "sql-tuning", "diagram"}
	usedServer := map[string]float64{"github": .9, "jira": .6, "slack": .5, "postgres": .4, "sentry": .15} // others never
	usedSkill := map[string]float64{"code-review": .5, "debugging": .4, "brainstorming": .2}

	var evs []*Event
	var msgs []invMsg
	contentLeft := 40
	for s := 0; s < 300; s++ {
		ti := rng.Intn(3)
		tenant := tenants[ti]
		session := fmt.Sprintf("t%d:sess-%04d", ti+1, s)
		model := models[rng.Intn(len(models))]
		p := prices[model]
		rd, wr := p.in*0.1, p.in*1.25
		// start: spread over 30 days, working hours, fewer at weekends
		day := start + rng.Int63n(30)*24*hour
		ts := day + (8+int64(rng.Intn(11)))*hour + rng.Int63n(60)*minute
		if wd := time.UnixMilli(day).Weekday(); (wd == time.Saturday || wd == time.Sunday) && rng.Intn(3) > 0 {
			ts -= 2 * 24 * hour
		}
		if ts > end-2*hour {
			ts = end - 2*hour - rng.Int63n(20)*hour
		}
		turns := 22 + rng.Intn(50)
		ctx := 26_000 + rng.Intn(8_000)
		agent := []string{"claude-code", "claude-code", "claude-code", "bob"}[rng.Intn(4)]
		ttl := "ephemeral_5m"
		if ti == 2 {
			ttl = "ephemeral_1h"
		}
		var prevCtx int
		for k := 0; k < turns && ts < end; k++ {
			gap := int64(8_000 + rng.Intn(80_000))
			pings, rescued, idle := 0, false, false
			if k > 0 {
				switch r := rng.Intn(100); {
				case r < 9: // away long enough for the 5-minute cache to expire, nobody pinging
					gap, idle = int64(6+rng.Intn(25))*minute, true
				case r < 17 && pinging[tenant]: // away, but pings kept the cache warm
					gap, idle, rescued = int64(6+rng.Intn(35))*minute, true, true
					pings = int(gap / (270_000))
				case r < 19: // a long break: cold even for 1h tenants
					gap, idle = int64(70+rng.Intn(120))*minute, true
				}
			}
			if k > 0 {
				if rescued { // emit the pings that fall inside the idle span, before the real request
					for i := 1; i <= pings; i++ {
						pe := mkEvent(ts+int64(i)*270_000, session, model, 0, 0)
						pe.TenantID, pe.KeepAlive, pe.Tools, pe.Provider, pe.Agent = tenant, true, 38, "anthropic", agent
						pe.CacheRead, pe.CacheWrite, pe.FreshInput, pe.OutputTokens, pe.Components = int64(prevCtx), 0, 5, 1, nil
						pe.CostUSD = float64(prevCtx)*rd + 5*p.in
						pe.BaselineCostUSD, pe.SavedUnique, pe.TokensBefore, pe.TokensAfter = pe.CostUSD, 0, 0, 0
						evs = append(evs, pe)
					}
				}
				ts += gap
			}
			growth := 800 + rng.Intn(3_200)
			if rng.Intn(100) < 20 { // a big tool output
				growth = 6_000 + rng.Intn(10_000)
			}
			before := ctx + growth
			after := before
			saved := 0
			if growth > 5_000 && rng.Intn(100) < 70 { // components trim the new tail only, never the cached prefix
				saved = int(float64(growth) * (0.25 + rng.Float64()*0.5))
				after = before - saved
			}
			e := mkEvent(ts, session, model, before, after)
			e.TenantID, e.Tools, e.Agent, e.Provider, e.CacheTTL = tenant, 38, agent, "anthropic", ttl
			e.Preset = []string{"codesmart", "codesmart", "general"}[rng.Intn(3)]
			e.Messages, e.Status, e.SystemBlocks, e.Stream = 2*k+3, 200, 3, true
			e.SavedUnique = saved
			cold := k == 0 || (idle && !rescued && (gap > 5*minute && ttl == "ephemeral_5m" || gap > 60*minute))
			e.FreshInput = int64(30 + rng.Intn(200))
			e.OutputTokens = int64(250 + rng.Intn(1_800))
			switch {
			case cold:
				e.CacheRead, e.CacheWrite = 0, int64(after)
				e.CacheMissReason = CacheTTLExpiry
				if k == 0 {
					e.CacheMissReason = CacheColdStart
				}
			default:
				e.CacheRead = int64(prevCtx)
				e.CacheWrite = int64(after) - e.CacheRead
				if e.CacheWrite < 0 { // a trim rewrote the prefix
					e.CacheRead, e.CacheWrite = 0, int64(after)
					e.CacheMissReason = CachePrefixChange
				} else {
					e.CacheMissReason = CacheHit
				}
			}
			if ttl == "ephemeral_1h" {
				e.CacheWrite1h = e.CacheWrite
			}
			if e.CacheRead == 0 && e.CacheMissReason == CacheHit {
				e.CacheMissReason = CacheUnknown
			}
			wp := wr
			if ttl == "ephemeral_1h" {
				wp = p.in * 2
			}
			e.CostUSD = float64(e.FreshInput)*p.in + float64(e.CacheRead)*rd + float64(e.CacheWrite)*wp + float64(e.OutputTokens)*p.out
			e.CacheSavedUSD = float64(e.CacheRead) * (p.in - rd)
			e.BaselineCostUSD = e.CostUSD + float64(saved)*rd*20
			if rescued {
				e.KeepAlivePings = pings
				e.KeepAliveSavedUSD = float64(prevCtx) * (wp - rd)
				e.KeepAliveStrategyID = "demo-fixed"
			}
			e.UpstreamMs = float64(900 + rng.Intn(5_000))
			e.CGLatencyMs = float64(2 + rng.Intn(6))
			e.TTFBMs = e.UpstreamMs * 0.4
			if idle {
				e.WakeCause = "human"
			} else {
				e.WakeCause = "tool_result"
			}
			e.WaitingFor = "own_tool"
			e.WaitTool = "Bash"
			if idle || rng.Intn(10) == 0 {
				e.WaitingFor, e.WaitTool = "human", ""
			}
			c1 := comps[rng.Intn(len(comps))]
			e.Components = []CompRow{
				{Component: c1.name, Kind: c1.kind, Acted: saved > 0, Mutated: saved > 0, SavedGross: saved + saved/5,
					SavedUnique: saved, SavedUSD: float64(saved) * rd * 6, DurationMs: 1 + rng.Float64()*4,
					Gates:  map[string]int{"below_min_size": rng.Intn(4), "not_frozen": rng.Intn(3)},
					Events: map[string]int{"offloaded": saved / 900}},
				{Component: "cacheinject", Kind: "reformat", Mutated: true, DurationMs: 0.1, Gates: map[string]int{"already_set": 1}},
			}
			if contentLeft > 0 && saved > 3_000 && rng.Intn(3) == 0 {
				contentLeft--
				e.Content = []ContentRow{{Path: "messages.12", BeforeTokens: saved + 400, AfterTokens: 400,
					Before: "$ make test\nok  \tdemo/pkg/a\t0.12s\nok  \tdemo/pkg/b\t0.31s\n(synthetic build log, 300 more lines)\n",
					After:  "$ make test\n<<cg:demo" + fmt.Sprint(contentLeft) + ">> 2 packages ok", Components: []string{c1.name}}}
			}
			evs = append(evs, e)
			prevCtx = after
			ctx = after
			ts += 30_000 + rng.Int63n(20_000)
		}
		// inventory: every session declares a subset of servers; usage comes from the weights above
		decls := []Decl{{Kind: KindTool, Name: "Bash", Tokens: 120}, {Kind: KindTool, Name: "Read", Tokens: 90}, {Kind: KindTool, Name: "Edit", Tokens: 140}}
		for i, sv := range servers {
			if s%len(servers) == i && i > 5 {
				continue // pagerduty and sentry go missing from some sessions
			}
			for j := 0; j < 3+i%3; j++ {
				decls = append(decls, Decl{Kind: KindMCPTool, Name: fmt.Sprintf("mcp__%s__op%d", sv, j), Server: sv, Tokens: 250 + 90*j + 40*i})
			}
		}
		for _, sk := range skills {
			decls = append(decls, Decl{Kind: KindSkill, Name: sk, Tokens: 180})
		}
		decls = append(decls, Decl{Kind: KindSkillListing, Server: SkillsOK, Tokens: 1_100})
		for n := 0; n < 3; n++ {
			used := []Used{{Name: "Bash", Calls: 3 + rng.Intn(9)}, {Name: "Read", Calls: 2 + rng.Intn(6)}}
			for _, sv := range servers {
				if rng.Float64() < usedServer[sv]/2 {
					used = append(used, Used{Name: "mcp__" + sv + "__op0", Server: sv, Calls: 1 + rng.Intn(3)})
				}
			}
			for _, sk := range skills {
				if rng.Float64() < usedSkill[sk]/3 {
					used = append(used, Used{Name: "Skill", Skill: sk, Calls: 1})
				}
			}
			msgs = append(msgs, invMsg{tenant: tenant, session: session, ts: ts - int64(3-n)*10*minute, text: true,
				inv: &Inventory{Digest: fmt.Sprintf("dg%d", s), Decls: decls, Used: used, UseFingerprint: uint64(s*10 + n + 1)}})
		}
	}
	for i := 0; i < len(evs); i += 500 {
		j := i + 500
		if j > len(evs) {
			j = len(evs)
		}
		seed(t, rec, evs[i:j]...)
	}
	w := &invWriter{db: rec.DB(), seen: map[string]*invSession{}}
	if err := w.write(msgs); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d events, %d inventory messages", len(evs), len(msgs))
}
