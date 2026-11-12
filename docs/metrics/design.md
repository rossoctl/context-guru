# Metrics reporting: design

Status: draft, updated to match the build (accountant#59 merged, context-guru#974 open; #1086 was closed and joined #974). Spec: [spec.md](spec.md). Issue: #886.

This design says how to build the requirements in the spec. It covers the code path from the `accountant` result to the central collector. It does not change the requirements. If the two disagree, the spec wins.

Facts below come from the `accountant` source (`accountant.go`, `internal/engine/result.go`, `internal/engine/types.go`). The context-guru proxy code was read and built against. The Cortex code was not read; Cortex wiring is not built. Items marked **Check** still need that reading.

## 1. What the `accountant` gives today

1. There is no hook. The library has `Compute`, which returns a `Result`. A host reads the `Result` and acts on it.
2. `Result` holds tokens by tier (`input`, `cacheWrite5m`, `cacheWrite1h`, `cacheRead`). It holds no money. The package comment says the host turns tokens into money.
3. `PricedOp` has `Operator`, `ChangeID`, `Saved`, `Caused`, `Call`, `DryRun` and `Estimated`. It has no `Kind`.
4. `Result` has `CacheEffects`, `Unpriced` (with a machine-stable reason) and `Checks` (failed checks).
5. A test (`boundary_test.go`) fails if the module imports anything outside the standard library (R23).

## 2. Decisions

| Topic | Decision | Why |
|---|---|---|
| Process boundary | R46 decides. Use a separate process only if it satisfies R46. | The requirement is that metrics do not change the request response time. The place of the code is a means, not a goal. |
| Starting point | Build the in-process library first, as the cheapest way to test R46. | `Observe` only adds to in-memory counters, after the response. Built and measured: the calls run in the handler's deferred `finish` and add about 12 to 35 microseconds at p50 (cost alone: p50 22 microseconds). The test machine is too noisy to state a p99. If a later benchmark breaks R46, move the work to a separate process. |
| Separate process | Accept it only if the benchmark shows that it meets R46. | It adds a socket write on each request. That write must never block. |
| Pricing | No shared rate table. Each proxy gives the module a `Pricer` (R47). | The two proxies already have their own tables. A `usd` figure belongs to the proxy in the `proxy` label. |
| Hook location | `Observer`, `Observation` and `Multi` live in the `accountant/metrics` module, not in the `accountant`. | The hook is only useful to a metrics consumer. The `accountant` keeps its pure `Compute` and gets one change only. A host with another consumer declares the same one-method interface itself (Go interfaces are structural). |
| Exporter | A hand-written OTLP/JSON exporter on the standard library, in each proxy. | The standard OTLP exporter adds about 12 MB (gRPC and protobuf). The JSON form is plain HTTP and the collector accepts it. `depguard_test.go` fails if those packages enter the default build. |
| Collector URL | No default. With no URL, the proxy logs one warning and reports nothing (R19). | Data leaves a machine only after the operator names a collector. |
| Metric units | The unit is in the name (`usd`, `tokens`). No OTel unit `USD`. | The exporter appends a unit as written, so unit `USD` gave `_USD_total`. Found on the real stack and fixed. |
| `Kind` | Add one field, `PricedOp.Kind`, copied from the input `Operation` when the library builds the result. | It is a pure copy and adds no import. It needs no second argument and no join by `OpID`. |

## 3. Parts

```
proxy
  Components pipeline  ->  accountant (Compute)  ->  Result
                                                        |
                              observer.Observe(Observation)   (after the response)
                                                        v
                         accountant/metrics  ->  OTel API  ->  OTel SDK  ->  push (background; in the proxy)
```

### 3.1 In `accountant` (standard library only)

- `PricedOp.Kind`, copied from `Operation.Kind` when the library builds the result. This is the only change. It adds no import, so R23 holds.
- The library never calls an observer. The host calls it after `Compute`. Pricing stays pure (R24).

`Observer` and `Multi` are in the `metrics` module (section 3.2).

### 3.2 New module `accountant/metrics` (OpenTelemetry API only)

- `type Observer interface { Observe(Observation) }`, with `type Observation struct { Meta RequestMeta; Usage Usage; Result Result }`. `Multi(observers ...Observer)` calls each in order, so the metrics, the local store and any proxy-only metric see the same data. It skips a nil observer, recovers a panic in one and goes on, and returns nil if none is left. Rule for every observer: return fast, make no network call, and do not keep the slices of the `Result`.
- `metrics.New(meter metric.Meter, p Pricer, opts ...Option) (*Sink, error)` builds every instrument once.
- `type Pricer interface { Price(model string, at time.Time, tier Tier) (usdPerToken float64, estimated bool) }`.
- `Observe` does this for each op in `Result.Ops`:
  1. Skip the op if `DryRun` is true.
  2. Add `Saved` to the saved counters and `Caused` to the added counters, by `kind`, `tier` and `component`.
  3. Book money only when `Billing.Mode` is `api`. Price both with the `Pricer` and add to the `usd` counters. For any other mode, move the token counters only, and count the request as estimated.
  4. If the `Pricer` says the rate is a fallback, or the op is `Estimated`, add to the estimated counters.
  5. For a spend op (a component's own LLM call), book the call's usage as `added`, priced with `Call.Model`. Skip it when the library refused to price that call.
- `component` is the operator name without the host prefix. A name outside the host's list becomes `other`. With no list, the first 64 names are kept and the rest become `other`, with no lock past that limit.
- The tokens that no op owns on a rescued request (`Result.Interaction` minus `InteractionCaused`) go to `llm_savings_interaction_usd_total{direction}`. It is a breakdown. Never add it to `saved` or `added`.
- Cache effects go to `llm_savings_cache_effect_usd_total` only (and only for `api` billing). They are never added to `saved` or `added` (no double counting, spec section 5.2).
- Each entry of `Unpriced` adds 1 to `llm_savings_unpriced_total{reason}`. Each entry of `Checks` adds 1 to `llm_savings_check_failures_total{check}`.
- The inference, keep-alive and cache-health instruments are **not** in the module yet. context-guru builds them in `proxy/inferencemetrics.go` (`NewInferenceMetrics`, `Observe(event, elapsed)`), under the shared `llm.*` names. Cortex must build the same names, or the code moves into the module when Cortex is wired. **Open.**

### 3.3 In each proxy: the push setup (OpenTelemetry SDK and exporter)

The SDK, the exporter, the collector URL and the token are settings of the host, not of the `accountant`. Each proxy owns a small package for them (`proxyapp/metricspush` in context-guru). `accountant/metrics` stays API-only, and the `accountant` repo has no network code. The cost is about 150 lines that both proxies hold. R26 is about the metric code, which stays shared, so it still holds.

- `Start(ctx, Config)` returns a meter provider and a shutdown function.
- `Config` holds the collector URL (no default; with none, one warning and no metrics, R19), the token (R44), the interval (30 s), the off switch, and the resource attributes `proxy`, `service.instance.id` (the `machine` install ID, R43), `service.version` and `preset`.
- The exporter is hand-written OTLP/JSON on the standard library. It sends cumulative sums, gauges and histograms. It follows the OTLP/JSON rules: int64 values and times are decimal strings, temporality is cumulative (2), and attribute values are typed.
- A counter of failed pushes is kept in `llm_proxy_export_failures_total{reason}`.
- It refuses a collector URL that is not `https`, except for a loopback host (R44).
- If the push is off, it returns a no-op provider and makes no connection.
- The install ID is 32 random hex characters in a file with mode 0600. It is not the host name (R43).
- Because the two copies must behave the same, both proxies run the same test file (a failing collector, a hanging collector, the off switch, the URL rule). Cortex still has to write its copy.

### 3.4 In each proxy (a few lines)

1. At startup: call its own `Start`, then `metrics.New(provider.Meter(...), pricer)`.
2. After `Compute`: call `observer.Observe(...)`, through `metrics.Multi` if there is more than one consumer. Keep-alive pings call it too, from the proxy's ping path.
3. At the end of each response: record the inference metrics (`InferenceMetrics.Observe`). Skip `/compact` rows, which are not inference requests.
4. At shutdown: call the shutdown function with a short timeout.

The proxy keeps its own `Pricer` and its own extra metrics under its own prefix (R28).

## 4. Latency (R46)

| Rule | How |
|---|---|
| A fixed small cost on the request path | `Observe` and `RecordInference` only call `counter.Add` and `histogram.Record`. They make no network call, no disk write and no blocking lock. |
| Nothing before the first byte | The proxy calls both after the last byte is sent. A stream's time to first byte is not touched. |
| A failure does not reach the request | The proxy wraps both calls in a recover. A panic is counted in `llm_proxy_component_failures_total` and the request goes on. |
| The push is off the path | The SDK exports from its own goroutine with a 10 s timeout. A hung or refusing collector delays no request. |
| The metric does not blame the provider | `llm_inference_added_seconds` stops its clock before the metrics calls. |
| Proof | The latency benchmark in the spec (section 10) runs with metrics on and off, with a collector that answers, hangs and refuses. It reports p50 and p99 for the total time and for the time to first byte. |

## 5. Failure and loss

- The SDK aggregates in memory. It keeps no queue of points. A failed push loses no count: the next push carries the totals.
- If the proxy stops without a clean shutdown, the counts since the last push are lost. At a 30 s interval this is a small, bounded loss.
- A machine that is offline sends its totals when it is back. The totals are correct. The time of the increase is the push time (spec section 12).

## 6. Tests

| Test | Checks |
|---|---|
| Same data points | One `Result` gives the same token data points from either proxy. Uses an in-memory `ManualReader`. |
| Conservation | The per-op sums equal the totals. |
| Dry run | A dry-run op adds nothing. |
| Unpriced | An unpriced op adds nothing to `saved` or `added`, and adds 1 to `unpriced_total` (R36). |
| Label rule | No label key outside R30 appears on any instrument. |
| Cardinality | A typical session stays under the budget (R42). |
| Boundary | `accountant` still imports only the standard library (R23). `accountant/metrics` imports only the OTel API. |
| Latency | The R46 benchmark. |
| Names | The names Prometheus shows match the spec tables. |

## 7. Differences between the spec and the library

Fix these in the spec, or map them in the module, before the work starts.

1. **Cache effect kinds.** The library has `break`, `rescue`, `split`, `ttlPremium`, `ttlRescue` and `markPremium`. The spec (section 5.2) lists the first five, in snake case. `markPremium` is missing. The module maps it to `mark_premium`, a value of `effect`. **Open:** add it to the spec table. The module maps the camel case names to snake case.
2. **Check names.** The library has `bad_usage`, `ratio_range`, `breakpoints`, `media_tokens`, `size_tokens` and `size_range`, which the spec's `check` list does not name.
3. **Interaction tokens.** `Result.Interaction` and `Result.InteractionCaused` are tokens that no op owns (a rescued request). The module prices the difference and books it in `llm_savings_interaction_usd_total{direction}`. **Open:** add it to the spec table.
4. **Compaction spend.** R45 books the cost of a component's own LLM call as `added`. The library carries such a call in `PricedOp.Call` for the host to price. **Check** that both proxies fill `Call.Usage` from the call's response.
5. **Cortex wiring.** **Check** that Cortex can call `Compute` after its cost settle step and that its savings step (`settle.Avoided`) can be replaced. The diagram assumes both. This may change the savings figures that Cortex users see.

## 8. Central stack

This part runs on the central server, not on the user machines. The spec gives the requirements (R15, R17, R19, R44, R11, sections 5.6 and 5.7). It is built in `deploy/central/` (context-guru#1086, open). Sections 8.2 to 8.6 say what was built, and 8.8 says what the real stack showed.

### 8.1 Pipeline

```
proxy SDK --OTLP/HTTPS--> OTel Collector --remote write--> Prometheus --> Grafana
                                                                |
                                                                +--> Alertmanager
```

### 8.2 OTel Collector

Use the contrib distribution. The core one lacks the remote-write exporter.

| Block | Setting | Why |
|---|---|---|
| Receiver | `otlp`, HTTP only. One receiver and one port for each proxy: 4318 for Cortex, 4319 for context-guru. TLS is done by a front end (nginx) in front of both ports. | R15: HTTPS, outgoing from the machine only. A separate port for each proxy lets the collector set `proxy` from the port, so a sender cannot choose it. |
| Authentication | **Off in the first deployment** (context-guru#1130 tracks it). The receivers accept any push. The `bearertokenauth` blocks (one token file for each proxy, one token for each install) stay in the config, commented out. | R44 is the target. Off keeps the first deployment simple; the cost is that anyone who can reach the host can write metrics. |
| Processor `memory_limiter` | First in the chain. | A burst from many machines must not stop the collector. |
| Processor `transform` | Set `proxy` from the receiver. Set `machine` from `service.instance.id`. Keep four labels only: `proxy`, `machine`, `preset`, `service_version`. Blank the scope. | R17, R42. Without this the exporter adds `instance`, `service_instance_id` and `otel_scope_name`, and no `machine`. The drop keeps a bad proxy from adding series. A new proxy needs a new receiver and token file (R11). |
| Processor `batch` | Default size, 10 s timeout. | Fewer writes to Prometheus. |
| Exporter | `prometheusremotewrite`, with resource attributes converted to labels. | R17. |
| Own metrics | The collector exposes its own metrics. Prometheus scrapes them. | An alert can say that the pipeline is down. |

The SDK sends cumulative totals (section 5). The remote-write exporter needs cumulative data. Do not set delta temporality.

### 8.3 Prometheus

1. Start with `--web.enable-remote-write-receiver`.
2. Set the retention to 30 days (open question 9 in the spec). Set a size limit too.
3. Load one rules file with the recording rules (spec section 7) and one with the alert rules (spec section 8, Alerts).
4. Scrape three targets only: the collector, Prometheus itself, and Alertmanager. It no longer scrapes any proxy (R22).
5. Do not open the port to the internet. Grafana and the collector are the only clients.

### 8.4 Grafana

1. Provision the Prometheus data source from a file. Do not click it together.
2. Provision the dashboards from `deploy/central/grafana/dashboards/`: fleet overview, savings, and cache and keep-alive. They are generated by `gen_dashboards.py`, so each panel is one line with its query. The 4 files in `deploy/grafana/` stay until R22 step 7, then they move or go.
3. Grafana has its own users and an admin password. There is no sign-up and no anonymous access. A first install seeds a random admin password that is not saved. The operator sets their own once. Single sign-on is not built. Give read access only to the named managers and operators (R44).

### 8.5 Alertmanager

1. Route the alerts in spec section 8 (Alerts) to the on-call channel.
2. Add two alerts about the pipeline: no push from a machine that pushed last week, and a rising `llm_proxy_export_failures_total`.

### 8.6 How it is installed

Today `deploy/service/install.sh grafana` installs Prometheus, Grafana, Loki and Promtail. It uses containers (podman or docker), because Prometheus is in no RHEL 9 repository. It copies the configs from `deploy/grafana/` to `/etc`, creates the state directories, and starts the containers on loopback only. `grafana-status` and `grafana-remove` are its partners. The central stack follows the same method.

| Item | As built |
|---|---|
| Server | A separate server, not the hosted context-guru box (`deploy/service/`). It serves Cortex and context-guru alike. |
| Images | Containers, pinned by version in `install.sh` (`OTELCOL_IMAGE` contrib, `PROM_IMAGE`, `ALERTMANAGER_IMAGE`, `GRAFANA_IMAGE`). No image of our own: all our changes are config files. |
| Config files | `deploy/central/`: `otel-collector.yml`, `prometheus/prometheus.yml`, `prometheus/rules/` (11 recording rules, 10 alerts), `prometheus/tests/` (`promtool` unit tests), `alertmanager/alertmanager.yml`, `grafana/`. |
| Script | `deploy/central/install.sh` with `install`, `status`, `nginx` and `remove` (the `token` commands are off). `deploy/central/deploy.sh` runs it from a laptop over ssh. It does not extend `deploy/service/install.sh`. `remove` keeps the data. |
| Secrets | None for the push while authentication is off. The Grafana password goes to the container in an env file on a tmpfs, not in argv. |
| Ports | The collector publishes 4318 and 4319 on loopback by default (`BIND`). The TLS front end on the same host forwards to them. Prometheus and Alertmanager are on a private container network. Grafana is on `127.0.0.1:3000` only. |
| Order at start | Prometheus, Alertmanager, the collector, Grafana. |
| Developer machine | `deploy/central/compose.yaml` starts the same four images with the same config and no TLS. The end-to-end tests in `proxyapp/metricspush` use it. The production path is the script. |
| Kubernetes | Not in scope. |

### 8.7 Operate

- **Open:** who owns the central server and who is on call. The spec does not say.
- The installer does not set up TLS. `nginx.example.conf` is an example. A proxy refuses to push over plain `http` to a remote host.
- One command must start the whole stack on a developer machine (the compose file). The end-to-end test uses it.
- Back up the Prometheus data directory if the history matters. The proxies hold no history to replay (section 5).
- Rotate the TLS certificate on a schedule. Token rotation comes with the authentication work (#1130).

### 8.8 What the built stack showed

1. **Unit in the name.** An OTel unit `USD` made four cost metrics end in `_USD_total`. The unit now sits in the name as `usd`.
2. **Labels.** The collector added `instance`, `service_instance_id` and `otel_scope_name`, and no `machine`. The `transform` processor now keeps exactly four labels.
3. **(Authentication is off today; this is the finding behind #1130.) A token does not tie a sender to a machine.** The collector sets `proxy` from the port, but `machine` is the sender's claim. A holder of a Cortex token can write under another Cortex machine's name. The token proves "a Cortex install", not "this Cortex install". **Open:** is that enough for tens of trusted machines (R43, R44)?
4. **Revoke needs a restart.** The collector reads the token files once, at start. `token add` and `token revoke` restart it (about 1.5 s). A push in that gap fails, and the next push repairs it, because the totals are cumulative.
5. **Atomic token list.** A failed `chown` in a first version of `token revoke` removed the whole list. The new file is now written beside the old one and renamed over it.
6. **Keep-alive value.** The rule counted only the saved side of a rescue. It now nets the read cost: `saved - added`.
7. **Cardinality.** The whole machine measured 1,325 series (budget under 2,000, R42; the spec estimate is about 1,700). A histogram is 14 series for each label set.
8. **Loss.** A push lost during a collector outage is repaired by the next one. The total ended at the expected value.

## 9. Order of work

1. `accountant`: add `PricedOp.Kind`, with tests. No OTel. Add `Observer`, `Observation` and `Multi` in the `accountant/metrics` module.
2. `accountant/metrics`: the savings set, with an in-memory reader test.
3. In the first proxy: the push setup (section 3.3), the failure counter and the off switch.
4. One proxy: wire it, add the inference and health sets, run the latency benchmark.
5. The second proxy: wire it with its own `Pricer`.
6. Central stack (section 8), including `deploy/central/` and `deploy/central/install.sh`: collector, Prometheus, recording rules, Grafana dashboards, Alertmanager and alerts. Start this at step 3, because step 4 needs a collector to test against.
7. Remove the pull path (R22).

This follows the delivery plan in the spec.

Status:

| Step | State |
|---|---|
| 1 | Built and merged: accountant#59 (merge commit 541e0fe). context-guru pins 6085d46, a commit on accountant `main` with no tag. It needs a tag (the nested module `metrics/` needs its own, for example `metrics/v0.1.0`) before a release. |
| 2 | Built and merged: accountant#59. |
| 3, 4 | Built for context-guru: #974 (open). Cortex is not wired. |
| 5 | Not started. |
| 6 | Built: in #974 (it was #1086). Authentication is off; see #1130. |
| 7 | Not started. Waits until the dashboards are in use. |

Step 4 measured p50 at 12 to 35 microseconds. p99 is not measured, because the noise is too large, so R46 is not proven. No separate process is built for now. Keep that option open until a p99 is measured against a chosen budget.

Not built: request and response byte counters, `source=client` on errors, ping outcomes `timeout` and `skipped`, shared-warm tokens (#853).
