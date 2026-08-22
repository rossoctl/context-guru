package dash

// Verification against REAL captured traffic, plus the per-request overhead measurement.
//
// The corpora are raw request bodies recorded by the benchmark harness — real Claude
// Code traffic that went through this proxy. They are not in the repository (311 MB and
// 49 MB, and they are transcripts), so every test here SKIPS when they are absent: it is
// a measurement harness that runs where the data is, not a CI gate. The CI gate is
// toolinventory_test.go, whose fixtures are built to the shape measured here.
//
// Run:
//
//	go test ./dash -run TestCorpus -v
//	go test ./dash -run XXX -bench ScanInventory -benchtime 200x

import (
	"bufio"
	"fmt"
	"hash/maphash"
	"os"
	"sort"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/tidwall/gjson"
)

const corpusDir = "/tmp/cg-runs"

var corpora = []string{"capture-swebench.jsonl", "capture-tb.jsonl"}

// corpusRecord is one captured request: the raw body and the dialect it arrived in.
type corpusRecord struct {
	body     []byte
	provider string
	session  string
}

// eachRecord streams one capture file. Sessions are keyed on the first user message,
// because these captures carry no session id — the same keying the spike used.
func eachRecord(t testing.TB, path string, fn func(corpusRecord)) int {
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("corpus not present (%v)", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	seed := maphash.MakeSeed()
	n := 0
	for sc.Scan() {
		rec := gjson.GetBytes(sc.Bytes(), "body")
		if !rec.Exists() {
			continue
		}
		body := []byte(rec.Raw)
		provider := gjson.GetBytes(sc.Bytes(), "provider").String()
		if provider == "" {
			provider = "anthropic"
		}
		var h maphash.Hash
		h.SetSeed(seed)
		h.WriteString(gjson.GetBytes(body, "messages.0").Raw)
		fn(corpusRecord{body: body, provider: provider,
			session: fmt.Sprintf("s%x", h.Sum64())})
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestCorpusInventory re-derives the spike's headline numbers with the repo's own BPE
// tokenizer instead of the (len+3)/4 fallback the spike had to use, by running the REAL
// capture path over every request: ScanInventory on each body, the writer's dedupe, then
// the read API's declared-vs-used diff. Whatever it prints is the number of record.
func TestCorpusInventory(t *testing.T) {
	for _, name := range corpora {
		t.Run(name, func(t *testing.T) {
			db := openTestDB(t)
			w := &invWriter{db: db, seen: map[string]*invSession{}}
			var evs []*Event
			var batch []invMsg
			sessions := map[string]bool{}
			listings, unknown, noInv := 0, 0, 0
			ts := int64(1000)
			total := eachRecord(t, corpusDir+"/"+name, func(r corpusRecord) {
				ts++
				inv := ScanInventory(r.provider, r.body)
				if inv == nil {
					noInv++
					return
				}
				sessions[r.session] = true
				for _, d := range inv.Decls {
					if d.Kind == KindSkillListing {
						listings++
						if d.Server == SkillsUnknown {
							unknown++
						}
					}
				}
				// One request row per captured request, all cache hits: the corpus is a
				// session's worth of turns, so its own request count IS the re-read multiplier.
				e := mkEvent(ts, r.session, gjson.GetBytes(r.body, "model").String(), 100, 100)
				e.Tools = len(gjson.GetBytes(r.body, "tools").Array())
				evs = append(evs, e)
				batch = append(batch, invMsg{session: r.session, ts: ts, inv: inv})
			})
			if err := db.insertBatch(evs); err != nil {
				t.Fatal(err)
			}
			if err := w.write(batch); err != nil {
				t.Fatal(err)
			}
			rep, err := db.ToolReportFor(Filter{TenantAll: true}, func(m string) (modelinfo.Price, bool) {
				// The deployed rate shape: cache read at 0.1x input. Only used for the
				// corpus-internal dollar figure; the production total is priced per model.
				return modelinfo.Price{Input: 15e-6, CacheRead: 1.5e-6, CacheWrite: 18.75e-6}, true
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d records, %d sessions, %d without tools", name, total, len(sessions), noInv)
			t.Logf("  skills listings parsed %d, unreadable %d, state=%s",
				listings, unknown, rep.Skills.State)
			t.Logf("  declared tokens/session %d, NEVER-USED %d (%.1f%%), requests/session %.1f",
				rep.Totals.DeclaredTokens, rep.Totals.UnusedTokens, rep.Totals.UnusedPct,
				rep.Totals.RequestsPerSession)
			t.Logf("  skills: declared %d, invoked %d, listing %d tok/request, listing waste %d tok",
				rep.Skills.Declared, rep.Skills.Invoked, rep.Skills.ListingTokens,
				rep.Skills.UnusedListingReads)
			t.Logf("  coverage: %d sessions, %d captured, %d not captured",
				rep.Coverage.Sessions, rep.Coverage.Captured, rep.Coverage.NotCaptured)
			t.Logf("  unused reads %d tokens", rep.Totals.UnusedReads)
			// The dead-weight table, biggest first.
			for i, s := range rep.Tools {
				if i >= 30 {
					break
				}
				t.Logf("    %-18s %6d tok  decl %3d  used %3d  calls %4d  unused_reads %10d",
					s.Name, s.Tokens, s.SessionsDeclared, s.SessionsUsed, s.Calls, s.UnusedReads)
			}
			// Invariants worth failing on, whatever the numbers turn out to be.
			if len(sessions) == 0 {
				t.Fatal("no sessions parsed out of a corpus that is known to hold them")
			}
			if unknown > 0 {
				t.Errorf("%d skill listings were unreadable: the parser has drifted from the corpus", unknown)
			}
			if rep.Totals.DeclaredTokens == 0 || rep.Totals.UnusedTokens == 0 {
				t.Error("declared or unused weight came out zero on real traffic")
			}
			names := map[string]bool{}
			for _, s := range rep.Tools {
				names[s.Name] = true
			}
			if !names["Bash"] {
				t.Error("Bash is declared by every session in these corpora and is missing")
			}
		})
	}
}

// TestCorpusAlwaysDeclaredNeverUsed lists the declarations that every session carried
// and no session invoked — the ones a filter could remove with the least argument.
func TestCorpusAlwaysDeclaredNeverUsed(t *testing.T) {
	db := openTestDB(t)
	w := &invWriter{db: db, seen: map[string]*invSession{}}
	var evs []*Event
	var batch []invMsg
	ts := int64(1000)
	for _, name := range corpora {
		eachRecord(t, corpusDir+"/"+name, func(r corpusRecord) {
			inv := ScanInventory(r.provider, r.body)
			if inv == nil {
				return
			}
			ts++
			e := mkEvent(ts, r.session, "m", 100, 100)
			e.Tools = 1
			evs = append(evs, e)
			batch = append(batch, invMsg{session: r.session, ts: ts, inv: inv})
		})
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	if err := w.write(batch); err != nil {
		t.Fatal(err)
	}
	rep, err := db.ToolReportFor(Filter{TenantAll: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var dead []string
	for _, s := range append(append([]ToolStat{}, rep.Tools...), rep.Skills.Skills...) {
		if s.SessionsDeclared == rep.Coverage.Captured && s.SessionsUsed == 0 {
			dead = append(dead, fmt.Sprintf("%s(%d tok)", s.Name, s.Tokens))
		}
	}
	sort.Strings(dead)
	t.Logf("%d sessions; declared by all and used by none: %d — %v",
		rep.Coverage.Captured, len(dead), dead)
}

// BenchmarkScanInventory measures what this capture adds to a request. Two cases,
// because they are what production sees: WARM is every request after a session's first
// (the declaration set is memoized by digest), COLD is that first request, which pays
// for the whole parse and the BPE count of every declaration.
func BenchmarkScanInventory(b *testing.B) {
	var body []byte
	eachRecord(b, corpusDir+"/capture-swebench.jsonl", func(r corpusRecord) {
		if body == nil && len(gjson.GetBytes(r.body, "tools").Array()) > 0 {
			body = r.body
		}
	})
	if body == nil {
		b.Skip("no corpus body with tools")
	}
	b.Logf("body %d bytes, %d tools, %d messages", len(body),
		len(gjson.GetBytes(body, "tools").Array()), len(gjson.GetBytes(body, "messages").Array()))
	b.Run("warm", func(b *testing.B) {
		ScanInventory("anthropic", body)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ScanInventory("anthropic", body)
		}
	})
	b.Run("cold", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			declMu.Lock()
			declCache = map[uint64][]Decl{}
			declMu.Unlock()
			// The SYSTEM prompt's memo too, or "cold" measures a cold declaration scan against
			// a warm system scan and understates the one figure this benchmark exists to bound.
			sysMu.Lock()
			sysCache, sysBytes = map[uint64]*SystemPrompt{}, 0
			sysMu.Unlock()
			ScanInventory("anthropic", body)
		}
	})
	// The two halves of the warm path, so a regression can be attributed.
	b.Run("warm_digest_only", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			t := gjson.GetBytes(body, "tools")
			var h maphash.Hash
			h.SetSeed(declSeed)
			h.WriteString(t.Raw)
			h.WriteString(skillRegion(body))
			_ = h.Sum64()
		}
	})
	b.Run("warm_used_only", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			usedFrom("anthropic", body)
		}
	})
	// The baselines this sits next to: the metadata pass the capture point ALREADY ran on
	// every request before this feature existed (one ForEach over the top-level object,
	// documented at 0.55 ms/MB), and one gjson array parse of the tools array.
	b.Run("baseline_metadata_pass", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			gjson.ParseBytes(body).ForEach(func(_, _ gjson.Result) bool { return true })
		}
	})
	b.Run("baseline_gjson_tools_array", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = len(gjson.GetBytes(body, "tools").Array())
		}
	})
}
