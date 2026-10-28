package counterfactual

import (
	"fmt"
	"time"
)

// Machine-stable reasons in Result.Unpriced.
const (
	ReasonEndRequestMissing = "end_request_missing"
	ReasonUnknownKind       = "unknown_kind"
	ReasonNegativeSize      = "negative_size"
	ReasonPenaltyUnknown    = "penalty_unknown" // the boundary is unknown, so no break penalty was charged (§6.4)
	ReasonNoUsage           = "no_usage"        // the response had no usage: nothing is priced (§8)
	ReasonNotSpend          = "not_spend"       // a standalone RequestOps holds only spend operations (§6.1)
)

// Compute prices the request and then replaces the thread's ThreadState (§6.1). It is
// deterministic and does no I/O (R26). If it panics, the ThreadState is unchanged, because the
// replace is its last step. If another request replaced the ThreadState after EndRequest, Compute
// calculates again from the current one and sets Result.Recomputed (R53).
func (r *Request) Compute(usage Usage) Result {
	if !r.ended {
		return r.notEnded()
	}
	t := r.trackers.tracker(ThreadKey(r.Meta.Session, r.Thread))
	for {
		s := t.snapshot()
		res, next := r.calc(s, usage)
		if t.replace(s.Version, next) {
			return res
		}
	}
}

// Preview does the same calculation as Compute and stores nothing (§6.1).
func (r *Request) Preview(usage Usage) Result {
	if !r.ended {
		return r.notEnded()
	}
	res, _ := r.calc(r.trackers.tracker(ThreadKey(r.Meta.Session, r.Thread)).snapshot(), usage)
	return res
}

// ComputeStandalone prices a RequestOps that holds only spend operations (a keep-alive ping or a
// detached call). It reads and changes no ThreadState (§6.1). A spend is not priced in tokens: its
// Call is returned for the host to price (D4). Any other kind is unpriced.
func ComputeStandalone(ops RequestOps) Result {
	res := Result{Version: 1, Phase: PhaseUnknown}
	for _, op := range ops.Ops {
		res.Ops = append(res.Ops, priced(op, op.ID))
		if op.Kind != KindSpend {
			res.Unpriced = append(res.Unpriced, Unpriced{OpID: op.ID, Reason: ReasonNotSpend})
		}
	}
	return res
}

func (r *Request) notEnded() Result {
	res := Result{Version: 1, Phase: PhaseUnknown, Unpriced: []Unpriced{{Reason: ReasonEndRequestMissing}}}
	for _, op := range r.Ops {
		res.Ops = append(res.Ops, priced(op, op.ID))
		res.Unpriced = append(res.Unpriced, Unpriced{OpID: op.ID, Reason: ReasonEndRequestMissing})
	}
	return res
}

func priced(op Operation, id string) PricedOp {
	return PricedOp{OpID: id, Operator: op.Operator, ChangeID: op.ChangeID, DryRun: op.DryRun, Call: op.Call}
}

// item is one operation of the request: reported now, or active from an earlier request.
type item struct {
	op     Operation
	id     string
	active bool
	spans  []span // in client space
	ended  bool   // a later splice removed it, or a client edit took its position away (§4.5)
	bad    string // the Unpriced reason when the operation cannot be priced
}

type span struct {
	from, to      Position
	before, after Size
}

func opSpans(op Operation) []span {
	if len(op.Spans) == 0 {
		return []span{{op.At, op.At, op.SizeBefore, op.SizeAfter}}
	}
	out := make([]span, len(op.Spans))
	for i, s := range op.Spans {
		out[i] = span{Position{s.Section, s.From}, Position{s.Section, s.To}, s.SizeBefore, s.SizeAfter}
	}
	return out
}

// calc prices the request against the state s and returns the result and the next state. It
// changes nothing it is given.
func (r *Request) calc(s ThreadState, usage Usage) (Result, ThreadState) {
	res := Result{Version: 1, Recomputed: s.Version != r.start.Version}
	items := r.items(s)
	c := r.newCalc(s, items, usage)

	c.phaseAndBoundary(&res)
	if !c.noUsage {
		c.cacheEffects(&res)
	}
	c.price(&res)
	c.checks(&res)
	return res, c.nextState()
}

