# PR #578 end-to-end review: sandbox, mock data, and oracle

Status: DESIGN, approved section by section in conversation on 2026-10-20. Awaiting review of this
written spec before an implementation plan is written.

## Purpose

Review PR #578 (`feat/insights-per-item-inventory`: 62 commits, 75 files, about 17,000 lines added) the way a user meets it: installed
from scratch on a machine with months of Claude Code history, used through the proxy, and read on
the dashboard. Every figure the dashboard shows is checked against a ground truth written before
the data was generated, so a pass means the PR computed the right answer, not that it agreed with
itself.

The sandbox replaces real usage with mock data of the same format and shape, because a clean
machine has none and the real machine's data must not be touched.

## Requirements, as stated

- A sandbox on this machine, isolated from the real `~/.claude`, proxy, and dashboard DB.
- The full context-guru solution installed from the PR, through its own install path.
- Mock data in the same format, and at the same scale in number of projects, over a timeframe.
- Fake MCP servers, skills, and other tools, for end-to-end coverage of every option.
- Tests run, UI snapshots taken, expected results compared with actual, final results reported.
- Quick smoke tests first, then a full regression run of about 45 minutes.

## Decisions taken

| Question | Decision |
|---|---|
| Isolation | One Docker container, `HOME=/sandbox`, nothing from the host mounted but `results/` |
| Measured traffic | Replayed through the proxy to a fake upstream; no real model, no key, no cost |
| Scale | The real shape, plus a 10x stress set |
| Coverage | Capability kinds, usage states, self-exclusion, and edge cases, all four |
| UI checks | Screenshots for a human, DOM assertions for pass/fail, any JS error is a failure |
| Review scope | Full repo test suite in the container, code review of the diff, install via `install.sh` |
| Architecture | Approach A: one container, one scripted pipeline |
| Tiers | Smoke (under 5 min, stops the run on failure), then full regression (~45 min) |
| Location | Branch `e2e/pr578-review` off the PR branch; #578's diff unchanged |

## Reference: the real data's shape

Measured from this machine's `~/.claude/projects`, counts only, no content read:

| | real |
|---|---|
| transcript directories (projects) | 12 |
| transcripts (sessions) | 41 |
| size | 97 MB |
| entries with timestamps | ~19,500 |
| span | about 6 weeks |
| MCP servers configured | 1 |
| installed skills | 27 plugin, 19 marketplace |

## Layout

```
e2e/                     not shipped; lives only on e2e/pr578-review
├─ Dockerfile.e2e        repo build stage + python3, chromium, curl
├─ run.sh                host entry: build, run a tier, copy results out
├─ gen/
│  ├─ manifest.py        the ground truth
│  ├─ transcripts.py     manifest -> ~/.claude/projects/*/*.jsonl
│  └─ inventory.py       manifest -> plugins, marketplace skills, ~/.claude.json MCP servers
├─ oracle.py             manifest -> expected.json, never calls PR code
├─ upstream.py           fake Anthropic /v1/messages, streamed and plain
├─ replay.py             post-install sessions -> POST through the proxy
├─ snap.mjs              headless Chrome: PNG, DOM values, console errors
├─ assert.py             expected vs actual -> one verdict per check
└─ report.py             results/ -> summary.md and report.html
```

## Isolation

1. Everything runs in the container with `HOME=/sandbox`. The real `~/.claude`, `~/.local`, the
   proxy on 8788, and its dashboard DB are never mounted.
2. Only `results/` is mounted, read-write, from a fresh host temp directory.
3. Preflight aborts if `HOME` resolves outside `/sandbox`, or if anything is listening on a port the
   harness did not start.

The proxy listens on 8788 inside the container, the same as the real install, so port-dependent
code takes the same path. The dashboard is published to the host on 18788.

## Ground truth

### The manifest

