package dash

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"gopkg.in/yaml.v3"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// Which deterministic compaction configuration would have cost this account least, found by
// RUNNING the shipped components on the account's own stored requests.
//
// # What is replayed, and what cannot be
//
// The database does not keep request bodies. request_content keeps, per request, the
// before/after text of each message block a component REWROTE — at most ContentMaxPerRequest
// per request, each `before` clipped at apply.TraceTextCap. So a request is rebuilt as a
// synthetic body that carries the stored blocks at their real message indices and a one-token
// placeholder everywhere else, and that body goes through apply.BodyOpts with a per-session
// store and the request's own clock. The product's own cache-aware freeze, sticky replay and
// never-worse guard then make every decision; nothing here re-implements a component.
//
// A clipped block cannot be run (a component would see a truncated output); it is modelled from
// its stored sizes instead, conservatively for dropping a component (adaptBlock.modelled). A
// message no component ever touched was never stored. Both are reported as coverage, and every
// dollar figure is over the stored blocks only. The second gap has a direction: a candidate that
// would act on content the history left alone is invisible, so the replay UNDER-states arms
// that do more than the account's history did, and fully sees arms that do less.
//
// # Pricing
//
// Gross, per request, at the deployed rates. A block's size change under an arm is priced at
// the rate the request actually paid for that position: its FIRST appearance in the session is
// the uncached tail (the write rate, 5m and 1h tiers blended by what the request wrote, or
// fresh where nothing was written), every later appearance at Event.repeatRate — the rule
// request_components.saved_usd already uses. An arm that rewrites a block it had already sent
// differently busts the prefix on a turn that read from cache, and is charged that turn's whole
// cache read re-billed as a write (an upper bound, charged against the arm). An arm whose
// pipeline can mint a marker carries the expand tool declaration on every request.
//
// Savings are replay(current) - replay(arm): both sides go through the same engine, so what
// the synthetic body gets wrong is common to both.

// AdaptCompactionCandidates are the components the search may add, drop, reorder or tune: the shipped
// deterministic compaction components, in the default pipeline's order. Everything else in an
// account's pipeline (cachesplit, toolfilter, the LLM components) keeps its place in the
// applied config and is not run by the replay.
var AdaptCompactionCandidates = []string{"format", "dedup", "toon", "cmdfilter", "searchfold", "textclean", "extract"}

// adaptKnobs are the settings the search varies, and the only settings the apply route accepts
// from a client. Values are the grid tried.
var adaptKnobs = map[string]map[string][]int{
	"extract": {"min_tokens": {200, 300, 400, 800}},
	"dedup":   {"min_tokens": {50, 100, 200}},
}

// AdaptCompactionKnobAllowed reports whether comp.key=v is a setting the apply route may store.
func AdaptCompactionKnobAllowed(comp, key string, v int) bool {
	for _, x := range adaptKnobs[comp][key] {
		if x == v {
			return true
		}
	}
	return false
}

func isAdaptCandidate(name string) bool {
	for _, c := range AdaptCompactionCandidates {
		if c == name {
			return true
		}
	}
	return false
}

const (
	// adaptSearchRequests caps the covered requests the SEARCH replays (most recent sessions
	// first); the reported arms are then re-priced on everything covered, up to adaptMaxRequests.
	adaptSearchRequests = 1000
	adaptMaxRequests    = 3000
	adaptSearchRounds   = 4
	// adaptMaxArms bounds the search: each arm is a full replay, and the plugin waits.
	adaptMaxArms = 30
	// adaptShownOptions is how many searched options are re-priced and shown beside current,
	// default and none.
	adaptShownOptions = 5
)

// AdaptCompactionOption is one priced configuration, in the plugin's adapt contract
// (docs/plugin/adapt.md): saved is versus no deterministic compaction at all, spend is the
// expand declaration the option carries on top of that, net = saved - spend.
type AdaptCompactionOption struct {
	// ID is short ("current", "default", "none", "c1"...), so the plugin can put it on a command
	// line; Label and Pipeline say what it is.
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
	// BilledUSD is the replayed cost of the covered requests under this option.
	BilledUSD float64  `json:"billed_usd"`
	SavedUSD  float64  `json:"saved_usd"`
	SpendUSD  float64  `json:"spend_usd"`
	NetUSD    *float64 `json:"net_usd"`
	// Unavailable says why NetUSD is null.
	Unavailable string `json:"unavailable,omitempty"`
	// Pipeline is the full pipeline that would be stored: the candidate components in the
	// option's order with the account's other components kept where they were.
	Pipeline []string `json:"pipeline"`
	// UnseenUSD is the part of BilledUSD charged for measured savings the replay cannot see,
	// from current components this option drops (see adaptPop).
	UnseenUSD float64 `json:"unseen_usd"`
	// Busts counts covered requests on which this option rewrote content it had already sent.
	Busts int `json:"busts"`

	spec adaptSpec
	cost float64
}

// AdaptCompactionRecommendation is the change the report proposes, when there is one.
type AdaptCompactionRecommendation struct {
	Option      string  `json:"option"`
	Change      string  `json:"change"`
	NetDeltaUSD float64 `json:"net_delta_usd"`
	PerMonthUSD float64 `json:"per_month_usd"`
	Effective   string  `json:"effective"`
	Reason      string  `json:"reason"`
}

// AdaptCompactionCoverage says how much of the window the dollar figures stand on.
type AdaptCompactionCoverage struct {
	Requests            int     `json:"requests"`
	Sessions            int     `json:"sessions"`
	BilledUSD           float64 `json:"billed_usd"`
	RequestsWithContent int     `json:"requests_with_content"`
	SessionsWithContent int     `json:"sessions_with_content"`
	// CoveredRequests are requests with content, complete token accounting and known rates:
	// the population every dollar figure is over. CoveredBilledUSD is what they cost.
	CoveredRequests  int     `json:"covered_requests"`
	CoveredBilledUSD float64 `json:"covered_billed_usd"`
	// Blocks are distinct stored blocks; ClippedBlocks could not be run and are modelled from
	// their stored sizes (see adaptBlock.modelled). The token
	// figures are the same split in original tokens.
	Blocks             int   `json:"blocks"`
	ClippedBlocks      int   `json:"clipped_blocks"`
	BlockTokens        int64 `json:"block_tokens"`
	ClippedBlockTokens int64 `json:"clipped_block_tokens"`
	RequestsOverCap    int   `json:"requests_over_cap"`
	TruncatedToRecent  bool  `json:"truncated_to_recent,omitempty"`
	SearchRequests     int   `json:"search_requests"`
	ArchivedSessions   int   `json:"archived_sessions"`
	FidelityBlocks     int   `json:"fidelity_blocks"`
	FidelityReproduced int   `json:"fidelity_reproduced"`
	ArmsEvaluated      int   `json:"arms_evaluated"`
	DurationMs         int64 `json:"duration_ms"`
}

// AdaptCompactionMeasured is one current component's saving, measured and replayed.
type AdaptCompactionMeasured struct {
	Component   string  `json:"component"`
	MeasuredUSD float64 `json:"measured_usd"`
	ReplayedUSD float64 `json:"replayed_usd"`
}

