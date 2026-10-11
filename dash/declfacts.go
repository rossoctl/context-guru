package dash

import (
	"database/sql"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"
)

// The tool/MCP/skill inventory is a function of tool_declarations, which holds a row per
// (session, declaration-SET digest, declaration): 2.3M rows for 123k distinct
// (tenant, session, kind, name, server) facts. Every inventory read wants those facts, and
// asking SQLite for them cost 11-24 s each: the planner either probes idx_tooldecl_session once
// per in-scope session and fetches ~1,000 table rows for each (11 s), or sorts all 2.3M rows into
// a temp b-tree (10 s). Neither is slow because of I/O — a plain sequential read of the table
// streams 2.3M rows in about 1 s once it is split across cores — it is slow because of the
// per-row random access or the sort. So the facts are read as a plain stream, in rowid slices
// read concurrently, and reduced in Go, where a map beats the sort.
//
// Used only for broad scopes (see declScanMinSessions); a narrow scope keeps the per-session
// probe, which is proportional to what it reads.

// declScanMinSessions (a var so a test can lower it) is how many in-scope sessions it takes for one pass over the whole table to
// beat probing the table once per session. Measured on the 2,362-session snapshot: probing costs
// ~5 ms a session (about a thousand declaration rows each), the pass costs ~1.2 s flat.
var declScanMinSessions = 250

// declScanShards is how many rowid slices the pass reads at once.
const declScanShards = 16

type declFactKey struct{ tenant, session, kind, name, server string }

// declFact is what the inventory reads off one declaration across all the digests it appears in.
type declFact struct {
	tokens      int
	hasText     bool
	first, last int64
}

// digestKey / digestFact are the per-digest view PromptViewFor ranks by.
type digestKey struct{ tenant, session, digest string }
type digestFact struct {
	ts      int64
	hasText bool
}

type declScan struct {
	facts   map[declFactKey]*declFact
	digests map[digestKey]*digestFact
}

// scanDecls reads tool_declarations in declScanShards concurrent rowid slices and folds them.
// withDigests also builds the per-digest view, which only the prompt read needs.
func (d *DB) scanDecls(f Filter, withDigests bool) (*declScan, error) {
	var lo, hi sql.NullInt64
	if err := d.sql.QueryRowContext(d.readCtx(),
		`SELECT MIN(rowid), MAX(rowid) FROM tool_declarations`).Scan(&lo, &hi); err != nil {
		return nil, err
	}
	out := &declScan{facts: map[declFactKey]*declFact{}}
	if withDigests {
		out.digests = map[digestKey]*digestFact{}
	}
	if !lo.Valid {
		return out, nil
	}
	step := (hi.Int64-lo.Int64)/declScanShards + 1
	parts := make([]*declScan, declScanShards)
	var g errgroup.Group
	for k := 0; k < declScanShards; k++ {
		k := k
		g.Go(func() error {
			digestCol := `''`
			if withDigests {
				digestCol = `digest`
			}
			q := `SELECT tenant_id, session_id, ` + digestCol + `, kind, name, server, tokens, ts,
				(text_hash IS NOT NULL OR text_gz IS NOT NULL)
				FROM tool_declarations WHERE rowid >= ? AND rowid < ?`
			args := []any{lo.Int64 + int64(k)*step, lo.Int64 + int64(k+1)*step}
			if !f.TenantAll {
				q += ` AND tenant_id = ?`
				args = append(args, f.Tenant)
			}
			rows, err := d.sql.QueryContext(d.readCtx(), q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			p := &declScan{facts: map[declFactKey]*declFact{}}
			if withDigests {
				p.digests = map[digestKey]*digestFact{}
			}
			for rows.Next() {
				var tenant, session, digest, kind, name, server string
				var tokens int
				var ts int64
				var text bool
				if err := rows.Scan(&tenant, &session, &digest, &kind, &name, &server, &tokens, &ts, &text); err != nil {
					return err
				}
				p.fold(declFactKey{tenant, session, kind, name, server}, tokens, ts, text)
				if withDigests {
					dk := digestKey{tenant, session, digest}
					if e := p.digests[dk]; e == nil {
						p.digests[dk] = &digestFact{ts: ts, hasText: text}
					} else {
						e.merge(ts, text)
					}
				}
			}
			parts[k] = p
			return rows.Err()
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for _, p := range parts {
		for k, v := range p.facts {
			out.fold(k, v.tokens, v.first, v.hasText)
			out.facts[k].last = max64(out.facts[k].last, v.last)
		}
		for k, v := range p.digests {
			if e := out.digests[k]; e == nil {
				out.digests[k] = v
			} else {
				e.merge(v.ts, v.hasText)
			}
		}
	}
	return out, nil
}

func (e *digestFact) merge(ts int64, text bool) {
	e.ts = max64(e.ts, ts)
	e.hasText = e.hasText || text
}

// fold adds one observation of a declaration: MAX(tokens), MIN(ts), MAX(ts), "any row has text".
func (s *declScan) fold(k declFactKey, tokens int, ts int64, text bool) {
	e := s.facts[k]
	if e == nil {
		s.facts[k] = &declFact{tokens: tokens, hasText: text, first: ts, last: ts}
		return
	}
	if tokens > e.tokens {
		e.tokens = tokens
	}
	e.hasText = e.hasText || text
	if ts < e.first {
		e.first = ts
	}
	e.last = max64(e.last, ts)
}

// declSessionSet is the sessions the filter selects that declared any tools, as scopedDecls's
// subquery selects them.
func (d *DB) declSessionSet(f Filter) (map[string]bool, error) {
	where, args := f.where()
	rows, err := d.sql.QueryContext(d.readCtx(),
		`SELECT DISTINCT r.session_id FROM requests r WHERE `+where+` AND r.tools > 0`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

// declScanOnce reads the table at most once for the callers that share it, and not at all if none
// of them needs it: a request that builds the inventory report AND the observation windows would
// otherwise make the same two-million-row pass twice.
type declScanOnce struct {
	d    *DB
	f    Filter
	once sync.Once
	scan *declScan
	err  error
}

func (h *declScanOnce) get() (*declScan, error) {
	h.once.Do(func() { h.scan, h.err = h.d.scanDecls(h.f, false) })
	return h.scan, h.err
}

// scopedDeclsScan is scopedDecls from one pass over the table: one row per
// (session, kind, name, server) for the sessions in scope, in the order GROUP BY would give them.
func (d *DB) scopedDeclsScan(h *declScanOnce, sessions func(string) bool) ([]declRow, error) {
	scan, err := h.get()
	if err != nil {
		return nil, err
	}
	type key struct{ session, kind, name, server string }
	merged := map[key]*declRow{}
	for k, v := range scan.facts {
		if !sessions(k.session) {
			continue
		}
		kk := key{k.session, k.kind, k.name, k.server}
		if r := merged[kk]; r == nil {
			merged[kk] = &declRow{session: k.session, kind: k.kind, name: k.name, server: k.server,
				tokens: v.tokens, hasText: v.hasText}
		} else {
			if v.tokens > r.tokens {
				r.tokens = v.tokens
			}
			r.hasText = r.hasText || v.hasText
		}
	}
	out := make([]declRow, 0, len(merged))
	for _, r := range merged {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.session != b.session {
			return a.session < b.session
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return a.server < b.server
	})
	return out, nil
}

// max64 is the int64 maximum. Not the builtin: this package's tests define their own int-only min,
// which shadows it for the whole package and breaks any use on another type.
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