// items builds the operation list in pipeline order: the active operations first, as they ran on
// earlier requests, then the reported ones. A reported operation with the ChangeID of an active one
// replaces it (the observe-to-enforce switch, §4.6, or an operator that changes its own change).
func (r *Request) items(s ThreadState) []*item {
	reported := map[string]bool{} // a spend may share the ChangeID of its change (§6.5 check 3)
	for _, op := range r.Ops {
		if op.ChangeID != "" && (op.Kind == KindRewrite || op.Kind == KindSetting) && !negative(op) {
			reported[op.ChangeID] = true
		}
	}
	var items []*item
	for _, op := range s.Active {
		if !reported[op.ChangeID] {
			items = append(items, &item{op: op, id: "active:" + op.ChangeID, active: true, spans: opSpans(op)})
		}
	}
	for _, op := range r.Ops {
		it := &item{op: op, id: op.ID}
		switch {
		case op.Kind != KindRewrite && op.Kind != KindSpend && op.Kind != KindSetting:
			it.bad = ReasonUnknownKind
		case negative(op):
			it.bad = ReasonNegativeSize
		}
		items = append(items, it)
	}
	// A reported operation's positions are in its own output: undo the splices up to and
	// including its own to reach client space (§4.3). Active ones are stored in client space.
	for k, it := range items {
		if it.active {
			continue
		}
		it.spans = opSpans(it.op)
		for i := range it.spans {
			for j := k; j >= 0; j-- {
				if j < k && (items[j].op.DryRun || items[j].bad != "") {
					continue // it changed nothing upstream
				}
				it.spans[i].from = undo(it.spans[i].from, items[j].op.MessageSplice)
				it.spans[i].to = undo(it.spans[i].to, items[j].op.MessageSplice)
			}
		}
	}
	return items
}

func negative(op Operation) bool {
	if op.SizeBefore.Bytes < 0 || op.SizeAfter.Bytes < 0 {
		return true
	}
	for _, s := range op.Spans {
		if s.SizeBefore.Bytes < 0 || s.SizeAfter.Bytes < 0 {
			return true
		}
	}
	return false
}

// undo maps a position in the output of a splice back to its input.
func undo(p Position, s *MessageSplice) Position {
	if s == nil || p.Section != SectionMessages || p.Index < s.At {
		return p
	}
	if p.Index >= s.At+s.Inserted {
		p.Index += s.Removed - s.Inserted
		return p
	}
	p.Index = s.At // inside what the splice inserted: the start of what it replaced
	return p
}

// apply maps a position in the input of a splice to its output. A position that the splice
// removed maps to the start of what it inserted when into is true, and has no output otherwise.
func apply(p Position, s *MessageSplice, into bool) (Position, bool) {
	if s == nil || p.Section != SectionMessages || p.Index < s.At {
		return p, true
	}
	if p.Index >= s.At+s.Removed {
		p.Index += s.Inserted - s.Removed
		return p, true
	}
	p.Index = s.At
	return p, into
}

// calcState is the working state of one calculation.
type calcState struct {
	r       *Request
	s       ThreadState
	items   []*item
	usage   Usage
	noUsage bool
	ratio   ratio

	client, fwd       Body
	clientBPs, fwdBPs []bp
	prevClientBPs     []bp // the previous request's client breakpoints, flat in this client body
	prevFwdBPs        []bp // the previous request's forwarded breakpoints, flat in this forwarded body

	phase      CachePhase
	unknown    bool // the phase was unknown before the usage was read: no break can be classified
	prevExt    int  // the previous extent the phase uses, flat in the previous forwarded body; -1 = none
	pingHot    bool // the entry is hot because a ping kept it (§5.2)
	bstar      int  // B*, flat in the client body; -1 = none
	bstarFwd   int  // B*, flat in the forwarded body; -1 = none
	readEnd    int  // what Usage.CacheRead covers, flat in the forwarded body; -1 = nothing
	cachedFwd  int  // what this request leaves cached, flat in the forwarded body; -1 = nothing
	fwdFD      int
	clientFD   int
	prevExtCli int // prevExt in client space, flat in the previous client body; -1 = none
}

