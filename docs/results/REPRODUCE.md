# Reproducing the SWE-bench Verified evaluation

How to install everything and run the three arms of the study — **baseline** (no
compaction), **context-guru**, and **headroom** — on SWE-bench Verified with the
Harbor harness and the `claude-code` agent on `aws/claude-sonnet-5`.

All three arms share one harness pattern: start a proxy (or none, for baseline) in
front of the model gateway, point Harbor's `claude-code` agent at it, run the tasks,
and parse each trial's `result.json` + `agent/trajectory.json` for reward, steps, and
cache-aware token/cost metrics.

## 1. Prerequisites (one-time)

- **OS**: Linux (RHEL/Fedora family here), passwordless `sudo`.
- **Go 1.26** + `CGO_ENABLED=1` (context-guru's `cg_skeleton` build tag needs cgo/tree-sitter).
- **Python 3.13** + [`uv`](https://docs.astral.sh/uv/) (Harbor needs ≥3.12).
- **Docker** (each task builds a container). Use it via `sg docker -c '...'` — do **not**
  loosen the socket permissions.
- **Harbor** checked out at `/home/vpcuser/projects/context-engineering/harbor`
  (`uv sync` in it).
- **Model gateway creds** in `~/.claude/settings.json` under `env`
  (`ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`) — the IBM LiteLLM gateway exposing
  `aws/claude-sonnet-5` (agent) and `aws/claude-haiku-4-5` (context-guru's cheap
  compaction model). `CG_GATEWAY_BASE` / `CG_GATEWAY_KEY` override both, and are how to
  run without that file.

    The harnesses **refuse to start** if either value names a context-guru route or a
    `cg_live_` token: a benchmark proxy pointed at another context-guru would report
    savings measured over traffic that was already compacted once. If you have routed
    your own Claude Code through the service, set `CG_GATEWAY_*` to the real gateway.

- **Only for the hosted (multi-tenant) proxy**: `CG_TOKEN` with your tenant token and
  `CG_PROXY_URL` naming the already-running proxy. There the pipeline comes from your
  tenant's own configuration rather than from `--configs`, which then only labels the
  run. With `CG_TOKEN` unset the harness starts its own single-tenant proxy and behaves
  exactly as it always did.

### Docker Hub authentication (required — avoids the 429 pull-quota wall)

SWE-bench task images live on Docker Hub (`docker.io/swebench/…`). Harbor pulls one per
task; ~100 pulls exhausts the **anonymous** quota (HTTP 429) and environments fail to
build. Authenticate once (an authenticated account has a separate 200-pulls/6h quota):

```
sg docker -c 'docker login -u <your-dockerhub-user>'   # paste a Read-only Personal Access Token
```

The harness also passes `--no-delete` so images persist and are **not** re-pulled across
runs. (Optionally `gh auth login` for `ghcr.io`, but the SWE images are not mirrored there.)

### Task list

`/tmp/cg-runs/swe50.txt` — 50 tasks (every 10th of SWE-bench Verified's 500).
`/tmp/cg-runs/swe3-verify.txt` — a 3-task smoke subset.

## 2. Build context-guru

```
cd /home/vpcuser/projects/context-engineering/context-guru
CGO_ENABLED=1 go build -tags cg_skeleton -o /tmp/cg-runs/cg-proxy-d1 ./cmd/context-guru-proxy
CGO_ENABLED=1 go test -tags cg_skeleton ./...      # all green
```

## 3. Run baseline + context-guru (one command)

The harness [`deploy/harbor/swebench.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/swebench.py) starts the
proxy per config (`off` = transparent passthrough baseline; `codesmart` = the tuned
cache-aware config), points Harbor at it (`ANTHROPIC_BASE_URL=http://$CG_LAN:4000/anthropic`),
runs the tasks, and writes `summary.json` + `rows-*.json`.

```
cd /home/vpcuser/projects/context-engineering/context-guru
# baseline + context-guru, 50 tasks, 2 trials each, dump context-guru's change log:
nohup python3 -u deploy/harbor/swebench.py \
  --tasks /tmp/cg-runs/swe50.txt \
  --configs off codesmart \
  --jobs-root /tmp/cg-runs/study --n 2 \
  --dump-configs codesmart > /tmp/cg-runs/study.log 2>&1 &
```

Notes / gotchas learned the hard way:
- **Set `CG_LAN` to this box's LAN address before launching.** Every harness reads it from
  the environment and falls back to `127.0.0.1` with a warning — and `127.0.0.1` inside a
  task container is the container, not the proxy, so every task fails. That warning is the
  first line to check when a whole run comes back empty.
- The login shell has **`errexit`**: a leading `pkill` that matches nothing returns 1 and
  aborts the whole launch. Launch with a bare `nohup python3 -u <abs-path> … &` (no
  leading `pkill`/`cd`, no trailing `sleep`).
- `codesmart` uses `CACHE_MODE=auto` (cache-aware on a prompt-caching agent) and
  `CHEAP_MODEL=aws/claude-haiku-4-5` for its own compaction calls.

**A re-run today will not reproduce the published figures exactly.** The harness embeds its
own `codesmart` and `codesafe` documents, and both have since been brought back into step
with the shipped presets, so the command above now runs the *current* pipelines: `codesmart`
gained `toon`, both swapped `cacheinject` for `cachesplit`, and a gating bug that kept
`failed_run` inert on this workload is fixed. Read any difference as a change in the
treatment, not as run-to-run noise, and re-measure before a cost claim leans on the current
pipeline. No published number has been adjusted.

A drift guard (`deploy/harbor/pipeline_drift_test.go`) checks every preset a harness names an
arm after, so a harness document that falls behind the shipped preset fails the test rather
than quietly producing an unlabelled treatment. Arms that deliberately run a non-preset
pipeline — `cacheonly`, which runs `[cacheinject]` alone to isolate the cache lever — are
exempt, since there is no preset to compare them against.

## 4. Run headroom

Headroom (`headroom-ai`, an HTTP proxy like context-guru) via its harness
`/tmp/hd-runs/swebench_headroom.py` (a copy of
`swebench.py` pointed at the headroom binary/port). Two flags are **required** for the
claude-code + Bedrock-gateway combo (both are real findings):
- `HEADROOM_TOOL_SEARCH=0` — headroom otherwise injects first-party-only
  `tool_search_tool_*` that the Bedrock gateway can't honor → `Content block not found`.
- `--no-ccr` — headroom's reversible-retrieval SSE re-emission corrupts claude-code's
  stream; its own `--help` says `--no-ccr` is right for streaming (compressors stay
  active, only reversibility is lost).

```
python3 /tmp/hd-runs/swebench_headroom.py --tasks /tmp/cg-runs/swe50.txt \
  --jobs-root /tmp/hd-runs/study --n 2
```

Use a **different proxy port** (e.g. 4010) and jobs-root than context-guru so the two
never collide; reuse the same authenticated Docker Hub + `--no-delete`.

## 4b. Run rtk (Rust Token Killer)

rtk is **not** a request-stream proxy — it is a Claude Code **`PreToolUse` hook** that
rewrites Bash commands (`pytest …` → `rtk pytest …`, `cat f` → `rtk read f`,
`git status` → `rtk git status`) **inside the task container**, compressing bash output
at the shell before it enters the model context. So there is nothing to proxy for
compaction: model routing is made **identical to the baseline** by running the same
context-guru `off` passthrough proxy on `:4000`. The only difference from baseline is
the in-container bash compression.

**1) Fetch the rtk binary** (static-musl, runs on any SWE-bench image):

```
mkdir -p /tmp/rtk-runs && cd /tmp/rtk-runs
curl -fsSL -o rtk.tgz https://github.com/rtk-ai/rtk/releases/download/v0.43.0/rtk-x86_64-unknown-linux-musl.tar.gz
tar xzf rtk.tgz && chmod +x rtk && ./rtk --version   # rtk 0.43.0
```

**2) Add the `claude-code-rtk` agent to the Harbor checkout** (one new file + two lines).
The agent subclasses the stock `claude-code` agent and, per trial: uploads the rtk binary
to `/usr/local/bin/rtk`, installs the rtk `PreToolUse` hook into the exact
`CLAUDE_CONFIG_DIR` Claude Code reads (`rtk init -g --auto-patch`, which honors that
env var — `--auto-patch` is **required** headless, else the piped-stdin patch defaults to
"no"), and dumps `rtk gain --all --format json` to `/logs/agent/rtk-gain.json`.

- add `harbor/src/harbor/agents/installed/claude_code_rtk.py` (a copy lives at
  [`deploy/harbor/claude_code_rtk_agent.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/claude_code_rtk_agent.py));
- register it: `CLAUDE_CODE_RTK = "claude-code-rtk"` in `harbor/src/harbor/models/agent/name.py`
  and `AgentName.CLAUDE_CODE_RTK: "harbor.agents.installed.claude_code_rtk:ClaudeCodeRTK"`
  in `harbor/src/harbor/agents/factory.py`.

**3) Run the 50 tasks** (starts the `off` proxy, runs `-a claude-code-rtk`):

```
cd /home/vpcuser/projects/context-engineering/context-guru
RTK_BIN_HOST=/tmp/rtk-runs/rtk python3 -u deploy/harbor/swebench_rtk.py \
  --tasks /tmp/cg-runs/swe50.txt --jobs-root /tmp/rtk-runs/swe50 --n 2
```

End-to-end metrics (reward, steps, cache-read/write, billed cost, cache-hit, wall) come
from the unchanged claude-code trajectory parser — measured **identically** to the other
three arms. Each trial's `agent/rtk-gain.json` also carries rtk's own bash-output savings
ledger (its native `bytes/4` estimate, a bash-output denominator — **not** the whole-request
content% the proxies report).

## 5. Analyze & plot

```
# Four-way matched analysis (baseline vs context-guru vs headroom vs rtk) — every dimension,
# per-task + aggregate + per-component, cumulative & unique tokens → deep_analysis.json:
python3 deploy/harbor/deep_analysis.py --out /tmp/cg-runs/deep
# Figures (validated CVD-safe palette) → docs/img/benchmark/:
/tmp/cg-runs/plotenv/bin/python deploy/harbor/deep_plots.py /tmp/cg-runs/deep/deep_analysis.json --out docs/img/benchmark
# Per-config result pages (per-task tables + totals):
python3 deploy/harbor/gen_result_docs.py <rows.json> "<label>" docs/results/<config>.md [--summary <summary.json>]
# Per-component unique-vs-cumulative token savings from the change-log dump:
python3 deploy/harbor/dump_unique.py /tmp/cg-runs/dump-swebench-codesmart.jsonl
```
(`deep_analysis.py` reads each run's `rows-*.json` at the paths in its `SRC` map —
`/tmp/cg-runs/final50/rows-off.json`, `/tmp/cg-runs/final50-v6/rows-codesmart.json`,
`/tmp/hd-runs/swe50/rows-hd-cache.json`, and — when present —
`/tmp/rtk-runs/swe50/rows-rtk.json`. Arms whose rows file is missing are dropped, so the
matched set is the intersection of tasks scored with **no exception** in every arm
supplied.)

Metrics collected per trial: reward, steps, prompt/cached/creation/read/completion
tokens, cache-aware billed cost (fresh $2/M · cache-read $0.20/M · cache-write $2.50/M ·
output $10/M), cache-hit rate, proxy savings %, per-component savings + own latency,
context-guru's own cheap-model cost (priced at the haiku rate), and expand/restoration
bounces.

## 7. Terminal-Bench 2.0 (second benchmark)

The same harness pattern extends to **Terminal-Bench 2.0** (89 open-ended terminal tasks,
harder/longer-horizon than SWE-bench). Only the Harbor dataset and jobs-root change; the
`claude-code` trajectory parser, cache-aware cost model, and summarizer are agent-specific,
so every metric is computed identically and is methodologically comparable to the SWE study.
Harness: [`deploy/harbor/terminalbench.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/terminalbench.py)
(a thin adaptation of `swebench.py` with `-d terminal-bench@2.0`).

```
cd /home/vpcuser/projects/context-engineering/context-guru
# task list: all 89 task names from the terminal-bench registry entry (see below)
# baseline (off passthrough), parallel:
python3 -u deploy/harbor/terminalbench.py \
  --tasks /tmp/tb-runs/tb89.txt --configs off --jobs-root /tmp/tb-runs/tb89 \
  --n 24 --agent-mult 1.5 --max-retries 4
```

Build the 89-task list from the registry:
```
python3 - <<'PY'
import json
d=json.load(open('/home/vpcuser/projects/context-engineering/harbor/registry.json'))
def find(o):
    r=[]
    if isinstance(o,list):
        for e in o:
            if isinstance(e,dict) and e.get('name')=='terminal-bench': r.append(e)
            r+=find(e)
    elif isinstance(o,dict):
        for v in o.values(): r+=find(v)
    return r
open('/tmp/tb-runs/tb89.txt','w').write('\n'.join(t['name'] for t in find(d)[0]['tasks'])+'\n')
PY
```

**Concurrency (feasibility on this 16-core / 62 GB box).** `--n` is Harbor's `--n-concurrent`.
Agents are network-bound (~300 MB, ~0.5% CPU while waiting on the ~26 s/request gateway), so RAM
is not the limit — **build-phase CPU** is (compile tasks run `make -j` and saturate cores; Harbor
self-limits build ramp), and **disk** (`--no-delete` image accumulation). `--n 24` is the sweet
spot (~10× faster than `n=2`, ~2.7 h for all 89); `n=45` only widens the disk/rate-limit blast
radius without building faster. Prune unused images (`docker image prune -af`, safe — protects
in-flight containers) if disk gets tight.

**Timeouts / time-budget policy.** TB's task timeouts assume a fast endpoint; this gateway is
~26 s/request, so long-horizon tasks can exhaust the wall-clock budget. Two effects, separated:
(1) at `n=24`, CPU oversubscription (load ~45) inflated wall time and timed out ~9 edge tasks —
**rerun those at `n=6`** (no oversubscription) to clear the artifact; (2) genuinely long tasks were
given an extended **`--agent-mult 4.0`** budget to measure capability. Merge the best clean result
per task (`/tmp/tb-runs/merge_tb.py`). Any task that still times out at 4× is a genuine failure
(reward 0). All 89 tasks carry a scored outcome (solved / failed / timeout).

**Baseline row provenance (needed to reproduce the published $100.81).** The published baseline is
a *two-stage* merge, and the intermediate file alone does not reproduce the totals:

1. `merge_tb.py` overlays the `n=6` rerun onto the clean `n=24` rows → `rows-off-final.json`
   (sums to $71.44 — **not** the published figure);
2. the 11 long-horizon tasks rerun at `--agent-mult 4.0` (`/tmp/tb-runs/tb-rerun2/rows-off.json`:
   `caffe-cifar-10`, `circuit-fibsqrt`, `cobol-modernization`, `feal-linear-cryptanalysis`,
   `gpt2-codegolf`, `make-doom-for-mips`, `make-mips-interpreter`, `path-tracing`,
   `path-tracing-reverse`, `schemelike-metacircular-eval`, `write-compressor`) are then overlaid
   on top → the published 89-task baseline (215,971,427 cache-read / 4,011,068 cache-write /
   58,893 fresh / 4,746,887 output = **$100.81**).

Do stage 2 explicitly; `merge_tb.py` does not do it. Keep the merged file so the totals stay
reconstructible:

```
python3 - <<'PY'
import json
fin={r['task']:r for r in json.load(open('/tmp/tb-runs/tb89/rows-off-final.json'))}
for r in json.load(open('/tmp/tb-runs/tb-rerun2/rows-off.json')): fin[r['task']]=r
json.dump(list(fin.values()), open('/tmp/tb-runs/tb89/rows-off-published.json','w'), indent=1)
PY
```

**Excluding degenerate trials.** A trial where the *baseline* aborted in a handful of steps is not
a measurement: the per-task delta then reflects the baseline not doing the work. Six tasks are in
this class (`extract-moves-from-video`, `polyglot-rust-c`, `mteb-leaderboard`, `regex-chess`,
`write-compressor`, `code-from-image`). Compare **per task, paired**, and report the clean subset
alongside the raw total — per-arm sums hide this entirely. See the
[comparison](terminal-bench-comparison.md) correction box.

Analyze → doc (totals, cache-aware cost, by-difficulty/category, timeouts, per-task):
```
# task metadata (difficulty/category) from Harbor's task cache — needs py3.11+ (tomllib):
/home/vpcuser/projects/context-engineering/harbor/.venv/bin/python - <<'PY'
import glob, json, os, tomllib
names=set(l.strip() for l in open('/tmp/tb-runs/tb89.txt') if l.strip()); meta={}
for f in glob.glob('/home/vpcuser/.cache/harbor/tasks/*/*/task.toml'):
    t=os.path.basename(os.path.dirname(f))
    if t not in names: continue
    d=tomllib.load(open(f,'rb')); m=d.get('metadata',{})
    if t in meta and (meta[t]['difficulty']!='unknown' or m.get('difficulty') is None): continue
    meta[t]=dict(difficulty=m.get('difficulty','unknown'),category=m.get('category','unknown'),
                 agent_timeout_sec=(d.get('agent',{}) or {}).get('timeout_sec'))
json.dump(meta, open('/tmp/tb-runs/task_meta.json','w'), indent=1)
PY
python3 deploy/harbor/gen_tb_docs.py /tmp/tb-runs/tb89/rows-off.json \
  docs/results/terminal-bench-baseline.md --meta /tmp/tb-runs/task_meta.json
```

### 7b. Terminal-Bench framework arms (context-guru / headroom / rtk)

The three compaction arms reuse the SWE harnesses, re-pointed at `terminal-bench@2.0`
([`terminalbench.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/terminalbench.py)
for context-guru's `codesmart`,
[`terminalbench_headroom.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/terminalbench_headroom.py),
[`terminalbench_rtk.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/terminalbench_rtk.py)).
All ran at a **flat `--agent-mult 4.0`** budget (see the [comparison](terminal-bench-comparison.md)
for why this is fair vs the baseline's mixed budget), `n=12` (headroom + rtk in parallel on
their separate ports 4010/4000; context-guru on 4000):

```
cd /home/vpcuser/projects/context-engineering/context-guru
# context-guru (codesmart) — dumps the change log for the per-component analysis
python3 -u deploy/harbor/terminalbench.py --tasks /tmp/tb-runs/tb89.txt --configs codesmart \
  --jobs-root /tmp/tb-runs/cg --n 12 --agent-mult 4.0 --max-retries 4 --dump-configs codesmart
# headroom (hd-cache; --no-ccr + HEADROOM_TOOL_SEARCH=0 baked in, as on SWE)
python3 -u deploy/harbor/terminalbench_headroom.py --tasks /tmp/tb-runs/tb89.txt --configs hd-cache \
  --jobs-root /tmp/tb-runs/hd --n 12 --agent-mult 4.0 --max-retries 4
# rtk (claude-code-rtk agent + off-proxy routing; needs the rtk binary + registered agent)
RTK_BIN_HOST=/tmp/rtk-runs/rtk python3 -u deploy/harbor/terminalbench_rtk.py --tasks /tmp/tb-runs/tb89.txt \
  --jobs-root /tmp/tb-runs/rtk --n 12 --agent-mult 4.0 --max-retries 4
```

Each writes `rows-<cfg>.json` + `summary.json` under its jobs-root (headroom also dumps
`/tmp/tb-runs/stats-hd-cache.json`; rtk's ledger is in each trial's `agent/rtk-gain.json`).
Per-arm pages via `gen_tb_docs.py --kind arm --label "<name>"`; the four-way
[comparison](terminal-bench-comparison.md) is assembled from the four `rows-*.json`.

### 7c. The merged-system arm (2026-08-10)

After the cache/filter/observe work landed on `main` (15 PRs), context-guru was re-measured as a
fifth arm. Two things differ from §7b and both matter for reproduction.

**The config is not `codesmart`.** It is `cgfinal = [format, dedup, cmdfilter, extract, cachesplit]`,
chosen on per-component evidence rather than maximal token reduction. `extract_llm` (82× underwater
at cache-read prices), `failed_run` (`acted=0`, 28.8 s of scanning) and `cacheinject` (removed from
every preset by #36) are excluded. See the
[comparison](terminal-bench-comparison.md#the-merged-system-a-fifth-arm-2026-08-10).

**Verify the mechanism fired before crediting it.** Each claim must name a counter:

```
# after the run, from /stats:
llm_calls == 0                 # extract_llm genuinely never ran
cmdfilter.acted / requests     # #42 firing rate (0.9% before, 28.7% after)
sse_buffered_pct               # #33 fast path (100% before, 44.2% after)
frozen_*                       # #40 -- ZERO here, because its only callers are excluded.
                               # That is the expected result, NOT evidence about #40.
cachesplit.acted               # 0 on TB: the Agent SDK sends no volatile tail to split
```

Two analysis requirements, both learned from earlier errors in this study:

- **Report the median per-task ratio and a leave-one-out beside any aggregate.** The −16.4%
  aggregate becomes −9.1% without `path-tracing` alone. A sum over heterogeneous tasks can be
  one task.
- **Use unique, never cumulative, token figures.** Measured overcount was **44.5×** for
  `cmdfilter` and **18.4×** for `extract` — a cumulative number is wrong by an order of magnitude.

The per-arm run commands are otherwise identical to §7b, with a distinct binary name, port and
jobs-root. Note that the harnesses `pkill` by binary **name**, so two concurrent runs sharing a
name will kill each other's proxy — this happened twice during the study and invalidated two arms.

## 8. Result docs

- [`baseline.md`](baseline.md) — SWE-bench baseline (`off`) full results.
- [`context-guru.md`](context-guru.md) — context-guru `codesmart` full results.
- [`headroom.md`](headroom.md) — headroom full results.
- [`rtk.md`](rtk.md) — rtk (Rust Token Killer) full results.
- [`comparison.md`](comparison.md) — the four-way SWE-bench comparison across all metrics.
- [`terminal-bench-comparison.md`](terminal-bench-comparison.md) — the four-way **Terminal-Bench 2.0** comparison.
- [`terminal-bench-baseline.md`](terminal-bench-baseline.md) · [`-context-guru`](terminal-bench-context-guru.md) · [`-headroom`](terminal-bench-headroom.md) · [`-rtk`](terminal-bench-rtk.md) — TB per-arm full results.
