# cmdfilter

!!! info "Offload — lossy, reversible"
    Shrinks tool output with declarative DSL filters, stashing the original and appending a recovery hint only when the filter was actually lossy.

## How it works

`cmdfilter` shrinks tool output with **declarative DSL filters** (see
[The DSL filter engine](dsl.md)). It matches a filter on the output's **first six non-empty lines**
(the selector), applies its 8-stage pipeline, stashes the original, and appends a recovery hint only
when the filter was actually lossy — typed by *what* was lost. Filters are tried in descending
`priority`, then by name. It is `Enabled` only when ≥1 filter is loaded.

Deterministic filtering costs nothing — no LLM call, ~0 latency — and it is cache-safe: it acts on
the newest tool output, in the mutable tail.

### Selectors match OUTPUT, not commands

The shipped filters are adapted from [rtk](https://github.com/rtk-ai/rtk) (Apache-2.0 — see
`THIRD-PARTY-NOTICES`), with one systematic rewrite. rtk is a shell hook: it matches a **command
string** (`^terraform\s+plan`). A proxy never sees the command — it sees the *result*. So every
filter here matches an **output-shape signature** against the output's first few non-empty lines
(`^Refreshing state`, `^> Task :`, `^==> Downloading`). rtk's command regexes are not portable as
written; copied over they would compile fine and never fire.

The selector spans the first **6** non-empty lines, not one, and that detail is load-bearing.
Measured on real agent traffic, a one-line selector missed 112 pytest runs (311 KB) outright: the
harness prepends its own preamble (`Exit code 1`, `Internet access disabled`) or the report opens
with a bare `ERROR path::test`, so pytest's session banner is never line 1. A one-line selector ties
a filter's reach to the agent's *output framing* rather than to the tool that produced the output.
Widening it took claimed output from 13 to 124 of 520 eligible outputs (2.5% → **23.8%**). It stays
at 6 lines on purpose: a whole-blob scan would let a generic pattern match some incidental line deep
inside unrelated output, which is the opposite failure. Match regexes compile with `(?m)`, so `^`
and `$` anchor per line.

That is also the structural advantage: rtk's hook only sees Bash calls, so an agent's built-in
`Read`/`Grep`/`Glob` tools are invisible to it. A proxy sees every tool result regardless of origin.

A `TestEveryBuiltinFilterHasTestsAndRoutes` guardrail asserts every filter ships ≥1 inline test and
that each test's input actually routes to *that* filter, so a selector rewrite is verified rather
than hoped for.

### Size floor

Below `min_size` bytes (default **400**) `cmdfilter` doesn't filter at all — below it a win is
implausible enough that it isn't worth the work or the stash.

The floor is a measured value, not an inherited constant. Sweeping it over two captured request
streams replayed through `/compact` (44-request Terminal-Bench, 1795-request SWE-bench):

| floor | TB acted / unique tokens | SWE acted / unique tokens | SWE `marker_no_win` |
|------:|-------------------------:|--------------------------:|--------------------:|
| 500 | 13 / 391 | 305 / 1,290 | 97 |
| **400 (shipped)** | **36 / 483** | **389 / 1,447** | **97** |
| 300 | 36 / 483 | 424 / 1,467 | 117 |
| 250 | 36 / 483 | 424 / 1,467 | 118 |
| 200 | 36 / 483 | 512 / 1,481 | 118 |
| 150 | 36 / 483 | 512 / 1,481 | 118 |

400 is where the evidence stops paying: it takes the entire Terminal-Bench win and 82% of the
SWE-bench one, and it is the last value at which the never-worse guard rejects nothing new. Below
400 the unique saving flattens while `marker_no_win` climbs — the floor and the guard start
refusing the same rewrites, on ever-smaller outputs where a ~12-token marker is a larger share of
the content.

**Tuning it yourself:** watch `components.cmdfilter.gates.below_min_size` against
`gates.marker_no_win` in `/stats`. Turning the floor down is only free while `marker_no_win` stays
flat. Judge the change on `saved_tokens_unique`, not `saved_tokens` — the latter re-counts the
same compaction every turn the agent re-sends its transcript.

## The shipped filter set

26 filters. Compression is measured on each filter's own fixtures (its inline tests), summed:

