package dash

// The prompt-text read side: what each capability actually PUTS in the prompt, and the
// whole prompt it puts it in.
//
// Why this is a separate route from /api/tools rather than more fields on it. The report is
// forty numbers and is fetched on every tab switch; the text is the tool schemas, the skill
// listing and the system prompt — tens of kilobytes for a real catalogue, and the reason
// somebody opens the page is almost never to read them. So the numbers stay cheap and the
// text is fetched when a reader asks for it.
//
// THE GATE. Everything here is CONTENT: a tool schema is whatever an SDK author wrote, a
// system prompt is whatever the user (or their CLAUDE.md, or something they pasted) wrote.
// It is stored only under the transcript-capture consent pair — the operator's
// --dashboard-content AND the tenant's own opt-in — so this route reports the same three
// states the diff view does, and it reports WHICH gate is shut: telling a reader to enable
// their own setting when the operator's is the one that is off is the bug captureState
// exists to prevent.
//
// The fourth state is the one that has nothing to do with consent: a row written before
// this column existed. Those read "not recorded yet" from an explicit coverage COUNT
// (PromptStat.Rows / TextRows), never from a fabricated empty string — the same discipline
// cache_ttl gets, and for the same reason: a blank panel is indistinguishable from a
// feature that is broken.

import (
	"database/sql"
	"net/http"
	"sort"
)

// PromptRegion is one readable slice of the prefix every request carries.
type PromptRegion struct {
	// Kind is the declaration kind, or KindSystemPrompt for the system prompt itself.
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Server string `json:"server,omitempty"`
	// Tokens is this region's measured BPE weight, and Share its percentage of the whole
	// prefix below. Both are computed from the STORED weights, so they agree with the
	// figures on the Inventory report rather than being re-derived from the text.
	Tokens int     `json:"tokens"`
	Share  float64 `json:"share"`
	// Text is the region itself, or "" when it was not stored. HasText distinguishes the
	// two, because a tool CAN legitimately have a very short declaration and "" is not
	// the same answer as "we do not have it".
	Text    string `json:"text,omitempty"`
	HasText bool   `json:"has_text"`
}

// PromptView is one session's prefix, decomposed.
type PromptView struct {
	// Captured is whether any text at all is available here. When false, BlockedBy names
	// the party who can change that (CaptureBlockedByOperator / CaptureBlockedByTenant), or
	// is "" when consent is fine and the rows simply predate the column.
	Captured  bool   `json:"captured"`
	BlockedBy string `json:"blocked_by,omitempty"`
	// Session / TS / Digest identify WHICH set this is. It is one session's, not an
	// aggregate: a prompt averaged over sessions is not a prompt anybody sent, and the
	// reader has to be able to tell which one they are reading.
	Session string `json:"session,omitempty"`
	TS      int64  `json:"ts,omitempty"`
	Digest  string `json:"digest,omitempty"`
	// Regions is the prefix, heaviest first. NOT wire order: the array order a client sent
	// its tools in is not stored, and ordering by weight is what the reader wants anyway —
	// but it means the UI must not describe this as "the order the model sees them in".
	Regions []PromptRegion `json:"regions"`
	// Tokens is the summed weight of every region here — the whole that Share is a part of.
	Tokens int `json:"tokens"`
	// Rows / TextRows is the scope-wide coverage count. Present even when this view has
	// text, because "you can read this session and not the other 300" is the fact a reader
	// needs before concluding anything about their history.
	Rows     int `json:"rows"`
	TextRows int `json:"text_rows"`
}

// promptRoutes is mounted from toolRoutes, so the scoping test walks it like every other.
func (a *API) prompt(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		unauthorized(w)
		return
	}
	view, err := a.rec.DB().PromptViewFor(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "could not read the prompt text")
		return
	}
	if !view.Captured {
		// Only now ask WHY there is nothing: a scope that has text needs no blame, and
		// captureState reports the CURRENT consent, which says nothing about whether a row
		// written last week has text. Ordering it this way keeps the two answers apart.
		if captured, blockedBy := a.captureState(f.Tenant); !captured {
			view.BlockedBy = blockedBy
		}
	}
	writeJSON(w, view)
}

