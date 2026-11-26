package dash

// GET /api/adapt/settings: what the client settings that cost money have cost, in hindsight.
//
// The plugin finds the settings on the user's own machine. This route prices them. For each check
// it returns the request count and the summed cost_usd of the rows that have the check's request
// shape, for the caller's tenant. A shape is request COLUMNS only (model, tools, system_blocks,
// max_tokens, stream, messages). No request text is read. A check with no known shape returns
// cost_usd null ("not measured"), never 0.
//
// The list of checks lives in ONE place for the plugin: context-guru-plugin/scripts/costly_settings.json.
// This table must carry the same shapes. A test in context-guru-plugin compares them.

import (
	"net/http"
	"strings"
)

// CostlyShape is one request shape. A nil pointer means "any".
type CostlyShape struct {
	ID            string
	ModelContains string // lower case; "" = any model
	Tools         *int
	SystemBlocks  *int
	MaxTokens     *int
	Stream        *int
	Messages      *int // the dashboard's count: the system prompt counts as one
	Status        *int // upstream HTTP status
	// SessionHas, when set, keeps only rows whose session also has a row of that shape. The inner row
	// must be in the same tenant and the same time window as the outer one.
	SessionHas *CostlyShape
}

// CostlyCheck is one check the route prices. Shapes is empty when the shape is not known yet.
type CostlyCheck struct {
	ID     string
	Shapes []CostlyShape // summed into the main figure
	Also   []CostlyShape // reported apart, never added to the main figure
}

func ip(n int) *int { return &n }

// CostlyChecks is the price side of the plugin's list.
var CostlyChecks = []CostlyCheck{
	{
		ID: "disable-experimental-betas",
		Shapes: []CostlyShape{
			// No system_blocks or messages: the third system block ("Session Context") is not always
			// sent, so a classifier request has 2 or 3 blocks (1 or 2 dashboard messages). Measured on
			// a clean Claude Code 2.1.305 and on a developer's own; the 4 fields left already select no
			// other request on either proxy.
			{ID: "classifier", ModelContains: "sonnet", Tools: ip(0), MaxTokens: ip(64), Stream: ip(0)},
		},
		Also: []CostlyShape{
			{ID: "classifier-stage-2", Tools: ip(0), SystemBlocks: ip(3), MaxTokens: ip(2112), Messages: ip(2)},
		},
	},
	// The recap is one side call that the fix stops. Its saving needs no price or attribution, so it
	// has no shape on purpose and the plugin says what it saves instead of a cost.
	{ID: "session-recap"},
	{
		ID: "small-model-rejects-server-check",
		// The same classifier shape as check 1, only in sessions that also have a Haiku request the
		// upstream answered with 400 (the gateway rejected the server-check beta). The 400 row itself
		// is a haiku row, so the sonnet filter never counts it. Check 1 counts these rows too.
		Shapes: []CostlyShape{
			{ID: "classifier-after-haiku-400", ModelContains: "sonnet", Tools: ip(0), MaxTokens: ip(64), Stream: ip(0),
				SessionHas: &CostlyShape{ModelContains: "haiku", Status: ip(400)}},
		},
	},
}

// CostlyPart is the count and cost of one shape.
type CostlyPart struct {
	ID       string   `json:"id"`
	Requests int      `json:"requests"`
	CostUSD  *float64 `json:"cost_usd"`
}

// CostlyRow is one check in the document. CostUSD is null when the check has no known shape.
type CostlyRow struct {
	ID       string       `json:"id"`
	Measured bool         `json:"measured"`
	Requests int          `json:"requests"`
	CostUSD  *float64     `json:"cost_usd"`
	Also     []CostlyPart `json:"also"`
}

// CostlyDoc is the document of GET /api/adapt/settings.
type CostlyDoc struct {
	Kind    string      `json:"kind"`
	Version int         `json:"version"`
	Class   string      `json:"class"`
	Window  CostlyWin   `json:"window"`
	Checks  []CostlyRow `json:"checks"`
}

// CostlyWin is the window the figures cover.
type CostlyWin struct {
	Since int64 `json:"since"`
	Until int64 `json:"until"`
}