func (r *Request) newCalc(s ThreadState, items []*item, usage Usage) *calcState {
	c := &calcState{r: r, s: s, items: items, usage: usage, client: r.Client, fwd: r.Forwarded,
		prevExt: -1, bstar: -1, bstarFwd: -1, readEnd: -1, cachedFwd: -1, prevExtCli: -1}
	c.noUsage = usage == (Usage{})
	c.ratio = ratio{tokens: usage.Input + usage.CacheRead + usage.CacheWrite, bytes: r.Forwarded.cumBytes(len(r.Forwarded.Pos) - 1)}
	c.clientBPs = c.client.breakpoints(r.ClientBreakpoints)
	c.fwdBPs = c.fwd.breakpoints(r.Breakpoints)
	if p := s.PrevPrint; p != nil {
		// The previous breakpoints, as positions of this request's bodies: an entry the previous
		// request wrote at one can be read here.
		c.prevClientBPs = c.client.breakpoints(p.ClientBreakpoints)
		c.prevFwdBPs = c.fwd.breakpoints(p.ForwardedBreakpoints)
		c.fwdFD = firstDiff(p.Forwarded, c.fwd)
		c.clientFD = firstDiff(p.Client, c.client)
	}
	c.forwardPositions()
	return c
}

// forwardPositions ends the active operations whose position a later splice removed (§4.3), or
// that are no longer in the client body (a client compaction).
func (c *calcState) forwardPositions() {
	for k, it := range c.items {
		if !it.active || it.op.Kind != KindRewrite { // a setting has no position (§4.6)
			continue
		}
		for _, sp := range it.spans {
			i, ok := c.client.flat(sp.from)
			if !ok || (c.s.PrevPrint != nil && c.clientFD <= i) {
				it.ended = true // the client edited at or before it, or compacted (§4.3)
			}
			if _, ok := c.toFwd(k, sp.from); !ok {
				it.ended = true
			}
		}
	}
}

// toFwd maps a client position of item k into the forwarded body. The splices of the items up to
// and including k ran before or with it, so k saw what they inserted. A later splice that removed
// the position ended k, unless the splice did not see the original content (sawOriginal): then k
// ran before it, and its change is inside what the splice replaced.
func (c *calcState) toFwd(k int, p Position) (Position, bool) {
	for j, it := range c.items {
		if it.ended || it.bad != "" || it.op.DryRun {
			continue
		}
		var ok bool
		if p, ok = apply(p, it.op.MessageSplice, j <= k || !c.sawOriginal(j)); !ok {
			return p, false
		}
	}
	return p, true
}

// sawOriginal says whether the splicing item j received the client's own bytes over the range it
// removed. The library cannot see the pipeline order of an active operation and a reported one, so
// it reads it from the sizes (§4.5): an operator that ran after a reducer received fewer bytes than
// the client sent. If the guess is wrong, the sum check fails.
func (c *calcState) sawOriginal(j int) bool {
	it := c.items[j]
	sp := it.op.MessageSplice
	if it.active {
		return false // both still active: the order was decided on the first request
	}
	if sp == nil || sp.Removed == 0 {
		return true
	}
	from, to := Position{SectionMessages, sp.At}, Position{SectionMessages, sp.At + sp.Removed - 1}
	for i := j - 1; i >= 0; i-- {
		if e := c.items[i]; !e.ended && e.bad == "" && !e.op.DryRun {
			from, to = undo(from, e.op.MessageSplice), undo(to, e.op.MessageSplice)
		}
	}
	f, ok1 := c.client.flat(from)
	t, ok2 := c.client.flat(to)
	if !ok1 || !ok2 {
		return true
	}
	return it.op.SizeBefore.Bytes == c.client.cumBytes(t)-c.client.cumBytes(f-1)
}

