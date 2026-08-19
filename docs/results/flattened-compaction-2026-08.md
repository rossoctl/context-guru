# The 734-request "flattened transcript" class: measured unreachable

A sibling analysis of the `below_trigger` population named a promising target: **734
`claude-cli` requests averaging ~120,000 tokens with one message and zero declared
tools**, described as Claude Code's own conversation-summarisation call — a whole
transcript flattened into a single user text block, with no `tool_result` for any
component to reduce.

The pitch was that a one-shot summarisation call has nothing to re-anchor, making it the
rare safe place to rewrite deeply.

**Both halves of that pitch are wrong, and the DB says so.** This page is the
measurement. No reducer was built. Nothing was enabled.

Method: read-only snapshot of `/var/lib/context-guru/cg.db` (`sudo cp` of the file plus
its WAL to `/tmp`, opened `mode=ro`; production was not modified, queried in place, or
restarted). Snapshot 2026-08-19 22:03 UTC — 14,411 requests spanning 2026-08-17 11:48 to
2026-08-19 21:11. Replay corpora `/tmp/cg-runs/capture-swebench.jsonl` and
`capture-tb.jsonl` were used only to bound the achievable reduction, not to size the class.

## 1. The class, isolated

`uncompressed_reason='below_trigger' AND agent='claude-cli' AND messages=1 AND tools=0`
is 985 requests / 94,831,530 tokens on this snapshot. Within it, one shape dominates:

| filter | n | tokens | cost |
|---|---|---|---|
| the whole one-message class | 985 | 94,831,530 | $47.76 |
| … of which `max_tokens=64` | 747 | 94,657,149 | $47.12 |
| … of which `cache_miss_reason='hit'` | **734** | avg 127,287 | **$45.65** |

The sibling's 734 is not an independent count. It is **exactly the number of requests in
this shape that the provider served from cache.** The class was defined by its own cache
hits.

3 tenants, 14 sessions, 2.5 days.

## 2. It is not a summarisation call

| field | value |
|---|---|
| `max_tokens` | 64 (735 of 747) |
| `stop_reason` | `stop_sequence` (735), `end_turn` (3) |
| `output_tokens` | min 0, max **9**, mean 7.1 |

A 120,000-token prompt answered in **nine tokens** against a stop sequence is a
classifier probe, not a compaction. Claude Code's actual `/compact` summarisation returns
thousands of tokens; those requests live in the `cache_frozen` and `''` populations, not
here. The label "summarisation call" attributed a rewrite opportunity to a request that
produces a one-word answer.

## 3. Cache tier: 95.6% `cache_read`

Distribution over the 985-request class, by each request's dominant tier — not an average:

| dominant tier | requests | cost |
|---|---|---|
| `cache_read` | 710 | $32.86 |
| no usage reported (`token_accounting=partial`) | 187 | $0.00 |
| fresh | 56 | $0.62 |
| `cache_write` | 32 | $14.28 |

Billed input tokens, summed:

| tier | tokens | share |
|---|---|---|
| `cache_read` | 171,279,143 | **95.6%** |
| `cache_write` | 7,535,186 | 4.2% |
| fresh | 277,731 | 0.2% |

`cache_miss_reason` agrees: 734 `hit`, 85 `cold_start`, 25 `ttl_expiry`, 4
`prefix_change`, 133 `unknown`.

This is the tier that decides the question. The ~1,000:1 asymmetry established for the
"unfrozen compaction reset" class applies here unchanged.

## 4. The bodies are re-sent, not one-off

This was the crux, and it resolves against acting. A cache hit on a 127,000-token prompt
is itself proof that the same prefix was sent before. Tracing one session in arrival
order:

| tokens_before | cache_read | cache_write | fresh |
|---|---|---|---|
| 109,377 | 37,664 | 171,137 | 90 |
| 110,035 | 208,801 | 996 | 90 |
| 111,231 | 37,664 | 173,930 | 90 |
| 111,565 | 211,594 | 523 | 90 |
| 113,544 | 37,664 | 177,392 | 90 |
| 115,277 | 215,056 | 2,548 | 90 |
| 116,126 | 217,604 | 1,323 | 90 |
| 116,911 | 218,927 | 1,196 | 90 |

Monotonically growing body, constant 90 fresh tokens, and each turn re-reading the
previous turn's prefix from cache while writing only the delta. That is the textbook
append-only prompt with a stable, near-perfectly cached prefix — the single worst place in
the corpus to rewrite bytes. `frozen_tokens` is 0 on these rows, which is the trap named
elsewhere in these results: `frozen=0` means *our* `MaxCachedIdx` was reset, not that the
provider's cache is cold. Here the provider's cache is emphatically warm.

## 5. The price of a prefix break, measured not modelled

Four requests in this class carry `cache_miss_reason='prefix_change'`. They are the
natural experiment:

| id | tokens_before | cache_read | cache_write | cost |
|---|---|---|---|---|
| 1547 | 188 | 0 | 37,986 | $0.0954 |
| 2774 | 4,415 | 0 | 44,413 | $0.1114 |
| 3207 | 6,354 | 0 | 47,488 | $0.1191 |
| 8722 | 350,149 | 0 | 603,083 | **$1.1462** |

Mean cost of a request in this class that hits: **$0.0622**. Cost of the one that broke
its prefix at scale: **$1.15 — 18x** — and it recovered zero cached tokens.

## 6. The arithmetic

At sonnet-5 rates ($3.00 / $0.30 / $3.75 per Mtok fresh / read / write), over the entire
production history of this class:

| quantity | value |
|---|---|
| total actual cost of all 985 requests, all time | **$47.76** |
| value of removing 1% of the `cache_read` mass | **+$0.51** |
| value of removing 10% of *all* input at the billed tier | +$8.05 |
| cost of one full prefix break (`cache_read` → `cache_write`) | **−$590.91** |
| ratio, 1% win vs one break | **1,150:1 against** |

A perfect, free, 100% reduction of this class is worth $47.76 of lifetime spend. One
mis-timed prefix break costs twelve times that.

## 7. And the reduction would be zero anyway

Two independent measurements say the two lossless text transforms available
(`components/reformat/textclean.go`, `components/reformat/searchfold.go`) remove nothing
from this shape:

**No bodies exist to reduce.** `request_content` holds 54,710 rows — and **zero** for any
`claude-cli` request with `messages=1 AND tools=0`, and zero for any `max_tokens=64`
`claude-cli` request. Content capture is scoped to the paths components mutate
(`messages.N.content.M.content` — tool results). The class is invisible at content level,
so its achievable reduction cannot be measured on the population at all.

**Where the shape does appear, it is already clean.** `capture-swebench.jsonl` holds 24
one-message zero-tool requests, 526,278 characters, 16 of them over 10,000 characters.
Across all of them:

| transform | opportunities found |
|---|---|
| ANSI/VT100 escape sequences (`textclean`) | **0** |
| carriage-return redraws (`textclean`) | **0** |
| repeated `path:line:` prefixes (`searchfold`) | **0** |

These 24 carry `max_tokens=64000` — they are first-turn task prompts, a different shape
from the production `max_tokens=64` probe, so this bounds rather than settles. But the
direction is consistent: a prompt Claude Code assembles itself from its own transcript
never contained a terminal escape or a redraw, because the agent's transcript stores tool
output already decoded. `textclean`'s corpus-wide gain of −16,721 tokens came entirely
from live `tool_result` blocks, which this class does not have.

## 8. Recommendation: do nothing

Ship no component for this class.

* It bills at `cache_read` for 95.6% of its tokens, so the 1,000:1 asymmetry holds.
* Its bodies are re-sent every turn with a growing, cache-hot prefix — the opposite of
  the one-shot call the opportunity was premised on. There is a great deal to re-anchor.
* Its entire lifetime cost is $47.76; a single prefix break costs $590.91.
* The measured available reduction from the two lossless transforms is 0 tokens.
* It is a **user** message. Every deployed component touches `tool` role content only.
  Rewriting the model's actual instruction to chase $0.51 is a risk with no payoff on the
  other side of it.

The honest statement of what this class is: not a missed reduction, but Claude Code
paying the correct, already-cache-optimised price for re-sending a growing prompt. The
$47.76 is not waste; it is 171M tokens billed at one tenth of fresh precisely because the
prefix was never disturbed. Our contribution to it is to keep not disturbing it.

### Reproducing

```bash
sudo cp /var/lib/context-guru/cg.db /tmp/cgsum-ro.db
sudo cp /var/lib/context-guru/cg.db-wal /tmp/cgsum-ro.db-wal
sudo chown "$USER" /tmp/cgsum-ro.db /tmp/cgsum-ro.db-wal
python3 - <<'PY'
import sqlite3
c = sqlite3.connect('file:/tmp/cgsum-ro.db?mode=ro', uri=True)
W = ("uncompressed_reason='below_trigger' AND agent='claude-cli' "
     "AND messages=1 AND tools=0")
print(list(c.execute(f"""SELECT CASE
    WHEN fresh_input+cache_read+cache_write=0 THEN 'no_usage'
    WHEN cache_read>=fresh_input AND cache_read>=cache_write THEN 'cache_read'
    WHEN cache_write>=fresh_input THEN 'cache_write' ELSE 'fresh' END tier,
  COUNT(*), SUM(cost_usd) FROM requests WHERE {W} GROUP BY 1 ORDER BY 2 DESC""")))
print(list(c.execute(f"SELECT cache_miss_reason,COUNT(*) FROM requests WHERE {W} GROUP BY 1")))
PY
```
