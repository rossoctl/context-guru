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

Endpoints on `:4000`: `/openai/v1/chat/completions`, `/anthropic/v1/messages`,
plus `/healthz`, `/stats` (savings rollups), `/expand?id=` (recover offloaded content).

## Measuring the effect

Run the same `EVAL_TASK_ID` twice — once normally, once with the agent sending
`x-context-guru-bypass: true` — and compare. `/stats` reports token-weighted
savings per component; per-task attribution improves once a session id is stamped
(`x-context-guru-session`).

## Known gaps (tracked, P5)

- OTel `gen_ai` spans are not yet emitted (eval-containers' otelcol ingests them);
  today savings come from `/stats`.
