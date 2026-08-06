# context-guru as an llm-d-router compaction service

A ready-to-run example that turns **context-guru** into a standalone HTTP **compaction service**
for [`llm-d-router`](https://github.com/ronenkat/llm-d-router). You don't need to know
context-guru to use this — everything you need is below.

---

## 1. What this is (for newcomers)

Large-language-model requests carry a lot of text: system prompts, past turns, and especially
**tool outputs** (file reads, command logs, API responses) that get re-sent on every turn. That
text costs tokens — money and latency — and pushes against the context window.

**context-guru** shrinks that text *before* it reaches the model, without changing the agent.
It parses the request's `messages` array, rewrites the bulky parts (compacting JSON, filtering
irrelevant lines, summarizing old turns), and hands back a **smaller request of the exact same
shape**. It is **fail-open**: if anything goes wrong, it returns your original request unchanged.

`llm-d-router`'s `request-inline-compaction` step (PR #1) is designed to call an external
service with exactly this contract:

```
POST <service>/compact         body = the inference request JSON
  200 + non-empty JSON  ->  router swaps that body in (now smaller)
  anything else          ->  router keeps the original (passthrough)
```

This example is that service. It runs with **no state storage** and **no markers**, so the body
it returns is a clean, directly-usable inference request.

```
             ┌─────────────────────── /compact ───────────────────────┐
 request ───▶│  parse messages[] ─▶ run pipeline ─▶ splice back  ─▶ 200 │───▶ smaller request
   (JSON)    │       (format · toon · extract · summarize · …)          │        (same schema)
             └──────────────────────────────────────────────────────────┘
                         no upstream call · fail-open · no markers
```

### A few terms you'll see

- **Component** — one compaction step (e.g. `toon`, `extract`, `summarize`). You pick which run,
  and in what order, in a small YAML config.
- **Pipeline** — the ordered list of components applied to a request.
- **Reformat vs Offload** — a *reformat* component (like `toon`) repacks text losslessly; an
  *offload* component (like `extract`/`summarize`) drops bulk and can leave a `<<cg:…>>` **marker**
  that a companion store could later expand. **This example disables the store and sets
  `marker_mode: off`**, so nothing is stored and no markers appear — compaction is one-way.
- **Cheap model** — the small LLM the LLM-based components call. You configure it (endpoint +
  credentials + model id); the example configs use `gpt-4o-mini` as a placeholder, but any
  OpenAI- or Anthropic-compatible model works (e.g. `claude-haiku-4-5`).

Deeper docs live in the repo: [`docs/components.md`](../../docs/components.md) (every component)
and [`docs/design.md`](../../docs/design.md) (the pipeline, store, fail-open model).

---

## 2. The three example configs

| Config | Component(s) | Uses an LLM? | What it does | Best for |
|---|---|:--:|---|---|
| [`configs/toon.yaml`](configs/toon.yaml) | `format` + `toon` | no | Re-encodes uniform JSON object-arrays in tool outputs as **TOON** (field names once, then one row per element). | Deterministic, zero-cost, zero-latency wins on structured/tabular tool output. |
| [`configs/extract-code.yaml`](configs/extract-code.yaml) | `extract` (`strategy: code`) | yes | A cheap LLM writes a sandboxed filter that **deletes irrelevant lines** from large tool outputs (verified deletion-only — never invents text). | Big, noisy tool outputs where only a slice is relevant to the current goal. |
| [`configs/summarize.yaml`](configs/summarize.yaml) | `summarize` | yes | A cheap LLM **summarizes the middle** of a long transcript into one message, keeping the head + last few turns. | Long agentic sessions where the transcript itself is the token cost. |

All three set `store: { enabled: false }` and `marker_mode: "off"` — irreversible, marker-free output.

See a real run of all three on one input, with the full `messages` before and after, in
[`sample/BEFORE-AFTER.md`](sample/BEFORE-AFTER.md).

---

## 3. Prerequisites

- **Go 1.26** and a **C toolchain** (`CGO_ENABLED=1` — the `skeleton` component links tree-sitter via cgo).
- Nothing else: **bifrost** is an ordinary published module dependency (`go.mod`), so this repo
  builds standalone from a plain clone.
- For the two **LLM** configs only: access to an LLM endpoint (any OpenAI- or Anthropic-compatible
  API, including a gateway) and its credentials.

---

## 4. Build

```sh
./examples/llm-d-service/build.sh        # -> bin/context-guru-proxy
```

---

## 5. Run

The deterministic config needs no credentials:

```sh
bin/context-guru-proxy --config examples/llm-d-service/configs/toon.yaml
# listens on :4000 (set LISTEN_ADDR to change)
```

The LLM configs need a model. Put the endpoint + credentials in the config's `model:` block, or
leave `api_key: ""` and supply the key via the environment:

```sh
# Option A — credentials in the environment (recommended; keeps secrets out of files)
export OPENAI_API_KEY=sk-...            # or ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN
bin/context-guru-proxy --config examples/llm-d-service/configs/extract-code.yaml

# Option B — a shared "cheap model" via env, and set model.source: config with an empty block
export CHEAP_MODEL=claude-haiku-4-5 CHEAP_MODEL_PROVIDER=anthropic \
       CHEAP_MODEL_BASE=https://your-gateway CHEAP_MODEL_KEY=... CHEAP_MODEL_AUTH=bearer
bin/context-guru-proxy --config examples/llm-d-service/configs/summarize.yaml
```

Edit the `model:` block in each LLM config to point at your endpoint (see the reference in §9).

---

