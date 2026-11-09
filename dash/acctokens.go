package dash

import (
	"strings"

	"github.com/rossoctl/accountant"
)

// The accountant library's figures in tokens (#998).
//
// The library measures every saving in tokens by billing tier and the dashboard prices them at
// write time (AccountantFigure). The tokens are stored beside the dollars, because a stored row
// does not keep the thread history the library priced from: after the write they cannot be
// computed again. A subscription user is limited in tokens, not billed in dollars, so every screen
// can now show every saving in tokens.
//
// The tiers are never added into one number. The provider does not publish how a usage limit
// weights them, and one feature can save in one tier and spend in another (the keep-alive rescue
// saves a write and spends reads).

// TierTokens is a token count by billing tier: accountant.TierTokens with JSON names.
type TierTokens struct {
	Input        int64 `json:"input"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite5m int64 `json:"cache_write_5m"`
	CacheWrite1h int64 `json:"cache_write_1h"`
}

func (t *TierTokens) add(o TierTokens) {
	t.Input += o.Input
	t.CacheRead += o.CacheRead
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
}

func tiersOf(t accountant.TierTokens) TierTokens {
	return TierTokens{Input: int64(t.Input), CacheRead: int64(t.CacheRead),
		CacheWrite5m: int64(t.CacheWrite5m), CacheWrite1h: int64(t.CacheWrite1h)}
}

// TokenFlow is a figure in tokens: what it saved and what it caused, each by tier. Its dollar
// value is tierUSD(Saved) - tierUSD(Caused), the library's own sign convention.
type TokenFlow struct {
	Saved  TierTokens `json:"saved"`
	Caused TierTokens `json:"caused"`
}

func (f *TokenFlow) add(o TokenFlow) {
	f.Saved.add(o.Saved)
	f.Caused.add(o.Caused)
}

func (f *TokenFlow) addTiers(saved, caused accountant.TierTokens) {
	f.add(TokenFlow{Saved: tiersOf(saved), Caused: tiersOf(caused)})
}

// addTok adds an operation's or an effect's tokens to component c.
func (f *AccFigure) addTok(c string, saved, caused accountant.TierTokens) {
	t := f.TokByComponent[c]
	t.addTiers(saved, caused)
	f.TokByComponent[c] = t
}

// AccEffectTokens is a request's cache effects in tokens: the ones no component row carries.
type AccEffectTokens struct {
	KeepAlive  TokenFlow `json:"keepalive"`    // rescue effects (keepalive_saved_usd)
	Split      TokenFlow `json:"cachesplit"`   // split effects (cachesplit_saved_usd)
	TTL        TokenFlow `json:"ttl"`          // head-TTL premium and rescue (on the headttl row's dollars)
	EndedBreak TokenFlow `json:"ended_change"` // the ended-change break (on the ended_change row's dollars)
}

// clearAccTokens zeroes every token figure of the accountant, as Price and PriceMode zero its dollars.
func (e *Event) clearAccTokens() {
	e.AccTokens = AccEffectTokens{}
	for i := range e.Components {
		e.Components[i].Tokens = TokenFlow{}
	}
}

// Storage. Eight INTEGER columns per figure: saved and caused, by tier. The names are built here,
// once, for the INSERT, the additive migration and the read queries. dash/schema.go's ddl spells the
// same names out; TestAccTokenColumnsExistOnAFreshDatabase holds the two together.

var tierColumnSuffixes = [4]string{"in", "rd", "w5m", "w1h"}

// flowColumns is the eight column names of one figure: prefix_saved_in ... prefix_caused_w1h.
func flowColumns(prefix string) []string {
	cols := make([]string, 0, 8)
	for _, side := range []string{"saved", "caused"} {
		for _, t := range tierColumnSuffixes {
			cols = append(cols, prefix+"_"+side+"_"+t)
		}
	}
	return cols
}

// args is the figure's eight values, in flowColumns order.
func (f TokenFlow) args() []any {
	return []any{f.Saved.Input, f.Saved.CacheRead, f.Saved.CacheWrite5m, f.Saved.CacheWrite1h,
		f.Caused.Input, f.Caused.CacheRead, f.Caused.CacheWrite5m, f.Caused.CacheWrite1h}
}

// dest is the figure's eight scan targets, in flowColumns order: the read side of args.
func (f *TokenFlow) dest() []any {
	return []any{&f.Saved.Input, &f.Saved.CacheRead, &f.Saved.CacheWrite5m, &f.Saved.CacheWrite1h,
		&f.Caused.Input, &f.Caused.CacheRead, &f.Caused.CacheWrite5m, &f.Caused.CacheWrite1h}
}

// compTokenColumns are request_components' token columns: the component's own figure.
var compTokenColumns = flowColumns("tok")

// The request's effect figures, in AccEffectTokens order. The prefix is also the feature name's
// column stem in the stats query.
var requestTokenEffects = []struct{ prefix, feature string }{
	{"tok_ka", "keepalive"},
	{"tok_split", "cachesplit"},
	{"tok_ttl", "ttl"},
	{"tok_ended", EndedChangeComponent},
}

// requestTokenColumns are requests' token columns, in requestTokenArgs order.
func requestTokenColumns() []string {
	var cols []string
	for _, x := range requestTokenEffects {
		cols = append(cols, flowColumns(x.prefix)...)
	}
	return cols
}

// totalColumns are requests' copy of the request's whole token figure: the sum of its component
// rows' figures and its four effects, written with them. They let the lean stats read take the totals
// from the one scan of requests it already makes, in 8 terms, with no join to request_components and
// no sum of the 32 effect columns (#502's budget).
var totalColumns = flowColumns("tok_total")

// requestStoredTokenColumns is every token column of requests, in requestStoredTokenArgs order.
func requestStoredTokenColumns() []string {
	return append(requestTokenColumns(), totalColumns...)
}

// requestStoredTokenArgs is the values of requestStoredTokenColumns for one event. A keep-alive ping
// stores zeros: it is not agent traffic, and the stats reads sum these columns over the pings' rows
// too. The library never prices a ping, so this drops nothing.
func requestStoredTokenArgs(e *Event) []any {
	if e.KeepAlive {
		return requestStoredTokenArgs(&Event{})
	}
	var total TokenFlow
	for _, c := range e.Components {
		total.add(c.Tokens)
	}
	for _, f := range []TokenFlow{e.AccTokens.KeepAlive, e.AccTokens.Split, e.AccTokens.TTL, e.AccTokens.EndedBreak} {
		total.add(f)
	}
	return append(requestTokenArgs(e.AccTokens), total.args()...)
}

// requestTokenSelect is the request token columns for requestCols (alias r), so a stored row reads
// back what the live event showed.
var requestTokenSelect = "r." + strings.Join(requestTokenColumns(), ", r.")

// requestTokenDest is the scan targets of requestTokenSelect, in the same order.
func requestTokenDest(a *AccEffectTokens) []any {
	var out []any
	for _, f := range []*TokenFlow{&a.KeepAlive, &a.Split, &a.TTL, &a.EndedBreak} {
		out = append(out, f.dest()...)
	}
	return out
}

func requestTokenArgs(a AccEffectTokens) []any {
	var out []any
	for _, f := range []TokenFlow{a.KeepAlive, a.Split, a.TTL, a.EndedBreak} {
		out = append(out, f.args()...)
	}
	return out
}

// The token columns are additive (see additiveColumns): an existing database gets them by ALTER
// TABLE on open, and every row written before them reads 0. A 0 there is "not measured", not
// "saved nothing": requests.acc_priced tells the two apart (1 when the library priced the request).
func init() {
	for _, c := range compTokenColumns {
		additiveColumns = append(additiveColumns, struct{ table, column, ddl string }{"request_components", c, "INTEGER NOT NULL DEFAULT 0"})
	}
	for _, c := range requestStoredTokenColumns() {
		additiveColumns = append(additiveColumns, struct{ table, column, ddl string }{"requests", c, "INTEGER NOT NULL DEFAULT 0"})
	}
}

// Reading.

// AccTokenStats is /api/stats' `tokens` object: the accountant's figures in tokens, for the period.
type AccTokenStats struct {
	// Saved and Caused are the sums over every feature below, by tier.
	Saved  TierTokens `json:"saved"`
	Caused TierTokens `json:"caused"`
	// ByFeature is one entry per compaction component (its request_components rows), plus the
	// request-level effects: keepalive (the rescue), cachesplit (the split; added to a cachesplit
	// component row's own figure if one exists), ttl (the head-TTL legs) and ended_change (the break an
	// ended change left behind). Tool filter and deferral are components (toolfilter, tooldefer)
	// when the library prices them. Features with no tokens are left out. The lean read has no
	// by_feature at all (absent, not empty): only the full read has the breakdown.
	ByFeature map[string]TokenFlow `json:"by_feature,omitempty"`
}

// AccTokensRead is DB.AccTokens' answer: the tokens object and the facts beside it.
type AccTokensRead struct {
	Tokens AccTokenStats
	// Unmeasured is the agent requests the library did not price (acc_priced = 0).
	Unmeasured int64
	// Ping is what the cache strategy's keep-alive pings paid, in billed tokens by tier (the ping
	// rows' own usage, not a library figure), and PingOutput their output tokens. Not in Tokens: a
	// ping is not agent traffic. The keep-alive's net in tokens is ByFeature["keepalive"] less Ping.
	Ping       TierTokens
	PingOutput int64
}

// apply puts the read on the Overview. The full and the lean path both call it.
func (r *AccTokensRead) apply(o *Overview) {
	o.Tokens, o.TokensUnmeasuredRequests = r.Tokens, r.Unmeasured
	o.KeepAlivePingTokens, o.KeepAlivePingOutputTokens = r.Ping, r.PingOutput
}

// accTokensRequestQuery is the full read's effects half: one aggregate over requests, bounded by
// the same conditions as the stats' main aggregate: the four effect figures, the pings' billed tokens
// and the count of agent rows the library did not price.
func accTokensRequestQuery(f Filter) (string, []any) {
	cond, args := withKeepAlive(f).where()
	return `SELECT ` + tokenSumTerms(requestTokenColumns()) + `,
			` + accTokenTailTerms + ` FROM requests r WHERE ` + cond, args
}

// accTokenLeanTerms is the lean read's 9 terms: the 8 totals and the unmeasured count, in
// leanAgg's dest order. They go in leanWindowMainQuery's one scan. Each term costs about 1 ms on
// the 20,000-request budget test (main: 53 ms; with all 40 token sums and the pings, 89 ms; with 14
// terms, 69 ms), so lean carries only what a status line shows: the totals.
var accTokenLeanTerms = tokenSumTerms(totalColumns) + `,
			` + unmeasuredTerm

// tokenSumTerms sums each column over requests r. No agent CASE: a ping row stores no token figure
// (requestStoredTokenArgs), so the plain SUM over a scope that has the pings is the agent rows' sum.
func tokenSumTerms(cols []string) string {
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = `COALESCE(SUM(r.` + c + `),0)`
	}
	return strings.Join(sel, ", ")
}

// accTokenTailTerms is the pings' billed tokens by tier, their output tokens, and the count of agent
// rows the library did not price (acc_priced = 0), over requests r scoped by withKeepAlive.
var accTokenTailTerms = func() string {
	ping := func(expr string) string {
		return `COALESCE(SUM(CASE WHEN ` + autoPing + ` THEN ` + expr + ` ELSE 0 END),0)`
	}
	return strings.Join([]string{
		ping("r.fresh_input"), ping("r.cache_read"),
		ping("MAX(r.cache_write - r.cache_write_1h, 0)"), ping("r.cache_write_1h"), ping("r.output_tokens"),
		unmeasuredTerm,
	}, ", ")
}()

// unmeasuredTerm counts the agent rows the library did not price.
const unmeasuredTerm = `COALESCE(SUM(CASE WHEN r.keepalive = 0 AND r.acc_priced = 0 THEN 1 ELSE 0 END),0)`

// leanAgg is the scan target of accTokenLeanTerms.
type leanAgg struct {
	total      [8]int64
	unmeasured int64
}

func (a *leanAgg) dest() []any {
	out := make([]any, 0, 9)
	for i := range a.total {
		out = append(out, &a.total[i])
	}
	return append(out, &a.unmeasured)
}

// read is the lean read's tokens: the totals and the unmeasured count. No by_feature and no ping
// tokens: the per-component entries need the join to request_components, and the effect entries and
// the pings 37 more terms, which made the lean read 1.3x to 4x slower than #502's budget allows. The
// pings' tokens are only useful against the keep-alive's own entry, which lean does not have. The
// full read has all of them.
func (a *leanAgg) read() *AccTokensRead {
	t := flowOf(a.total[:])
	return &AccTokensRead{Tokens: AccTokenStats{Saved: t.Saved, Caused: t.Caused}, Unmeasured: a.unmeasured}
}

// accTokensAgg is the scan target of the token sums (n columns) and accTokenTailTerms.
type accTokensAgg struct {
	sums       []int64
	ping       TierTokens
	pingOutput int64
	unmeasured int64
}

func (a *accTokensAgg) dest(n int) []any {
	a.sums = make([]int64, n)
	out := make([]any, 0, n+6)
	for i := range a.sums {
		out = append(out, &a.sums[i])
	}
	return append(out, &a.ping.Input, &a.ping.CacheRead, &a.ping.CacheWrite5m, &a.ping.CacheWrite1h,
		&a.pingOutput, &a.unmeasured)
}

func (a *accTokensAgg) tail(r *AccTokensRead) {
	r.Ping, r.PingOutput, r.Unmeasured = a.ping, a.pingOutput, a.unmeasured
}

// accTokensComponentQuery is the components half: the component rows' figures, joined to the
// requests in scope. requests drives the join (the session or time index), each request's rows are
// found by idx_rc_request, so the cost follows the rows in scope, never the table. Only rows the
// library priced carry tokens: every other row reads 0.
func accTokensComponentQuery(f Filter) (string, []any) {
	cond, args := f.where()
	var sel []string
	for _, c := range compTokenColumns {
		sel = append(sel, `COALESCE(SUM(rc.`+c+`),0)`)
	}
	return `SELECT rc.component, ` + strings.Join(sel, ", ") + `
		FROM requests r JOIN request_components rc ON rc.request_id = r.id
		WHERE ` + cond + `
		GROUP BY rc.component`, args
}

// AccTokens answers the `tokens` object, tokens_unmeasured_requests and the pings' tokens for the
// filter, for the FULL /api/stats. The lean read has the same totals and count from its one scan
// (accTokenLeanTerms), and no by_feature and no ping tokens.
func (d *DB) AccTokens(f Filter) (*AccTokensRead, error) {
	r := &AccTokensRead{Tokens: AccTokenStats{ByFeature: map[string]TokenFlow{}}}
	s := &r.Tokens
	q, args := accTokensRequestQuery(f)
	var agg accTokensAgg
	n := len(requestTokenColumns())
	if err := d.sql.QueryRowContext(d.readCtx(), q, args...).Scan(agg.dest(n)...); err != nil {
		return nil, err
	}
	agg.tail(r)
	for i, x := range requestTokenEffects {
		s.addFeature(x.feature, flowOf(agg.sums[i*8:i*8+8]))
	}
	q, args = accTokensComponentQuery(f)
	rows, err := d.sql.QueryContext(d.readCtx(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		v := make([]int64, 8)
		if err := rows.Scan(&name, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7]); err != nil {
			return nil, err
		}
		s.addFeature(name, flowOf(v))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

func flowOf(v []int64) TokenFlow {
	return TokenFlow{
		Saved:  TierTokens{Input: v[0], CacheRead: v[1], CacheWrite5m: v[2], CacheWrite1h: v[3]},
		Caused: TierTokens{Input: v[4], CacheRead: v[5], CacheWrite5m: v[6], CacheWrite1h: v[7]},
	}
}

// addFeature adds a figure to its feature and to the totals. A zero figure adds no entry.
func (s *AccTokenStats) addFeature(name string, f TokenFlow) {
	if f == (TokenFlow{}) {
		return
	}
	cur := s.ByFeature[name]
	cur.add(f)
	s.ByFeature[name] = cur
	s.Saved.add(f.Saved)
	s.Caused.add(f.Caused)
}