```
timeline      day 0 .. day 42; install at day 28, 12:00
              days 0-27   transcripts only           (imported history)
              days 28-42  transcripts and replayed   (measured by the proxy)
projects      name, root, subfolders, exists on disk, measured after install
sessions      project, start day, turns; each turn has a cwd, usage, and invocations
capabilities  kind, owner, on disk, declared in the prompt, token size
```

Generation is seeded, so two runs produce byte-identical data and expected values.

### Projects in the realistic set

| Project | Case it exercises |
|---|---|
| `alpha` | busy, measured, normal |
| `beta` | measured; a skill set disjoint from `alpha` |
| `gamma` + 4 subfolders | rollup: must appear as one project |
| `my-repo` | hyphen in the folder name: directory-name ambiguity |
| `deleted-proj` | history only, folder removed from disk |
| `history-only` | pre-install only: listed in History, absent from Cleanup |
| `mixed` | a session spanning the install moment |
| `wanderer` | a session that works in `/tmp/scratch` mid-flight |
| `/tmp/scratch-*` (3) | throwaway directories |
| `$HOME` | the folder that broke path-nesting |

### Capability matrix

Every marked cell is filled at least once.

| Kind | Never used | One project | Several | Stale | Pre-install only | Post-install only |
|---|---|---|---|---|---|---|
| MCP server (3 tools) | yes | yes | | yes | | yes |
| Plugin skill `plugin:x` | yes | yes | yes | yes | yes | yes |
| Marketplace skill (bare) | yes | yes | | | yes | |
| Bundled skill (not on disk) | | yes | | | | |
| Built-in tool | | | yes | | | |
| `context-guru:*`, `cg:*` | yes | yes | | | yes | |

### The oracle

`oracle.py` reads the manifest and writes `expected.json`. It implements the agreed rules on its own
and imports nothing from the PR:

- History, pooled: never used = on disk, not self-owned, invoked nowhere. Bundled skills are
  excluded. Self-owned skills are counted in the disclosure line.
- History owners: each owner with its count, and the total.
- Cleanup picker: project roots with measured traffic only. `history-only` is absent.
- Cleanup per project: which capabilities were declared and never invoked in that project's
  measured sessions, which decides which items appear.
- Request counts per root after rollup: own folder, subfolders, and folders its sessions wandered
  into.

### Money: property checks, not values

Exact dollar figures are not recomputed. Doing so would mean writing a second copy of the proxy's
pricing, and a mismatch would not say which copy was wrong. Money is checked by properties instead:

- every unused item has `usd > 0`; used items have none
- a project's total equals the sum of its rows
- the pooled total is at least each project's total

## Tier 1: smoke (under 5 minutes)

A small fixed dataset: 3 projects (one measured, one with a subfolder, one history-only), about 6
sessions, and about 10 capabilities with one of each kind. Any failure stops the run and reports.

| # | Check | Answers |
|---|---|---|
| S0 | preflight | is the sandbox isolated? |
| S1 | image builds; `go vet`, `go build ./...` (compiles only; the unit suite runs in tier 2) | does the PR compile? |
| S2 | `install.sh` installs; proxy starts; `/api/history` answers | does it install and run? |
| S3 | import one transcript set; replay a few requests | do both data paths write rows? |
| S4 | History headline count equals the oracle | is the main number right? |
| S5 | Cleanup picker excludes history-only; subfolder folded | do both picker rules hold? |
| S6 | each page loads with zero JS errors; one PNG each | does the UI render? |
| S7 | `dashboard.sh --no-open` reports the right port | does the new skill work? |

Smoke runs the same harness as the full tier with a smaller manifest, so a smoke pass also shows the
harness itself works.

## Tier 2: full regression (about 45 minutes)

Runs only if smoke is green.

