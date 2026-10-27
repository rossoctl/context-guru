package offload

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/schema"
)

// BATCHING (#132). One question used to keep the 12 largest candidates and drop the rest
// (sweep_over_ask_cap), so a long session was only partly swept: the outputs left out could never be
// offered, however old they were.
//
// The candidates are now split into k = ceil(n / batch) BALANCED batches (16 gives 8+8, 23 gives 12+11),
// and the k questions are asked IN SEQUENCE over the same cached prefix, under the one question slot and
// the one thread claim the caller already holds. Concurrency does not change. Each answer is judged
// exactly as the single question was, including prefixAskCacheOK on its own. A batch that fails (an error,
// a refused cache read, a truncated or unparseable reply) fails OPEN: its outputs are left out of the held
// answer, so the next question may offer them again, and the other batches still count. The verdicts of
// the batches that answered are merged into ONE held answer, keyed by tool_use id, so the next cold
// request does one recheck and one cut.
//
// A batch of 12 or fewer is the old single question, byte for byte: n <= 12 makes one ask.
//
// Why 12 is the batch size: one reply carries a verdict (with a verbatim quote) for EVERY candidate in the
// question, so the reply grows with the count and a reply cut off mid-array parses as nothing. Live
// measurement put a verdict at about 600 tokens with long quotes, so a batch of 12 sits well inside the
// flat 16,000-token reply budget. A batch that is still cut short loses its batch and not the sweep.

const (
	// defaultMaxAskBatches is how many batches one sweep asks unless max_ask_batches says otherwise.
	defaultMaxAskBatches = 4
	// maxAskBatchesCap is the largest max_ask_batches accepted. A larger value reads as this.
	maxAskBatchesCap = 8
)

// askBatch is one question's candidates. idx are positions in the full candidate list.
type askBatch struct {
	cands []sweepCand
	idx   []int
}

// splitAskBatches deals cands, largest first, round-robin into k = ceil(n / batch) batches, so the
// batches are balanced in count and each mixes big and small outputs. n <= batch gives one batch holding
// the candidates in their original order.
func splitAskBatches(cands []sweepCand, batch int) []askBatch {
	n := len(cands)
	if n == 0 {
		return nil
	}
	if batch < 1 {
		batch = maxAskItems
	}
	if n <= batch {
		one := askBatch{cands: append([]sweepCand(nil), cands...), idx: make([]int, n)}
		for i := range one.idx {
			one.idx[i] = i
		}
		return []askBatch{one}
	}
	k := (n + batch - 1) / batch
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return schema.TextTokens(cands[order[a]].content) > schema.TextTokens(cands[order[b]].content)
	})
	out := make([]askBatch, k)
	for pos, ci := range order {
		b := &out[pos%k]
		b.cands = append(b.cands, cands[ci])
		b.idx = append(b.idx, ci)
	}
	return out
}

// askBatchesLimit is how many batches one sweep asks: max_ask_batches, default 4, at most 8.
func (e *ExtractSweep) askBatchesLimit() int {
	if e.maxAskBatches < 1 {
		return defaultMaxAskBatches
	}
	if e.maxAskBatches > maxAskBatchesCap {
		return maxAskBatchesCap
	}
	return e.maxAskBatches
}

// askCandidateLimit is how many candidates one sweep may ask about in all: the batch size times the
// batches. The smallest candidates beyond it are counted as sweep_over_ask_cap.
func (e *ExtractSweep) askCandidateLimit() int { return maxAskItems * e.askBatchesLimit() }

// plannedAskBatches is how many questions a sweep of n offered candidates will ask: ceil(n / 12). The
// keeper prices that many answers against its per-ping cost ceiling.
func plannedAskBatches(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + maxAskItems - 1) / maxAskItems
}

// batchRun is the outcome of every question of one sweep, merged.
type batchRun struct {
	// judged are the candidates of the batches that ANSWERED, in batch order; drop indexes into judged.
	judged []sweepCand
	drop   []int
	// gates and events are the questions' own gate and event names, summed; gateList is the raw list.
	gates, events map[string]int
	gateList      []string
	// Usage and cost summed over the questions that reached the provider.
	prompt, completion, cacheRead, cacheWrite int64
	costUSD, ms                               float64
	// answered and failed count the questions by outcome; skipped counts batches never started because
	// the sweep's context ended first. panicked counts batches that PANICKED: a panic can come after the
	// model call returned and was paid for, and the recover erases that batch's usage from the result, so
	// the caller must not read zero usage as "nothing was sent". The sink still holds what was billed.
	answered, failed, skipped, panicked int
}