// shapeWhere renders a shape as a predicate on the requests table aliased as alias. Column names are
// fixed here; every value is a bound parameter.
func shapeWhere(s CostlyShape, alias string) (string, []any) {
	var conds []string
	var args []any
	if s.ModelContains != "" {
		conds, args = append(conds, "instr(lower("+alias+".model), ?) > 0"), append(args, strings.ToLower(s.ModelContains))
	}
	for col, v := range map[string]*int{"tools": s.Tools, "system_blocks": s.SystemBlocks, "max_tokens": s.MaxTokens,
		"stream": s.Stream, "messages": s.Messages, "status": s.Status} {
		if v != nil {
			conds, args = append(conds, alias+"."+col+" = ?"), append(args, *v)
		}
	}
	if len(conds) == 0 {
		return "1=1", nil
	}
	return strings.Join(conds, " AND "), args
}

// sessionHasWhere renders the SessionHas condition of a shape: an EXISTS on a second alias of the table,
// in the same session, with the same tenant and time scope as the outer row.
func sessionHasWhere(f Filter, in CostlyShape) (string, []any) {
	conds := []string{"x.session_id = r.session_id", "r.session_id <> ''"}
	var args []any
	if !f.WithKeepAlive {
		conds = append(conds, "x.keepalive = 0")
	}
	if !f.TenantAll {
		conds, args = append(conds, "x.tenant_id = ?"), append(args, f.Tenant)
	}
	if f.Since > 0 {
		conds, args = append(conds, "x.ts >= ?"), append(args, f.Since)
	}
	if f.Until > 0 {
		conds, args = append(conds, "x.ts < ?"), append(args, f.Until)
	}
	w, a := shapeWhere(in, "x")
	return "EXISTS (SELECT 1 FROM requests x WHERE " + strings.Join(conds, " AND ") + " AND " + w + ")", append(args, a...)
}

func (d *DB) costlyPart(f Filter, s CostlyShape) (CostlyPart, error) {
	where, args := f.where()
	sw, sargs := shapeWhere(s, "r")
	if s.SessionHas != nil {
		iw, iargs := sessionHasWhere(f, *s.SessionHas)
		sw, sargs = sw+" AND "+iw, append(sargs, iargs...)
	}
	var n int
	var usd float64
	err := d.sql.QueryRowContext(d.readCtx(),
		`SELECT COUNT(*), COALESCE(SUM(r.cost_usd),0) FROM requests r WHERE `+where+` AND `+sw,
		append(args, sargs...)...).Scan(&n, &usd)
	return CostlyPart{ID: s.ID, Requests: n, CostUSD: &usd}, err
}

// CostlySettings builds the document for one scope.
func (d *DB) CostlySettings(f Filter) (*CostlyDoc, error) {
	out := &CostlyDoc{Kind: "settings", Version: 1, Class: "counterfactual",
		Window: CostlyWin{Since: f.Since, Until: f.Until}, Checks: []CostlyRow{}}
	for _, c := range CostlyChecks {
		row := CostlyRow{ID: c.ID, Measured: len(c.Shapes) > 0, Also: []CostlyPart{}}
		if row.Measured {
			var sum float64
			for _, s := range c.Shapes {
				p, err := d.costlyPart(f, s)
				if err != nil {
					return nil, err
				}
				row.Requests += p.Requests
				sum += *p.CostUSD
			}
			row.CostUSD = &sum
		}
		for _, s := range c.Also {
			p, err := d.costlyPart(f, s)
			if err != nil {
				return nil, err
			}
			row.Also = append(row.Also, p)
		}
		out.Checks = append(out.Checks, row)
	}
	return out, nil
}

// costlySettings serves GET /api/adapt/settings. The caller's own tenant, unless a manager names another.
func (a *API) costlySettings(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("tenant") == "" {
		q := r.URL.Query()
		q.Set("tenant", "me")
		r = r.Clone(r.Context())
		r.URL.RawQuery = q.Encode()
	}
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	doc, err := a.db(r).CostlySettings(f)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, doc)
}
