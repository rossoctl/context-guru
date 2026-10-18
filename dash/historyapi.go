package dash

import (
	"encoding/json"
	"net/http"
	"sort"
)

// GET /api/history — what the user's own past says about the capabilities they installed.
//
// A DIFFERENT KIND OF CLAIM FROM THE REST OF THIS DASHBOARD, which is why it is a separate
// surface rather than more columns on the insights page. Every other figure here is "this cost
// you X and you never used it", and that needs the proxy to have watched the traffic. This one is
// "you used this / you never used this", which Claude Code's own transcripts can answer alone —
// so it works on the day the proxy is installed, across months of history the proxy never saw.
//
// WHAT IT REFUSES TO SAY is as much the point as what it says. Whether a capability was used is
// in the history; what carrying it COST is not, because a transcript records a request's total
// usage and never the list of tools it declared. So this names candidates and declines to price
// them. A figure invented here would be worse than absent: it would cover a longer span than the
// measured ones beside it and would therefore look more authoritative.

// HistoryUse is one capability the history shows being invoked.
type HistoryUse struct {
	Name  string `json:"name"`
	Skill bool   `json:"skill"`
	Calls int64  `json:"calls"`
	// FirstTS / LastTS: when it was first and last reached for. Both, because "used once in
	// March" and "used daily since March" are different facts and a bare count cannot tell them
	// apart — and a date is what makes "used" checkable against the reader's own memory.
	FirstTS int64 `json:"first_ts"`
	LastTS  int64 `json:"last_ts"`
}