func (c *calcState) clientToFwd(p Position) Position { q, _ := c.toFwd(len(c.items), p); return q }

func (c *calcState) fwdToClient(p Position) Position {
	for j := len(c.items) - 1; j >= 0; j-- {
		if it := c.items[j]; !it.ended && it.bad == "" && !it.op.DryRun {
			p = undo(p, it.op.MessageSplice)
		}
	}
	return p
}

func prevFwdToClient(p *Fingerprint, pos Position) Position {
	for j := len(p.splices) - 1; j >= 0; j-- {
		s := p.splices[j]
		pos = undo(pos, &s)
	}
	return pos
}

// candidates are the forwarded positions where a cache entry can end: the breakpoints of this and
// the previous request, and the previous extent. With none, every position is one.
func (c *calcState) candidates() []bp {
	out := append(append([]bp(nil), c.fwdBPs...), c.prevFwdBPs...)
	if c.prevExt >= 0 {
		if i, ok := c.fwd.flat(c.s.PrevPrint.Forwarded.position(c.prevExt)); ok {
			out = append(out, bp{at: i})
		}
	}
	return out
}

// phaseAndBoundary finds the cache phase from the gaps (§5.2) and B* (§5.1).
func (c *calcState) phaseAndBoundary(res *Result) {
	prev, f := c.s.PrevPrint, c.r.Facts
	c.phase = PhaseUnknown
	if prev != nil && f.TTL > 0 && c.s.Cache.Extent != nil {
		gap := c.r.Meta.At.Sub(prev.At)
		last := prev.At
		if f.LastPing.After(last) {
			last = f.LastPing
		}
		entryGap := c.r.Meta.At.Sub(last)
		switch {
		case gap <= f.TTL:
			c.phase, c.prevExt = PhaseHot, mustFlat(prev.Forwarded, *c.s.Cache.Extent)
		case f.Lifetime != LifetimeExact:
			// a minimum or unknown lifetime cannot prove an expiry (R45)
		case !f.LastPing.IsZero() && entryGap <= f.TTL && f.PingRead != nil:
			c.phase, c.prevExt, c.pingHot = PhaseHot, mustFlat(prev.Forwarded, *f.PingRead), true
		default:
			c.phase = PhaseCold
		}
	} else if prev != nil && f.TTL > 0 && f.Lifetime == LifetimeExact && c.r.Meta.At.Sub(prev.At) > f.TTL {
		c.phase = PhaseCold // the previous request cached nothing, and it would have expired anyway
	}

	if !c.noUsage {
		c.readEnd = extent(c.fwd, c.ratio, c.usage.CacheRead, c.candidates())
		c.cachedFwd = extent(c.fwd, c.ratio, c.usage.CacheRead+c.usage.CacheWrite, c.candidates())
	}

	switch c.phase {
	case PhaseHot:
		c.prevExtCli = mustFlat(prev.Client, prevFwdToClient(prev, prev.Forwarded.position(c.prevExt)))
		x := readable(min(c.prevExtCli, c.clientFD-1), c.clientBPs)
		c.bstar = -1
		for _, cand := range append(append([]bp{{at: c.prevExtCli}}, c.clientBPs...), c.prevClientBPs...) {
			if cand.at <= x && cand.at > c.bstar {
				c.bstar = cand.at
			}
		}
	case PhaseUnknown:
		c.unknown = true
		res.Unpriced = append(res.Unpriced, Unpriced{Reason: ReasonPenaltyUnknown})
		if c.readEnd >= 0 { // a read in the usage is evidence (§5.2)
			c.phase = PhaseHot
			c.bstar = mustFlat(c.client, c.fwdToClient(c.fwd.position(c.readEnd)))
		}
	}
	res.Phase = c.phase
	if c.bstar >= 0 {
		p := c.client.position(c.bstar)
		res.Boundary = &p
		c.bstarFwd = mustFlat(c.fwd, c.clientToFwd(p))
	}
}

