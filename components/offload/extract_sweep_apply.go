package offload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/coref"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// THE COLD APPLY (#404, points 3 and 4).
//
// The sweep asks at the keep-alive ping and HOLDS the answer (extract_sweep_hold.go). This file uses
// it. On the next request of the thread whose prompt cache is gone, the whole conversation is written
// anyway, so a dropped output makes that write smaller and costs nothing extra. That request:
//
//  1. rechecks the held answer (recheckHeld),
//  2. replaces each still-valid output with its shape descriptor plus a <<cg:HASH>> marker and stashes
//     the original, so `expand` gives it back byte for byte,
//  3. writes one drop record per output (cg:swd:), which later requests REPLAY.
//
// A WARM REQUEST NEVER CHANGES (H1). Without a record there is nothing to replay, and the held answer
// is only read, never applied, while the cache is live. After a cold apply every later request
// replays the same bytes, so the prefix the provider now holds stays byte-stable.
//
// THE CHANGED-PREFIX CASE (point 4). A rewind, an edited turn or a fork changes the conversation at
// some message. The provider rewrites everything from there on, so a drop AFTER that point is free
// and a drop BEFORE it is not. changePoint finds the first message that differs from the conversation
// that was judged; only held drops after it are applied, and the ones before it stay held. They are
// counted apart (sweep_applied_changed_prefix).
//
// FAIL OPEN. Every drop is reversible (marker plus stash), a store that cannot hold the original
// refuses the drop and leaves the output verbatim, and a panic anywhere in the apply restores every
// message it touched and writes no drop record. A drop record exists only for a drop the request
// carries.

// Events the apply raises on the request's Report.
const (
	eventAppliedCold          = "sweep_applied_cold"
	eventAppliedChangedPrefix = "sweep_applied_changed_prefix"
	eventApplyDeclined        = "sweep_apply_declined"
	eventApplyPanic           = "sweep_apply_panic"
	eventFalseRef             = "sweep_false_ref"
	// The warm path (#491) applies through the same code and counts apart.
	eventAppliedWarm  = "sweep_applied_warm"
	eventFalseRefWarm = "sweep_false_ref_warm"
)

// gateApplyAlreadyMarked: a held drop's output carries a marker one of our components made, so it is
// not dropped again (#721). A quoted marker is not one.
const gateApplyAlreadyMarked = "sweep_apply_already_marked"

// dropVersion is bumped when sweepDropRec changes shape. A record of another version reads as absent.
const dropVersion = 1

// dropKindWarm is the Kind of a drop applied on a warm request (#491).
const dropKindWarm = "warm"

// maxDropsPerThread bounds the per-thread index. A cold apply drops at most maxAskItems x max_ask_batches
// (at most 12 x 8 = 96) outputs per question set, so this is many cold applies deep.
const maxDropsPerThread = 512

// sweepDropRec is one applied drop. METADATA ONLY: ids, hashes, counts. No byte of the output is in it
// (the output itself is in the Store's stash, behind the marker, exactly as for every other offloader).
type sweepDropRec struct {
	V         int    `json:"v"`
	Thread    string `json:"thread"`
	ToolUseID string `json:"id"`
	// CK is extract.ContentKey of the dropped output: what the replay matches on.
	CK string `json:"ck"`
	// Marker is the expand id behind the marker (hashKey of the output), what an expand call names.
	Marker string `json:"marker"`
	// Tokens is what the drop removed, net of the marker and descriptor that replace the output.
	Tokens int    `json:"tokens"`
	AtMs   int64  `json:"at_ms"`
	Kind   string `json:"kind"` // "cold", "changed_prefix" or "warm"
	// Idents are short hashes of the identifiers the output introduced. A later turn that uses one,
	// without expanding, is the referenced_later signal.
	Idents []string `json:"idents,omitempty"`
	// Seen is how many of the agent's messages have been scanned for such a reference.
	Seen       int  `json:"seen"`
	Referenced bool `json:"referenced,omitempty"`
	Expanded   bool `json:"expanded,omitempty"`
}

type sweepDropIndex struct {
	V   int      `json:"v"`
	Cks []string `json:"cks"`
}

// Two different separators, so an index key and a record key cannot collide for odd thread names.
func dropIndexKey(threadKey string) string { return store.SweepDropPrefix + threadKey + "\x02" }
func dropKey(threadKey, ck string) string  { return store.SweepDropPrefix + threadKey + "\x01" + ck }
func dropMarkerKey(marker string) string   { return store.SweepExpandPrefix + marker }

func getDrop(st store.Store, threadKey, ck string) (sweepDropRec, bool) {
	if st == nil || threadKey == "" || ck == "" {
		return sweepDropRec{}, false
	}
	b, ok := st.Get(dropKey(threadKey, ck))
	if !ok || len(b) == 0 {
		return sweepDropRec{}, false
	}
	var r sweepDropRec
	if json.Unmarshal(b, &r) != nil || r.V != dropVersion || r.Thread != threadKey || r.CK != ck {
		return sweepDropRec{}, false
	}
	return r, true
}