// AdaptCompactionReport is GET /api/adapt/compaction's body: the plugin's adapt contract, plus
// the coverage and the per-component measurement it rests on.
type AdaptCompactionReport struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
	Summary string `json:"summary"`
	// Window's sessions and requests are the COVERED ones, the population every figure is over;
	// Coverage has the whole window.
	Window struct {
		Since    int64 `json:"since"`
		Until    int64 `json:"until"`
		Sessions int   `json:"sessions"`
		Requests int   `json:"requests"`
	} `json:"window"`
	Class          string                         `json:"class"`
	Method         string                         `json:"method"`
	BasisID        string                         `json:"basis_id"`
	Current        string                         `json:"current"`
	Default        string                         `json:"default"`
	Best           string                         `json:"best"`
	Options        []AdaptCompactionOption        `json:"options"`
	Recommendation *AdaptCompactionRecommendation `json:"recommendation"`
	Warnings       []string                       `json:"warnings"`
	Coverage       AdaptCompactionCoverage        `json:"coverage"`
	Pricing        string                         `json:"pricing"`
	// Measured is each current candidate component's saving on the covered requests, as the
	// product's own accounting booked it and as the replay sees dropping it.
	Measured []AdaptCompactionMeasured `json:"measured"`

	// choices maps each option id to what applying it stores; the handler offers them.
	choices map[string]AdaptCompactionChoice
}

// AdaptCompactionChoice is what applying an option stores: the candidates in their order and the
// searched settings. The report hands these out under short ids, and the apply route takes only
// an id from a report the same account just fetched (see API.AdaptOffer).
type AdaptCompactionChoice struct {
	Candidates []string
	Settings   map[string]map[string]int
	Label      string
	// ConfigHash is AdaptConfigHash of the document the report was computed against.
	ConfigHash string
}

// AdaptConfigHash identifies a configuration document; an apply against a document that has
// changed since the report is refused, because the recommendation was for one that is gone.
func AdaptConfigHash(doc string) string {
	sum := sha256.Sum256([]byte(doc))
	return fmt.Sprintf("%x", sum[:6])
}

// adaptBlock is one stored, replayable block.
type adaptBlock struct {
	path        string
	msg, blk, j int // j < 0: the tool_result's content is a string; else content[j].text
	key         string
	text        string
	tokens      int // original size
	sent        int // what was actually sent (stored after_tokens)
	replayed    int // scratch: this block's size under the arm being replayed
	// modelled marks a block whose text was clipped at capture, so it is never run. An arm
	// that keeps every candidate in touched (the candidates that rewrote it) is charged what
	// was sent; an arm missing any of them the original size. The second is the conservative
	// reading for a component being DROPPED, which is the decision these blocks would otherwise
	// hide: the largest outputs are the clipped ones, and they are where offloaders act.
	modelled bool
	touched  []string
}

type adaptReq struct {
	id, ts                              int64
	model                               string
	fresh, read, write, write1h, output int64
	cost                                float64
	price                               modelinfo.Price
	factor                              float64
	blocks                              []*adaptBlock
	observed                            map[string]float64 // candidate -> its booked saving here, re-priced
	// inWindow is false on a session's lead-in: replayed for its state, never priced or counted.
	inWindow bool
}

type adaptSession struct {
	id   string
	last int64
	reqs []*adaptReq
}

// adaptSpec is a candidate arm before pricing.
type adaptSpec struct {
	comps    []string
	settings map[string]map[string]int
}

func (s adaptSpec) key() string {
	if len(s.comps) == 0 {
		return "none"
	}
	var b strings.Builder
	b.WriteString(strings.Join(s.comps, "+"))
	for _, c := range slices.Sorted(maps.Keys(s.settings)) {
		if !s.has(c) {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(s.settings[c])) {
			fmt.Fprintf(&b, ";%s.%s=%d", c, k, s.settings[c][k])
		}
	}
	return b.String()
}

// adaptTieLess orders two options of equal cost: fewer moves from current, then fewer
// components, then the spelling of the option.
func adaptTieLess(a, b, cur adaptSpec) bool {
	if ma, mb := a.moves(cur), b.moves(cur); ma != mb {
		return ma < mb
	}
	if len(a.comps) != len(b.comps) {
		return len(a.comps) < len(b.comps)
	}
	return a.key() < b.key()
}

// moves counts how far s is from c: components added or dropped, pairs run in the other
// order, and settings that differ.
func (s adaptSpec) moves(c adaptSpec) int {
	n := 0
	for _, x := range s.comps {
		if !c.has(x) {
			n++
		}
	}
	for _, x := range c.comps {
		if !s.has(x) {
			n++
		}
	}
	for i, x := range s.comps {
		for _, y := range s.comps[i+1:] {
			if xi, yi := slices.Index(c.comps, x), slices.Index(c.comps, y); xi >= 0 && yi >= 0 && xi > yi {
				n++
			}
		}
	}
	for _, x := range s.comps {
		for k, v := range s.settings[x] {
			if cv, ok := c.settings[x][k]; !ok || cv != v {
				n++
			}
		}
	}
	return n
}

func (s adaptSpec) clone() adaptSpec {
	out := adaptSpec{comps: append([]string(nil), s.comps...), settings: map[string]map[string]int{}}
	for c, m := range s.settings {
		out.settings[c] = map[string]int{}
		for k, v := range m {
			out.settings[c][k] = v
		}
	}
	return out
}

func (s adaptSpec) has(c string) bool {
	for _, x := range s.comps {
		if x == c {
			return true
		}
	}
	return false
}

// pipeline builds the replay pipeline: candidate components only.
func (s adaptSpec) pipeline() (*components.Pipeline, error) {
	doc := map[string]any{"pipeline": s.comps}
	if len(s.settings) > 0 {
		comps := map[string]any{}
		for c, m := range s.settings {
			if s.has(c) && len(m) > 0 {
				comps[c] = m
			}
		}
		if len(comps) > 0 {
			doc["components"] = comps
		}
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadBytes(raw)
	if err != nil {
		return nil, err
	}
	return cfg.Build(components.NopEmitter{})
}

// adaptSpecFromDoc reads the candidate half of a stored config document, plus its full
// resolved pipeline.
func adaptSpecFromDoc(doc string) (adaptSpec, []string, error) {
	f, err := config.ParseForm(doc)
	if err != nil {
		return adaptSpec{}, nil, err
	}
	if f.ParseError != "" {
		return adaptSpec{}, nil, fmt.Errorf("config does not load: %s", f.ParseError)
	}
	s := adaptSpec{settings: map[string]map[string]int{}}
	for _, c := range f.Pipeline {
		if isAdaptCandidate(c) {
			s.comps = append(s.comps, c)
		}
	}
	for c, knobs := range adaptKnobs {
		for k := range knobs {
			if v, ok := asIntAny(f.Components[c][k]); ok {
				if s.settings[c] == nil {
					s.settings[c] = map[string]int{}
				}
				s.settings[c][k] = v
			}
		}
	}
	return s, f.Pipeline, nil
}

func asIntAny(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == math.Trunc(n)
	}
	return 0, false
}

// AdaptCompactionMerge returns the pipeline to store: cands in their new order, and every other
// component of cur kept right after the candidate it followed in cur (walking back past
// candidates the new order drops), so cachesplit stays after the offloaders and an LLM
// component stays where its owner put it relative to what remains.
func AdaptCompactionMerge(cur, cands []string) []string {
	keep := map[string]bool{}
	for _, c := range cands {
		keep[c] = true
	}
	after := map[string][]string{} // surviving candidate ("" = head) -> fixed comps after it
	anchor := ""
	for _, c := range cur {
		if isAdaptCandidate(c) {
			if keep[c] {
				anchor = c
			}
			continue
		}
		after[anchor] = append(after[anchor], c)
	}
	out := append([]string(nil), after[""]...)
	for _, c := range cands {
		out = append(out, c)
		out = append(out, after[c]...)
	}
	return out
}