| # | Phase | Proves |
|---|---|---|
| 1 | `make test`, all packages, `-race` | whether the plugin install-test failures seen on the host are real |
| 2 | generate the realistic set and `expected.json` | data matches the real shape |
| 3 | `install.sh`, user scope, this build's binary | install path and port recording |
| 4 | `cg-import-history -apply`, then `-backfill-projects` | pre-install history lands, with folders |
| 5 | fake upstream up; replay days 28-42 through the proxy | live capture and per-folder recording |
| 6 | read `/api/history`, `/api/facets?measured=1`, `/api/insights` per project, `/api/stats` | the numbers, independent of rendering |
| 7 | each page x each project x light/dark x desktop/400px: PNG, DOM values, console errors | what the user sees |
| 8 | `dashboard.sh --no-open` through the installed copy | the new skill |
| 9 | assert expected vs actual | verdicts |
| 10 | phases 2-6 on the 10x set, timed | import time, page load, facet speed at scale |

### Running it

```
e2e/run.sh smoke          tier 1 only
e2e/run.sh full           tier 1, then tier 2 if green
e2e/run.sh full --keep    leave the container running, dashboard on :18788
```

### Errors

- A phase that crashes stops the run. The report shows how far it got and the log tail.
- A check that fails is recorded with expected, actual, and the snapshot, and the run continues.
- The fake upstream returns usage from the manifest, so token counts repeat from run to run.
- The container is removed at the end; `results/` is kept.

## Report

```
results/
├─ summary.md      verdict, counts, every failure with expected and actual
├─ report.html     the same with screenshots inline, filterable; written locally, published nowhere
├─ expected.json
├─ actual.json
├─ snapshots/      e.g. history-pooled-light-desktop.png, cleanup-alpha-dark-phone.png
├─ logs/           per phase, plus the proxy log
├─ review.md       code review findings
└─ timings.json
```

| Verdict | Meaning |
|---|---|
| PASS | actual matches expected |
| FAIL | they differ; both values and the snapshot are shown |
| BLOCKED | could not run because an earlier phase crashed |
| KNOWN | fails, and also fails on `main`, so it is not the PR's doing; shown, not hidden |

A failing check is re-run against `main` before it is labelled, so a pre-existing bug is not blamed
on the PR, and the PR cannot hide behind one either.

## Code review

Run in parallel with the sandbox, over `main...feat/insights-per-item-inventory`. It covers
correctness; security, meaning credential leakage, path handling of transcript data (the importer
reads arbitrary user files), the test-only `home` override reaching production, and SQL built from
folder names; and design. Each finding has file:line, a severity, and a concrete failure scenario.

The review is done by a fresh reviewer agent given the diff and no context from the session that
wrote it, since a reviewer sharing the author's assumptions mostly confirms them. Its findings are
verified against the code before they go into `review.md`.

## Self-checks

The harness checks that it can fail, because three tests in the session that wrote this PR passed
for the wrong reason: one on a file header, one on a regex that crossed lines, one on a fixture that
never set the field it was testing.

1. **Mutation round.** After a green run, five breaks are made in the PR code, one at a time: move
   the self-exclusion, drop the folder rollup, let history-only projects back into the picker, return
   null timeline buckets, hardcode a port. Each must turn at least one check red. A break that stays
   green is reported as a gap in the harness.
2. **Data shape.** Generated transcripts are compared with the real files' JSON keys and entry
   types, keys only, before anything else runs.
3. **Determinism.** A fixed seed, checked by generating twice and comparing.

## Risks

| Risk | Handling |
|---|---|
| Streaming capture is different code from plain JSON | Replay sends about 80% streamed and 20% plain; the upstream answers both |
| `install.sh` downloads a release binary; this PR has none | The PR's build is placed where `install.sh` looks first, the "already present" path. The download path is left to the plugin suite in phase 1, and the report says so |
| Chromium in an arm64 container | Debian's native `chromium`; if it fails, host Chrome against `:18788`, recorded in the report |
| Generator drifts from the real format | The data-shape check fails the run first |
| The oracle is written by the same author | Small, separate from PR code, and exercised by the mutation round; the report states it is not independent of its author |

## Out of scope

- Real model calls, real keys, any cost
- The Codex plugin
- Exact dollar values
- Pixel-diff baselines
- Fixing what is found: findings are reported, and nothing is changed without asking