func putDrop(st store.Store, r sweepDropRec) {
	r.V = dropVersion
	if b, err := json.Marshal(r); err == nil {
		st.Put(dropKey(r.Thread, r.CK), b)
	}
}

// tombstoneDrop makes a drop record read as absent (a version that is not dropVersion). Used to undo a
// record written by an attempt whose request was restored. Caller holds dropMu.
func tombstoneDrop(st store.Store, r sweepDropRec) {
	r.V = -1
	if b, err := json.Marshal(r); err == nil {
		st.Put(dropKey(r.Thread, r.CK), b)
	}
}

func getDropIndex(st store.Store, threadKey string) sweepDropIndex {
	var ix sweepDropIndex
	if st == nil || threadKey == "" {
		return ix
	}
	if b, ok := st.Get(dropIndexKey(threadKey)); ok && len(b) > 0 {
		if json.Unmarshal(b, &ix) != nil || ix.V != dropVersion {
			return sweepDropIndex{}
		}
	}
	return ix
}

func addToDropIndex(st store.Store, threadKey string, cks []string) {
	if len(cks) == 0 {
		return
	}
	ix := getDropIndex(st, threadKey)
	have := make(map[string]struct{}, len(ix.Cks))
	for _, ck := range ix.Cks {
		have[ck] = struct{}{}
	}
	for _, ck := range cks {
		if _, ok := have[ck]; !ok {
			ix.Cks = append(ix.Cks, ck)
			have[ck] = struct{}{}
		}
	}
	if len(ix.Cks) > maxDropsPerThread {
		ix.Cks = ix.Cks[len(ix.Cks)-maxDropsPerThread:]
	}
	ix.V = dropVersion
	if b, err := json.Marshal(ix); err == nil {
		st.Put(dropIndexKey(threadKey), b)
	}
}

// sweepMarkerOwner is one thread that carries a marker, and the content key of its drop record.
type sweepMarkerOwner struct {
	Thread string `json:"t"`
	CK     string `json:"ck"`
}

// sweepMarkerRef lists EVERY thread whose drop carries the marker. The marker id is hashKey(content)
// and has no thread in it, so two threads that drop the same bytes share one marker (and one stash). A
// single owner would let the second overwrite the first and credit an expand to the wrong thread.
type sweepMarkerRef struct {
	Owners []sweepMarkerOwner `json:"o"`
}

// dropMu serialises every read-modify-write of a drop record and of a marker ref (the reference scan,
// the expand note, the commit and its undo). store.Store has no compare-and-set, so without it two
// requests of one thread can each read the record, set a different flag, and lose one. One process-wide
// mutex is enough: the sections are a few map reads and writes.
var dropMu sync.Mutex

func getMarkerRef(st store.Store, marker string) sweepMarkerRef {
	var ref sweepMarkerRef
	if b, ok := st.Get(dropMarkerKey(marker)); ok && len(b) > 0 {
		if json.Unmarshal(b, &ref) != nil {
			return sweepMarkerRef{}
		}
	}
	return ref
}

func putMarkerRef(st store.Store, marker string, ref sweepMarkerRef) {
	if b, err := json.Marshal(ref); err == nil {
		st.Put(dropMarkerKey(marker), b)
	}
}

// addMarkerOwner adds (thread, ck) to a marker's owners, once. Caller holds dropMu.
func addMarkerOwner(st store.Store, marker, thread, ck string) {
	ref := getMarkerRef(st, marker)
	for _, o := range ref.Owners {
		if o.Thread == thread && o.CK == ck {
			return
		}
	}
	ref.Owners = append(ref.Owners, sweepMarkerOwner{Thread: thread, CK: ck})
	putMarkerRef(st, marker, ref)
}

// removeMarkerOwner undoes addMarkerOwner. Caller holds dropMu.
func removeMarkerOwner(st store.Store, marker, thread, ck string) {
	ref := getMarkerRef(st, marker)
	out := ref.Owners[:0]
	for _, o := range ref.Owners {
		if !(o.Thread == thread && o.CK == ck) {
			out = append(out, o)
		}
	}
	ref.Owners = out
	putMarkerRef(st, marker, ref)
}

// identHash is the short, one-way form an identifier is kept in. The records hold metadata only.
func identHash(t string) string {
	h := sha256.Sum256([]byte("cg-swd|" + t))
	return hex.EncodeToString(h[:4])
}

// ---------------------------------------------------------------------------------------------------
// The quality ledger (#404 point 7). Per drop, from day 1.

// SweepQuality counts what became of the drops the sweep applied. A drop is FALSE when a later turn
// needed it: the agent expanded the marker (Expanded), or a later turn used something the output
// introduced without expanding it (Referenced). A drop that is both is counted once (Overlap).
type SweepQuality struct {
	Applied, AppliedCold, AppliedChangedPrefix int64
	// AppliedWarm and the three Warm counters below are the warm path's share (#491). They are PART of
	// Applied, Referenced, Expanded and Overlap, never added on top, so the cold figures are the totals
	// minus these.
	AppliedWarm                               int64
	ReferencedWarm, ExpandedWarm, OverlapWarm int64
	// ApplyDeclined: valid drops the store or the never-worse check refused. They stayed verbatim.
	ApplyDeclined int64
	Referenced    int64
	Expanded      int64
	Overlap       int64
	// ExpandEvents and ExpandTokens count every expand call of a sweep marker, and what it returned.
	ExpandEvents, ExpandTokens int64
}