// --- loading -------------------------------------------------------------------

func (d *DB) adaptDataset(ctx context.Context, f Filter, p modelinfo.Pricer, cov *AdaptCompactionCoverage) ([]*adaptSession, error) {
	// A session the window cuts into is read from its START: a block it re-sends was written
	// before the window opened, and pricing its first in-window appearance as the uncached tail
	// would charge it the write rate on what was really a cache read. The lead-in is replayed
	// for that state and never priced.
	win, winArgs := f.where()
	lead := f
	lead.Since = 0
	cond, args := lead.where()
	cond += ` AND r.session_id IN (SELECT r.session_id FROM requests r WHERE ` + win + `)`
	args = append(args, winArgs...)
	rows, err := d.sql.QueryContext(ctx, `SELECT r.id, r.session_id, r.model, r.ts, r.fresh_input,
		r.cache_read, r.cache_write, r.cache_write_1h, r.output_tokens, r.cost_usd, r.token_accounting,
		EXISTS(SELECT 1 FROM request_content c WHERE c.request_id = r.id)
		FROM requests r WHERE `+cond+` ORDER BY r.session_id, r.ts, r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pf struct {
		p      modelinfo.Price
		ok     bool
		factor float64
	}
	prices := map[string]pf{}
	byID := map[int64]*adaptReq{}
	var sessions []*adaptSession
	seenSess, contentSess := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		r := &adaptReq{}
		var sess, acct string
		var has int
		if err := rows.Scan(&r.id, &sess, &r.model, &r.ts, &r.fresh, &r.read, &r.write, &r.write1h,
			&r.output, &r.cost, &acct, &has); err != nil {
			return nil, err
		}
		r.inWindow = r.ts >= f.Since
		if r.inWindow {
			cov.Requests++
			cov.BilledUSD += r.cost
			if !seenSess[sess] {
				seenSess[sess] = true
				cov.Sessions++
			}
		}
		if has == 0 {
			continue
		}
		if r.inWindow {
			cov.RequestsWithContent++
			if !contentSess[sess] {
				contentSess[sess] = true
				cov.SessionsWithContent++
			}
		}
		pr, ok := prices[r.model]
		if !ok {
			pr.p, pr.ok = p.Price(ctx, r.model)
			pr.ok = pr.ok && !pr.p.Zero()
			pr.factor, _ = tokens.BilledDeltaFactor(r.model)
			prices[r.model] = pr
		}
		if !pr.ok || acct != AccountingComplete {
			continue
		}
		r.price, r.factor = pr.p, pr.factor
		if n := len(sessions); n == 0 || sessions[n-1].id != sess {
			sessions = append(sessions, &adaptSession{id: sess})
		}
		s := sessions[len(sessions)-1]
		s.reqs = append(s.reqs, r)
		s.last = r.ts
		byID[r.id] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Most recent sessions first, then cap the replayed population.
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].last > sessions[j].last })
	n := 0
	for i, s := range sessions {
		if n+len(s.reqs) > adaptMaxRequests && n > 0 {
			for _, dropped := range sessions[i:] {
				for _, r := range dropped.reqs {
					delete(byID, r.id)
				}
			}
			sessions = sessions[:i]
			cov.TruncatedToRecent = true
			break
		}
		n += len(s.reqs)
	}

	crow, err := d.sql.QueryContext(ctx, `SELECT c.request_id, c.path, c.before_tokens, c.after_tokens,
		c.before_gz, c.components FROM request_content c JOIN requests r ON r.id = c.request_id
		WHERE `+cond+` ORDER BY c.request_id, c.seq`, args...)
	if err != nil {
		return nil, err
	}
	defer crow.Close()
	texts := map[string]*adaptBlock{} // session|key -> canonical block (text kept once)
	counted := map[string]bool{}      // session|key already counted in coverage
	perReq := map[int64]int{}
	sessOf := map[int64]string{}
	for _, s := range sessions {
		for _, r := range s.reqs {
			sessOf[r.id] = s.id
		}
	}
	for crow.Next() {
		var id int64
		var path string
		var bt, at int
		var gz []byte
		var touched string
		if err := crow.Scan(&id, &path, &bt, &at, &gz, &touched); err != nil {
			return nil, err
		}
		r := byID[id]
		if r == nil {
			continue
		}
		if r.inWindow {
			perReq[id]++
		}
		msg, blk, j, ok := parseAdaptPath(path)
		if !ok {
			continue
		}
		text := gunzipText(gz)
		sum := sha256.Sum256([]byte(text))
		key := path + "|" + fmt.Sprintf("%x", sum[:8])
		sk := sessOf[id] + "|" + key
		canon := texts[sk]
		if canon == nil {
			tok := schema.TextTokens(text)
			canon = &adaptBlock{path: path, msg: msg, blk: blk, j: j, key: key, text: text, tokens: tok}
			// A clipped `before` ends with apply.clip's marker, and its token count no longer
			// matches the stored count of the full string. Such a block cannot be run; it is
			// MODELLED from its stored sizes instead (see adaptBlock.touched).
			if strings.Contains(text[max(0, len(text)-40):], "…[+") || absInt(tok-bt) > max(2, bt/50) {
				canon.text, canon.tokens, canon.modelled = "", bt, true
				for _, c := range strings.Split(touched, ",") {
					if isAdaptCandidate(c) {
						canon.touched = append(canon.touched, c)
					}
				}
			}
			texts[sk] = canon
		}
		if r.inWindow && !counted[sk] {
			counted[sk] = true
			cov.Blocks++
			cov.BlockTokens += int64(bt)
			if canon.modelled {
				cov.ClippedBlocks++
				cov.ClippedBlockTokens += int64(bt)
			}
		}
		b := *canon
		b.sent = at
		r.blocks = append(r.blocks, &b)
	}
	if err := crow.Err(); err != nil {
		return nil, err
	}
	// The product's own per-component booking on these requests. MAX per (request, component):
	// request_components has no unique key and older rows carry duplicates.
	// The product's own per-component booking on these requests, in TOKENS, re-priced here at
	// the same rates the replay uses. Not the stored saved_usd: rows written before the
	// tokenizer correction carry no BilledDeltaFactor, so that column is on a different ruler
	// from one deployment to the next. MAX per (request, component): request_components has
	// no unique key and older rows carry duplicates.
	orow, err := d.sql.QueryContext(ctx, `SELECT rc.request_id, rc.component, MAX(rc.saved_gross),
		MAX(rc.saved_unique) FROM request_components rc JOIN requests r ON r.id = rc.request_id
		WHERE `+cond+` AND rc.saved_gross > 0 GROUP BY rc.request_id, rc.component`, args...)
	if err != nil {
		return nil, err
	}
	defer orow.Close()
	for orow.Next() {
		var id, gross, unique int64
		var comp string
		if err := orow.Scan(&id, &comp, &gross, &unique); err != nil {
			return nil, err
		}
		if r := byID[id]; r != nil && r.inWindow {
			if r.observed == nil {
				r.observed = map[string]float64{}
			}
			if unique < 0 {
				unique = 0
			} else if unique > gross {
				unique = gross // shared content keys can over-attribute, as in Event.Price
			}
			tail, repeat, _ := adaptRates(r)
			r.observed[comp] += (float64(unique)*tail + float64(gross-unique)*repeat) * r.factor
		}
	}
	if err := orow.Err(); err != nil {
		return nil, err
	}
	for _, n := range perReq {
		if n >= defaultContentPerReq {
			cov.RequestsOverCap++
		}
	}
	// A request with no usable block drops out of the covered population, so the denominator
	// is the requests the figures actually stand on.
	out := sessions[:0]
	for _, s := range sessions {
		kept, priced := s.reqs[:0], 0
		for _, r := range s.reqs {
			if len(r.blocks) == 0 {
				continue
			}
			kept = append(kept, r)
			if r.inWindow {
				priced++
				cov.CoveredRequests++
				cov.CoveredBilledUSD += r.cost
			}
		}
		if s.reqs = kept; priced > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// parseAdaptPath reads messages.N.content.K.content[.J.text].
func parseAdaptPath(p string) (msg, blk, j int, ok bool) {
	f := strings.Split(p, ".")
	if len(f) != 5 && len(f) != 7 || f[0] != "messages" || f[2] != "content" || f[4] != "content" {
		return 0, 0, 0, false
	}
	var err1, err2 error
	msg, err1 = strconv.Atoi(f[1])
	blk, err2 = strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return 0, 0, 0, false
	}
	j = -1
	if len(f) == 7 {
		if f[6] != "text" {
			return 0, 0, 0, false
		}
		var err error
		if j, err = strconv.Atoi(f[5]); err != nil {
			return 0, 0, 0, false
		}
	}
	return msg, blk, j, true
}

// --- replay --------------------------------------------------------------------

// synthBody rebuilds a request from its stored blocks: real indices, a paired tool_use for
// every tool_result, a one-token placeholder everywhere else.
func synthBody(r *adaptReq) []byte {
	byMsg := map[int][]*adaptBlock{}
	last := 0
	for _, b := range r.blocks {
		if b.modelled {
			continue
		}
		byMsg[b.msg] = append(byMsg[b.msg], b)
		last = max(last, b.msg)
	}
	msgs := make([]any, last+1)
	for i := range msgs {
		msgs[i] = map[string]any{"role": "user", "content": "."}
	}
	for i, bs := range byMsg {
		width := 0
		for _, b := range bs {
			width = max(width, b.blk+1)
		}
		content := make([]any, width)
		uses := make([]any, width)
		for k := range content {
			id := "t" + strconv.Itoa(i) + "_" + strconv.Itoa(k)
			content[k] = map[string]any{"type": "tool_result", "tool_use_id": id, "content": "."}
			uses[k] = map[string]any{"type": "tool_use", "id": id, "name": "tool", "input": map[string]any{}}
		}
		for _, b := range bs {
			blk := content[b.blk].(map[string]any)
			if b.j < 0 {
				blk["content"] = b.text
				continue
			}
			parts := make([]any, b.j+1)
			if old, ok := blk["content"].([]any); ok && len(old) > len(parts) {
				parts = old
			}
			for k := range parts {
				if parts[k] == nil {
					parts[k] = map[string]any{"type": "text", "text": "."}
				}
			}
			parts[b.j] = map[string]any{"type": "text", "text": b.text}
			blk["content"] = parts
		}
		msgs[i] = map[string]any{"role": "user", "content": content}
		if i > 0 && byMsg[i-1] == nil {
			msgs[i-1] = map[string]any{"role": "assistant", "content": uses}
		}
	}
	raw, _ := json.Marshal(map[string]any{"model": r.model, "max_tokens": 1, "messages": msgs})
	return raw
}

// adaptRates are the per-token rates a size change is priced at on r: the tail rate for a
// block's first appearance and Event.repeatRate for a re-sent one, both with the write rate
// blended across the 5m and 1h tiers by what r actually wrote.
func adaptRates(r *adaptReq) (tail, repeat, write float64) {
	p := r.price
	write = p.CacheWrite
	if r.write > 0 && r.write1h > 0 {
		f := math.Min(1, float64(r.write1h)/float64(r.write))
		write = (1-f)*p.CacheWrite + f*2*p.Input
	}
	pw := p
	pw.CacheWrite = write
	e := Event{FreshInput: r.fresh, CacheRead: r.read, CacheWrite: r.write}
	repeat = e.repeatRate(pw)
	// The tail is the newest write, and with a mixed-TTL request that is the 5m entry at the end;
	// the 1h part is the head. Only a request that wrote everything at 1h wrote its tail there.
	tail = p.Input
	if r.write > 0 && r.write >= r.fresh {
		tail = p.CacheWrite
		if r.write1h >= r.write {
			tail = 2 * p.Input
		}
	}
	return tail, repeat, write
}

// adaptEval is one arm's replay over a population.
type adaptEval struct {
	deltaUSD   float64 // vs what was actually sent, unseenUSD and declUSD included
	unseenUSD  float64
	declUSD    float64 // the expand declaration's part, relative to the account's own pipeline
	busts      int
	reproduced int // blocks whose replayed size equals the stored sent size
	compared   int
}

// replayAdapt runs spec over sessions and prices it against what was actually sent. decl is the
// change in expand-declaration tokens every request carries under this arm (see adaptRun.eval).
func replayAdapt(ctx context.Context, spec adaptSpec, sessions []*adaptSession, decl int) (adaptEval, error) {
	pipe, err := spec.pipeline()
	if err != nil {
		return adaptEval{}, err
	}
	// Sessions are independent (each has its own store), so two run at once: the pipeline is
	// already safe for concurrent requests, which is how the proxy uses it.
	// ponytail: fixed at 2 workers, the host's per-job CPU budget; a pool if this ever runs hot.
	// Each session's result lands in its own slot and the slots are summed in session order, so
	// the total is the same float on every run: the ranking compares totals, and a sum whose
	// order followed goroutine scheduling could reorder near-ties from one run to the next.
	var firstErr error
	per := make([]adaptEval, len(sessions))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				per[i] = replaySession(ctx, pipe, spec, sessions[i], decl)
			}
		}()
	}
	for i := range sessions {
		if firstErr = ctx.Err(); firstErr != nil {
			break
		}
		next <- i
	}
	close(next)
	wg.Wait()
	var ev adaptEval
	for _, e := range per {
		ev.add(e)
	}
	if firstErr == nil {
		firstErr = ctx.Err() // a replay cut short by a cancel must never be priced
	}
	return ev, firstErr
}

func (ev *adaptEval) add(e adaptEval) {
	ev.deltaUSD += e.deltaUSD
	ev.declUSD += e.declUSD
	ev.busts += e.busts
	ev.reproduced += e.reproduced
	ev.compared += e.compared
}

// replaySession replays one session under pipe.
func replaySession(ctx context.Context, pipe *components.Pipeline, spec adaptSpec, s *adaptSession, decl int) adaptEval {
	var ev adaptEval
	st := store.NewMemory(store.Options{})
	seen := map[string]adaptSeen{}
	sentBefore := map[string]bool{} // keys the lead-in already sent
	var prevSig string
	for i, r := range s.reqs {
		// A client that went away stops the replay within one request: the report can run for
		// minutes and nobody is waiting for it.
		if ctx.Err() != nil {
			return ev
		}
		// The lead-in is not run: all it decides is which blocks were already in the cache when
		// the window opened, and every candidate is a deterministic function of its input, so
		// the first in-window run reaches the sizes the lead-in would have.
		if !r.inWindow {
			for _, b := range r.blocks {
				sentBefore[b.key] = true
			}
			continue
		}
		// A request carrying exactly the blocks of the one before it adds no tail, and every
		// candidate is a deterministic function of the content it sees, so its sizes are the
		// previous request's. Most of a session is such requests: re-running the pipeline on
		// them was 85% of the replay's time and changed nothing.
		sig := adaptSig(r)
		if sig != prevSig {
			res := apply.BodyOpts(ctx, pipe, st, apply.Opts{
				Provider: bschemas.Anthropic, Body: synthBody(r), Session: s.id,
				Now: time.UnixMilli(r.ts),
			})
			after := map[string]int{}
			for _, c := range res.Changes {
				after[c.Path] = c.AfterTokens
			}
			for _, b := range r.blocks {
				switch a, ok := after[b.path]; {
				case b.modelled:
					b.replayed = b.sent
					for _, c := range b.touched {
						if !spec.has(c) {
							b.replayed = b.tokens
						}
					}
				case ok:
					b.replayed = a
				default:
					b.replayed = b.tokens
				}
			}
			prevSig = sig
		} else {
			for k, b := range r.blocks {
				b.replayed = s.reqs[i-1].blocks[k].replayed
			}
		}
		sizes := make([]int, len(r.blocks))
		for k, b := range r.blocks {
			sizes[k] = b.replayed
			if b.modelled {
				continue
			}
			ev.compared++
			if absInt(sizes[k]-b.sent) <= max(2, b.sent/100) {
				ev.reproduced++
			}
		}
		for k, b := range r.blocks {
			if _, ok := seen[b.key]; !ok && sentBefore[b.key] {
				seen[b.key] = adaptSeen{sizes[k], b.sent} // a re-send, at the size it has now
			}
		}
		delta, declUSD, bust := priceAdaptReq(r, sizes, seen, i == 0, decl)
		if bust {
			ev.busts++
		}
		ev.deltaUSD += delta
		ev.declUSD += declUSD
	}
	return ev
}

// adaptSig identifies a request's block set, in order.
func adaptSig(r *adaptReq) string {
	var b strings.Builder
	for _, x := range r.blocks {
		b.WriteString(x.key)
		b.WriteByte(';')
	}
	return b.String()
}

// adaptSeen is a block's previous appearance in the session: what the arm sent and what was
// actually sent.
type adaptSeen struct{ got, sent int }

// priceAdaptReq prices one request's replayed block sizes against what was sent. seen carries
// each block's previous appearance across the session; first says r opens the session (the
// declaration is then part of the tail). bust reports a request that read from cache on which
// the arm re-sent a block at a new size while the history re-sent it unchanged: an edit to the
// cached prefix that the bill does not already contain. (A rewrite the HISTORY made is in the
// bill already; an arm that avoids one is not credited for it — the conservative direction.)
func priceAdaptReq(r *adaptReq, sizes []int, seen map[string]adaptSeen, first bool, decl int) (delta, declUSD float64, bust bool) {
	tail, repeat, write := adaptRates(r)
	changed := false
	for k, b := range r.blocks {
		got := sizes[k]
		rate := repeat
		if prev, ok := seen[b.key]; !ok {
			rate = tail
		} else if prev.got != got && prev.sent == b.sent {
			changed = true
		}
		seen[b.key] = adaptSeen{got, b.sent}
		delta += float64(got-b.sent) * rate
	}
	if decl != 0 {
		rate := repeat
		if first {
			rate = tail
		}
		declUSD = float64(decl) * rate * r.factor
	}
	delta = delta*r.factor + declUSD
	if changed && r.read > 0 {
		bust = true
		delta += float64(r.read) * (write - r.price.CacheRead)
	}
	return delta, declUSD, bust
}

// adaptHasOffload says whether a full pipeline can mint a marker, i.e. carries the expand
// declaration. A pipeline that does not build (an LLM component's settings) is read off the
// candidates, which are the offloaders that matter here.
func adaptHasOffload(pipeline []string) bool {
	cfg, err := config.LoadBytes([]byte("pipeline: [" + strings.Join(pipeline, ", ") + "]\n"))
	if err == nil {
		if p, err := cfg.Build(components.NopEmitter{}); err == nil {
			return p.HasOffload()
		}
	}
	for _, c := range pipeline {
		if c == "dedup" || c == "cmdfilter" || c == "extract" {
			return true
		}
	}
	return false
}

// expandDeclTokens is the expand tool declaration's size on an Anthropic request.
func expandDeclTokens() int {
	before := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"."}]}`)
	after, ok := expand.Inject(string(bschemas.Anthropic), expand.InjectAlways, before, true)
	if !ok {
		return 0
	}
	return schema.TextTokens(string(after)) - schema.TextTokens(string(before))
}

