<div align="center">

# Context Guru

**Lower agent costs. Preserve the context that matters.**

[Get started](#get-started) · [Docs](https://rossoctl.github.io/context-guru/) · [Benchmarks](docs/RESULTS.md) · [Contribute](CONTRIBUTING.md)

</div>

Context Guru is an open-source proxy that reduces tool output, manages prompt caching, times compaction, and helps identify unused tool declarations. Keep your agent and workflow; route requests through the proxy.

**Reduce API costs with no loss in quality, through better context and cache management.**

[Benchmark results](docs/RESULTS.md)

> **[Demo video — 20–30 seconds]** A tool result before and after reduction, followed by the dashboard showing cost savings and cache reuse.

## Four ways to save

### 1. Smaller tool outputs

**[XX%] fewer tool-output tokens**

Remove unnecessary spaces, tabs, indentation, and line breaks from JSON formatting without changing its values. Strip terminal color codes and overwritten progress updates. These deterministic changes preserve information and require no model calls.

Additional filters reduce repeated results and noisy logs, keeping offloaded originals available for recovery while retained in the store.

### 2. Better cache management

**[8%] lower input cost with cache management**

Reusing cached context costs less than rebuilding it after it expires. Context Guru helps preserve reusable prefixes and can keep eligible caches warm during idle periods, within a configured spending limit.

The context stays intact. Savings come from cache reuse, after accounting for the cost of keeping it warm.

### 3. Compaction at the right time

**[XX%] lower input cost with compaction**

Long conversations eventually need compaction. Waiting until the context limit is reached can miss a cheaper opportunity to summarize while the original context is still cached.

Context Guru uses context size and cache state to decide when compaction should run. Reusing a warm prefix can reduce the cost of summarization, while the shorter history reduces input costs on later turns.

### 4. Find unused MCP tools and skills

**[XX%] fewer tool-declaration tokens**

MCP servers can expose dozens of tools. Their names, descriptions, and parameter schemas occupy context even when never used—including tools left over from other projects or configurations.

Context Guru shows which tools and skills you use and what their declarations cost. You choose what to remove, so future requests carry less unnecessary context. Removal is opt-in; an infrequently used tool may still matter.

*Percentages use separate baselines and are not additive. Token reduction does not equal bill reduction. See [measurements and configuration](docs/RESULTS.md).*

## Preserve quality

Lossless formatting preserves data, and cache reuse keeps the context intact. For filtering, summarization, and tool removal, we measure task success alongside cost.

**[No observed drop in task success on BENCHMARK and user use: YY% with Context Guru vs. ZZ% baseline, across N tasks and R runs on K benchmarks.]**

## Get started

Requires Go 1.26.4 or newer and Make:

```bash
git clone https://github.com/rossoctl/context-guru.git
cd context-guru
make build
./bin/context-guru-proxy --preset codesafe --dashboard
```

In another terminal, route your configured Claude Code client through the proxy:

```bash
export ANTHROPIC_BASE_URL=http://localhost:4000/anthropic
claude
```

Open http://localhost:4000/dashboard/ to inspect changes and cost estimates. This preset enables deterministic reduction. Keep-alive, summarization, and declaration removal require separate configuration.

[Claude Code plugin](docs/how-to/install-plugin.md) · [Other clients](docs/integrations.md) · [Choose a preset](docs/how-to/choose-a-preset.md)

## Contributing

Try it on your workload, report a problem, or contribute a filter or integration. See [CONTRIBUTING.md](CONTRIBUTING.md).

[Apache-2.0](LICENSE).