// FalseDrops is the number of distinct drops a later turn needed.
func (q SweepQuality) FalseDrops() int64 { return q.Referenced + q.Expanded - q.Overlap }

// FalseDropsWarm is FalseDrops for the drops applied on a warm request.
func (q SweepQuality) FalseDropsWarm() int64 {
	return q.ReferencedWarm + q.ExpandedWarm - q.OverlapWarm
}

// FalseDropRateWarm is FalseDropsWarm over AppliedWarm, or 0 with none applied.
func (q SweepQuality) FalseDropRateWarm() float64 {
	if q.AppliedWarm <= 0 {
		return 0
	}
	return float64(q.FalseDropsWarm()) / float64(q.AppliedWarm)
}

// FalseDropRateCold is the false-drop rate of every drop that was not applied warm.
func (q SweepQuality) FalseDropRateCold() float64 {
	n := q.Applied - q.AppliedWarm
	if n <= 0 {
		return 0
	}
	return float64(q.FalseDrops()-q.FalseDropsWarm()) / float64(n)
}

// FalseDropRate is FalseDrops over Applied, or 0 with nothing applied.
func (q SweepQuality) FalseDropRate() float64 {
	if q.Applied <= 0 {
		return 0
	}
	return float64(q.FalseDrops()) / float64(q.Applied)
}

// SweepThreadQuality is one thread's SweepQuality under a label that is not the session id.
type SweepThreadQuality struct {
	Label string
	SweepQuality
}

// SweepDropStats is the sweep's quality ledger for /stats.
type SweepDropStats struct {
	Overall SweepQuality
	// Threads lists the busiest threads (most applied first), at most maxLedgerShown.
	Threads []SweepThreadQuality
}

const (
	maxLedgerThreads = 256
	maxLedgerShown   = 20
)

var sweepLedger = struct {
	mu      sync.Mutex
	overall SweepQuality
	threads map[string]*ledgerThread
	seq     uint64
}{threads: map[string]*ledgerThread{}}

type ledgerThread struct {
	SweepQuality
	used uint64
}

// threadLabel is a short hash of the thread key: /stats must not print a session id.
func threadLabel(threadKey string) string {
	h := sha256.Sum256([]byte("cg-swl|" + threadKey))
	return hex.EncodeToString(h[:5])
}

func ledgerNote(threadKey string, f func(q *SweepQuality)) {
	sweepLedger.mu.Lock()
	defer sweepLedger.mu.Unlock()
	f(&sweepLedger.overall)
	ledgerThreadLocked(threadKey, f)
}

// ledgerNoteThread books into one thread only; ledgerNoteOverall into the process total only. An expand
// of a shared marker books each owner's row and the total once.
func ledgerNoteThread(threadKey string, f func(q *SweepQuality)) {
	sweepLedger.mu.Lock()
	defer sweepLedger.mu.Unlock()
	ledgerThreadLocked(threadKey, f)
}

func ledgerNoteOverall(f func(q *SweepQuality)) {
	sweepLedger.mu.Lock()
	defer sweepLedger.mu.Unlock()
	f(&sweepLedger.overall)
}

func ledgerThreadLocked(threadKey string, f func(q *SweepQuality)) {
	t, ok := sweepLedger.threads[threadKey]
	if !ok {
		if len(sweepLedger.threads) >= maxLedgerThreads {
			oldest, oldKey := ^uint64(0), ""
			for k, v := range sweepLedger.threads {
				if v.used < oldest {
					oldest, oldKey = v.used, k
				}
			}
			delete(sweepLedger.threads, oldKey)
		}
		t = &ledgerThread{}
		sweepLedger.threads[threadKey] = t
	}
	sweepLedger.seq++
	t.used = sweepLedger.seq
	f(&t.SweepQuality)
}

// SweepDropCounters snapshots the quality ledger.
func SweepDropCounters() SweepDropStats {
	sweepLedger.mu.Lock()
	defer sweepLedger.mu.Unlock()
	out := SweepDropStats{Overall: sweepLedger.overall}
	for k, v := range sweepLedger.threads {
		out.Threads = append(out.Threads, SweepThreadQuality{Label: threadLabel(k), SweepQuality: v.SweepQuality})
	}
	sort.Slice(out.Threads, func(i, j int) bool {
		if out.Threads[i].Applied != out.Threads[j].Applied {
			return out.Threads[i].Applied > out.Threads[j].Applied
		}
		return out.Threads[i].Label < out.Threads[j].Label
	})
	if len(out.Threads) > maxLedgerShown {
		out.Threads = out.Threads[:maxLedgerShown]
	}
	return out
}