// --- search --------------------------------------------------------------------

// neighbours are the one-step moves from s: add or drop one candidate, swap two neighbours,
// or move one knob.
func (s adaptSpec) neighbours() []adaptSpec {
	var out []adaptSpec
	for _, c := range AdaptCompactionCandidates {
		n := s.clone()
		if s.has(c) {
			n.comps = removeName(n.comps, c)
		} else {
			n.comps = insertCanonical(n.comps, c)
		}
		out = append(out, n)
	}
	for i := 0; i+1 < len(s.comps); i++ {
		n := s.clone()
		n.comps[i], n.comps[i+1] = n.comps[i+1], n.comps[i]
		out = append(out, n)
	}
	for _, c := range slices.Sorted(maps.Keys(adaptKnobs)) {
		if !s.has(c) {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(adaptKnobs[c])) {
			for _, v := range adaptKnobs[c][k] {
				if cur, ok := s.settings[c][k]; ok && cur == v {
					continue
				}
				n := s.clone()
				if n.settings[c] == nil {
					n.settings[c] = map[string]int{}
				}
				n.settings[c][k] = v
				out = append(out, n)
			}
		}
	}
	return out
}

func removeName(xs []string, v string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// insertCanonical places c before the first present candidate that follows it in
// AdaptCompactionCandidates' order.
func insertCanonical(xs []string, c string) []string {
	rank := map[string]int{}
	for i, n := range AdaptCompactionCandidates {
		rank[n] = i
	}
	for i, x := range xs {
		if rank[x] > rank[c] {
			return append(xs[:i:i], append([]string{c}, xs[i:]...)...)
		}
	}
	return append(xs, c)
}

// adaptRun is one report's fixed inputs.
type adaptRun struct {
	ctx        context.Context
	cur        adaptSpec
	curPipe    []string
	declTok    int
	curOffload bool
}

// adaptPop is a population arms are priced over.
//
// observed is the account's own measured saving per candidate on these requests
// (request_components' token counts, re-priced at the replay's rates), and seenLoss what the replay sees dropping it from the
// current arm costs. The difference is saving the replay cannot see — blocks past the capture
// cap, content never stored — and an arm that drops that component is charged it. Without this
// the cap would make every offloader look free to drop on exactly the long sessions it acts on.
type adaptPop struct {
	sessions []*adaptSession
	observed map[string]float64
	seenLoss map[string]float64
	memo     map[string]adaptEval
	raw      map[string]adaptEval // replays without the declaration change
	specs    map[string]adaptSpec
}

func (run *adaptRun) population(sessions []*adaptSession) (*adaptPop, error) {
	pop := &adaptPop{sessions: sessions, observed: map[string]float64{}, seenLoss: map[string]float64{},
		memo: map[string]adaptEval{}, raw: map[string]adaptEval{}, specs: map[string]adaptSpec{}}
	for _, s := range sessions {
		for _, r := range s.reqs {
			for c, v := range r.observed {
				pop.observed[c] += v
			}
		}
	}
	base, err := run.replay(pop, run.cur)
	if err != nil {
		return nil, err
	}
	for _, c := range run.cur.comps {
		drop := run.cur.clone()
		drop.comps = removeName(drop.comps, c)
		e, err := run.replay(pop, drop)
		if err != nil {
			return nil, err
		}
		pop.seenLoss[c] = e.deltaUSD - base.deltaUSD
	}
	return pop, nil
}

// replay is replayAdapt on pop with no declaration change, memoised: the search's neighbours of
// the current option are the same replays population() needs.
func (run *adaptRun) replay(pop *adaptPop, spec adaptSpec) (adaptEval, error) {
	if e, ok := pop.raw[spec.key()]; ok {
		return e, nil
	}
	e, err := replayAdapt(run.ctx, spec, pop.sessions, 0)
	if err == nil {
		pop.raw[spec.key()] = e
	}
	return e, err
}

// adaptDeclUSD prices decl more declaration tokens on every request of sessions, at the rate
// priceAdaptReq would: the tail rate on a session's first request, the repeat rate after.
func adaptDeclUSD(sessions []*adaptSession, decl int) float64 {
	var usd float64
	for _, s := range sessions {
		for i, r := range s.reqs {
			if !r.inWindow {
				continue
			}
			tail, repeat, _ := adaptRates(r)
			rate := repeat
			if i == 0 {
				rate = tail
			}
			usd += float64(decl) * rate * r.factor
		}
	}
	return usd
}

// eval prices spec on pop: the replay, the declaration change, and the unseen saving of every
// current component spec drops.
func (run *adaptRun) eval(pop *adaptPop, spec adaptSpec) (adaptEval, error) {
	if e, ok := pop.memo[spec.key()]; ok {
		return e, nil
	}
	decl := 0
	switch has := adaptHasOffload(AdaptCompactionMerge(run.curPipe, spec.comps)); {
	case has && !run.curOffload:
		decl = run.declTok
	case !has && run.curOffload:
		decl = -run.declTok
	}
	e, err := run.replay(pop, spec)
	if err != nil {
		return e, err
	}
	if decl != 0 {
		e.declUSD = adaptDeclUSD(pop.sessions, decl)
		e.deltaUSD += e.declUSD
	}
	for _, c := range run.cur.comps {
		if !spec.has(c) {
			e.unseenUSD += math.Max(0, pop.observed[c]-pop.seenLoss[c])
		}
	}
	e.deltaUSD += e.unseenUSD
	pop.memo[spec.key()], pop.specs[spec.key()] = e, spec
	return e, nil
}

// AdaptCompaction builds the report for one tenant. curDoc is the account's stored config,
// defDoc the service default.
func (d *DB) AdaptCompaction(ctx context.Context, f Filter, p modelinfo.Pricer, curDoc, defDoc string) (*AdaptCompactionReport, error) {
	start := time.Now()
	rep := &AdaptCompactionReport{Kind: "compaction", Version: 1, Class: "counterfactual",
		Method: "the deterministic compaction components replayed on your stored requests, " +
			"searched over which run, their order and two thresholds",
		Pricing: "deployed gateway rates; gross over every covered request: fresh input, cache writes " +
			"(5m and 1h tiers as billed), cache reads and output; a block's change priced at its " +
			"first appearance as the uncached tail and at each later request as a read (hit), a write " +
			"(full-prefix rewrite) or fresh; rewriting already-sent content re-bills that request's " +
			"cache read as a write; offloading options carry the expand tool declaration"}
	rep.Window.Since, rep.Window.Until = f.Since, f.Until
	if p == nil {
		return nil, fmt.Errorf("no pricer: this proxy has no rate card")
	}
	cur, curPipe, err := adaptSpecFromDoc(curDoc)
	if err != nil {
		return nil, fmt.Errorf("your stored configuration: %w", err)
	}
	def, _, err := adaptSpecFromDoc(defDoc)
	if err != nil {
		return nil, fmt.Errorf("default configuration: %w", err)
	}
	sessions, err := d.adaptDataset(ctx, f, p, &rep.Coverage)
	if err != nil {
		return nil, err
	}
	rep.Coverage.ArchivedSessions = d.adaptArchived(ctx, f)
	rep.Window.Sessions, rep.Window.Requests = len(sessions), rep.Coverage.CoveredRequests
	run := &adaptRun{ctx: ctx, cur: cur, curPipe: curPipe, declTok: expandDeclTokens(),
		curOffload: adaptHasOffload(curPipe)}

	// The search population: the most recent sessions up to adaptSearchRequests.
	var sample []*adaptSession
	n := 0
	for _, s := range sessions {
		if n >= adaptSearchRequests {
			break
		}
		sample = append(sample, s)
		n += len(s.reqs)
	}
	if n >= rep.Coverage.CoveredRequests*4/5 {
		// Close enough to everything that a second population would cost more than it tells.
		sample, n = sessions, rep.Coverage.CoveredRequests
	}
	rep.Coverage.SearchRequests = n
	pop, err := run.population(sample)
	if err != nil {
		return nil, err
	}
	safe := adaptSpec{comps: []string{"format", "searchfold", "textclean"}, settings: map[string]map[string]int{}}
	best, bestV := cur, math.Inf(1)
	for _, seed := range []adaptSpec{cur, def, safe} {
		e, err := run.eval(pop, seed)
		if err != nil {
			return nil, err
		}
		if e.deltaUSD < bestV-1e-9 {
			best, bestV = seed, e.deltaUSD
		}
	}
	for round := 0; round < adaptSearchRounds; round++ {
		improved := false
		for _, nb := range best.neighbours() {
			if len(pop.memo) >= adaptMaxArms {
				break
			}
			e, err := run.eval(pop, nb)
			if err != nil {
				return nil, err
			}
			if e.deltaUSD < bestV-1e-9 {
				best, bestV, improved = nb, e.deltaUSD, true
			}
		}
		if !improved {
			break
		}
	}
	rep.Coverage.ArmsEvaluated = len(pop.memo)

	// The reported arms, re-priced on the whole covered population: current, default, and the
	// search's eight best.
	ranked := make([]adaptSpec, 0, len(pop.specs))
	for _, sp := range pop.specs {
		ranked = append(ranked, sp)
	}
	// Cheapest first; equal to a tenth of a cent, fewest moves from current, then by name, so
	// the same history always yields the same list.
	sort.Slice(ranked, func(i, j int) bool {
		a, b := pop.memo[ranked[i].key()].deltaUSD, pop.memo[ranked[j].key()].deltaUSD
		if math.Abs(a-b) > 0.001 {
			return a < b
		}
		return adaptTieLess(ranked[i], ranked[j], cur)
	})
	whole := pop
	if len(sample) != len(sessions) {
		if whole, err = run.population(sessions); err != nil {
			return nil, err
		}
	}
	option := func(s adaptSpec) (AdaptCompactionOption, adaptEval, error) {
		e, err := run.eval(whole, s)
		if err != nil {
			return AdaptCompactionOption{}, e, err
		}
		cost := rep.Coverage.CoveredBilledUSD + e.deltaUSD
		return AdaptCompactionOption{ID: s.key(), Label: adaptLabel(s), BilledUSD: cost,
			Pipeline: AdaptCompactionMerge(curPipe, s.comps), UnseenUSD: e.unseenUSD, Busts: e.busts,
			SpendUSD: e.declUSD, spec: s, cost: cost}, e, nil
	}
	none, _, err := option(adaptSpec{})
	if err != nil {
		return nil, err
	}
	curOpt, curEval, err := option(cur)
	if err != nil {
		return nil, err
	}
	rep.Coverage.FidelityBlocks, rep.Coverage.FidelityReproduced = curEval.compared, curEval.reproduced
	for _, c := range cur.comps {
		rep.Measured = append(rep.Measured, AdaptCompactionMeasured{Component: c,
			MeasuredUSD: whole.observed[c], ReplayedUSD: whole.seenLoss[c]})
	}
	rep.choices = map[string]AdaptCompactionChoice{}
	idOf := map[string]string{} // spec key -> short id
	confHash := AdaptConfigHash(curDoc)
	add := func(o AdaptCompactionOption, id, detail string, searched bool) string {
		if have, ok := idOf[o.ID]; ok {
			return have
		}
		// An option that costs what one already listed costs tells the reader nothing new: most
		// are reorderings of components that act on none of the stored content.
		if searched {
			for _, x := range rep.Options {
				if math.Abs(x.cost-o.cost) < 0.001 {
					return ""
				}
			}
		}
		idOf[o.ID] = id
		rep.choices[id] = AdaptCompactionChoice{Candidates: o.spec.comps, Settings: o.spec.settings,
			Label: o.Label, ConfigHash: confHash}
		o.ID, o.Detail = id, detail
		if detail == "" {
			o.Detail = strings.Join(o.Pipeline, ", ")
		}
		o.SpendUSD -= none.SpendUSD
		if rep.Coverage.CoveredRequests == 0 {
			o.Unavailable = "no stored request content in this window"
		} else {
			net := none.cost - o.cost
			o.NetUSD, o.SavedUSD = &net, net+o.SpendUSD
		}
		rep.Options = append(rep.Options, o)
		return id
	}
	rep.Current = add(curOpt, "current", "your configuration", false)
	defOpt, _, err := option(def)
	if err != nil {
		return nil, err
	}
	rep.Default = add(defOpt, "default", "the service default", false)
	add(none, "none", "no deterministic compaction, for reference", false)
	// The search's best few, re-priced on everything covered. Ties are dropped on the SEARCH
	// score, before the re-pricing, because each re-priced option is a full replay.
	var picked []float64
	shown := 0
	for _, s := range ranked {
		if len(picked) >= adaptShownOptions {
			break
		}
		v := pop.memo[s.key()].deltaUSD
		if slices.ContainsFunc(picked, func(x float64) bool { return math.Abs(x-v) < 0.001 }) {
			continue
		}
		picked = append(picked, v)
		o, _, err := option(s)
		if err != nil {
			return nil, err
		}
		if add(o, "c"+strconv.Itoa(shown+1), "", true) == "c"+strconv.Itoa(shown+1) {
			shown++
		}
	}
	// A tenth of a cent is noise from reordering components that do nothing; ties stay current.
	// Among options within that of each other, the one closest to the current configuration
	// wins, so the recommendation does not flip between equal reorderings from run to run.
	// Ties then go to fewer components, then the option's own spelling: a total order, so the
	// same data always gives the same best.
	bestOpt := rep.Options[0]
	for _, o := range rep.Options {
		if o.cost < bestOpt.cost-0.001 || (math.Abs(o.cost-bestOpt.cost) <= 0.001 && adaptTieLess(o.spec, bestOpt.spec, cur)) {
			bestOpt = o
		}
	}
	rep.Best = bestOpt.ID
	gain := curOpt.cost - bestOpt.cost
	threshold := math.Max(0.01, 0.005*curOpt.cost)
	switch {
	case rep.Coverage.CoveredRequests == 0:
		rep.Summary = "No stored request content in this window, so nothing could be replayed."
	case bestOpt.ID == curOpt.ID || gain < threshold:
		rep.Summary = fmt.Sprintf("Your compaction configuration is already the best one found: no "+
			"option would have saved $%.2f or more on the %d replayed requests.", threshold,
			rep.Coverage.CoveredRequests)
	default:
		days := float64(f.Until-f.Since) / float64(24*time.Hour/time.Millisecond)
		if f.Until == 0 {
			days = float64(time.Now().UnixMilli()-f.Since) / float64(24*time.Hour/time.Millisecond)
		}
		rec := &AdaptCompactionRecommendation{Option: bestOpt.ID, NetDeltaUSD: gain, Effective: "now",
			Change: "run " + bestOpt.Label + " instead of " + curOpt.Label,
			Reason: fmt.Sprintf("%.1f%% less on the %d requests whose content is stored; savings on the "+
				"others are not claimed", 100*gain/curOpt.cost, rep.Coverage.CoveredRequests)}
		if days >= 1 {
			rec.PerMonthUSD = gain * 30 / days
		}
		rep.Recommendation = rec
		rep.Summary = fmt.Sprintf("%s would have cost $%.2f less than your configuration on %d replayed requests.",
			bestOpt.Label, gain, rep.Coverage.CoveredRequests)
	}
	rep.Warnings = adaptNotes(rep, curPipe, whole.observed)
	rep.Coverage.DurationMs = time.Since(start).Milliseconds()
	return rep, nil
}

func adaptLabel(s adaptSpec) string {
	if len(s.comps) == 0 {
		return "no deterministic compaction"
	}
	l := strings.Join(s.comps, " > ")
	for _, c := range slices.Sorted(maps.Keys(s.settings)) {
		if !s.has(c) {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(s.settings[c])) {
			l += fmt.Sprintf(" (%s %s %d)", c, k, s.settings[c][k])
		}
	}
	return l
}

func adaptNotes(rep *AdaptCompactionReport, curPipe []string, observed map[string]float64) []string {
	c := rep.Coverage
	notes := []string{
		fmt.Sprintf("Dollar figures cover %d of %d requests ($%.2f of $%.2f billed): only requests whose "+
			"rewritten content is stored can be replayed.", c.CoveredRequests, c.Requests, c.CoveredBilledUSD, c.BilledUSD),
		"Messages no component rewrote were never stored, so an option that would compact them is " +
			"under-stated; an option that compacts less is fully seen.",
	}
	if c.ClippedBlocks > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d stored blocks (%d of %d tokens) were clipped at capture, "+
			"so they are not run: an option keeping every component that rewrote one is charged what was "+
			"sent, an option dropping any of them its original size (sizes as stored, not re-run).", c.ClippedBlocks, c.Blocks,
			c.ClippedBlockTokens, c.BlockTokens))
	}
	if c.RequestsOverCap > 0 {
		notes = append(notes, fmt.Sprintf("%d requests hit the per-request capture cap; their later blocks "+
			"are missing.", c.RequestsOverCap))
	}
	if c.ArchivedSessions > 0 {
		notes = append(notes, fmt.Sprintf("%d sessions in this window have content in cold storage, which "+
			"is not read.", c.ArchivedSessions))
	}
	if c.FidelityBlocks > 0 {
		notes = append(notes, fmt.Sprintf("Replaying your current configuration reproduces what was "+
			"actually sent on %.0f%% of blocks.", 100*float64(c.FidelityReproduced)/float64(c.FidelityBlocks)))
	}
	for _, n := range curPipe {
		if !isAdaptCandidate(n) && (strings.HasPrefix(n, "extract_llm") || strings.Contains(n, "summar")) {
			notes = append(notes, fmt.Sprintf("%s calls a model, so it is not replayed: it stays as it is in "+
				"every option (it booked $%.2f on these requests).", n, observed[n]))
		}
	}
	if c.TruncatedToRecent {
		notes = append(notes, fmt.Sprintf("Only the most recent %d covered requests were replayed.", adaptMaxRequests))
	}
	notes = append(notes, "Output tokens and the agent's own behaviour are assumed unchanged; expand-backs "+
		"of offloaded content are not charged.")
	return notes
}

