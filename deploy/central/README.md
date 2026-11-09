# Central metrics stack

This folder holds the server side of the metrics spec (issue #886). Proxies push their metrics here.
A manager reads the totals of many machines in one Grafana.

It runs four containers: an OpenTelemetry Collector, Prometheus, Alertmanager and Grafana. It is
separate from the hosted context-guru box (`deploy/service/`). It serves Cortex and context-guru
proxies alike.

```
proxy --OTLP/JSON over HTTPS--> TLS front end --> Collector --remote write--> Prometheus --> Grafana
                                                                                   |
                                                                                   +--> Alertmanager
```

No machine opens a port. Each proxy pushes out. Prometheus scrapes no proxy.

## What is here

| File | What it does |
|---|---|
| `otel-collector.yml` | Receives pushes. Checks the token. Sets the labels. Sends to Prometheus. |
| `prometheus/prometheus.yml` | Receives remote write. Scrapes the stack's own parts only. |
| `prometheus/rules/recording.yml` | Pre-computed sums: net saving, 5xx ratio, cache read ratio, keep-alive net. |
| `prometheus/rules/alerts.yml` | The alerts of the spec, plus two that watch the pipeline itself. |
| `prometheus/tests/alerts_test.yml` | Unit tests for the rules, in simulated time. |
| `alertmanager/alertmanager.yml` | Routes alerts. Ships no delivery method. |
| `grafana/gen_dashboards.py` | Generates the three dashboards. Each panel is one line with its query. |
| `grafana/dashboards/*.json` | Fleet overview, Savings, Cache and keep-alive. Generated. |
| `compose.yaml` | The whole stack on one machine, for development and the end-to-end test. |
| `install.sh` | Installs the stack on a server. Also makes and revokes tokens. |
| `nginx.example.conf` | The TLS front end. |

## Install on a server

1. Copy this repository to the server. Install `podman` or `docker`.
2. Run `sudo ./install.sh install`.
3. Make one token for each install: `sudo ./install.sh token add cortex alice-laptop`. The token is
   printed **once**. Give it to that install. It is not stored in a readable form.
4. Put TLS in front (see below).
5. Run `sudo ./install.sh status`.

Each proxy needs a collector URL and its token. The URL has no default. A proxy with no URL logs one
warning and sends nothing.

| Proxy | URL | Settings |
|---|---|---|
| context-guru | `https://metrics.example.com/context-guru` | `--metrics-push-url`, `--metrics-push-token` |
| Cortex | `https://metrics.example.com/cortex` | the same two settings |

## TLS is required, and the installer does not do it

A proxy refuses to send a token over plain `http` to a remote host. Only `https`, or `http` to a
loopback address, works. The collector speaks plain HTTP. So any remote proxy needs a TLS front end.
`nginx.example.conf` is one. Copy it, set the server name and the two certificate paths, and install
`central-proxy.inc` beside it. A proxy also needs to trust the certificate. A proxy never skips the
check.

The installer binds the collector to loopback by default (`BIND=127.0.0.1`), so the front end must run
on the same host. Set `BIND=0.0.0.0` only if the front end is on another machine.

## What a token does, and what it does not

- A token lets one install push. Each proxy has its own list, on its own port, so a Cortex token does
  not work for context-guru. The `proxy` label is set by the collector. A sender cannot choose it.
- You can revoke one install without touching the others: `sudo ./install.sh token revoke cortex alice-laptop`.
- **A token does not tie a sender to one machine.** The `machine` label is what the sender says it is.
  A holder of a Cortex token can write under another Cortex machine's name. The token proves "a
  Cortex install". It does not prove "this Cortex install". For tens of machines you trust, that may be
  enough. Do not read `machine` as proof of identity.
- The collector reads the token files when it starts. A change takes effect after a restart, which
  `token add` and `token revoke` do for you. A restart takes about 1.5 seconds. A push that lands in that
  time fails, and the next push repairs it, because proxies send running totals and not changes.

## What is lost when something fails

Nothing is queued anywhere. Proxies send running totals every 30 seconds.

| Event | Effect |
|---|---|
| Collector or Prometheus down for a while | The next push after it returns carries the totals. No count is lost. The time of the increase is the time of that push, so a graph shows one step and not a slope. |
| A proxy stops without a clean exit | The counts since its last push are lost. At most about 30 seconds. |
| A machine is off for a day | Its totals resume when it is back. The days between are not in the graph. |
| A token is revoked | That install is refused from then on. Its old data stays. |

## The Grafana admin password

A first install gives the built-in `admin` account a random password that is **not** saved. Nobody
needs it at install time. Set your own, once:

```
sudo podman exec cm-grafana grafana cli admin reset-admin-password '<new password>'
```

Then add a user for each manager in Grafana (Administration, Users). There is no sign-up, and no
anonymous access. Grafana listens on `127.0.0.1:3000` only. Reach it with
`ssh -L 3000:127.0.0.1:3000 <this host>`, or put your own front end in front of it.

## Alerts

`alerts.yml` holds the alerts of the spec (section 8). The thresholds are proposals. Change them after
you have seen real data. Alertmanager ships with no delivery method, so alerts show in its own list
(`http://127.0.0.1:9093` inside the network) until you add a webhook, an e-mail or a Slack block to
`/etc/central-metrics/alertmanager/alertmanager.yml`. The installer never overwrites that file once it
exists.

## Tests

| Test | How to run | What it proves |
|---|---|---|
| Rule syntax | `promtool check rules prometheus/rules/*.yml` | The rules parse. |
| Alert and rule logic | `promtool test rules prometheus/tests/alerts_test.yml` | Each alert fires when it should and stays quiet when it should not. |
| Collector config | `otelcol-contrib validate --config=otel-collector.yml` | The config is valid. |
| Names, labels, tokens | `internal/metricspush` end-to-end tests (see `central_e2e_test.go`) | A real push arrives under the names and labels of the spec. |
| Recording rules | the same package, with `CENTRAL_SLOW=1` (about 6 minutes) | The rules compute the right number from traffic at a known rate. |

Start the stack for the end-to-end tests with `docker compose -f compose.yaml up -d`. If port 3001 is
taken on your machine, set `GRAFANA_PORT`.

## Not built yet

- The request and response byte counters. The proxies do not count them on every path.
- `source=client` on errors. The proxies do not record a client that hung up.
- Ping outcomes `timeout` and `skipped`. Those pings leave no record.
- A saving rate (net over baseline). There is no baseline counter.
- Shared-warm tokens (#853).
- Retiring the old pull path and the local Grafana (R22). That waits until these dashboards are in use.