## 6. Try it

### With curl (deterministic `toon`)

```sh
curl -s -XPOST localhost:4000/compact -H 'content-type: application/json' -d '{
  "model": "gpt-4o-mini",
  "messages": [
    {"role": "user", "content": "list users"},
    {"role": "tool", "tool_call_id": "c1",
     "content": "[{\"id\":1,\"name\":\"Alice\",\"role\":\"admin\"},{\"id\":2,\"name\":\"Bob\",\"role\":\"user\"},{\"id\":3,\"name\":\"Carol\",\"role\":\"admin\"},{\"id\":4,\"name\":\"Dave\",\"role\":\"user\"},{\"id\":5,\"name\":\"Eve\",\"role\":\"admin\"},{\"id\":6,\"name\":\"Frank\",\"role\":\"user\"}]"}
  ]
}'
```

The response is the same request with the tool output rewritten to TOON (smaller, no markers):

```
[6]{id,name,role}:
1,Alice,admin
2,Bob,user
...
```

> Outputs below a component's `min_tokens` gate (or below an LLM config's `trigger`) pass through
> unchanged — that's expected. Use a few-row array (or the Go client below) to see it act.

### From Go — the exact pattern llm-d uses

[`client/main.go`](client/main.go) is dependency-free. It builds a sample request, POSTs it to
`/compact`, and applies the returned body **only** on a `200`-with-JSON (passthrough otherwise) —
the same contract as the router step. Copy `compact()` into your own step and adapt.

```sh
go run ./examples/llm-d-service/client                 # default http://localhost:4000
go run ./examples/llm-d-service/client -addr http://compactor:4000
```

---

## 7. Wire it into llm-d-router

Point the `request-inline-compaction` step's `address` parameter at this service; the step appends
its path so requests land on `/compact`:

```yaml
# llm-d-router pipeline step params
address: http://context-guru:4000      # this service's base URL
```

Send the step's opt-in header on requests you want compacted:

```
x-llm-d-optimization: compaction
```

Run one service instance per config, or one instance plus per-request overrides (§8).

---

## 8. Per-request overrides

The router forwards the request body verbatim, so overrides ride on **headers / query params**:

| Override | Effect |
|---|---|
| `?provider=anthropic` | Treat the body as Anthropic Messages instead of OpenAI (the default). |
| `?preset=<name>` | Swap the pipeline for a built-in preset (e.g. `summarize`), keeping this config's component blocks. |
| `x-context-guru-pipeline: format,toon` | Run exactly these components, in order. |
| `x-context-guru-session: <id>` | Explicit session key (otherwise a content hash). |
| `x-context-guru-bypass: true` | Skip the pipeline entirely (returns the body unchanged). |

> `/compact` has no authentication — it's meant to run as an in-cluster sidecar reachable only by
> the router. Note that a caller can use `?preset=`/`x-context-guru-pipeline` to invoke the
> LLM-backed components, which spend the service's configured credentials. Front it accordingly
> (or omit the LLM configs) if the endpoint is exposed more broadly.

---

## 9. Configuration reference

**Store flag** — top-level `store:` block:

```yaml
store:
  enabled: false     # disable the state store (this example). Omit/true = on.
```

You can also override at launch: `--store=false` or `STORE=false` (wins over the file).

**Model block** — on `extract` / `summarize` under `components:`:

```yaml
model:
  source: config          # "config" = use the block below; "incoming" = reuse the request's own model+key
  provider: openai        # openai | anthropic
  base_url: http://your-llm-endpoint:8000   # your endpoint (default: provider public API)
  model: gpt-4o-mini      # model id the cheap calls use
  api_key: ""             # empty => env: OPENAI_API_KEY, or ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN
  auth: bearer            # anthropic only: x-api-key (default) | bearer (LiteLLM/gateways)
```

**Other knobs** (see [`docs/components.md`](../../docs/components.md) for the full set):

- `marker_mode: full | summary | "off"` — this example uses `"off"` (must be quoted in YAML).
- `trigger: { min_request_tokens, min_output_tokens, min_messages }` — gates when a (costly) LLM
  component fires; zero means "always". The committed LLM configs use production-scale triggers.

---

## 10. Troubleshooting

- **The response is identical to the input.** The request didn't meet a gate: below `min_tokens`
  (toon/format), or below the LLM config's `trigger`. Send a larger tool output / longer transcript,
  or lower the thresholds (see [`sample/configs/`](sample/configs) for lowered-trigger demo copies).
- **LLM config does nothing / logs "no model".** No reachable model: check `base_url`, the key
  (`api_key` or the env fallback), and `auth` (bearer vs x-api-key). With no model, `extract`
  degrades to a deterministic line projection and `summarize` no-ops — the request still returns `200`.
- **`summarize` output says tool outputs are "masked".** `include_tool_calls` defaults to `false`,
  so the summarizer sees the transcript shape, not tool bodies. Set `include_tool_calls: true` to fold them in.
- **`/compact` never errors your caller.** Unparseable bodies, bodies without a `messages` array,
  or any component failure return the **original body with `200`** — the router's passthrough
  contract always holds.

---

## 11. Notes

- **Fail-open by design** — a component that errors, panics, or would *grow* the request is reverted;
  the original request is always a valid fallback.
- **Secrets** — prefer leaving `api_key: ""` and supplying the key via the environment rather than
  committing it into a config file.
- **Files in this example:** [`configs/`](configs) (the 3 configs), [`build.sh`](build.sh),
  [`client/main.go`](client/main.go), [`sample/`](sample) (a captured before/after run).
