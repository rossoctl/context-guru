# context-guru as an eval-containers gateway

`context-guru-proxy` is a drop-in eval-containers gateway flavor: it runs the
context-engineering pipeline on every request, then forwards to the real
provider. Swap it in with `EVAL_GATEWAY_IMAGE` — no agent or benchmark changes.

## Build

The build context is this repo — bifrost is an ordinary published dependency, so no
sibling checkout is needed:

```sh
docker build -t context-guru-proxy:latest .
```

## Run under eval-containers

```sh
EVAL_GATEWAY_IMAGE=context-guru-proxy:latest \
  eval-containers run <benchmark> --agent <agent> --model <provider>/<model>
```

The gateway (`start`) reads:
- `EVAL_MODEL=<provider>/<model>` — the model pins every call (`FORCE_MODEL`); the provider selects the upstream.
- `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` — the real key, injected on forward (the agent holds only `sk-proxy`).
- `OPENAI_API_BASE` / `ANTHROPIC_API_BASE` — optional upstream overrides.
- `CONTEXT_GURU_PRESET` (default `balanced`) — which pipeline preset to run.

Three ways to pick a pipeline, in precedence order — the first non-empty one wins:

1. `CONTEXT_GURU_CONFIG_YAML` — a full config document, written to `/tmp/cg-config.yaml`
   and passed as `--config`. The only form that can pin per-component settings (e.g.
   `extract_llm`'s `strategy` and `model.source`), which is what a sweep needs.
2. `CONTEXT_GURU_PIPELINE` — a bare comma-separated component list, expanded to
   `pipeline: [...]`. An **empty** value falls through rather than forcing a passthrough,
   so an always-present-but-blank compose variable does not silently disable compaction.
3. `CONTEXT_GURU_PRESET` — a named preset. Use `off` for the passthrough baseline.

Endpoints on `:4000`: `/openai/v1/chat/completions`, `/anthropic/v1/messages`,
`/compact` (stateless — pipeline in, rewritten body out, no upstream call), plus
`/healthz`, `/stats` (savings rollups), `/metrics` (the same counters as Prometheus text)
and `/expand?id=` (recover offloaded content).

## Measuring the effect

Run the same `EVAL_TASK_ID` twice — once normally, once with the agent sending
`x-context-guru-bypass: true` — and compare. `/stats` reports token-weighted
savings per component; per-task attribution improves once a session id is stamped
(`x-context-guru-session`).

## Known gaps (tracked, P5)

- OTel `gen_ai` spans are not yet emitted (eval-containers' otelcol ingests them);
  today savings come from `/stats`.
