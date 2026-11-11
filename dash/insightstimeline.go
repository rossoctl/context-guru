package dash

import (
	"time"
)

// The per-item timeline: when each capability was CARRIED, and when it was USED, over the same
// day buckets, so a page can draw one sparkline per item with the gap between the two series
// visible as a shape.
//
// WHY A PICTURE RATHER THAN ANOTHER VERDICT. The `stale` verdict answers "is this abandoned" with
// a fourteen-day threshold, and a threshold is a judgement this report cannot defend — a fortnight
// off work looks identical to a capability finished with. A timeline needs none: a block of
// invocations at the left and a flat carried line since IS the answer, and the reader decides what
// counts as abandoned for their own work. That is the right division of labour, because only they
// know whether they are done with the planning phase.
//
// It also surfaces what the table buries. Sorted by cost, the one stale item on a real store
// landed 26th of 26 inside a collapsed fold, because "abandoned ninety days ago" has weak cost and
// strong evidence — a combination the ranking has no way to express.

// TimelineBucket is one day, by the epoch-ms of its start.
type TimelineBucket int64

// TimelineItem is one capability's two series, aligned to the shared bucket list: Calls[i] and
// Carried[i] describe the same day. Aligned here rather than left to the page, because joining two
// differently-bucketed series is the one job this endpoint exists to do.
type TimelineItem struct {
	// Calls is invocations that fell in each bucket.
	Calls []int `json:"calls"`
	// Carried is how many requests in each bucket carried the declaration. The baseline the calls
	// are drawn against: where it runs on and the calls stop, the difference is the waste.
	Carried []int `json:"carried"`
	// FirstUsed / LastUsed bound the invocations, for a label that does not need the series read.
	FirstUsed int64 `json:"first_used_ts,omitempty"`
	LastUsed  int64 `json:"last_used_ts,omitempty"`
}

// ToolTimeline is the whole picture: one bucket list, and one item per capability.
type ToolTimeline struct {
	// Buckets are day starts in epoch ms, ascending and contiguous — including days with no
	// traffic at all, because a gap is the thing this view is for and a compacted axis would
	// close it up.
	Buckets []TimelineBucket         `json:"buckets"`
	Items   map[string]*TimelineItem `json:"items"`
	// Days is the window actually covered, so a caller can tell a short history from a narrow
	// request.
	Days int `json:"days"`
}

// bucketOf truncates an epoch-ms timestamp to the start of its local day.
//
// LOCAL, not UTC. The reader is comparing these bars against their own memory of which days they
// worked, and a UTC boundary splits an evening's work across two bars for anybody west of
// Greenwich.
func bucketOf(ms int64) TimelineBucket {
	t := time.UnixMilli(ms)
	return TimelineBucket(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).UnixMilli())
}

// ToolTimeline builds the per-item series. `days` of 0 means every day the store holds; a positive
// value keeps only that many most recent days, so a three-month history does not force ninety bars
// into a sparkline.
//
// The default is deliberately unbounded: the value of this view is seeing a phase CHANGE, and a
// default window would hide the phase before it — which is the exact question the reader came with.
func (d *DB) ToolTimeline(f Filter, days int) (*ToolTimeline, error) {
	where, args := f.where()
	// Buckets starts as an EMPTY slice, not nil: Go marshals nil as null, and the chart reads
	// .buckets.length. A scope with no measured traffic -- a project whose history was all imported
	// from transcripts -- reached it with null, threw, and the reader got a broken page instead of
	// the reason it was empty.
	tl := &ToolTimeline{Items: map[string]*TimelineItem{}, Buckets: []TimelineBucket{}}

	// Requests per day, per session: the denominator for carriage. A declaration is carried by
	// every request of the session that declared it, and the session is the only grain the
	// inventory records a declaration at.
	reqs := map[string]map[TimelineBucket]int{}
	rows, err := d.sql.QueryContext(d.readCtx(),
		`SELECT r.session_id, r.ts FROM requests r WHERE `+where+` AND r.tools > 0`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sid string
		var ts int64
		if err := rows.Scan(&sid, &ts); err != nil {
			rows.Close()
			return nil, err
		}
		m := reqs[sid]
		if m == nil {
			m = map[TimelineBucket]int{}
			reqs[sid] = m
		}
		m[bucketOf(ts)]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return tl, nil
	}

	// Which declarations each session carried.
	decls, err := d.scopedDecls(f, where, args)
	if err != nil {
		return nil, err
	}
	uses, err := d.scopedUses(f, where, args)
	if err != nil {
		return nil, err
	}

	// The axis: every day between the earliest and latest bucket seen, contiguous.
	var lo, hi TimelineBucket
	for _, m := range reqs {
		for b := range m {
			if lo == 0 || b < lo {
				lo = b
			}
			if b > hi {
				hi = b
			}
		}
	}
	if days > 0 {
		// Counted back from the LAST day with traffic, not from now: a report read a week after
		// the traffic stopped would otherwise return an empty window and look like a broken query.
		if cut := hi - TimelineBucket((days-1)*24*3600*1000); cut > lo {
			lo = cut
		}
	}
	idx := map[TimelineBucket]int{}
	for b := lo; b <= hi; b += TimelineBucket(24 * 3600 * 1000) {
		idx[b] = len(tl.Buckets)
		tl.Buckets = append(tl.Buckets, b)
	}
	tl.Days = len(tl.Buckets)
	if tl.Days == 0 {
		return tl, nil
	}

	item := func(name string) *TimelineItem {
		it := tl.Items[name]
		if it == nil {
			it = &TimelineItem{
				Calls:   make([]int, len(tl.Buckets)),
				Carried: make([]int, len(tl.Buckets)),
			}
			tl.Items[name] = it
		}
		return it
	}

	// Carriage: every request of the declaring session, on the day that request happened.
	for _, dr := range decls {
		if dr.kind == KindSystemPrompt || dr.name == "" {
			continue
		}
		it := item(dr.name)
		for b, n := range reqs[dr.session] {
			if i, ok := idx[b]; ok {
				it.Carried[i] += n
			}
		}
	}

	// Invocations. ALL of a session's calls land in the bucket of its LAST use, and that is a
	// deliberate refusal rather than a shortcut: tool_uses holds one accumulated row per
	// (session, name) — 961 calls in a 101-hour session is a single row — so the store does not
	// know which day each call fell on. Spreading them across the span would draw a shape nobody
	// measured, which is worse than a bar in the wrong place, because it looks like evidence.
	for _, u := range uses {
		if u.name == "" {
			continue
		}
		names := []string{u.name}
		if u.skill != "" {
			names = append(names, u.skill)
		}
		for _, n := range names {
			it := item(n)
			if i, ok := idx[bucketOf(u.lastTS)]; ok {
				it.Calls[i] += u.calls
			}
			if it.FirstUsed == 0 || u.lastTS < it.FirstUsed {
				it.FirstUsed = u.lastTS
			}
			if u.lastTS > it.LastUsed {
				it.LastUsed = u.lastTS
			}
		}
	}
	return tl, nil
}
