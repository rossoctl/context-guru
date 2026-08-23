# Streaming decision point, and what a round retains (2026-08)

Two measurements behind the expand splice in `proxy/ssepeek.go`.

`ttfb-by-decision-point.tsv` — 39 turns, every SSE event timestamped with no proxy in the
path. It answers where a streaming proxy may put the client's first byte. Deciding at the
first `tool_use` block withholds 98.3% ± 2.1% of the streaming span on tool-calling turns,
99.0% ± 1.6% with thinking on, 99.5% ± 0.6% on `opus-4-7` (n=3), and **100%** on a turn with no
tool call at all, where the decision point becomes `message_stop` and the whole response is
buffered: +3.17 s ± 0.66, +3.48 s ± 0.96, +3.96 s ± 0.64 and +4.53 s ± 0.68 of client wait
respectively, on nearly every response, to intercept the ~1.3% that call the expand tool.
Splicing pays on that 1.3% only.

Every figure above is what you get by recomputing from the rows in this directory. Times are
rounded to 10 ms here, so a recomputation lands within ~0.1 of the live-run figures; where the
two ever differ, THIS file is the one to quote, because it is the one a reader can check.

Both arms are derived from one timeline per turn, which makes the comparison paired by
construction — but `peek-far >= splice` is an identity of that construction, so the sign is
not evidence. The magnitudes are, and the prose-only rows settle it without any statistics.

It also re-confirms, on two models, that this upstream **does** stream: the first event lands
at a median of 0.49 of wall (range 0.23-0.81) and the deltas run across the rest. An older comment in `ssepeek.go` claimed
the opposite from 6 turns; individual turns do read 1.000 when generation finishes fast enough
to arrive in one burst.

`retention-and-churn.tsv` — the allocation cost of holding a round, per code path, plus the
attribution that found it (`sseEventPayload` called twice per event, not the event buffer) and
the `max_tokens` ceilings the upstream enforces. It also records the two instrument hazards it
hit: a GC running inside one arm, and the `httptest` recorder's own buffer growth.

`sse-bytes-per-output-token.tsv` — what a round costs in memory while the splice holds it.
~35 bytes per output token, mechanically explained, so a full-length response at this
gateway's `max_tokens` ceiling of 128,000 is ~4.5 MB — which is 3.4x UNDER the bound and is
not what sets it. `sseRetainMaxBytes` is set by the worst case, not the typical one: a stream
emitting one token per delta pays the ~123 bytes of framing per token instead of per ~4
tokens, reaching ~16.3 MB, a margin of about 3% under 16 MiB. Overshoot is graceful by
construction — the round is forwarded whole, the call is counted, the repair answers it next
request — so that margin is not a cliff. The `max_tokens` ceiling is the UPSTREAM's, not this
proxy's: it never caps the field, it only reads it, so re-derive the bound if the upstream
changes.

## Reproducing

Both were produced by timestamping the raw event stream of a `POST /v1/messages` with
`stream: true` — no proxy, no harness. Read the gateway URL and token from the environment,
send the prompt shapes named in the `shape` column, record the monotonic time of every `data:`
line and the total bytes read, and take output tokens from the final `message_delta`.