| filter | family | preserves | drops | saved |
|---|---|---|---|--:|
| `pytest` | tests | failures, summary line | `PASSED` lines, progress dots, session header | 57% |
| `npm-install` | pkg | added/removed counts, vulnerability counts | `npm warn`, spinner lines | 46% |
| `make` | builds | compiler lines, errors | `Entering/Leaving directory`, `Nothing to be done` | 56% |
| `gradle` | builds | executed tasks, test results, BUILD result | `UP-TO-DATE`/`NO-SOURCE`/`FROM-CACHE` tasks, daemon + download chatter | 49% |
| `xcodebuild` | builds | errors, warnings, test results, BUILD result | 31 build-phase and tool-invocation patterns | 62% |
| `gcc` | builds | **every** error and warning, with its source context | include-chain traces, `N warnings generated` counters | 21% |
| `swift-build` | builds | diagnostics; collapses a clean build to `ok` | `Compiling`/`Linking` lines | 29% |
| `dotnet-build` | builds | diagnostics; collapses a clean build to `ok` | MSBuild banner, restore chatter | 54% |
| `turbo` | builds | task output, errors | cache hit/miss/bypass, scope + duration lines | 66% |
| `nx` | builds | task output, errors | `> NX Running target`, log links, rule bars | 75% |
| `terraform-plan` | iac | planned changes, `Plan:` line, no-change result | `Refreshing state`, state locks, `# (N unchanged …)` | 53% |
| `terraform-init` | iac | the initialization result, errors | provider download/install lines | 72% |
| `pulumi` | iac | resource rows, outputs, resource counts, error messages | banners, permalinks, per-resource progress rows, JS stack frames | 59% |
| `liquibase` | builds | version, changeset status, errors | ASCII banner, jar inventory, INFO chatter | 76% |
| `ssh` | net | the remote command's output | `debug1:` flood, host-key and connection banners | 52% |
| `ping` | net | the statistics block, timeouts | per-packet replies | 62% |
| `rsync` | net | errors; collapses a clean sync to `ok` | file list, byte counters | 48% |
| `bundle-install` | pkg | installs, conflicts; collapses a complete bundle | `Using <gem>` lines, metadata fetch | 81% |
| `poetry-install` | pkg | lock writes, solver errors; collapses an up-to-date lock | download/install lines, virtualenv chatter | 70% |
| `composer-install` | pkg | lock writes, warnings; collapses a no-op install | download/install lines | 75% |
| `pip-install` | pkg | the install manifest, real warnings (PATH, scripts), errors | `Collecting`/`Downloading`/`Requirement already satisfied` chatter, the root-user nag, pip-upgrade notices | 64% |
| `uv-sync` | pkg | the installed-package list; collapses an audited-only sync | download/cache lines | 51% |
| `apt` | pkg | `E:`/`W:`/`N:` lines, dpkg errors, prompts | `Setting up`/`Unpacking`/`Get:`/trigger boilerplate | 76% |
| `brew-install` | pkg | the install summary; collapses an already-installed formula | download/pour/progress lines | 59% |
| `latex` | builds | `Overfull`/`Underfull` diagnostics, `!` errors, output summary | TeX engine banner, absolute distribution package paths, font-map page markers, transcript notice | 72% |
| `quarto-render` | builds | errors, warnings; collapses a successful render | per-file processing and pandoc lines | 54% |

`terraform-plan` and `make` additionally assert a **≥60% floor** on a realistic large fixture
(`TestCompressionFloors`), matching the floors rtk asserts for its equivalents.

### Two filters came from measurement, not from rtk

`apt` and `gcc`'s widened selector are not ports — they came from replaying the shipped
selectors over a recorded Terminal-Bench tool-output dump and counting what matched nothing. Two
shapes dominated the misses:

- **apt/dpkg install boilerplate** — 584 outputs, ~1.0 MB, the single largest reachable family on
  that benchmark. rtk has no apt filter at all. 76% compression, and pure boilerplate collapses to
  one line.
- **`<file>: In function 'main':`** — gcc's diagnostic *header* line, 108 outputs. rtk's `gcc` filter
  matches the command, so its patterns never had to name this shape; ported as-is, the filter would
  miss the most common way gcc output starts.

Between them those two shapes carry roughly **73% of the live savings** on that dump.

Meanwhile the filters that had been *planned* as Tier 1 — `pulumi`, `terraform-plan`, `xcodebuild`,
`gradle` — fired **zero** times on it. They are kept: they are correct, tested and cost nothing when
inert. But the honest reading is that a filter set's value is decided by the workload, not by its
size, and the prediction about which filters would matter was wrong. The
`cmdfilter_selector_misses` ledger exists so that stays measurable rather than assumed.