func (d *DB) adaptArchived(ctx context.Context, f Filter) int {
	q := `SELECT COUNT(*) FROM archived_sessions WHERE content_path <> '' AND last_ts >= ?`
	args := []any{f.Since}
	if f.Until > 0 {
		q += ` AND first_ts < ?`
		args = append(args, f.Until)
	}
	if !f.TenantAll {
		q += ` AND tenant_id = ?`
		args = append(args, f.Tenant)
	}
	var n int
	_ = d.sql.QueryRowContext(ctx, q, args...).Scan(&n)
	return n
}

// --- HTTP ----------------------------------------------------------------------

// AdaptOfferTTL is how long a report's options can be applied by id.
const AdaptOfferTTL = 30 * time.Minute

// adaptOffers holds, per (kind, account), the options of the latest report: what an apply may
// name. One report per account and kind, so the map is bounded by accounts.
type adaptOffers struct {
	mu sync.Mutex
	m  map[string]adaptOffer
	// running and started are adaptReportSlot's state, under the same lock.
	running map[string]bool
	started map[string][]time.Time
}

type adaptOffer struct {
	basis string
	opts  map[string]any
	exp   time.Time
}

// OfferAdapt records a report's options for one account under a fresh basis id, which it returns
// for the report's basis_id. It replaces that account's previous report of the same kind. Shared
// by every /api/adapt/* kind: an apply may only name an option a report just offered.
func (a *API) OfferAdapt(kind, tenantID string, opts map[string]any) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	basis := fmt.Sprintf("%x", b)
	a.offers.mu.Lock()
	defer a.offers.mu.Unlock()
	if a.offers.m == nil {
		a.offers.m = map[string]adaptOffer{}
	}
	a.offers.m[kind+"|"+tenantID] = adaptOffer{basis: basis, opts: opts, exp: time.Now().Add(AdaptOfferTTL)}
	return basis
}