// PromptViewFor returns the most recently captured prefix in scope whose text was stored,
// falling back to the most recent one at all so the caller still learns the WEIGHTS when
// the text is absent.
//
// One session's rows, chosen by the newest ts. The alternative — the union of every name
// ever declared — would be a prompt no request ever carried, with two versions of the same
// tool's schema in it.
func (d *DB) PromptViewFor(f Filter) (*PromptView, error) {
	where, args := f.where()
	v := &PromptView{}
	// Coverage first and over the WHOLE scope: it is the honest denominator for anything
	// the reader concludes from the one session below.
	sub := `(SELECT r.session_id FROM requests r WHERE ` + where + ` AND r.tools > 0)`
	// Counted the SAME WAY the report counts it: one per (session, kind, name, server), not one
	// per raw row. The table holds a row per DIGEST as well, so a session that changed its
	// declaration set mid-way has several rows saying the same thing — raw rows came out at
	// 4,192 where the report's PromptStat said 309, and two coverage figures 13x apart on one
	// page with nothing connecting them is how a dashboard loses a reader's trust for good.
	tq := `SELECT COUNT(*), SUM(txt) FROM (
		SELECT MAX(CASE WHEN d.text_gz IS NOT NULL THEN 1 ELSE 0 END) txt
		FROM tool_declarations d WHERE d.session_id IN ` + sub
	ta := append([]any{}, args...)
	if !f.TenantAll {
		tq += ` AND d.tenant_id = ?`
		ta = append(ta, f.Tenant)
	}
	tq += ` GROUP BY d.session_id, d.kind, d.name, d.server)`
	var rows, textRows *int
	if err := d.sql.QueryRow(tq, ta...).Scan(&rows, &textRows); err != nil {
		return nil, err
	}
	if rows != nil {
		v.Rows = *rows
	}
	if textRows != nil {
		v.TextRows = *textRows
	}

	// Which set to show: the newest that HAS text, else the newest at all. Two clauses of
	// one ORDER BY rather than two queries, so "has text" always wins a tie on ts.
	pq := `SELECT d.tenant_id, d.session_id, d.digest, MAX(d.ts) ts,
		MAX(CASE WHEN d.text_gz IS NOT NULL THEN 1 ELSE 0 END) txt
		FROM tool_declarations d WHERE d.session_id IN ` + sub
	pa := append([]any{}, args...)
	if !f.TenantAll {
		pq += ` AND d.tenant_id = ?`
		pa = append(pa, f.Tenant)
	}
	pq += ` GROUP BY 1, 2, 3 ORDER BY txt DESC, ts DESC LIMIT 1`
	var tenant, session, digest string
	var ts int64
	var txt int
	switch err := d.sql.QueryRow(pq, pa...).Scan(&tenant, &session, &digest, &ts, &txt); {
	case err == sql.ErrNoRows:
		return v, nil // nothing captured in scope at all
	case err != nil:
		return nil, err
	}
	v.Session, v.Digest, v.TS, v.Captured = session, digest, ts, txt == 1

	// Pinned to the tenant the chosen row belongs to, ALWAYS — not only when the filter
	// narrows. A session id is client-supplied, so two accounts can present the same one
	// (the reason tenant_id leads this table's primary key), and in a manager's service-wide
	// view an unpinned read would splice two accounts' prompts into one panel: two system
	// prompts, one heading, no way to tell whose.
	rq := `SELECT kind, name, server, tokens, text_gz FROM tool_declarations
		WHERE tenant_id = ? AND session_id = ? AND digest = ?`
	ra := []any{tenant, session, digest}
	rs, err := d.sql.Query(rq, ra...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var reg PromptRegion
		var gz []byte
		if err := rs.Scan(&reg.Kind, &reg.Name, &reg.Server, &reg.Tokens, &gz); err != nil {
			return nil, err
		}
		if gz != nil {
			reg.Text, reg.HasText = gunzipText(gz), true
		}
		v.Regions = append(v.Regions, reg)
		v.Tokens += reg.Tokens
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	for i := range v.Regions {
		if v.Tokens > 0 {
			v.Regions[i].Share = 100 * float64(v.Regions[i].Tokens) / float64(v.Tokens)
		}
	}
	// Heaviest first, with the system prompt pinned to the front: it is the region every
	// other one sits inside the same request as, and a reader scanning for "where did my
	// prompt go" should not have to find it among forty tool schemas.
	sort.SliceStable(v.Regions, func(i, j int) bool {
		a, b := v.Regions[i], v.Regions[j]
		if (a.Kind == KindSystemPrompt) != (b.Kind == KindSystemPrompt) {
			return a.Kind == KindSystemPrompt
		}
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		return a.Name < b.Name
	})
	return v, nil
}