// readable is the last position up to x that a request with the breakpoints bps can read. The
// provider looks the cache up from a breakpoint, so a prefix that ends at x is readable when a
// breakpoint is at or after x. Otherwise the read snaps down to the last breakpoint at or before x
// (§5.1, §5.2 row 4). With no breakpoint nothing is read.
func readable(x int, bps []bp) int {
	for _, b := range bps {
		if b.at >= x {
			return x
		}
	}
	return lastAtOrBefore(bps, x)
}

// mustFlat is flat for a position the library derived itself. A position in an empty section is
// the start of the next section; one past the end of a section clamps to its last position; both
// clamp to the end of the body.
func mustFlat(b Body, p Position) int {
	if i, ok := b.flat(p); ok {
		return i
	}
	s := sectionIndex(p.Section)
	if s < 0 || p.Index < 0 {
		return -1
	}
	if b.sectionEnd(s) == b.Start[s] {
		return min(b.Start[s], len(b.Pos)-1)
	}
	return min(b.sectionEnd(s)-1, len(b.Pos)-1)
}

// cacheEffects classifies a break (§5.2) and adds a rescue (§4.4).
func (c *calcState) cacheEffects(res *Result) {
	if c.pingHot {
		c.rescue(res)
	}
	switch {
	case c.unknown || c.s.PrevPrint == nil:
		return // row 6
	case c.phase == PhaseCold:
		c.addEffect(res, CacheEffect{Kind: EffectBreak}) // row 5: the expiry, on the record, moving no tokens
		return
	}
	fwdFDCli := mustFlat(c.client, c.fwdToClient(c.fwd.position(c.fwdFD)))
	switch {
	case c.fwdFD <= c.prevExt && fwdFDCli < c.clientFD: // row 1
		c.addBreak(res, c.readEnd, c.changeAt(c.fwdFD))
		return
	case c.clientFD <= c.prevExtCli && fwdFDCli >= c.clientFD: // row 2
		c.addBreak(res, c.readEnd, "")
		return
	}
	predicted := c.ratio.of(c.fwd.cumBytes(readable(mustFlat(c.fwd, c.s.PrevPrint.Forwarded.position(c.prevExt)), c.fwdBPs)))
	low := c.usage.CacheRead+128+predicted/50 < predicted // the one-block tolerance (row 4)
	if !low {
		return
	}
	if op := c.reportedSetting(); op != nil { // row 3
		from, known := c.settingFrom(*op)
		if !known {
			res.Unpriced = append(res.Unpriced, Unpriced{OpID: op.ID, Reason: ReasonPenaltyUnknown})
		}
		c.addBreak(res, from, op.ChangeID)
		return
	}
	c.addBreak(res, c.readEnd, "") // row 4: the entry was lost early
}

func (c *calcState) reportedSetting() *Operation {
	for _, it := range c.items {
		if !it.active && it.bad == "" && !it.op.DryRun && it.op.Kind == KindSetting {
			return &it.op
		}
	}
	return nil
}

// settingFrom is the start of a setting break: the earliest SettingChange.At, snapped down to the
// last forwarded breakpoint before it (§5.2 row 3). With no At, it is the read the usage shows.
func (c *calcState) settingFrom(op Operation) (int, bool) {
	at, known := len(c.fwd.Pos), false
	for _, sc := range op.Settings {
		if sc.At != nil {
			if i := mustFlat(c.fwd, *sc.At); i >= 0 && i < at {
				at, known = i, true
			}
		}
	}
	if !known {
		return c.readEnd, false
	}
	return lastAtOrBefore(c.candidates(), at-1), true
}

// changeAt is the ChangeID of the applied operation whose forwarded span covers the flat position.
func (c *calcState) changeAt(at int) string {
	for k, it := range c.items {
		if it.ended || it.bad != "" || it.op.DryRun || it.op.Kind != KindRewrite {
			continue
		}
		for _, sp := range it.spans {
			from, ok1 := c.toFwd(k, sp.from)
			to, ok2 := c.toFwd(k, sp.to)
			if !ok1 || !ok2 {
				continue
			}
			if f, t := mustFlat(c.fwd, from), mustFlat(c.fwd, to); f <= at && at <= t {
				return it.op.ChangeID
			}
		}
	}
	return ""
}