// resetSweepLedgerForTest clears the ledger between tests.
func resetSweepLedgerForTest() {
	sweepLedger.mu.Lock()
	sweepLedger.overall = SweepQuality{}
	sweepLedger.threads = map[string]*ledgerThread{}
	sweepLedger.mu.Unlock()
}

// SweepExpandNote says what an expand call of a marker meant to the sweep.
type SweepExpandNote struct {
	// Sweep: the marker is one the sweep made. The caller charges the expand to the sweep.
	Sweep bool
	// First: this is the first expand of that drop. Only a first expand is a new false drop.
	First bool
	// Overlap: the drop had already been counted as referenced, so it is not a new false drop.
	Overlap bool
	// Warm: the first expand was of a drop applied on a warm request (#491).
	Warm bool
}

// NoteSweepExpand is called by the host for every expand call that resolved; tokens is what the
// restored text measures. It reports whether the marker is a sweep drop and records the expand on the
// drop and in the ledger. It never fails the expand.
//
// threadKey is the thread that made the expand (thread.Key(session, thread)), or "" when the host does
// not know it yet (the request-path repair runs before the thread is resolved). A marker can be carried by
// several threads that dropped the same bytes. With a thread, only that thread is credited, and only if it
// is an owner. Without one, every owner is credited, because every owner carries the marker.
func NoteSweepExpand(st store.Store, markerID string, tokens int, threadKey string) (note SweepExpandNote) {
	defer func() { _ = recover() }()
	if st == nil || markerID == "" {
		return
	}
	dropMu.Lock()
	defer dropMu.Unlock()
	ref := getMarkerRef(st, markerID)
	credited, firsts, overlaps := 0, 0, 0
	warmFirsts, warmOverlaps := 0, 0
	for _, o := range ref.Owners {
		if threadKey != "" && o.Thread != threadKey {
			continue
		}
		rec, ok := getDrop(st, o.Thread, o.CK)
		if !ok || rec.Marker != markerID {
			continue
		}
		note.Sweep = true
		first := !rec.Expanded
		overlap := first && rec.Referenced
		if first {
			rec.Expanded = true
			putDrop(st, rec)
			if !note.First {
				note.First, note.Overlap = true, overlap
				note.Warm = rec.Kind == dropKindWarm
			}
		}
		isWarm := rec.Kind == dropKindWarm
		ledgerNoteThread(rec.Thread, func(q *SweepQuality) {
			q.ExpandEvents++
			q.ExpandTokens += int64(tokens)
			if first {
				q.Expanded++
				if overlap {
					q.Overlap++
				}
				if isWarm {
					q.ExpandedWarm++
					if overlap {
						q.OverlapWarm++
					}
				}
			}
		})
		if first && isWarm {
			warmFirsts++
			if overlap {
				warmOverlaps++
			}
		}
		credited++
		if first {
			firsts++
			if overlap {
				overlaps++
			}
		}
	}
	// Overall is counted ONCE per expand call, however many threads carry the marker: it is one expand
	// event, and the per-thread rows above hold the per-owner detail.
	if credited > 0 {
		ledgerNoteOverall(func(q *SweepQuality) {
			q.ExpandEvents++
			q.ExpandTokens += int64(tokens)
			q.Expanded += int64(firsts)
			q.Overlap += int64(overlaps)
			q.ExpandedWarm += int64(warmFirsts)
			q.OverlapWarm += int64(warmOverlaps)
		})
	}
	return note
}

// SweepMarkerID is the marker id an original is stashed under: the id an expand of it names. For a
// host that has the restored original but not the id it was asked for.
func SweepMarkerID(original string) string { return hashKey(original) }

// ---------------------------------------------------------------------------------------------------
// The change point.

// msgPrint is a cheap fingerprint of one message of the agent's conversation: role, tool ids, length
// and the two ends of the text. It is a pure function of the bytes the agent sent, so a message that
// did not change never changes it. A same-length edit that keeps both ends is missed, and a missed
// change only means the apply waits for a cold request. It never applies a drop on a warm request.
func msgPrint(m bschemas.ChatMessage) uint32 {
	h := fnv.New32a()
	h.Write([]byte(m.Role))
	if m.ChatToolMessage != nil && m.ChatToolMessage.ToolCallID != nil {
		h.Write([]byte(*m.ChatToolMessage.ToolCallID))
	}
	if m.ChatAssistantMessage != nil {
		for _, tc := range m.ChatAssistantMessage.ToolCalls {
			if tc.ID != nil {
				h.Write([]byte(*tc.ID))
			}
			fmt.Fprintf(h, "|%d", len(tc.Function.Arguments))
		}
	}
	txt := schema.MessageText(m)
	fmt.Fprintf(h, "|%d|", len(txt))
	const end = 48
	if len(txt) <= 2*end {
		h.Write([]byte(txt))
	} else {
		h.Write([]byte(txt[:end]))
		h.Write([]byte(txt[len(txt)-end:]))
	}
	return h.Sum32()
}

