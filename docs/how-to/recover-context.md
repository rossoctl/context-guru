# Reversibility & recovery

Every lossy [Offload](../components.md#offload-lossy-reversible) is reversible: the
component replaces the content with a `<<cg:HASH>>` marker and stashes the original in the
[store](../design.md#state-the-store) under that hash. An offload that drops content but
returns no key is treated as a failure and reverted, so nothing is ever silently lost.

The marker is the recovery handle:

```text
[older tool output masked] <<cg:b162e82de872a202>> [full output: call context_guru_expand]
```

## Three ways to recover

**1. The model asks for it.** The host injects a `context_guru_expand(id)` tool into the
request. When the model needs what is behind a marker, it calls the tool with the hash.

**2. The host resolves it and continues.** The host looks the hash up, appends the original
as a tool result, and re-invokes upstream so the model finishes with the full content in
hand — all before the response reaches your agent.

!!! info "Recovered content is APPENDED, never put back where the marker was"
    The marker stays exactly where the offloader wrote it, hash id and all, and the original
    arrives at the **end** of the conversation as a `tool_result`. That is a prompt-cache
    property before it is anything else: `messages` is hashed in order, so rewriting a message
    in the middle of a transcript discards every cached byte from that point on, while
    appending two turns at the end leaves the whole prefix intact. Splicing the original back
    over its marker would read as a tidier transcript and cost the entire prefix on every
    expand call.

    Pinned by `TestContinuationAppendsAndNeverRewritesAnEarlierMessage`, which asserts that no
    pre-existing message changes by a single byte, that the marker survives, and that the
    recovered text appears **only** in the last message.

```mermaid
sequenceDiagram
  participant M as Model
  participant Host
  participant Store
  participant Up as Upstream
  Host->>Up: request (content replaced by <<cg:HASH>> + expand tool)
  Up-->>Host: response calls context_guru_expand(id=HASH)
  Host->>Store: Resolve(HASH)
  Store-->>Host: original bytes
  Host->>Up: append assistant tool-call + tool_result(original), re-invoke
  Up-->>M: final answer with full content in hand
```

**3. Out of band.** `GET /expand?id=<hash>` returns an offloaded original directly.

## What an expand costs, across turns

Everything above is about a single turn. Across turns an expand has two consequences that are
easy to miss, because the in-turn behaviour is deliberately cache-safe and the cross-turn
behaviour is not the same thing.

**An expand permanently un-compacts that content.** The handler marks the restored original
kept-verbatim (`cg:keep:`), and every offloader then skips it — otherwise re-compacting would
bounce the agent straight into another expand, which is the loop that mark exists to prevent. So
from the next turn onward the original goes upstream **in full, at its original position**.

**That costs one cache-write of the suffix.** Turn N sent compacted bytes at that position and turn
N+1 sends the full original, which is a change inside the provider's cached prefix — at ~11.5× a
cache read, the cost the [cache-tail gate](../reference/config.md) exists to avoid everywhere else.
It happens once per expanded content, on the turn after the expand, and it is counted:
`expand_prefix_flips` at [`/stats`](../reference/routes.md#get-stats) and
`cg_expand_prefix_flips_total` at `/metrics`.

That figure is **per turn per message**, not per distinct content — every later turn re-sends the
same original and the same abandonment is observed again, while only the first is a real
cache-write. Read it as "expansion is churning cached prefixes in this deployment", not as a count
of cache-writes. The per-component gate `kept_verbatim_after_expand` is the per-message view; its
sibling `already_marked` is the benign case (content some component had already compacted), and the
two were one label until they were split, which is why an older dashboard may show neither.

**The in-turn expansion does not persist as its own turn.** On the intercepted path the client never
sees the tool_use/tool_result pair — the proxy answers it and returns only the final assistant
response — so the next turn the client re-sends its own transcript without the expansion. What
persists is the *flag*, not the content. There is no in-place replacement of a marker by its
content, and `inject_expand` controls only whether the tool is **advertised**, never how a call is
answered.

**The repair path is the exception, and it is a fallback for a failure rather than a mode.** When
interception structurally cannot work — a client tool batched alongside expand, the round cap, a
stream that will not reconstruct, a non-Anthropic stream, or a bypassed turn carrying older markers
— the client did see the call and answered `No such tool available`. The proxy repairs that
tool_result on every later turn, so this is the only configuration in which the round-trip lives in
the client's transcript.

On that path the repaired tool_result carries a **pointer**, not a second copy: the content is
already in the transcript at its own position (kept-verbatim, above), so repeating it there sent the
same tool output upstream twice on every turn. Measured on a 200-line output: 252 bytes when the
content is compacted and no expand round-trip exists, against 21,511 bytes with one — two full
copies, permanently. If the original is *not* in the transcript (the agent's own compaction can drop
that message while keeping the round-trip) the tool_result carries the content, exactly as before,
because then it is the model's only copy.

## Recovery needs the store

The store *is* the reversibility mechanism. It defaults to in-memory TTL+LRU — 10000s
sliding TTL (refreshed on every read), 5000 entries, a 256 MiB rewind reserve, 100 sticky
sessions — and holds, per session:

- **Rewind** — `cache_key → original bytes`, what the expand loop resolves. These live in a
  **reserve** that is never evicted to admit a new payload: once a marker has been sent the
  promise is outstanding, so a full reserve makes the pipeline **decline the next removal**
  (counted as `stash_refused`) instead of quietly breaking an older one.
- **Sticky** — content ids already reduced on earlier turns, so output stays byte-stable
  across turns.
- **Frozen decisions** — the exact replacement bytes an offloader replays so an
  already-cached message stays byte-identical. These are pinned against eviction: losing one
  flips a message inside the provider's cached prefix and rewrites the whole suffix at
  ~11.5× the read price. The pin is capped at half `max_entries`, and pins plus rewind
  payloads together leave a quarter of `max_entries` always evictable, so neither exemption
  can starve the other or the plain cache.

Set `store.enabled: false` and offloads become **one-way** — nothing is stashed and nothing
can be recovered. The [llm-d compaction service](../examples/llm-d-service.md) does this
deliberately (with `marker_mode: off`) so `/compact` returns a clean, marker-free body.

<details markdown="1">
<summary>Troubleshooting</summary>

**The model called expand and got a placeholder back.** The original expired or was evicted
from the store. The provider requires one `tool_result` per `tool_call_id`, so an explicit
placeholder is sent rather than nothing — which turns that offload lossy. Check `stash_missing`
and `stash_expired` at [`/stats`](../reference/routes.md#get-stats): both mean a payload left the
store, so raise `store.ttl_seconds`. `stash_refused` is the *other* case and needs
`store.max_entries` or `store.stash_max_bytes` — there nothing became irreversible, because the
removals were declined.

The turn still **completes**: the placeholder continuation is sent even when *nothing*
resolved, so the model reads "no longer available" and finishes with text. It used to replay
the model's raw `tool_use` to the client in that case, which is a turn a client cannot answer
— and on an agent's own compaction request reads as a summary that came back empty. That is
also what lets the tool be advertised on every request in a session: the advertise condition
no longer has to avoid ever reaching this path, so it no longer has to read the turn, so the
`tools` array no longer changes shape and the prompt-cache prefix survives.

**Expansion did not happen at all.** The loop is capped at 3 rounds, and it bails if the
model calls another tool alongside the expand call — the response is returned as-is. Check
`bounces` in `/stats` to confirm whether restoration fired.

**Restoration does not fire on my streaming OpenAI traffic.** Streaming reconstruction is
Anthropic-only. A marker-bearing OpenAI *streaming* response is replayed raw (fail-open) and
restoration does not fire on it. Non-streaming OpenAI and streaming Anthropic both work
normally.

**One request lost its streaming.** A streaming response is inspected with a **bounded
peek**: the proxy buffers only up to the first `content_block_start`, and if that block is
not a call to the expand tool it flushes the peek and streams the remainder. Only a response
that *opens* with the expand call is read in full, and for that one time-to-first-byte
becomes time-to-last-byte. `/stats` reports the split: `sse_streamed`, `sse_buffered`,
`sse_buffered_pct`, `sse_ttfb_ms_avg`, `sse_ttfb_ms_avg_buffered`. The last of those is
time-to-*last*-byte by construction, so it is not comparable to `sse_ttfb_ms_avg`.

The peek's accepted limit: a response that opens with thinking or text and calls expand
*later* streams through, so the client receives the model's raw expand `tool_use` instead of
the proxy resolving it — the same outcome you already get when the model batches expand with
another tool, or when no id resolves. `sse_expand_after_stream` counts it, so the rate is a
number rather than an assumption. The trade is deliberate: buffering every advertised-tool
response cost ~21 s of extra time-to-first-byte on a third of all responses, and the expand
loop resolved 5 calls in 5,752 requests.

**A page or message that merely quotes a marker counts as marker-bearing.** Marker matching
accepts both the plain `<<cg:HASH>>` spelling and the HTML-escaped
`&lt;&lt;cg:HASH&gt;&gt;` form that Go's JSON encoder normally produces, case-insensitively.
The bias is deliberate: over-inspecting a response costs latency, missing a real expand call
costs content.

**Frozen-decision health.** `frozen_hits` / `frozen_misses` / `frozen_dropped` /
`frozen_repaired` / `frozen_flips` — see
[Routes](../reference/routes.md#freeze-replay-health). A dropped decision is re-derived when
re-derivation is reproducible (`mask`, `failed_run`, whose replacement is a pure function of
content and config). `extract_llm` is excluded on purpose: its replacement is sampled model
output, so re-deriving could splice *different* bytes into a cached prefix.

</details>

See also: [Components](../components.md) · [When your agent compacts](agent-compaction.md) ·
[Routes & headers](../reference/routes.md)