// addBreak adds a break that moves the forwarded tokens after from, up to B*, from read to write.
func (c *calcState) addBreak(res *Result, from int, changeID string) {
	e := CacheEffect{Kind: EffectBreak, ChangeID: changeID, Estimated: true}
	if from >= 0 {
		p := c.fwd.position(from)
		e.From = &p
	}
	if c.bstarFwd >= 0 {
		p := c.fwd.position(c.bstarFwd)
		e.To = &p
		if n := c.ratio.of(c.fwd.cumBytes(c.bstarFwd) - c.fwd.cumBytes(from)); n > 0 {
			e.Saved.CacheRead = n
			addWrite(&e.Caused, ttlAtOrAfter(c.fwdBPs, c.bstarFwd), n) // "" counts as 5m
		}
	}
	c.addEffect(res, e)
}

// rescue adds the keep-alive rescue when the provider read more than it wrote (§4.4).
func (c *calcState) rescue(res *Result) {
	if c.usage.CacheRead <= c.usage.CacheWrite {
		return
	}
	n := min(c.ratio.of(c.fwd.cumBytes(mustFlat(c.fwd, *c.r.Facts.PingRead))), c.usage.CacheRead)
	e := CacheEffect{Kind: EffectRescue, ChangeID: c.r.Facts.PingWindow, To: c.r.Facts.PingRead, Estimated: true}
	addWrite(&e.Saved, ttlName(c.r.Facts.TTL), n)
	e.Caused.CacheRead = n
	c.addEffect(res, e)
}

func (c *calcState) addEffect(res *Result, e CacheEffect) {
	e.ID = fmt.Sprintf("ce:%d", len(res.CacheEffects)+1)
	res.CacheEffects = append(res.CacheEffects, e)
}

func ttlName(d time.Duration) string {
	if d >= time.Hour {
		return "1h"
	}
	return "5m"
}

// ttlAtOrAfter is the TTL of the first breakpoint at or after p; "" when there is none.
func ttlAtOrAfter(bps []bp, p int) string {
	best, ttl := -1, ""
	for _, b := range bps {
		if b.at >= p && (best < 0 || b.at < best) {
			best, ttl = b.at, b.ttl
		}
	}
	return ttl
}

func addWrite(t *TierTokens, ttl string, n int) {
	if ttl == "1h" {
		t.CacheWrite1h += n
	} else {
		t.CacheWrite5m += n
	}
}

// addTier adds n tokens at flat position p by the tier rule (§6.2): read at or before B*, write up
// to the last breakpoint at the TTL of the next one, input after it or with no breakpoint.
func addTier(t *TierTokens, p, bstar int, bps []bp, n int) {
	switch ttl := ttlAtOrAfter(bps, p); {
	case len(bps) > 0 && p <= bstar:
		t.CacheRead += n
	case ttl == "":
		t.Input += n
	default:
		addWrite(t, ttl, n)
	}
}

// price prices each operation: the saved tokens in client space, the added tokens in forwarded
// space (§6.2). Output order: the reported operations, then the active ones (§6.3).
func (c *calcState) price(res *Result) {
	var reported, active []PricedOp
	for k, it := range c.items {
		if it.ended {
			continue
		}
		p := priced(it.op, it.id)
		switch {
		case it.bad != "":
			res.Unpriced = append(res.Unpriced, Unpriced{OpID: it.id, Reason: it.bad})
		case c.noUsage:
			res.Unpriced = append(res.Unpriced, Unpriced{OpID: it.id, Reason: ReasonNoUsage})
		case it.op.Kind == KindRewrite:
			p.Estimated = true // the tokens come from the bytes ratio (§4.2 rule 7)
			for _, sp := range it.spans {
				d := c.ratio.of(sp.before.Bytes) - c.ratio.of(sp.after.Bytes)
				if d > 0 {
					addTier(&p.Saved, mustFlat(c.client, sp.from), c.bstar, c.clientBPs, d)
				} else if d < 0 {
					fp, _ := c.toFwd(k, sp.from)
					addTier(&p.Caused, mustFlat(c.fwd, fp), c.bstarFwd, c.fwdBPs, -d)
				}
			}
		}
		if it.active {
			active = append(active, p)
		} else {
			reported = append(reported, p)
		}
	}
	res.Ops = append(reported, active...)
}

