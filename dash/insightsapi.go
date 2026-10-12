package dash

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// GET /api/insights — the shaped document behind the served insights view.
//
// It exists rather than letting the page shape /api/tools itself for two reasons, and the second
// is the one that settles it:
//
//   - The honesty rules (what may be recommended, at what scope, on what evidence) belong
//     somewhere a test can reach them. In page JavaScript they would not be.
//   - A description comes from the item's own SKILL.md on disk. A browser cannot read one, so a
//     page shaping its own data would simply lose the "what will I lose by removing this" column —
//     which is the question a reader asks before changing their own configuration.

// insights serves the shaped document. Windowing and the per-session breakdown come from the same
// query parameters /api/tools honours, because they are the same question asked of the same rows.
func (a *API) insights(w http.ResponseWriter, r *http.Request) {
	f, p, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	// NOT a.priceFn(r): that closes over r.Context(), and serveJSON DETACHES its computation from
	// the caller on purpose — see its comment at the top of serveJSON, which states that
	// r.Context() is already cancelled by the time a background refresh runs. A price lookup bound
	// to it would then be cancelled too, and a cancelled pricer does not error: it reports every
	// model as unpriced, which this document renders as $0.00.
	//
	// That failure is latent rather than observed — the pricers in use ignore their context — but
	// a zero meaning "we could not read the rate" is indistinguishable from one meaning "this is
	// free", and that is the one confusion this surface exists to prevent. The cache entry's
	// lifetime is the correct one for a lookup the cache entry needs.
	price := a.detachedPriceFn()
	state := a.toolFilterStateForScope(f)
	nSessions := atoiDefault(r.URL.Query().Get("sessions"), 0)

	a.serveJSON(w, r, &a.insightsCache, cacheKey(p, r), func(db *DB) ([]byte, error) {
		f := f
		if nSessions > 0 {
			ids, err := db.RecentSessions(f, nSessions)
			if err != nil {
				return nil, err
			}
			// An empty window stays empty. A silent widening would report all history under a
			// label saying "the last N", which is the one thing a windowed view must not do.
			f.SessionIn = ids
			if len(ids) == 0 {
				f.SessionIn = []string{""}
			}
		}
		rep, err := db.ToolReportFor(f, price)
		if err != nil {
			return nil, err
		}

		// THE FLOOR IS THE SERVER'S OWN, not a reimplementation. A ToolStat carries no dates at
		// all, so a local check could only test the session count — which passes for five
		// sessions in one afternoon, the exact case the seven-day span exists to reject.
		//
		// Best-effort: an unreadable document yields an empty qualified set, which demotes every
		// item to `consider`. Failing closed matters here, because the alternative to "we could
		// not read the floor" is recommending a removal whose evidence nobody checked.
		qualified := map[string]bool{}
		if doc, err := db.ToolFilterDocFor(f, price, state); err == nil && doc != nil {
			for _, s := range doc.Suggestions {
				qualified[s.Name] = true
			}
		}

		doc := ShapeInsights(rep, InsightsOptions{
			Qualified:    qualified,
			Descriptions: skillDescriptions(""),
		})

		// The per-item timeline, when asked for. Opt-in because it is two int arrays per item over
		// every day the store holds, which is a materially bigger payload than the verdicts, and
		// most readers of this document want the table rather than the picture.
		//
		// `timeline_days` narrows the axis; absent means every day there is. Unbounded by default
		// on purpose: the value of the picture is seeing a phase CHANGE, and a default window
		// would hide the phase before it — the exact thing the reader came to look at.
		if r.URL.Query().Get("timeline") != "" {
			tl, err := db.ToolTimeline(f, atoiDefault(r.URL.Query().Get("timeline_days"), 0))
			if err != nil {
				return nil, err
			}
			doc.Timeline = tl
		}
		return json.Marshal(doc)
	})
}

// insightsRoutes is this feature's mounted surface, in the same shape as api.go's table so the
// scope decision is data here too. In the table and not mounted beside it: a route registered
// anywhere else is a route whose scope class the scoping tests never walk, which is how a
// tenant-scoped surface quietly becomes a public one.
func (a *API) insightsRoutes() []route {
	// scopeTenant, like the inventory it reads: the rows are the caller's own configuration, and
	// a manager reaches another tenant's view through the same ?tenant= every other route honours.
	return []route{{"GET /api/insights", scopeTenant, a.insights}}
}

