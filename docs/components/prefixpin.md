# prefixpin

!!! warning "Offload — lossy, reversible"
    Re-sends the **first** rendering of an early message that the agent rewrites in
    place every turn, so the cached prefix stays byte-identical. The original is
    stashed, so [`expand`](../how-to/recover-context.md) can recover it.

## The problem it solves

[`cacheinject`](cacheinject.md) optimises *where* the cache boundary goes. prefixpin
fixes the case where **no boundary can help**: an agent that mutates an early message
on every turn.

Providers hash the request prefix cumulatively. A single changed character at message
index 1 makes every token above it unmatchable — there is no breakpoint position whose
hash excludes an earlier block.

Measured across 1,955 real Bob requests on SWE-bench:

| | requests | uncached input |
|---|--:|--:|
| append-only turns (cached fine) | 98.0% | 28.2% |
| **one early message mutated** | **2.0%** | **71.8%** (5,796,220 tokens) |

On one task the agent re-emitted a running `<scratchpad>`/`<state_snapshot>` at index
1, re-rendering an iteration counter in ~20 places (`"THIRTY-SECOND"` →
`"THIRTY-THIRD"`, `"32"` → `"33"`): **152 changed characters out of 6,024 — 98.5%
identical content** — sitting ~1,374 tokens into a 181k-token prefix. Only 0.76% of the
prefix survived; the cache-hit rate collapsed from 98% to 5.7%.

The economics are lopsided. A cache read costs 0.1× base input and uncached input
costs 1.0×, so a mutation below the boundary makes every token above it **ten times**
more expensive. Placement tuning only ever moves tokens between read (0.1×) and
cache-write (1.25×) — which is why this is worth ~31% of Bob's input cost while every
placement change measured ~0%.

## How it works

For each early message, remember the first text seen for its `(index, role)` slot in
this session. On a later turn, if that slot's text **changed** but is still
recognisably the same content (same shape, high similarity), re-send the **first**
rendering so the prefix stays byte-identical.

## Lossiness

Deliberately an Offload, not a Reformat. The model sees the pinned (older) text rather
than what the agent just wrote, so information **is** withheld: a counter reads stale.
That is a real behavioural change, and the reason for the guards.

## Guards

Each closes a way this could do harm:

- only messages at index `< max_pin_index` — an early, structural slot, never the
  working tail the agent is actively reasoning about;
- only when similarity `>= min_similarity` — a genuinely rewritten-in-place block, not
  a different message that happens to occupy the slot;
- only after the slot has churned `repeat_threshold` times, so a one-off edit is never
  pinned — only a per-turn churn *pattern*;
- never on the newest message, never on tool results.

## Configuration

```yaml
pipeline: [prefixpin, cacheinject]
components:
  prefixpin:
    max_pin_index: 4        # 0 disables
    min_similarity: 0.80
    repeat_threshold: 2
    min_tokens: 200
```

| key | default | meaning |
|---|--:|---|
| `max_pin_index` | `4` | bounds pinning to structurally-early messages; `0` disables |
| `min_similarity` | `0.80` | character-level overlap required to treat a changed slot as the same content rewritten in place |
| `repeat_threshold` | `2` | how many times a slot must churn before pinning starts |
| `min_tokens` | `200` | skips slots too small to be worth the behavioural risk |

## When to enable it

Enabled for every provider: the failure mode is provider-independent. It bites hardest
on **implicit-cache backends** (Gemini/Bob, OpenAI) where there is no `cache_control`
to place and prefix stability is the *only* available lever.
