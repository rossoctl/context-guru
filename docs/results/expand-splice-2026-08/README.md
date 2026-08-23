# Streaming decision point, and what a round retains (2026-08)

Two measurements behind the expand splice in `proxy/ssepeek.go`.

`ttfb-by-decision-point.tsv` — 39 turns, every SSE event timestamped with no proxy in the
path. It answers where a streaming proxy may put the client's first byte. Deciding at the
first `tool_use` block withholds 98.4% ± 2.1% of the streaming span on tool-calling turns,
99.1% ± 1.7% with thinking on, and **100%** on a turn with no tool call at all, where the
decision point becomes `message_stop` and the whole response is buffered: +3.2 s to +4.5 s of
client wait, on nearly every response, to intercept the ~1.3% that call the expand tool.
Splicing pays on that 1.3% only.

Both arms are derived from one timeline per turn, which makes the comparison paired by
construction — but `peek-far >= splice` is an identity of that construction, so the sign is
not evidence. The magnitudes are, and the prose-only rows settle it without any statistics.

It also re-confirms, on two models, that this upstream **does** stream: the first event lands
at 40-55% of wall and the deltas run across the rest. An older comment in `ssepeek.go` claimed
the opposite from 6 turns; individual turns do read 1.000 when generation finishes fast enough
to arrive in one burst.

`retention-and-churn.tsv` — the allocation cost of holding a round, per code path, plus the
attribution that found it (`sseEventPayload` called twice per event, not the event buffer) and
the `max_tokens` ceilings the upstream enforces. It also records the two instrument hazards it
hit: a GC running inside one arm, and the `httptest` recorder's own buffer growth.

`sse-bytes-per-output-token.tsv` — what a round costs in memory while the splice holds it.
~35 bytes per output token, mechanically explained, so a full-length response at this
gateway's `max_tokens` ceiling of 128,000 is ~4.5 MB. That is what sets `sseRetainMaxBytes`.

## Reproducing

Both were produced by timestamping the raw event stream of a `POST /v1/messages` with
`stream: true` — no proxy, no harness. Read the gateway URL and token from the environment,
send the prompt shapes named in the `shape` column, record the monotonic time of every `data:`
line and the total bytes read, and take output tokens from the final `message_delta`.