// askInBatches asks the candidates in balanced batches, in sequence, and merges the answers. It is THE
// batching function: the ping's ask calls it today, and #720's warm path will. It holds no slot or claim
// of its own: the caller already holds the thread's claim and a question slot, so a sweep of k questions is
// still one question's worth of concurrency.
//
// IT ENFORCES THE OPERATOR'S BOUND ITSELF. It asks at most max_ask_batches batches (and at most
// maxBatches when the caller passes a lower limit, as the keeper does when its cost ceiling admits fewer
// answers); a caller that did not pre-cap cannot get around it. The candidates beyond that (the smallest)
// are not asked and are counted as sweep_over_ask_cap.
//
// A batch that fails keeps its items out of the merged answer (they were never judged) and is counted by
// reason. A PANIC in one batch is one failed batch (reason "batch_panic"): the answers already paid for are kept.
// A batch never STARTS after the context ended, so k slow batches cannot run past the keeper's outer
// timeout. No item is judged twice: each candidate is in exactly one batch.
func (e *ExtractSweep) askInBatches(req *bschemas.BifrostChatRequest, c *components.Ctx, cands []sweepCand,
	opt askOpts, maxBatches int) batchRun {

	br := batchRun{gates: map[string]int{}, events: map[string]int{}}
	limit := e.askBatchesLimit()
	if maxBatches > 0 && maxBatches < limit {
		limit = maxBatches
	}
	if over := len(cands) - maxAskItems*limit; over > 0 {
		// The smallest are the ones left out: ask only about the largest the limit allows.
		cands = largestFirst(cands)[:maxAskItems*limit]
		br.gates["sweep_over_ask_cap"] += over
		sweepOverAskCapBatch.Add(int64(over))
	}
	batches := splitAskBatches(cands, maxAskItems)
	sweepAskBatches.Add(int64(len(batches)))
	br.events["sweep_ask_batches"] += len(batches)
	for bi, b := range batches {
		// A batch never starts once the sweep's deadline has passed.
		if c.Ctx != nil && c.Ctx.Err() != nil {
			br.skipped += len(batches) - bi
			sweepBatchSkipped.Add(int64(len(batches) - bi))
			br.events["sweep_batch_skipped_deadline"] += len(batches) - bi
			break
		}
		drop, r, ms := e.askOneBatch(req, c, b, opt)
		br.ms += ms
		br.prompt += r.rec.PromptTokens
		br.completion += r.rec.CompletionTokens
		br.cacheRead += r.rec.CacheRead
		br.cacheWrite += r.rec.CacheWrite
		br.costUSD += r.rec.CostUSD
		countNames(br.gates, r.gates)
		countNames(br.events, r.events)
		br.gateList = append(br.gateList, r.gates...)
		if !r.answered {
			br.failed++
			if r.raised("sweep_batch_panic") {
				br.panicked++
			}
			reason := batchFailReason(r)
			noteBatchFailed(reason)
			br.events["sweep_batch_failed_"+trimReason(reason)]++
			logging.From(c.Ctx).Debug("cg.sweep.batch_failed", "batch", bi, "of", len(batches), "reason", reason)
			continue
		}
		br.answered++
		noteBatchJudged(bi, len(b.cands), r.judgedCount, r.fabricatedCount)
		base := len(br.judged)
		br.judged = append(br.judged, b.cands...)
		for _, k := range drop {
			br.drop = append(br.drop, base+k)
		}
	}
	return br
}

// askOneBatch asks ONE batch, with its own recover: a panic here is a failed batch (reason "batch_panic")
// and does not lose the batches before it. The recover erases this batch's result, so its usage is not in
// the sums; the caller counts the panic (batchRun.panicked) and keeps the thread marked asked, and the
// cost is still booked from the sink, which saw the call.
func (e *ExtractSweep) askOneBatch(req *bschemas.BifrostChatRequest, c *components.Ctx, b askBatch,
	opt askOpts) (drop []int, r sweepResult, ms float64) {

	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			sweepPingPanics.Add(1)
			slog.Error("cg.sweep.batch_panic: recovered; this batch fails open, the others still count",
				"session", c.Session, "thread", c.Thread, "panic", fmt.Sprint(p), "gate", "sweep_batch_panic")
			drop, r = nil, sweepResult{}
			r.gate("sweep_batch_panic")
		}
		ms = float64(time.Since(start).Microseconds()) / 1000.0
	}()
	rep := &components.Report{Component: "extract_llm_sweep", Kind: "offload"}
	drop, r = e.adjudicateWith(req, c, rep, b.cands, opt)
	return drop, r, ms
}

// largestFirst returns cands sorted by size, largest first, without changing the input.
func largestFirst(cands []sweepCand) []sweepCand {
	out := append([]sweepCand(nil), cands...)
	sort.SliceStable(out, func(a, b int) bool {
		return schema.TextTokens(out[a].content) > schema.TextTokens(out[b].content)
	})
	return out
}

