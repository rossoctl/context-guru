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
| `otel-collector.yml` | Receives pushes. Sets the labels. Sends to Prometheus. Checks no token (authentication is off). |
| `prometheus/prometheus.yml` | Receives remote write. Scrapes the stack's own parts only. |
| `prometheus/rules/recording.yml` | Pre-computed sums: net saving, 5xx ratio, cache read ratio, keep-alive net. |
| `prometheus/rules/alerts.yml` | The alerts of the spec, plus two that watch the pipeline itself. |
| `prometheus/tests/alerts_test.yml` | Unit tests for the rules, in simulated time. |
| `alertmanager/alertmanager.yml` | Routes alerts. Ships no delivery method. |
| `grafana/gen_dashboards.py` | Generates the dashboards. Each panel is one line with its query. |
| `grafana/dashboards/*.json` | Fleet overview, Savings, Cache and keep-alive, Machine comparison, Requests, Inference. Generated. |
| `compose.yaml` | The whole stack on one machine, for development and the end-to-end test. |
| `install.sh` | Installs the stack on a server (also `status`, `nginx`, `remove`). |
| `deploy.sh` | Runs `install.sh` on the server from your laptop, over ssh. |
| `nginx.example.conf` | The TLS front end. |

## What is stored

Prometheus keeps metadata only, for 30 days by default: counters and histograms with the labels
`proxy`, `machine`, `preset`, `service_version`, `model`, `provider`, `tier`, `component` and a few
reason codes. It keeps no prompt, no response, no session ID and no host name. `machine` is a random
install ID. Anyone with Grafana access can read it. To stop the storage, remove the stack
(`install.sh remove`) and delete `/var/lib/central-metrics`. To stop one install from sending, turn its push off
(`--metrics-push=false`).

## Deploy from your laptop

`deploy.sh` does the steps below over ssh, so you need not log in to the server. It targets a RHEL 9 server
with sudo for the ssh user.

```
./deploy.sh --dry-run alice@metrics-host deploy      # show what it will do
./deploy.sh alice@metrics-host deploy                # check, copy, install, status
CENTRAL_HOST=metrics.example.com TLS_CERT=/etc/ssl/central/fullchain.pem TLS_KEY=/etc/ssl/central/privkey.pem \
  ./deploy.sh alice@metrics-host nginx               # TLS front end with your company certificate
CENTRAL_HOST=metrics.example.com ./deploy.sh alice@metrics-host check
./deploy.sh alice@metrics-host status
```

The certificate and key paths are paths on the server. `nginx` checks that the certificate is valid, matches the
key, includes its chain and names the host. It puts the new nginx files live only if `nginx -t` passes, and it puts
the old ones back if not. The Claude Code skill `central` (`.claude/skills/central/`) drives the same script.

## Install on a server

1. Copy this repository to the server. Install `podman` or `docker`.
2. Run `sudo ./install.sh install`.
3. Put TLS in front (see below).
4. Run `sudo ./install.sh status`.

Each proxy needs a collector URL. The URL has no default. A proxy with no URL logs one warning and
sends nothing. Authentication is off (see below), so no token is needed.

| Proxy | URL | Setting |
|---|---|---|
| context-guru | `https://metrics.example.com/context-guru` | `--metrics-push-url` or `METRICS_PUSH_URL` |
| Cortex | `https://metrics.example.com/cortex` | the same setting (the Cortex side is not built yet) |

## TLS is required, and the installer does not do it

A proxy refuses to push over plain `http` to a remote host. Only `https`, or `http` to a
loopback address, works. The collector speaks plain HTTP. So any remote proxy needs a TLS front end.
`nginx.example.conf` is one. Copy it, set the server name and the two certificate paths, and install
`central-proxy.inc` beside it. A proxy also needs to trust the certificate. A proxy never skips the
check.

The installer binds the collector to loopback by default (`BIND=127.0.0.1`), so the front end must run
on the same host. Set `BIND=0.0.0.0` only if the front end is on another machine.

## Authentication is off

The collector accepts any push that reaches it. A proxy may still send an `Authorization` header
(`--metrics-push-token`), and the collector ignores it. Today the protection is TLS in front of the
collector and the private network.

What this means:
- Anyone who can reach the host can write metrics, under any `machine` name. The data is metadata only,
  so the harm is wrong dashboards and wrong alerts.
- The collector cannot say which install sent a push. The `machine` label is the sender's own claim.
- Do not expose the front end beyond the network you trust.

The work to switch authentication on is tracked in rossoctl/context-guru#1130: a token for each install,
how to make and hand out tokens, binding a token to a machine or using mutual TLS, and the setup on the
consumer. `otel-collector.yml` keeps the old token blocks, commented out.

## What is lost when something fails

Nothing is queued anywhere. Proxies send running totals every 30 seconds.

| Event | Effect |
|---|---|
| Collector or Prometheus down for a while | The next push after it returns carries the totals. No count is lost. The time of the increase is the time of that push, so a graph shows one step and not a slope. |
| A proxy stops without a clean exit | The counts since its last push are lost. At most about 30 seconds. |
| A machine is off for a day | Its totals resume when it is back. The days between are not in the graph. |

## The Grafana admin password

A first install gives the built-in `admin` account a random password that is **not** saved. Nobody
needs it at install time. Set your own, once:

```
sudo podman exec cm-grafana grafana cli admin reset-admin-password '<new password>'
```

Then add a user for each manager in Grafana (Administration, Users). There is no sign-up, and no
anonymous access. Grafana listens on `127.0.0.1:3000` only. Reach it with
`ssh -L 3000:127.0.0.1:3000 <this host>`, or put your own front end in front of it.

`nginx.example.conf` also serves Grafana at `https://<host>/grafana/`, behind Grafana's own login.
For that, install with `GRAFANA_ROOT_URL=https://<host>/grafana/`. Without it, Grafana serves at the
root of port 3000 and the `/grafana/` path does not work.

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
| Names and labels | `proxyapp/metricspush` end-to-end tests (see `central_e2e_test.go`) | A real push arrives under the names and labels of the spec. |
| Recording rules | the same package, with `CENTRAL_SLOW=1` (about 6 minutes) | The rules compute the right number from traffic at a known rate. |

Start the stack for the end-to-end tests with `docker compose -f compose.yaml up -d`. If port 3001 is
taken on your machine, set `GRAFANA_PORT`.

## Not built yet

- The request and response byte counters. The proxies do not count them on every path.
- `source=client` on errors. The proxies do not record a client that hung up.
- Ping outcomes `timeout` and `skipped`. Those pings leave no record.
- The exact baseline counter (`llm_savings_baseline_usd_total`). The saving rate tiles use spend plus net saved instead.
- Authentication (rossoctl/context-guru#1130).
- Shared-warm tokens (#853).
- Retiring the old pull path and the local Grafana (R22). That waits until these dashboards are in use.
