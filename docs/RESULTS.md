# Benchmarks

**context-guru is the cheapest and highest-reward context-compaction layer on
SWE-bench Verified** — evaluated live, end-to-end, with the **claude-code** agent on
**`aws/claude-sonnet-5`**, against a no-compaction baseline, against
[**headroom**](https://pypi.org/project/headroom-ai/) (a request-stream proxy), and against
[**rtk**](https://github.com/rtk-ai/rtk) (Rust Token Killer, a shell-level Bash-output hook).

50 tasks, all of which scored under all **four** arms (zero infrastructure exceptions).

| dimension | baseline | **context-guru** | headroom | rtk |
|---|--:|--:|--:|--:|
| reward (solved / 50) | 43 | **44** | 40 | 43 |
| **billed cost** (matched total) | $31.98 | **$27.77 (−13.2%)** | $30.30 (−5.3%) | $29.09 (−9.0%) |
| cache-read tokens | 102.8M | **84.5M** | 96.4M | 91.7M |
| cache-write tokens | 1.855M | 1.847M | 1.839M | 1.835M |
| mean steps / task | 36.1 | **31.1** | 35.1 | 33.2 |
| added latency / req | — | 117 ms | 63 ms | **0 ms** |
| tool LLM cost | $0 | $0.31 | $0 | $0 |

These numbers record the SWE-bench run exactly as it happened. The shipped `codesmart`
pipeline has changed since — `toon` was added, `cachesplit` replaced `cacheinject`, and a
gating bug that kept `failed_run` inert was fixed — so a claim about today's default needs
a fresh run. See [Reproduce the results](results/REPRODUCE.md).

<!-- Static bars: same figures as the table above, one measure each, drawn in CSS.
     Bar widths are proportional to the value, zero-anchored, against the largest
     arm. No chart library and no JS, so they render identically offline. Update
     both the value and the --cg-w percentage together if a number changes. -->
<figure>
  <div class="cg-bars">
    <div class="cg-bars__row">
      <span class="cg-bars__label">baseline</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:100%"></span></span>
      <span class="cg-bars__value">$31.98</span>
    </div>
    <div class="cg-bars__row cg-bars__row--me">
      <span class="cg-bars__label">context-guru</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:86.8%"></span></span>
      <span class="cg-bars__value">$27.77</span>
    </div>
    <div class="cg-bars__row">
      <span class="cg-bars__label">headroom</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:94.7%"></span></span>
      <span class="cg-bars__value">$30.30</span>
    </div>
    <div class="cg-bars__row">
      <span class="cg-bars__label">rtk</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:91.0%"></span></span>
      <span class="cg-bars__value">$29.09</span>
    </div>
  </div>
  <figcaption>Total billed cost over the matched 50 tasks — lower is better.</figcaption>
</figure>

<figure>
  <div class="cg-bars">
    <div class="cg-bars__row">
      <span class="cg-bars__label">baseline</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:100%"></span></span>
      <span class="cg-bars__value">102.8M</span>
    </div>
    <div class="cg-bars__row cg-bars__row--me">
      <span class="cg-bars__label">context-guru</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:82.2%"></span></span>
      <span class="cg-bars__value">84.5M</span>
    </div>
    <div class="cg-bars__row">
      <span class="cg-bars__label">headroom</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:93.8%"></span></span>
      <span class="cg-bars__value">96.4M</span>
    </div>
    <div class="cg-bars__row">
      <span class="cg-bars__label">rtk</span>
      <span class="cg-bars__track"><span class="cg-bars__fill" style="--cg-w:89.2%"></span></span>
      <span class="cg-bars__value">91.7M</span>
    </div>
  </div>
  <figcaption>Cache-read tokens over the matched 50 tasks — the dominant cost term on a
  ~98%-cached agent, and where the saving actually comes from.</figcaption>
</figure>

<!-- Markdown image syntax inside the figure, not a raw <img>: MkDocs only rewrites
     relative paths it finds in Markdown, and only those are checked by
     `validation.unrecognized_links`. A raw src= here resolves against /RESULTS/ and
     404s silently. -->
<figure markdown="1">
![Six panels comparing baseline, context-guru, headroom and rtk on reward, billed cost, mean agent steps, cache-read tokens, cache-write tokens and added latency per request.](img/benchmark/headline.png)
<figcaption>All six measures at once, four arms each. Every value in this figure is
  also in the table above.</figcaption>
</figure>

**context-guru wins on cost, cache usage, steps, and reward.** It is cheaper despite
removing less *raw* content per request because it **freezes each compaction and re-applies
it byte-identically every turn**, so the reduction compounds across the session's
cache-reads while never mutating the cached prefix. The surprise is **rtk**: a simple
deterministic shell filter is the **2nd-cheapest** arm (−9.0%), **reward-neutral** (43 = 43),
at **zero request-path latency and $0 tool cost** — it **beats the headroom proxy on both
cost and reward**. Its ceiling is that it only compresses **Bash-tool** output (built-in
`Read`/`Grep`/`Glob` bypass its hook), which is why the whole-request proxy goes deeper.

## The results suite

- **[vs headroom & rtk — what we did and why it won](results/vs-competitors.md)** — the
  mechanism summary: where each design intercepts, both benchmarks side by side, and which
  design choice the money landed on. **Start here.**
- **[Full comparison](results/comparison.md)** — all four arms, cost decomposition,
  per-task plots, per-component breakdown, and the honest caveats.
- **[Component internals & real examples](results/components.md)** — how every
  context-guru component, headroom compressor, and rtk command filter works, when it
  triggers, and real before→after compactions from the run logs, side by side.
- Per-config detail (per-task tables + totals):
  [baseline](results/baseline.md) · [context-guru](results/context-guru.md) ·
  [headroom](results/headroom.md) · [rtk](results/rtk.md).
- **[Reproduce](results/REPRODUCE.md)** — install and run all four arms yourself.

Method note: cache-aware billed **input** cost = fresh $2/M · cache-read $0.20/M ·
cache-write $2.50/M (recomputed from each trial's own token tiers) + output $10/M;
**total** adds the tool's own compaction-model cost. Reward/step counts carry agent
run-to-run nondeterminism at n=1/task; the deterministic cache-write and per-component
token signals are the fully trustworthy ones.