// HistoryProject is the traffic recorded against one project folder.
type HistoryProject struct {
	Project  string `json:"project"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// HistoryDoc is what the history can support, and nothing beyond it.
type HistoryDoc struct {
	// Used, most-called first.
	Used []HistoryUse `json:"used"`
	// NeverUsed is installed on this machine and absent from every invocation in the window.
	//
	// THE FINDING THIS SURFACE EXISTS FOR. The inventory is on disk and the invocations are in
	// the transcripts, so "you have 46 skills and have used 10 of them" needs no inference and is
	// available immediately — where a cost figure would have to wait for the proxy to watch.
	NeverUsed []string `json:"never_used"`
	// InstalledSkills is the denominator. Half the finding: "31 unused" means nothing without it.
	InstalledSkills int `json:"installed_skills"`
	// UsedInstalled is how many of InstalledSkills were invoked. STATED RATHER THAN INFERRED,
	// because Used and NeverUsed do not partition: a used capability that is not on disk — a
	// built-in, or one installed under a name since changed — is in neither set. Without this
	// field a reader who adds "10 used" to "40 never used" gets 50 against 46 installed and stops
	// believing the rest of the page.
	UsedInstalled int `json:"used_installed"`
	// Projects, busiest first.
	Projects []HistoryProject `json:"projects"`
	// The span the claims rest on. "Never used" is meaningless without it.
	FirstTS int64 `json:"first_ts"`
	LastTS  int64 `json:"last_ts"`
	// Requests seen, split by whether the proxy watched them.
	MeasuredRequests int64 `json:"measured_requests"`
	ImportedRequests int64 `json:"imported_requests"`
	// SelfExcluded is this tool's own capabilities: held out of every figure above, and reported
	// here because they do occupy the prompt.
	//
	// EXCLUDING IT FROM THE FIGURES IS RIGHT; HIDING THAT IT COSTS ANYTHING IS NOT. Counting its
	// own weight as a saving it recommends would be circular, so it never reaches Used or
	// NeverUsed — but a reader who counts more skills in `/plugin` than appear here must be able
	// to find out why. The insights page reports its own weight the same way; this reuses the
	// shape so the two read alike. Tokens and USD stay zero here: what CARRYING a capability
	// costs needs the declaration list, which a transcript does not record — the same refusal
	// Basis states for everything else on this surface.
	SelfExcluded SelfExclusion `json:"self_excluded"`
	// Basis states what kind of claim this is, in the same spirit as InsightsDoc.Basis.
	Basis string `json:"basis"`
}

// HistoryDocFor builds the document for one window.
//
// `home` overrides the home directory, for tests. An empty installed inventory is the normal
// state of a machine with no plugins, and of a HOSTED proxy — which has no access to the user's
// disk at all, so NeverUsed is empty there rather than wrong. That is the same limit
// skillDescriptions documents.
func (d *DB) HistoryDocFor(f Filter, home string) (*HistoryDoc, error) {
	where, args := f.where()
	doc := &HistoryDoc{Used: []HistoryUse{}, NeverUsed: []string{}, Projects: []HistoryProject{}}

	// The span and the measured/imported split, from the requests themselves.
	if err := d.sql.QueryRow(`SELECT
		COALESCE(MIN(r.ts), 0), COALESCE(MAX(r.ts), 0),
		COALESCE(SUM(CASE WHEN r.source = '' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN r.source <> '' THEN 1 ELSE 0 END), 0)
		FROM requests r WHERE `+where, args...).
		Scan(&doc.FirstTS, &doc.LastTS, &doc.MeasuredRequests, &doc.ImportedRequests); err != nil {
		return nil, err
	}

	// What was invoked. Joined to requests so the window applies: tool_uses is keyed by session,
	// and a session can straddle a window boundary.
	rows, err := d.sql.Query(`SELECT
		CASE WHEN u.skill <> '' THEN u.skill ELSE u.name END AS cap,
		CASE WHEN u.skill <> '' THEN 1 ELSE 0 END AS is_skill,
		SUM(u.calls), MIN(u.first_ts), MAX(u.last_ts)
		FROM tool_uses u
		WHERE u.session_id IN (SELECT DISTINCT r.session_id FROM requests r WHERE `+where+`)
		GROUP BY cap, is_skill ORDER BY SUM(u.calls) DESC`, args...)
	if err != nil {
		return nil, err
	}
	called := map[string]bool{}
	for rows.Next() {
		var u HistoryUse
		var isSkill int
		if err := rows.Scan(&u.Name, &isSkill, &u.Calls, &u.FirstTS, &u.LastTS); err != nil {
			rows.Close()
			return nil, err
		}
		u.Skill = isSkill == 1
		// THE REPORTER STAYS OUT OF ITS OWN REPORT, under every name it has shipped under —
		// isSelfOwned, not a prefix match on the current one, because this surface reads history
		// from before the rename.
		if isSelfOwned(u.Name) {
			// Counted, not merely skipped — see HistoryDoc.SelfExcluded.
			doc.SelfExcluded.Plugin = selfPlugin
			doc.SelfExcluded.Items++
			continue
		}
		called[u.Name] = true
		doc.Used = append(doc.Used, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Installed but never called.
	installed := installedSkills(home)
	doc.InstalledSkills = len(installed)
	for name := range installed {
		if called[name] {
			// COUNTED HERE, against the inventory, rather than left to arithmetic on Used. A used
			// capability absent from disk is in neither set, so Used and NeverUsed do not
			// partition — and a reader who adds them gets more than the denominator.
			doc.UsedInstalled++
			continue
		}
		doc.NeverUsed = append(doc.NeverUsed, name)
	}
	sort.Strings(doc.NeverUsed)

	// Spend per project. Both sources, because usage is the one thing they agree on.
	prows, err := d.sql.Query(`SELECT r.project, COUNT(*),
		COALESCE(SUM(r.fresh_input + r.cache_read + r.cache_write + r.output_tokens), 0)
		FROM requests r WHERE `+where+` AND r.project <> ''
		GROUP BY r.project ORDER BY 2 DESC`, args...)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var p HistoryProject
		if err := prows.Scan(&p.Project, &p.Requests, &p.Tokens); err != nil {
			prows.Close()
			return nil, err
		}
		doc.Projects = append(doc.Projects, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}

	// THE BASIS, and the refusal it carries. Stated rather than left to the reader, because the
	// absence of a cost column reads as zero unless something says otherwise.
	doc.Basis = "whether each capability was invoked, from this machine's own session history. " +
		"What carrying the unused ones COST is not in this data: a transcript records what a " +
		"request spent in total and never which tools it declared, so only traffic the proxy " +
		"watched can be priced."
	if doc.SelfExcluded.Items > 0 {
		doc.Basis += " " + selfPlugin + "'s own skills are excluded from these figures — counting " +
			"the weight of the tool producing this view would be circular — though they do " +
			"occupy the prompt like any other."
	}
	return doc, nil
}

// history serves the document.
func (a *API) history(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	a.serveJSON(w, r, &a.historyCache, cacheKey(p, r), func(db *DB) ([]byte, error) {
		// skillDescriptions' home rules apply: "" means this machine's own, and a hosted proxy
		// finds nothing, which is correct rather than degraded.
		doc, err := db.HistoryDocFor(f, "")
		if err != nil {
			return nil, err
		}
		return json.Marshal(doc)
	})
}

// historyRoutes is this surface's mounted route, in api.go's table shape so its scope class is
// one the scoping tests walk — a route registered anywhere else is one whose scope nobody checks.
func (a *API) historyRoutes() []route {
	// scopeTenant, like the inventory it sits beside: these are the caller's own capabilities.
	return []route{{"GET /api/history", scopeTenant, a.history}}
}
