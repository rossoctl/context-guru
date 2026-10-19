package dash

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
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
	// UsedHere / UnusedHere are the same split for the SELECTED PROJECT, and are 0 when no
	// project is selected. They partition InstalledSkills.
	UsedHere   int `json:"used_here"`
	UnusedHere int `json:"unused_here"`
	// UnusedHereButUsedElsewhere is the honesty figure, and the reason this split is computed on
	// the server at all.
	//
	// "43 skills not used in this project" is NOT a waste figure: some of those are used in other
	// projects, which is correct behaviour. Reporting the bare count turns deliberate usage into
	// an accusation — so the view subtracts this from it and says the rest is waste. A
	// project-scoped document cannot derive it, because the other projects' usage is not in it.
	UnusedHereButUsedElsewhere int `json:"unused_here_but_used_elsewhere"`
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
	// Read once: every folder in this document resolves through the same grouping, and
	// ProjectGroups parses every transcript on disk.
	groups := ProjectGroups(home)

	// The span and the measured/imported split, from the requests themselves.
	if err := d.sql.QueryRow(`SELECT
		COALESCE(MIN(r.ts), 0), COALESCE(MAX(r.ts), 0),
		COALESCE(SUM(CASE WHEN r.source = '' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN r.source <> '' THEN 1 ELSE 0 END), 0)
		FROM requests r WHERE `+where, args...).
		Scan(&doc.FirstTS, &doc.LastTS, &doc.MeasuredRequests, &doc.ImportedRequests); err != nil {
		return nil, err
	}

	// What was invoked.
	//
	// SCOPED BY FOLDER WHEN A PROJECT IS SELECTED, not by session. A session that touched the
	// folder is not the same thing as work done in it: one Claude Code session works in several
	// folders, and selecting any of them used to return the whole session's tool list. Measured
	// on a live store, a 6-request folder reported a 2,626-request session's usage, which is why
	// every project showed the same figures.
	//
	// Rows written before `tool_uses.project` existed carry '' and are therefore absent from
	// every project's list. That is correct rather than lossy: their folder is unknown, and
	// attributing them to the selected one would be a guess presented as a measurement. They
	// remain in the unscoped view, where a figure nobody can attribute belongs.
	useWhere := `u.session_id IN (SELECT DISTINCT r.session_id FROM requests r WHERE ` + where + `)`
	useArgs := args
	if f.Project != "" {
		// EVERY FOLDER THAT ROLLS UP TO THE SELECTED PROJECT, not just the project's own path.
		// A reader selecting `notes` is asking about the work done in `notes/server` too —
		// matching only the exact path would mean selecting a project showed LESS than the
		// unscoped view did, which is the trap folding the picker without folding the query
		// walks straight into.
		folders := groups.FoldersOf(f.Project)
		ph := make([]string, len(folders))
		pre := make([]any, len(folders))
		for i, fo := range folders {
			ph[i] = "?"
			pre[i] = fo
		}
		useWhere = `u.project IN (` + strings.Join(ph, ",") + `) AND ` + useWhere
		useArgs = append(pre, args...)
	}
	rows, err := d.sql.Query(`SELECT
		CASE WHEN u.skill <> '' THEN u.skill ELSE u.name END AS cap,
		CASE WHEN u.skill <> '' THEN 1 ELSE 0 END AS is_skill,
		SUM(u.calls), MIN(u.first_ts), MAX(u.last_ts)
		FROM tool_uses u
		WHERE `+useWhere+`
		GROUP BY cap, is_skill ORDER BY SUM(u.calls) DESC`, useArgs...)
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

	// Installed but never called ANYWHERE.
	//
	// MACHINE-WIDE ON PURPOSE, and deliberately a different scope from Used above. A skill unused
	// in one folder is not waste: a user runs `brainstorming` in their notes project and never in
	// a Go repo, and that is correct behaviour rather than a finding. Scoping this to the selected
	// project reported every such skill as abandoned — observed on a live store, where each
	// project claimed 43 of 46 skills were unused, and the busiest one said nothing had been used
	// at all while listing fifteen invoked capabilities beside it.
	//
	// The two scopes are stated in Basis, because two figures from different scopes sitting
	// side by side are read as sharing one unless something says otherwise.
	calledAnywhere, err := d.calledAnywhere()
	if err != nil {
		return nil, err
	}
	installed := installedSkills(home)
	doc.InstalledSkills = len(installed)
	for name := range installed {
		if calledAnywhere[name] {
			// COUNTED HERE, against the inventory, rather than left to arithmetic on Used. A used
			// capability absent from disk is in neither set, so Used and NeverUsed do not
			// partition — and a reader who adds them gets more than the denominator.
			doc.UsedInstalled++
			continue
		}
		doc.NeverUsed = append(doc.NeverUsed, name)
	}
	sort.Strings(doc.NeverUsed)

	// THE PER-PROJECT SPLIT. Only meaningful under a project filter, where `called` holds this
	// folder's invocations and `calledAnywhere` holds every project's.
	if f.Project != "" {
		for name := range installed {
			if called[name] {
				doc.UsedHere++
				continue
			}
			doc.UnusedHere++
			// Used somewhere, just not here. NOT waste, and counted separately so the view can
			// say so rather than letting a reader take the whole unused count as money burning.
			if calledAnywhere[name] {
				doc.UnusedHereButUsedElsewhere++
			}
		}
	}

	// Spend per project. Both sources, because usage is the one thing they agree on.
	//
	// ROLLED UP TO THE PROJECT, not listed per directory. The raw column holds every cwd a
	// session was launched in: on a real machine that was 40 folders for nine projects, most of
	// them subdirectories of a repository or throwaway /private/tmp directories. Claude Code's
	// own transcript grouping decides which is which — see ProjectGroups.
	prows, err := d.sql.Query(`SELECT r.project, COUNT(*),
		COALESCE(SUM(r.fresh_input + r.cache_read + r.cache_write + r.output_tokens), 0)
		FROM requests r WHERE `+where+` AND r.project <> ''
		GROUP BY r.project ORDER BY 2 DESC`, args...)
	if err != nil {
		return nil, err
	}
	rolled := map[string]*HistoryProject{}
	var order []string
	for prows.Next() {
		var p HistoryProject
		if err := prows.Scan(&p.Project, &p.Requests, &p.Tokens); err != nil {
			prows.Close()
			return nil, err
		}
		root := groups.Project(p.Project)
		agg := rolled[root]
		if agg == nil {
			agg = &HistoryProject{Project: root}
			rolled[root] = agg
			order = append(order, root)
		}
		agg.Requests += p.Requests
		agg.Tokens += p.Tokens
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}
	// Busiest first, which the per-directory ORDER BY no longer guarantees once rows are merged.
	sort.Slice(order, func(i, j int) bool {
		if rolled[order[i]].Requests != rolled[order[j]].Requests {
			return rolled[order[i]].Requests > rolled[order[j]].Requests
		}
		return order[i] < order[j]
	})
	for _, root := range order {
		doc.Projects = append(doc.Projects, *rolled[root])
	}

	// THE BASIS, and the refusal it carries. Stated rather than left to the reader, because the
	// absence of a cost column reads as zero unless something says otherwise.
	doc.Basis = "whether each capability was invoked, from this machine's own session history. " +
		"What carrying the unused ones COST is not in this data: a transcript records what a " +
		"request spent in total and never which tools it declared, so only traffic the proxy " +
		"watched can be priced."
	if f.Project != "" {
		doc.Basis = "what was invoked in " + f.Project + ", from this machine's own session " +
			"history. The never-used count covers every project on this machine, not just this " +
			"one: a skill you use elsewhere and not here is not waste. " + doc.Basis
	}
	if doc.SelfExcluded.Items > 0 {
		doc.Basis += " " + selfPlugin + "'s own skills are excluded from these figures — counting " +
			"the weight of the tool producing this view would be circular — though they do " +
			"occupy the prompt like any other."
	}
	return doc, nil
}

// calledAnywhere is every capability this machine's history shows being invoked, in any project
// and any window.
//
// UNSCOPED BY DESIGN — see the NeverUsed comment above. It answers "has this ever been reached
// for", which is the only question whose answer makes "never used" a finding rather than a
// statement about which folder the reader happens to have selected.
func (d *DB) calledAnywhere() (map[string]bool, error) {
	rows, err := d.sql.Query(`SELECT DISTINCT
		CASE WHEN skill <> '' THEN skill ELSE name END FROM tool_uses`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
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