func msgPrints(msgs []bschemas.ChatMessage) []uint32 {
	out := make([]uint32, len(msgs))
	for i := range msgs {
		out[i] = msgPrint(msgs[i])
	}
	return out
}

// changePoint returns the index of the first message of raw that differs from the conversation the
// held answer judged, or -1 when no message in the judged range changed (or there is nothing to
// compare against). A conversation SHORTER than the judged one is not handled here: the recheck voids
// the whole answer.
func changePoint(raw []bschemas.ChatMessage, h heldAnswer) int {
	n := len(h.Prints)
	if n == 0 || len(raw) < n {
		return -1
	}
	for i := 0; i < n; i++ {
		if msgPrint(raw[i]) != h.Prints[i] {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------------------------------
// The apply.

// sweepColdRequest is the one test for "the cache is gone": apply's own verdict (Ctx.ColdCache), which
// carries a clock margin. Shadow mode and the apply use it alike, so shadow reports what the apply
// would do. CachePhase(0) also calls the minute before the margin cold, which is the wrong error here.
//
// It is the SAME test the summary reserve graduates on (ColdApplyPhase, #688): cold, or unknown. One
// narrowing for the sweep: an unknown phase on a request that still has a known cached prefix
// (CacheAware with MaxCachedIdx >= 0) is NOT cold. Unknown there means "cannot say how much life is
// left", not "gone", and a drop would rewrite a live prefix. The summary never removes content from
// inside the prefix, so it can afford the wider test.
func sweepColdRequest(c *components.Ctx) bool {
	if c == nil {
		return false
	}
	p := c.CachePhase(0)
	if !ColdApplyPhase(p) {
		return false
	}
	return !(p == components.CachePhaseUnknown && c.CacheAware && c.MaxCachedIdx >= 0)
}

// maxApplyAttempts bounds how often a panicking cold apply gives the answer back.
const maxApplyAttempts = 2

// sweepApplyPanics counts recovered panics in the cold apply (also an event on the request's Report).
var sweepApplyPanics atomic.Int64

// SweepApplyPanics is the process-wide count of recovered cold-apply panics.
func SweepApplyPanics() int64 { return sweepApplyPanics.Load() }

// coldStep runs at the top of every request of the sweep, outside shadow mode. It replays earlier
// drops, scans for references to them, and applies a held answer when the request is cold or its
// prefix changed. It returns the stash keys of the drops it wrote or replayed.
func (e *ExtractSweep) coldStep(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) (keys []string, changed int) {
	if c == nil || c.Store == nil || c.Session == "" || req == nil {
		return nil, 0
	}
	tkey := thread.Key(c.Session, c.Thread)
	val := savedTokenValue(c)

	// 1. Replay the drops this thread already made, at any depth: the provider holds the dropped form.
	rk, rc := e.replayDrops(req, rep, c, tkey, val)
	keys, changed = append(keys, rk...), changed+rc

	// 2. References to earlier drops.
	e.scanReferences(req, rep, c, tkey)

	// 3. A held answer.
	ak, ac := e.applyHeld(req, rep, c, tkey)
	keys, changed = append(keys, ak...), changed+ac
	return keys, changed
}

// replayDrops puts back, byte for byte, every drop this thread already made. The replacement is a pure
// function of (content, config), so the bytes equal the ones sent when the drop was applied.
func (e *ExtractSweep) replayDrops(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	tkey string, val tokenValue) (keys []string, changed int) {

	if len(getDropIndex(c.Store, tkey).Cks) == 0 {
		return nil, 0
	}
	for _, i := range toolIndices(req) {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			continue
		}
		content := schema.MessageText(*msg)
		if content == "" || carriesRealMarker(c, content) {
			continue
		}
		ck := extract.ContentKey(content)
		if _, ok := getDrop(c.Store, tkey, ck); !ok {
			continue
		}
		metrics.RecordExtractionCacheLookup(rep.Component, true)
		// The agent expanded it: leave it verbatim (kept-verbatim is the host's mark, set on expand).
		if isKeptVerbatim(c, ck) {
			rep.Gate(GateKeptVerbatim)
			continue
		}
		k, ok := applySweepDropReplay(c, rep, e.mode, msg, content)
		if !ok {
			continue
		}
		if saved := schema.TextTokens(content) - schema.TextTokens(schema.MessageText(*msg)); saved > 0 {
			metrics.RecordExtractionValue(rep.Component, float64(saved)*val.repeatPerToken)
		}
		changed++
		if k != "" {
			keys = append(keys, k)
		}
		rep.Replay("reapplied_same_session")
	}
	return keys, changed
}

// scanReferences looks, once per new message, for a use of something a dropped output introduced.
func (e *ExtractSweep) scanReferences(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx, tkey string) {
	defer func() { _ = recover() }() // a signal, never a reason to fail the request
	ix := getDropIndex(c.Store, tkey)
	if len(ix.Cks) == 0 {
		return
	}
	raw := req.Input
	if c.RawInput != nil {
		raw = c.RawInput
	}
	for _, ck := range ix.Cks {
		rec, ok := getDrop(c.Store, tkey, ck)
		if !ok || rec.Referenced || rec.Expanded || len(rec.Idents) == 0 || rec.Seen >= len(raw) {
			continue
		}
		want := make(map[string]struct{}, len(rec.Idents))
		for _, id := range rec.Idents {
			want[id] = struct{}{}
		}
		hit := false
		for j := rec.Seen; j < len(raw) && !hit; j++ {
			for t := range coref.Idents(modelAuthoredText(raw[j])) {
				if _, ok := want[identHash(t)]; ok {
					hit = true
					break
				}
			}
		}
		seen := len(raw)
		// The flags are written under dropMu from a FRESH read, so an expand that landed while this
		// request was scanning is not overwritten.
		func() {
			// DEFERRED unlock: the function-level recover swallows a panic from the Store or the
			// ledger, and a plain Unlock after it would leave dropMu held for the life of the process,
			// blocking every later scan, expand note and commit.
			dropMu.Lock()
			defer dropMu.Unlock()
			cur, ok := getDrop(c.Store, tkey, ck)
			if !ok {
				return
			}
			if seen > cur.Seen {
				cur.Seen = seen
			}
			if hit && !cur.Referenced {
				cur.Referenced = true
				rep.Event(eventFalseRef)
				warmDrop := cur.Kind == dropKindWarm
				if warmDrop {
					rep.Event(eventFalseRefWarm)
				}
				ledgerNote(tkey, func(q *SweepQuality) {
					q.Referenced++
					if warmDrop {
						q.ReferencedWarm++
					}
				})
			}
			putDrop(c.Store, cur)
		}()
	}
}

// modelAuthoredText is the reference-bearing surface of a message: what the agent or the model wrote,
// never a tool output. The same surface coref reads.
func modelAuthoredText(m bschemas.ChatMessage) string {
	if m.Role == bschemas.ChatMessageRoleTool {
		return ""
	}
	s := schema.MessageText(m)
	if m.ChatAssistantMessage != nil {
		for _, tc := range m.ChatAssistantMessage.ToolCalls {
			name := ""
			if tc.Function.Name != nil {
				name = *tc.Function.Name
			}
			s += " " + name + " " + tc.Function.Arguments
		}
	}
	return s
}

// pendingDrop is a drop applied to req but not yet recorded.
type pendingDrop struct {
	rec    sweepDropRec
	idx    int
	before bschemas.ChatMessage
}

// heldSource is where a held answer lives. The cold apply reads the Store. The warm path (#491) reads a
// per-thread map in process memory and NEVER writes the answer to the Store, so its source's put is a
// no-op: a warm answer that is claimed is gone, whatever happens next.
type heldSource interface {
	get(tkey string) (heldAnswer, bool)
	claim(tkey string) (heldAnswer, bool)
	put(h heldAnswer)
}

type storeHeld struct{ st store.Store }

func (s storeHeld) get(tkey string) (heldAnswer, bool)   { return getHeld(s.st, tkey) }
func (s storeHeld) claim(tkey string) (heldAnswer, bool) { return claimHeldSpend(s.st, tkey) }
func (s storeHeld) put(h heldAnswer)                     { putHeld(s.st, h) }

// warmApply turns applyHeldFrom into the warm apply. gate decides, after the recheck and before any
// message is changed, whether the valid drops pay; it returns the reason when they do not.
type warmApply struct {
	// rewriteBilled is set by the gate: the rewritten tokens, billed-scaled. The apply books it as an
	// event on the request that paid it, so the dashboard can charge the rewrite there.
	rewriteBilled int
	// The gate's numbers for the request that applied: removed (billed-scaled), expected warm turns, the
	// net the gate expected (saving - rewrite, in dollars), and whether the drop was deeper than the
	// provider's look-back. Booked as events and as a record in the warm ledger.
	removedBilled int
	turns, netUSD float64
	deep          bool
	gate          func(res recheckResult, raw []bschemas.ChatMessage) (ok bool, reason string)
}

// applyHeld applies the thread's held answer when the request is cold, or when its prefix changed (the
// drops after the change point). Anything else leaves the answer held and the request unchanged.
func (e *ExtractSweep) applyHeld(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx,
	tkey string) (keys []string, changed int) {
	return e.applyHeldFrom(storeHeld{c.Store}, nil, req, rep, c, tkey)
}

// applyHeldFrom is applyHeld over any source. With warm == nil it is the cold apply, exactly. With warm
// set it is the warm apply (#491): the request is warm and unchanged, which the cold apply never
// touches, so the cold test and the changed-prefix test are skipped and the economic gate stands in
// their place. Every other step is the same: claim, recheck, apply with a marker and a stash, commit
// the drop records, restore on a panic.
func (e *ExtractSweep) applyHeldFrom(src heldSource, warm *warmApply, req *bschemas.BifrostChatRequest,
	rep *components.Report, c *components.Ctx, tkey string) (keys []string, changed int) {

	h, ok := src.get(tkey)
	if !ok {
		return nil, 0
	}
	if h.Attempts >= maxApplyAttempts {
		return nil, 0
	}
	raw := req.Input
	if c.RawInput != nil {
		raw = c.RawInput
	}
	cold := sweepColdRequest(c)
	// warmGated: a warm apply on an UNCHANGED prefix, the one case the cold apply never touches. It is
	// let through only by the economic gate below. A cold request, or a CHANGED prefix, is the cold
	// apply's own case exactly as #695 defines it: the provider rewrites from there on anyway, so the
	// drops after it cost nothing extra and need no gate. (A warm answer met by one is applied that way.)
	warmGated := warm != nil && !cold && changePoint(raw, h) < 0
	if !cold && !warmGated && changePoint(raw, h) < 0 {
		return nil, 0 // warm and unchanged: H1. The answer stays held.
	}
	kind := "cold"
	switch {
	case warmGated:
		kind = dropKindWarm
	case !cold:
		kind = "changed_prefix"
	}

	// CLAIM FIRST: two cold requests of one thread must not both apply. claimHeldSpend marks the answer
	// spent for exactly one caller and hands back what it read.
	claimed, won := src.claim(tkey)
	if !won {
		return nil, 0
	}
	h = claimed
	// THE CHANGE POINT COMES FROM THE ANSWER THE CLAIM RETURNED, not from the one read above: a newer
	// answer can land between the read and the claim, and the split of drops into "after the change" and
	// "before it" must be made against the answer that is being applied.
	cp := -1
	if !cold && !warmGated {
		cp = changePoint(raw, h)
		if cp < 0 {
			// The answer we won is not changed-prefix against this request (the newer one judged what
			// this request carries). Nothing to apply on a warm request: give it back, unspent.
			h.Spent = false
			src.put(h)
			return nil, 0
		}
	}

	// Fail open. A panic restores every message touched, writes no record, counts the panic, and GIVES
	// THE ANSWER BACK (RETRIABLE) so the drops are not lost silently. The next cold request tries again,
	// at most maxApplyAttempts times in all: a cold apply that panics costs one request its saving and
	// nothing else, while a poisoned answer that panicked on every cold request would cost a recheck each
	// time for nothing, hence the bound.
	var pending []pendingDrop
	whole := h
	// committed is every record written by THIS attempt. The commit is atomic in effect: if anything
	// panics after the first record is written, the recover tombstones every one of them, so the next
	// request does not replay a form this request never sent.
	var committed []sweepDropRec
	defer func() {
		if r := recover(); r != nil {
			for i := len(pending) - 1; i >= 0; i-- {
				req.Input[pending[i].idx] = pending[i].before
			}
			func() {
				defer func() { _ = recover() }() // the Store may be what panicked
				dropMu.Lock()
				defer dropMu.Unlock()
				for _, rec := range committed {
					tombstoneDrop(c.Store, rec)
					if rec.Marker != "" {
						removeMarkerOwner(c.Store, rec.Marker, rec.Thread, rec.CK)
					}
				}
			}()
			keys, changed = nil, 0
			rep.Event(eventApplyPanic)
			sweepApplyPanics.Add(1)
			func() {
				defer func() { _ = recover() }() // the Store may be what panicked
				back := whole
				back.Attempts++
				back.Spent = back.Attempts >= maxApplyAttempts
				src.put(back)
			}()
			slog.Error("cg.sweep.apply_panic: recovered; the request is forwarded unchanged",
				"session", c.Session, "thread", c.Thread, "panic", fmt.Sprint(r))
		}
	}()

	convBefore := 0
	if warmGated {
		convBefore = localTokens(req.Input)
	}
	res := recheckHeld(c, req, h, e.minTokens)
	// THE WARM GATE (#491). Between the recheck and the first change to the request. A refusal drops
	// the answer: it was claimed above and the warm source keeps nothing.
	if warmGated && warm.gate != nil {
		if ok, reason := warm.gate(res, raw); !ok {
			rep.Event("sweep_warm_gate_refuse_" + trimReason(reason))
			// The refused decision's numbers on the row too, so a refusal reads as numbers.
			if d, have := lastWarmDecision(); have {
				rep.EventN("sweep_warm_refused_removed_tokens", d.RemovedBilled)
				rep.EventN("sweep_warm_refused_rewrite_tokens", d.RewrittenBilled)
				rep.EventN("sweep_warm_refused_expected_turns", int(d.Turns))
				if d.Deep {
					rep.Event("sweep_warm_refused_deep")
				}
			}
			noteWarmGate(false, reason)
			return nil, 0
		}
		rep.Event("sweep_warm_gate_accept")
		rep.EventN("sweep_warm_rewrite_tokens", warm.rewriteBilled)
		rep.EventN("sweep_warm_removed_tokens", warm.removedBilled)
		rep.EventN("sweep_warm_expected_turns", int(warm.turns))
		if warm.deep {
			rep.Event("sweep_warm_deep")
		}
		noteWarmGate(true, "")
	}
	// The drops that go in now, and the valid ones that stay held (before the change point).
	var now []heldOutput
	var later []heldOutput
	for _, o := range res.Valid {
		if cold {
			now = append(now, o)
			continue
		}
		if ri, ok := rawToolMessage(raw, o.ToolUseID); ok && ri > cp {
			now = append(now, o)
		} else {
			later = append(later, o)
		}
	}
	for _, o := range now {
		idx, ok := rawToolMessage(req.Input, o.ToolUseID)
		if !ok {
			continue
		}
		msg := &req.Input[idx]
		content := schema.MessageText(*msg)
		if !schema.Rewritable(*msg) || content == "" {
			continue
		}
		// The same rule as the ping's candidate scan (carriesRealMarker): text that only QUOTES marker
		// syntax was offered and judged, so its drop must apply. A real marker is skipped and counted.
		if carriesRealMarker(c, content) {
			rep.Gate(gateApplyAlreadyMarked)
			continue
		}
		before := *msg
		key, ok := applySweepDrop(c, rep, e.mode, msg, content)
		if !ok {
			rep.Event(eventApplyDeclined)
			ledgerNote(tkey, func(q *SweepQuality) { q.ApplyDeclined++ })
			continue
		}
		saved := schema.TextTokens(content) - schema.TextTokens(schema.MessageText(*msg))
		pending = append(pending, pendingDrop{idx: idx, before: before, rec: sweepDropRec{
			Thread: tkey, ToolUseID: o.ToolUseID, CK: extract.ContentKey(content), Marker: key,
			Tokens: saved, AtMs: time.Now().UnixMilli(), Kind: kind,
			Idents: res.identsOf(o.ToolUseID), Seen: len(raw),
		}})
		changed++
		if key != "" {
			keys = append(keys, key)
		}
	}

	// Committed only now, after every drop of this request is in req: a record exists only for a drop
	// the request carries. Each write is tracked in `committed` so a panic part-way undoes all of them.
	cks := make([]string, 0, len(pending))
	func() {
		// Deferred unlock: a panic in a Store write must release dropMu before the recover below takes
		// it again to tombstone what was written.
		dropMu.Lock()
		defer dropMu.Unlock()
		for _, p := range pending {
			committed = append(committed, p.rec)
			putDrop(c.Store, p.rec)
			if p.rec.Marker != "" {
				addMarkerOwner(c.Store, p.rec.Marker, tkey, p.rec.CK)
			}
			cks = append(cks, p.rec.CK)
		}
	}()
	addToDropIndex(c.Store, tkey, cks)
	for reason, n := range res.reasonCounts() {
		rep.EventN("sweep_apply_dropped_"+trimReason(reason), n)
	}
	slog.Info("cg.sweep.applied", "kind", kind, "session", c.Session, "thread", c.Thread,
		"proposed_valid", len(res.Valid), "applied", len(pending), "held_back", len(later),
		"dropped_reasons", res.reasonCounts())

	// The held answer: spent on a cold request, trimmed to the drops before the change point
	// otherwise. What is kept is judged against the intact prefix only.
	if !(cold || len(later) == 0) {
		h.Outputs = later
		h.Judged = cp
		if cp < len(h.Prints) {
			h.Prints = h.Prints[:cp]
		}
		src.put(h)
	}
	// Counted LAST, once the commit and the held trim both succeeded: the ledger and the events describe
	// only drops the request carries.
	if n := len(pending); n > 0 {
		ev := eventAppliedCold
		switch {
		case warmGated:
			ev = eventAppliedWarm
		case !cold:
			ev = eventAppliedChangedPrefix
		}
		rep.EventN(ev, n)
		if warmGated {
			noteWarmSweep(tkey, WarmSweep{Drops: n, RemovedLocal: removedNow(pending), RemovedBilled: warm.removedBilled,
				RewriteBilled: warm.rewriteBilled, ConvBefore: convBefore, ConvAfter: localTokens(req.Input),
				ExpectedTurns: warm.turns, NetUSD: warm.netUSD, Deep: warm.deep})
		}
		ledgerNote(tkey, func(q *SweepQuality) {
			q.Applied += int64(n)
			switch {
			case warmGated:
				q.AppliedWarm += int64(n)
			case cold:
				q.AppliedCold += int64(n)
			default:
				q.AppliedChangedPrefix += int64(n)
			}
		})
	}
	_, _, write := agentRates(c)
	for _, p := range pending {
		if p.rec.Tokens > 0 {
			metrics.RecordExtractionSaving(rep.Component, p.rec.Tokens)
			metrics.RecordExtractionValue(rep.Component, float64(p.rec.Tokens)*write)
		}
	}
	pending, committed = nil, nil // committed: nothing to restore
	return keys, changed
}

// identsOf returns the identifier hashes recorded for a tool id by the recheck's index pass.
func (r recheckResult) identsOf(toolID string) []string {
	ids := r.novel[toolID]
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, t := range ids {
		out = append(out, identHash(t))
	}
	return out
}
