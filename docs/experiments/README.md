# Experiment log

Chronological, per-run record of every measurement, in the `iterNNN` notation used by the
`forever` project so the two are readable side by side. One directory per run, never edited
after the fact except to add a retraction banner.

**These are the runs. The [results](../RESULTS.md) pages are the *arguments*** — they synthesise
across runs, get rewritten as understanding changes, and are where a reader should start. An
iteration page is the opposite: what was executed, what came back, what it does and does not
prove. If the two ever disagree, the iteration page is the record of fact.

Each page carries the same sections: **Result** (a table), **What this proves**, **What this does
NOT prove** (the caveats, in full), **Next levers**, **Artifacts** (paths, so a number can be
traced back to the bytes that produced it).

## Index

| Iteration | Date | What | Headline | Status |
|---|---|---|---|---|
| [captures/iter001](captures/iter001/results.md) | 2026-08-20 | Component gating on `capture-swebench`, tail vs non-tail | The tail gate costs `mask` 93% of its effect and `failed_run` all of it; `extract_llm` cannot fire on SWE-bench at all | ✅ |
| [loca/iter001](loca/iter001/results.md) | 2026-08-20 | First LOCA replay, one request per conversation | `mask` 52.3% vs `coref` 6.5% | ⚠️ **retracted** — cold-start artifact |
| [loca/iter002](loca/iter002/results.md) | 2026-08-20 | Sequential LOCA replay, 197 turns, 5 arms, summarizer at a context max | **Deferral works: 72% fewer summarizations**, delivered by `coref` | ✅ |

## Conventions

- **Retractions stay.** A wrong run is deleted from the argument, never from the log — the
  retraction and its cause are usually more instructive than the number was.
- **Cost is always stated**, even when it is $0, because "free" is a property worth knowing.
- **Every arm names its binary.** A stale binary silently produced a null result in
  `loca/iter002`; recording the build is the cheapest guard against repeating that.