// AdaptOffer returns option id of the report basis offered this account, while it is fresh.
func (a *API) AdaptOffer(kind, tenantID, basis, id string) (any, bool) {
	a.offers.mu.Lock()
	defer a.offers.mu.Unlock()
	o, ok := a.offers.m[kind+"|"+tenantID]
	if !ok || basis == "" || o.basis != basis || time.Now().After(o.exp) {
		return nil, false
	}
	v, ok := o.opts[id]
	return v, ok
}

// adaptReportBurst bounds one account's /api/adapt/* reports per hour.
const adaptReportBurst = 12

// adaptReportSlot admits one report for key: none already running for it, and fewer than
// adaptReportBurst started in the last hour. done releases the running mark.
func (a *API) adaptReportSlot(key string) (done func(), ok bool) {
	a.offers.mu.Lock()
	defer a.offers.mu.Unlock()
	if a.offers.running == nil {
		a.offers.running, a.offers.started = map[string]bool{}, map[string][]time.Time{}
	}
	now := time.Now()
	recent := a.offers.started[key][:0]
	for _, t := range a.offers.started[key] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	a.offers.started[key] = recent
	if a.offers.running[key] || len(recent) >= adaptReportBurst {
		return nil, false
	}
	a.offers.running[key] = true
	a.offers.started[key] = append(recent, now)
	return func() {
		a.offers.mu.Lock()
		delete(a.offers.running, key)
		a.offers.mu.Unlock()
	}, true
}