// checks runs the sum, spans and unique-ChangeID checks (§6.5). The order, attribution and
// conservation checks are not implemented: attribution holds by construction, because the library
// only gives a break the ChangeID of one of the request's operations.
func (c *calcState) checks(res *Result) {
	var got [3]int
	seen := map[string]bool{}
	for _, it := range c.items {
		if it.ended || it.bad != "" {
			continue
		}
		op := it.op
		if (op.Kind == KindRewrite || op.Kind == KindSetting) && op.ChangeID != "" {
			if seen[op.ChangeID] {
				res.Checks = append(res.Checks, CheckFailure{Check: "unique_change", Operator: op.Operator})
			}
			seen[op.ChangeID] = true
		}
		if op.Kind != KindRewrite {
			continue
		}
		if len(op.Spans) > 0 {
			var b, a int
			for _, s := range op.Spans {
				b, a = b+s.SizeBefore.Bytes, a+s.SizeAfter.Bytes
			}
			if b != op.SizeBefore.Bytes || a != op.SizeAfter.Bytes {
				res.Checks = append(res.Checks, CheckFailure{Check: "spans", Operator: op.Operator,
					Detail: fmt.Sprintf("spans=%d/%d op=%d/%d bytes", b, a, op.SizeBefore.Bytes, op.SizeAfter.Bytes)})
			}
		}
		if op.DryRun {
			continue
		}
		for _, sp := range it.spans {
			if s := sectionIndex(sp.from.Section); s >= 0 {
				got[s] += sp.before.Bytes - sp.after.Bytes
			}
		}
	}
	for s := range 3 {
		want := c.client.sectionBytes(s) - c.fwd.sectionBytes(s)
		if got[s] != want {
			res.Checks = append(res.Checks, CheckFailure{Check: "sum",
				Detail: fmt.Sprintf("section=%s ops=%d bodies=%d bytes", sections[s], got[s], want)})
		}
	}
}

// nextState is the state after this request: the applied rewrite and setting operations in client
// space, this request's fingerprint, and what it left cached (§4.6, §6.1).
func (c *calcState) nextState() ThreadState {
	next := ThreadState{Version: c.s.Version}
	for _, it := range c.items {
		if it.ended || it.bad != "" || it.op.ChangeID == "" || (it.op.Kind != KindRewrite && it.op.Kind != KindSetting) {
			continue
		}
		op := it.op
		op.At = it.spans[0].from
		if len(op.Spans) > 0 {
			op.Spans = make([]Span, len(it.spans))
			for i, sp := range it.spans {
				op.Spans[i] = Span{Section: sp.from.Section, From: sp.from.Index, To: sp.to.Index, SizeBefore: sp.before, SizeAfter: sp.after}
			}
		}
		next.Active = append(next.Active, op)
	}
	fp := &Fingerprint{Thread: c.r.Thread.ID, At: c.r.Meta.At, Client: c.client, Forwarded: c.fwd,
		ClientBreakpoints: c.r.ClientBreakpoints, ForwardedBreakpoints: c.r.Breakpoints}
	for _, it := range c.items {
		if !it.ended && it.bad == "" && !it.op.DryRun && it.op.MessageSplice != nil {
			fp.splices = append(fp.splices, *it.op.MessageSplice)
		}
	}
	switch {
	case c.noUsage:
		next.Cache = CacheState{} // unknown
	case c.cachedFwd < 0:
		next.Cache = CacheState{CachePhase: PhaseCold}
	default:
		fp.Cached = c.fwd.position(c.cachedFwd)
		ext := fp.Cached
		next.Cache = CacheState{CachePhase: PhaseHot, Extent: &ext, TTL: ttlName(c.r.Facts.TTL)}
	}
	next.PrevPrint = fp
	return next
}
