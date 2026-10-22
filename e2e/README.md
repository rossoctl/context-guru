# context-guru end-to-end harness

Installs context-guru from this checkout in a throwaway container, gives it months of **fake** Claude Code
history, sends traffic through the real proxy to a fake Anthropic upstream, and checks every number the
dashboard shows against a ground truth written before the data was generated. A PASS means the product
computed the right answer, not that it agreed with itself.

Nothing leaves the machine. There is no real model, no real key and no cost. `HOME` in the container is
`/sandbox`; your real `~/.claude`, proxy and dashboard DB are never mounted. The report is a local HTML file.

## Run it

| Command | Time | What it does |
|---|---|---|
| `make e2e-unit` | 30 s | the harness's own unit tests, on the host; no container |
| `make e2e-smoke` | ~5 min | small dataset, every phase; a gate for the rest |
| `make e2e-full` | ~45 min | smoke, then the full regression tier, then a 10x stress tier |
| `make e2e-mutate` | ~12 min | breaks the product five ways; each break must turn its check red |

Exit codes for `e2e/run.sh` (what the Make targets call):

- **0** PASS
- **1** FAIL, or a phase crashed
- **2** setup problem (image build, missing tool)
- **4** INCOMPLETE: a check could not be judged, so the run proved less than a pass

### Prerequisites

- Podman 5 or Docker, with about 8 GB free for the image
- `git`, `rsync`, `python3` (stdlib only) on the host
- for `e2e-full`: keep the machine on power with the lid open. `run.sh` holds `caffeinate` on macOS, but a
  closed lid on battery still sleeps.

`e2e/run.sh full --keep` leaves the container running with its dashboard on `http://127.0.0.1:18788/dashboard/`.

## Read the result

Each run writes `e2e/results/<UTC time>-<tier>/`. That folder is git-ignored. The main files:

- `summary.md`: the first line is `# context-guru e2e <branch>@<sha>: PASS|FAIL|INCOMPLETE`, then every failure
  with expected and actual values
- `report.html`: the same content, with every screenshot inline and filterable; it loads nothing remote
- `verdicts.json`, `expected.json`, `actual-api.json`, `actual-ui.json`, `logs/`, `snapshots/`, `db/` (a copy of
  the dashboard DB)

Each check gets one of four verdicts:

| Verdict | Meaning |
|---|---|
| PASS | actual matches the oracle |
| FAIL | it does not; both values and the snapshot are shown |
| BLOCKED | it could not be judged (an empty set, a page that never rendered). Never a silent pass |
| KNOWN | it also fails on the baseline (by default the merge base with `main`; set `E2E_BASE=<ref>` to change it) |

A failing full run is re-run on the baseline, Go suite only, so a pre-existing bug is not blamed on the branch.

## Known findings

`e2e/known-findings.txt` lists the bugs the smoke gate lets through to the full tier. Each line is:

```
<check-id globs> | <evidence>  # what it is  issue:#NNN
```

- **The check id and the evidence must both match.** A line explains a FAIL only when one of its globs matches
  the check id AND the evidence appears in that check's actual value. So a new defect under an old check id is
  not passed off as the old one.
- **Every line must cite its GitHub issue.** `report.known` refuses a line without one.
- **Delete the line once the issue is fixed.** The next run then proves the fix.

## How it works

| File | Role |
|---|---|
| `gen/manifest.py` | the seeded ground truth: projects, sessions, capabilities over a 6-week timeline |
| `gen/transcripts.py`, `gen/inventory.py` | write that as Claude Code transcripts, plugins, skills and MCP servers |
| `oracle.py` | manifest to `expected.json`; it imports nothing from the product |
| `upstream.py`, `replay.py` | a fake Anthropic API, and the post-install sessions replayed through the proxy |
| `collect.py`, `snap.mjs` | read the APIs and DB; drive headless Chromium for screenshots and DOM values |
| `check.py`, `report.py` | verdicts, then `summary.md` and `report.html` |
| `phases.sh`, `run.sh`, `timeout.sh` | in-container phases (bounded, with hang evidence), and the host driver |
| `mutate.sh`, `mutation.py`, `mutations/` | the mutation round |

## Extending it

- **New dashboard figure:**
  1. Add the rule to `oracle.py`, written from the spec, never by calling product code.
  2. Add a check in `check.py`.
  3. Add a unit test that makes the check FAIL, and one that makes it BLOCKED if it can run on an empty set.
- **New kind of capability or usage state:** add it to `gen/manifest.py`. `coverage()` in the manifest proves
  every cell is filled, from the generated data, not from labels.
- **New mutation:** add `mutations/<name>.{file,from,to,expect}`.
  - `.file`: the product file to edit.
  - `.from` / `.to`: an exact replacement in that file.
  - `.expect`: the check-id prefix that must turn red.

The oracle is independent of the product's code, but not of whoever wrote it. The mutation round is what shows
the checks can fail.