// AdaptCompactionConfigFunc returns a tenant's stored config document and the service default. ok=false
// means the proxy cannot say (no accounts), and reason is shown.
type AdaptCompactionConfigFunc func(tenantID string) (current, def string, ok bool, reason string)

// SetAdaptCompactionConfig wires the config read the adapt report needs; dash holds no tenant config.
func (a *API) SetAdaptCompactionConfig(fn AdaptCompactionConfigFunc) { a.adaptCompactionConfigFn = fn }

func (a *API) adaptRoutes() []route {
	return []route{{"GET /api/adapt/compaction", scopeTenant, a.adaptCompaction}}
}

const adaptDefaultWindow = 14 * 24 * time.Hour

func (a *API) adaptCompaction(w http.ResponseWriter, r *http.Request) {
	f, pr, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	if a.adaptCompactionConfigFn == nil {
		httpErr(w, http.StatusNotImplemented, "this proxy has no accounts, so it has no per-user configuration to adapt")
		return
	}
	// One account's history against that account's configuration. A manager's default scope is
	// the whole service, which has no single configuration to compare with, so it narrows to
	// their own account unless they named one. Single-tenant, the deployment is the account.
	id := pr.TenantID
	if a.auth != nil {
		if f.Tenant != "" {
			id = f.Tenant
		}
		f.Tenant, f.TenantAll = id, false
	}
	cur, def, ok, reason := a.adaptCompactionConfigFn(id)
	if !ok {
		httpErr(w, http.StatusConflict, reason)
		return
	}
	if f.Since == 0 {
		now := time.Now()
		if f.Until > 0 {
			now = time.UnixMilli(f.Until)
		}
		f.Since = now.Add(-adaptDefaultWindow).UnixMilli()
	}
	// The global slot first, so a 503 never spends the account's hourly budget.
	if err := acquireKVCache(r.Context()); err != nil {
		w.Header().Set("Retry-After", "30")
		httpErr(w, http.StatusServiceUnavailable, "the dashboard is busy with other reports; try again in a minute")
		return
	}
	defer releaseKVCache()
	// A report is minutes of replay, and an account's proxy token can ask for one: at most one
	// at a time per account and adaptReportBurst per hour.
	done, ok := a.adaptReportSlot("compaction|" + id)
	if !ok {
		httpErr(w, http.StatusTooManyRequests, "a report is already running for this account, or "+
			"too many were asked for in the last hour; wait and try again")
		return
	}
	defer done()
	out, err := a.db(r).AdaptCompaction(r.Context(), f, a.pricer, cur, def)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	opts := make(map[string]any, len(out.choices))
	for id, c := range out.choices {
		opts[id] = c
	}
	out.BasisID = a.OfferAdapt("compaction", id, opts)
	writeJSON(w, out)
}