// batchFailReason names why a batch did not answer, from the gates its question raised. The order is the
// order a question can fail in.
func batchFailReason(r sweepResult) string {
	for _, g := range []string{"sweep_batch_panic", "sweep_reply_truncated", "sweep_unparseable", "sweep_ask_failed",
		"sweep_prefix_cache_read_ZERO", "sweep_prefix_cache_read_below_turn", "sweep_prefix_cache_write",
		"sweep_stale_prefix", "sweep_no_prefix", "sweep_no_asker"} {
		if r.raised(g) {
			return strings.TrimPrefix(g, "sweep_")
		}
	}
	return "no_answer"
}

var (
	sweepAskBatches      atomic.Int64
	sweepBatchSkipped    atomic.Int64
	sweepOverAskCapBatch atomic.Int64
	sweepBatchFailedMu   sync.Mutex
	sweepBatchFailedBy   = map[string]int64{}
	sweepBatchFailedAll  atomic.Int64
)

// maxBatchIndexStats bounds the per-index tally: max_ask_batches is at most 8.
const maxBatchIndexStats = 8

// SweepBatchIndexStats is the tally for one batch position.
type SweepBatchIndexStats struct {
	Asked, Judged, Fabricated int64
	// Items is the candidates the batches at this position carried, so Judged/Items shows how many verdicts
	// came back, and a batch of 5 is not read like a batch of 12.
	Items int64
}

var (
	sweepBatchIdxMu sync.Mutex
	sweepBatchIdx   [maxBatchIndexStats]SweepBatchIndexStats
)

// noteBatchFabricated books one batch's judging: how many candidates it carried, how many verdicts were
// judged and how many carried a quote that is not in the transcript (the sweep_quote_fabricated count).
func noteBatchJudged(index, items, judged, fabricated int) {
	if index < 0 {
		return
	}
	if index >= maxBatchIndexStats {
		index = maxBatchIndexStats - 1
	}
	sweepBatchIdxMu.Lock()
	s := &sweepBatchIdx[index]
	s.Asked++
	s.Items += int64(items)
	s.Judged += int64(judged)
	s.Fabricated += int64(fabricated)
	sweepBatchIdxMu.Unlock()
}

func noteBatchFailed(reason string) {
	sweepBatchFailedAll.Add(1)
	sweepBatchFailedMu.Lock()
	sweepBatchFailedBy[reason]++
	sweepBatchFailedMu.Unlock()
}

// SweepBatchStats is the batching ledger for /stats: how many questions were asked in all, how many
// batches failed, and why.
type SweepBatchStats struct {
	// ByIndex tells the batches apart by their position in a sweep: how many were asked, how many verdicts
	// they judged, and how many of those carried a fabricated quote. A fabrication rate that differs between
	// the first batch and the later ones is a batch effect; one that is the same everywhere is the model.
	// ByIndex[0] is the first batch of every sweep. Capped at maxBatchIndexStats entries.
	ByIndex         []SweepBatchIndexStats
	Batches, Failed int64
	// Skipped counts batches never started because the sweep's deadline passed. OverCap counts candidates
	// left unasked because the limit (max_ask_batches, or the keeper's cost ceiling) allowed fewer batches.
	Skipped, OverCap int64
	FailedBy         map[string]int64
}

// SweepBatchCounters snapshots the batching ledger.
func SweepBatchCounters() SweepBatchStats {
	sweepBatchFailedMu.Lock()
	defer sweepBatchFailedMu.Unlock()
	by := make(map[string]int64, len(sweepBatchFailedBy))
	for k, v := range sweepBatchFailedBy {
		by[k] = v
	}
	sweepBatchIdxMu.Lock()
	var idx []SweepBatchIndexStats
	last := -1
	for i := range sweepBatchIdx {
		if sweepBatchIdx[i].Asked > 0 {
			last = i
		}
	}
	if last >= 0 {
		idx = append(idx, sweepBatchIdx[:last+1]...)
	}
	sweepBatchIdxMu.Unlock()
	return SweepBatchStats{ByIndex: idx, Batches: sweepAskBatches.Load(), Failed: sweepBatchFailedAll.Load(),
		Skipped: sweepBatchSkipped.Load(), OverCap: sweepOverAskCapBatch.Load(), FailedBy: by}
}

func resetSweepBatchForTest() {
	sweepBatchIdxMu.Lock()
	sweepBatchIdx = [maxBatchIndexStats]SweepBatchIndexStats{}
	sweepBatchIdxMu.Unlock()
	sweepAskBatches.Store(0)
	sweepBatchSkipped.Store(0)
	sweepOverAskCapBatch.Store(0)
	sweepBatchFailedAll.Store(0)
	sweepBatchFailedMu.Lock()
	sweepBatchFailedBy = map[string]int64{}
	sweepBatchFailedMu.Unlock()
}