That 73% / zero-fire split is Terminal-Bench tool output; a repo full of Terraform would invert
it. Read the ledger on *your* traffic before pruning a filter.

### A cautionary note on strip rules

The `apt` filter originally stripped `^debconf: `, which also swallowed
`debconf: unable to initialize frontend` — a real diagnostic. `TestAptKeepsProblems` catches that
class of mistake by asserting a list of must-survive lines against a wall of boilerplate. Any new
strip rule on a high-volume filter should be run past a list like it.

### Collapsing to one line is safe two different ways

Collapsing output to a one-line summary is the dangerous operation in this component: in a proxy the
agent **cannot re-run the command** to find the warning that got swallowed. Two mechanisms collapse,
and they are safe for different reasons. `TestCollapseInvariants` enforces both against the shipped
filter data, so a new filter must satisfy whichever it uses.

**1. `match_output` — guarded.** Nine of rtk's eleven success-collapse rules are unguarded: a build
that emits a warning *and* a success marker collapses to `ok` and the warning is gone. rtk learned
this itself (its `swift-build` test is named "warnings not swallowed when Build complete present").
Every collapse rule here carries an `unless` naming the diagnostics that veto the collapse, plus an
explicit negative test proving a warning + success marker does **not** collapse.

**2. `on_empty` — structural, and this is the common case.** `on_empty` fires only when
`strip_lines_matching` removed *everything*. **12 of the filters collapse this way** — `apt`,
`pytest`, `make`, `gcc`, `gradle`, `xcodebuild`, `pulumi`, `terraform-plan`, `terraform-init`,
`liquibase`, `turbo`, `npm-install` — and none carries an `unless`, because it would be redundant.
Every strip list is an **explicit allow-list of known boilerplate prefixes with no catch-all**, so an
unrecognised line is simply not stripped, the output is therefore not empty, and the collapse never
fires.

Verified behaviourally on the heaviest-firing filter, `apt`:

| input | output |
|---|---|
| boilerplate + `E: Unable to locate package nginx` | `E: Unable to locate package nginx` |
| boilerplate + `W: Failed to fetch … 404 Not Found` | `W: Failed to fetch … 404 Not Found` |
| boilerplate + `dpkg: error processing package nginx` | `dpkg: error processing package nginx` |
| clean boilerplate only | `apt: install ok` |

This is arguably the **stronger** of the two designs: a guard enumerates what to *fear*, while an
allow-list enumerates what is *known-harmless*, so it stays safe against diagnostics nobody
anticipated. `apt`'s deliberate exclusion of `^debconf: ` shows the shape of the reasoning — that
prefix would swallow `debconf: unable to initialize frontend`, so only the specific delaying notice
is stripped.

The failure mode the test exists for: adding `.*` or `^.*$` to any strip list would silently void the
guarantee, and **nothing else would catch it** — the filter's own inline tests would still pass,
because they exercise the boilerplate it was written for.

`dotnet-build`'s guard is worth noting: dotnet prints `0 Error(s)` on success, so a guard on the
word "error" would never let it collapse. It guards on the diagnostic *form* instead
(`error CS1002` / `warning CS0168`).

### Ordering: `priority`, because a wider selector shadows

A multi-line selector lets a generic filter claim output a specific one should own. That is a real
hazard, and `TestEveryBuiltinFilterHasTestsAndRoutes` catches it: it asserts every filter's own test
input routes to *that* filter. Resolved with explicit `priority`:

- **20** — tool-identity banners (`make`, `apt`, `terraform-*`, `pulumi`, the package managers …).
  Unambiguous, so they win.
- **10** — `pytest`, matched on its report shapes.
- **-10** — `gcc`. Its selector is the *generic* `file:line:col: error:` diagnostic shape, which also
  occurs inside make, swift and dotnet output. It is the deliberate last resort for "some compiler
  said something" when no tool-specific filter claimed the output.

### A selector must key on tool IDENTITY, not a generic verb

Found in a live run: `swift-build`'s `^Compiling ` claimed **Cython** output and stripped its
`Compiling x.pyx because it changed` lines. Cython, cargo and others all print `Compiling`. The
selector now requires Swift identity (a `.swift` file or a Swift build phase), and
`TestSelectorsDoNotClaimForeignOutput` asserts Cython and cargo output are not claimed. Prefer a
signature no other tool emits over a verb that many do.

### Line budgets: shared `cap` classes