// skillDescriptions reads every skill description available on the machine running this proxy,
// keyed as the inventory names them.
//
// WHAT THIS IS NOT. It is not a complete map, and must not be read as one. Three kinds of skill
// reach a prompt and only two leave a file here: a plugin's skills live under the plugin cache, a
// marketplace's standalone skills under the marketplace directory, and the skills built into
// Claude Code have no file anywhere. For the third kind there is nothing to read, so the item
// carries no description and says so — a plausible sentence invented here would be
// indistinguishable from one read out of a file.
//
// A HOSTED PROXY SEES NOTHING. These paths are on the machine serving the request, which is the
// user's own for a local install and somebody else's for a shared one. There the map is empty and
// every item reports its description as unknown, which is correct rather than degraded.
//
// `home` overrides the home directory, for tests.
func skillDescriptions(home string) map[string]string {
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		} else {
			return map[string]string{}
		}
	}
	base := filepath.Join(home, ".claude", "plugins")
	out := map[string]string{}

	// Plugin skills: cache/<marketplace>/<plugin>/<version>/skills/<name>/SKILL.md, keyed
	// <plugin>:<name> to match the inventory. A newer version of the same plugin wins, because
	// that is the one Claude Code loads.
	cache := filepath.Join(base, "cache")
	for _, mkt := range dirNames(cache) {
		for _, plug := range dirNames(filepath.Join(cache, mkt)) {
			for _, ver := range dirNames(filepath.Join(cache, mkt, plug)) {
				skills := filepath.Join(cache, mkt, plug, ver, "skills")
				for _, name := range dirNames(skills) {
					if d := frontMatterDescription(filepath.Join(skills, name, "SKILL.md")); d != "" {
						out[plug+":"+name] = d
					}
				}
			}
		}
	}
	// Marketplace standalone skills: marketplaces/<m>/skills/<name>/SKILL.md, keyed bare.
	markets := filepath.Join(base, "marketplaces")
	for _, mkt := range dirNames(markets) {
		skills := filepath.Join(markets, mkt, "skills")
		for _, name := range dirNames(skills) {
			if _, taken := out[name]; taken {
				continue
			}
			if d := frontMatterDescription(filepath.Join(skills, name, "SKILL.md")); d != "" {
				out[name] = d
			}
		}
	}
	return out
}

// dirNames lists subdirectory names, or nothing. A missing plugin cache is the normal state of a
// machine with no plugins installed, not an error worth failing a cost report over.
func dirNames(path string) []string {
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// frontMatterDescription reads the `description:` scalar from a SKILL.md's YAML front matter,
// flattened to one line.
//
// Deliberately not a YAML parser: this package vendors no YAML dependency for one scalar at the top
// of a small file, and the shapes that actually occur are a plain value, a quoted value, and a
// folded block whose continuation lines are indented. Anything it cannot read it reports as absent
// rather than guessing.
//
// Flattening is not cosmetic. A description is prose somebody else wrote; it contains newlines, and
// it reaches readers that are line-oriented, so an unflattened value would be read as two facts.
func frontMatterDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// The front matter is at the top and small. Reading all of a large SKILL.md to find it would
	// make this scale with the body rather than the header.
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	head := string(buf[:n])
	if !strings.HasPrefix(head, "---") {
		return ""
	}
	if end := strings.Index(head[3:], "\n---"); end != -1 {
		head = head[3 : 3+end]
	} else {
		head = head[3:]
	}

	lines := strings.Split(head, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "description:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		switch value {
		case ">", "|", ">-", "|-", ">+", "|+":
			// A folded or literal block: the value is the indented lines that follow.
			var parts []string
			for _, nxt := range lines[i+1:] {
				if strings.TrimSpace(nxt) != "" && !strings.HasPrefix(nxt, " ") &&
					!strings.HasPrefix(nxt, "\t") {
					break
				}
				parts = append(parts, strings.TrimSpace(nxt))
			}
			value = strings.Join(parts, " ")
		default:
			// A plain scalar may still continue onto indented lines.
			parts := []string{value}
			for _, nxt := range lines[i+1:] {
				if strings.TrimSpace(nxt) == "" || (!strings.HasPrefix(nxt, " ") &&
					!strings.HasPrefix(nxt, "\t")) {
					break
				}
				parts = append(parts, strings.TrimSpace(nxt))
			}
			value = strings.Join(parts, " ")
			if len(value) >= 2 && value[0] == value[len(value)-1] &&
				(value[0] == '"' || value[0] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		return strings.Join(strings.Fields(value), " ")
	}
	return ""
}

// detachedPriceFn is priceFn without the request's lifetime.
//
// serveJSON's computation outlives its caller by design, so a price function closing over
// r.Context() would be cancelled before a background refresh could use it. A cancelled pricer does
// not error — it reports every model as unpriced, which renders as $0.00, and a zero meaning "we
// could not read the rate" is indistinguishable from one meaning "this is free".
//
// Latent, not observed: every pricer in use today ignores its context, so nothing has broken yet.
// Written this way because the hazard is documented in serveJSON itself and costs nothing to
// avoid, not because a bug was measured.
func (a *API) detachedPriceFn() func(string) (modelinfo.Price, bool) {
	if a.pricer == nil {
		return nil
	}
	return func(model string) (modelinfo.Price, bool) {
		if model == "" {
			return modelinfo.Price{}, false
		}
		return a.pricer.Price(context.Background(), model)
	}
}
