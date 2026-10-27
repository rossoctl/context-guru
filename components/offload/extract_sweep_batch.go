package offload

import (
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
	// answered and failed count the questions by outcome; batches is how many were asked.
	answered, failed, batches int
}

// askInBatches asks the candidates in balanced batches, in sequence, and merges the answers. It is THE
// batching function: the cold sweep calls it, and any other path that asks a list of outputs should
// (#720's warm path will). It holds no slot or claim of its own: the caller already holds the thread's
// claim and a question slot, so a sweep of k questions is still one question's worth of concurrency.
//
// A batch that fails keeps its items out of the merged answer (they were never judged) and is counted by
// reason. No item is judged twice: each candidate is in exactly one batch.
func (e *ExtractSweep) askInBatches(req *bschemas.BifrostChatRequest, c *components.Ctx, cands []sweepCand, opt askOpts) batchRun {
	br := batchRun{gates: map[string]int{}, events: map[string]int{}}
	batches := splitAskBatches(cands, maxAskItems)
	br.batches = len(batches)
	sweepAskBatches.Add(int64(len(batches)))
	br.events["sweep_ask_batches"] += len(batches)
	for bi, b := range batches {
		start := time.Now()
		rep := &components.Report{Component: "extract_llm_sweep", Kind: "offload"}
		drop, r := e.adjudicateWith(req, c, rep, b.cands, opt)
		br.ms += float64(time.Since(start).Microseconds()) / 1000.0
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
			reason := batchFailReason(r)
			noteBatchFailed(reason)
			br.events["sweep_batch_failed_"+trimReason(reason)]++
			logging.From(c.Ctx).Debug("cg.sweep.batch_failed", "batch", bi, "of", len(batches), "reason", reason)
			continue
		}
		br.answered++
		base := len(br.judged)
		br.judged = append(br.judged, b.cands...)
		for _, k := range drop {
			br.drop = append(br.drop, base+k)
		}
	}
	return br
}

// batchFailReason names why a batch did not answer, from the gates its question raised. The order is the
// order a question can fail in.
func batchFailReason(r sweepResult) string {
	for _, g := range []string{"sweep_reply_truncated", "sweep_unparseable", "sweep_ask_failed",
		"sweep_prefix_cache_read_ZERO", "sweep_prefix_cache_read_below_turn", "sweep_prefix_cache_write",
		"sweep_stale_prefix", "sweep_no_prefix", "sweep_no_asker"} {
		if r.raised(g) {
			return strings.TrimPrefix(g, "sweep_")
		}
	}
	return "no_answer"
}

var (
	sweepAskBatches     atomic.Int64
	sweepBatchFailedMu  sync.Mutex
	sweepBatchFailedBy  = map[string]int64{}
	sweepBatchFailedAll atomic.Int64
)

func noteBatchFailed(reason string) {
	sweepBatchFailedAll.Add(1)
	sweepBatchFailedMu.Lock()
	sweepBatchFailedBy[reason]++
	sweepBatchFailedMu.Unlock()
}

// SweepBatchStats is the batching ledger for /stats: how many questions were asked in all, how many
// batches failed, and why.
type SweepBatchStats struct {
	Batches, Failed int64
	FailedBy        map[string]int64
}

// SweepBatchCounters snapshots the batching ledger.
func SweepBatchCounters() SweepBatchStats {
	sweepBatchFailedMu.Lock()
	defer sweepBatchFailedMu.Unlock()
	by := make(map[string]int64, len(sweepBatchFailedBy))
	for k, v := range sweepBatchFailedBy {
		by[k] = v
	}
	return SweepBatchStats{Batches: sweepAskBatches.Load(), Failed: sweepBatchFailedAll.Load(), FailedBy: by}
}

func resetSweepBatchForTest() {
	sweepAskBatches.Store(0)
	sweepBatchFailedAll.Store(0)
	sweepBatchFailedMu.Lock()
	sweepBatchFailedBy = map[string]int64{}
	sweepBatchFailedMu.Unlock()
}