Filters select a budget by **signal density** (`cap: errors`) rather than each hand-picking a
`max_lines`, so the whole set is tunable from one map (`dsl.Caps`). See
[the DSL engine](dsl.md#line-budgets-cap-classes).

## What is deliberately NOT ported

rtk ships 63 DSL filters and ~50 native Rust ones. 26 ship here — the table above is the
authoritative list, and `apt` plus `gcc`'s widened selector came from measurement rather than
from a port. The rest is excluded on purpose:

- **The ~24 `truncate_lines_at`-only filters** (`df`, `ps`, `du`, `jq`, `jira`, `markdownlint`,
  `yamllint`, `stat`, `gcloud`, `helm`, `iptables`, `skopeo`, `yadm`, `hadolint`, …). Their whole
  "filter" is "strip blank lines + cap line width" — whole-blob lossy for a modest, unmeasured
  saving. If that effect is wanted, one generic width-cap filter beats 24 files.
- **The blank-line-only linter filters** (`shellcheck`, `systemctl-status`, `sops`, `fail2ban`,
  `basedpyright`, `ty`, `oxlint`, `biome`, `mix-format`, `tofu-fmt`, `tofu-validate`). Same reason;
  the useful part is their `on_empty` collapse, which one or two generic linter filters can carry.
- **`spring-boot`** — a `keep_lines_matching` allowlist over unbounded application logs. Too easy
  to drop the one line that mattered, and nothing recovers it but a full expand.
- **`filter_stderr`** — no proxy analogue; by the time output reaches a proxy the streams are
  already merged.
- **rtk's 86 command-detection rules** — entirely about rewriting shell commands to `rtk <cmd>`.
  Irrelevant to a proxy. (Its per-tool `savings_pct` figures are hand-estimated, not measured, so
  they are not reused as expected-gain data either.)
- **rtk's ~50 native Rust filters** (43,850 lines: `--format json` → parse → re-render for cargo,
  rubocop, golangci, ruff, phpstan; TRX/binlog parsing; git diffstat compression). Its
  highest-compression technique, but inexpressible in the DSL and mostly unreachable from a proxy
  that cannot inject `--format json` into a command it never sees. The output-side half — a JSON or
  TRX blob that *arrives* as a tool result and could be re-rendered — remains open.

## Observability

Savings are attributed **per command family** and per filter in `/stats`, cumulative and unique
(deduped by content key, since the agent re-sends history verbatim every turn):

```json
"cmdfilter_families": { "iac": {"acts": 3, "saved_tokens": 640, "saved_tokens_unique": 340} },
"cmdfilter_filters":  { "terraform-plan": {"acts": 2, "saved_tokens": 600, "saved_tokens_unique": 300} },
"cmdfilter_selector_misses": [ {"selector": "Some unrecognized first line", "count": 7} ]
```

`cmdfilter_selector_misses` is the **ledger of output shapes that matched no filter**, ranked by
frequency (after rtk's `parse_failures` table). It makes "which filter to write next" data instead
of guesswork. The ledger is bounded at 200 distinct selectors.

## Before → After

```
before:  terraform plan … 40 "Refreshing state" lines + state locks + unchanged-attribute comments
after:   Terraform will perform the following actions:
           # aws_instance.web will be created
         Plan: 1 to add, 0 to change, 0 to destroy.
         <<cg:…>> [full output: call context_guru_expand]
```

## Lossiness

Lossy but reversible — the original is stashed and recovered via `context_guru_expand` /
`GET /expand`. A recovery hint is appended only when the filter actually dropped content, and it is
**typed by what was lost**: a clean contiguous tail cut names the cut point (cheap partial recovery),
a whole-blob loss points at the expand tool. See [the DSL engine](dsl.md#lossiness).

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `filters` | `[]` | Inline filter YAML docs, added with no recompile. |
| `disable_builtins` | `false` | Disable the shipped filter set and run only your own. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |
| `min_size` | `400` | Byte floor; smaller outputs are left alone. See [the size floor](#size-floor). |

## When it shines

Noisy but structured command output: build tools, test runners, package managers, IaC plans, verbose
network clients.

## When it's inert

Output whose first line matches no filter (logged as a selector miss), output under `min_size`, or
filtering that doesn't shrink the message once the marker is counted.

See also: [Components overview](../components.md) · [The DSL filter engine](dsl.md) ·
[Write a custom DSL filter](../how-to/custom-dsl-filter.md) ·
[Choose a preset](../how-to/choose-a-preset.md)
