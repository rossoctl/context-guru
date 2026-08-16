# Grafana for context-guru

Two provisioned dashboards over the proxy's own `/metrics`, plus the Prometheus job that
feeds them. Everything here is **file-provisioned**: nothing is clicked into Grafana, so a
redeploy cannot lose it and a review can read it.

```
prometheus-scrape.yml                     the scrape job (merge into your prometheus.yml)
provisioning/datasources/prometheus.yml   the datasource, uid pinned to cg-prom
provisioning/dashboards/context-guru.yml  where Grafana looks for the JSON
dashboards/context-guru.json              the service dashboard, 6 rows
dashboards/context-guru-slo.json          the SLO dashboard, 4 rows
```

## The fast path on the box that runs the proxy

```bash
sudo deploy/service/install.sh grafana
```

That is the whole install: it needs `docker` (or `podman`), pulls Prometheus and Grafana,
writes the config under `/etc/context-guru/observability`, and starts both bound to
**loopback only**. It is idempotent — re-run it after editing a dashboard JSON and it
re-copies the files and recreates both containers, which is how the edit takes effect. The
TSDB and Grafana's database live outside the containers, so nothing is lost. Then:

```
http://127.0.0.1:3000/d/context-guru/context-guru       # ssh -L 3000:127.0.0.1:3000 to reach it
```

### The admin password

Nothing here has a default password and `admin/admin` is never set.

- **First install:** the installer generates a password and **prints it once**, as
  `admin / <value>`. It is not written to any file — copy it out of that output. Or set
  `GRAFANA_ADMIN_PASSWORD` in the environment of the `install.sh grafana` call and it uses
  yours instead.
- **Every later run:** no password is set or needed. It already lives in Grafana's own
  database under `/var/lib/context-guru/observability/grafana`, so re-running the installer
  never demands a secret and never resets one.

#### Rotating the admin password

There is no recovery, only a reset. Grafana stores a hash, so a lost password means
choosing a new one:

```bash
# Typed at a prompt and piped in on stdin: the value never reaches a command line, so it
# lands in neither your shell history nor the host process list.
read -rs NEWPW
printf '%s' "$NEWPW" | sudo docker exec -i cg-grafana \
  sh -c 'read -r pw; grafana cli admin reset-admin-password "$pw"'
unset NEWPW
# -> Admin password changed successfully ✔
```

Rotate the same way on a schedule. `GRAFANA_ADMIN_PASSWORD` is only consulted when
Grafana's database does not yet exist, so re-running the installer is *not* a rotation
path.

`sudo deploy/service/install.sh grafana-status` prints container state, scrape health and
the provisioned dashboard uids. `grafana-remove` deletes the containers (the TSDB under
`/var/lib/context-guru/observability` is kept).

## From zero on a RHEL 9 box, by hand

The subcommand above does exactly this. Reproduced so it can be audited or adapted:

```bash
# 1. a container runtime
sudo dnf -y install docker-ce docker-ce-cli containerd.io    # or: sudo dnf -y install podman
sudo systemctl enable --now docker

# 2. config and state
ETC=/etc/context-guru/observability
ST=/var/lib/context-guru/observability
sudo install -d -m0755 "$ETC" "$ETC/grafana/dashboards" \
                       "$ETC/grafana/provisioning/datasources" \
                       "$ETC/grafana/provisioning/dashboards" \
                       "$ETC/grafana/provisioning/plugins" \
                       "$ETC/grafana/provisioning/alerting"
sudo install -d -m0755 "$ST"
sudo install -d -m0755 -o 65534 -g 65534 "$ST/prometheus"   # prometheus runs as nobody
sudo install -d -m0755 -o 472   -g 0     "$ST/grafana"      # grafana runs as uid 472

# 3. our files. install -m0644 rather than cp: root's umask leaves cp output 0640, and a
#    container user that cannot read its own config crash-loops with "permission denied".
sudo install -m0644 deploy/grafana/prometheus-scrape.yml "$ETC/prometheus.yml"
sudo install -m0644 deploy/grafana/provisioning/datasources/*.yml "$ETC/grafana/provisioning/datasources/"
sudo install -m0644 deploy/grafana/provisioning/dashboards/*.yml  "$ETC/grafana/provisioning/dashboards/"
sudo install -m0644 deploy/grafana/dashboards/*.json              "$ETC/grafana/dashboards/"

# 4. Prometheus. --network=host so 127.0.0.1:4000 means the proxy, and an explicit
#    --web.listen-address because host networking would otherwise expose 9090 on every
#    interface.
sudo docker run -d --name cg-prometheus --network=host --restart=unless-stopped \
  -v "$ETC/prometheus.yml:/etc/prometheus/prometheus.yml:ro,Z" \
  -v "$ST/prometheus:/prometheus:Z" \
  docker.io/prom/prometheus:v3.2.1 \
  --config.file=/etc/prometheus/prometheus.yml \
  --storage.tsdb.path=/prometheus \
  --storage.tsdb.retention.time=90d \
  --web.listen-address=127.0.0.1:9090

# 5. Grafana. GF_SERVER_HTTP_ADDR keeps it on loopback; reach it over an ssh tunnel.
#    Choose the admin password first and pass it from the environment — never write it into
#    a file here. Only the FIRST run needs it; drop the -e line on later runs, because after
#    this the password lives in Grafana's database under $ST/grafana.
read -rs GRAFANA_ADMIN_PASSWORD && export GRAFANA_ADMIN_PASSWORD
sudo -E docker run -d --name cg-grafana --network=host --restart=unless-stopped \
  -e GF_SERVER_HTTP_ADDR=127.0.0.1 -e GF_SERVER_HTTP_PORT=3000 \
  -e GF_AUTH_ANONYMOUS_ENABLED=false -e GF_ANALYTICS_REPORTING_ENABLED=false \
  -e GF_SECURITY_ADMIN_PASSWORD="$GRAFANA_ADMIN_PASSWORD" \
  -v "$ETC/grafana/provisioning:/etc/grafana/provisioning:ro,Z" \
  -v "$ETC/grafana/dashboards:/var/lib/grafana/dashboards/context-guru:ro,Z" \
  -v "$ST/grafana:/var/lib/grafana:Z" \
  docker.io/grafana/grafana:11.6.0
```

The dashboards mount **inside** the Grafana data volume on purpose: the provider yml points
at `/var/lib/grafana/dashboards/context-guru`, and nested bind mounts are resolved
outermost-first, so the read-only dashboard directory survives inside the writable data
directory.

### Using distro packages instead of containers

`dnf install grafana` exists in EPEL and Prometheus does not; you would be mixing a
packaged Grafana with a tarball Prometheus, and then the paths differ from everything
above. If you go that way the only changes are: provisioning lives at
`/etc/grafana/provisioning`, dashboards at `/var/lib/grafana/dashboards/context-guru`, and
you set `http_addr = 127.0.0.1` in `/etc/grafana/grafana.ini` instead of the env var.

## Verify it

Four checks, in the order that isolates a failure. **Every one of these must produce the
output shown** — a healthy-looking Grafana in front of an empty Prometheus is the failure
this catches.

**1. The proxy is exporting.** Must print a number, not nothing:

```console
$ curl -s http://127.0.0.1:4000/metrics | grep '^cg_requests_total'
cg_requests_total 66
```

Refusals are exported the same way, and every reason is present at 0 rather than absent —
a `rate()` over a family that only appears once something breaks renders "No data", which
reads as healthy:

```console
$ curl -s http://127.0.0.1:4000/metrics | grep '^cg_refused_requests_total'
cg_refused_requests_total{reason="rate_limit"} 3
cg_refused_requests_total{reason="concurrency"} 0
cg_refused_requests_total{reason="spend_cap"} 1
cg_refused_requests_total{reason="auth"} 2
cg_refused_requests_total{reason="forbidden"} 0
cg_refused_requests_total{reason="no_upstream"} 0
cg_refused_requests_total{reason="upstream_error"} 0
```

**2. Prometheus is scraping it.** `health` must be `up` and the query must return a value:

```console
$ curl -s http://127.0.0.1:9090/api/v1/targets \
    | python3 -c 'import json,sys;[print(t["health"],t["scrapeUrl"],repr(t["lastError"])) for t in json.load(sys.stdin)["data"]["activeTargets"]]'
up http://127.0.0.1:4000/metrics ''

$ curl -sG http://127.0.0.1:9090/api/v1/query --data-urlencode 'query=cg_requests_total'
{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"cg_requests_total","instance":"127.0.0.1:4000","job":"context-guru","service":"context-guru"},"value":[1786755616.479,"66"]}]}}
```

An empty `result` array means the target is up but the exposition changed — check the
series names against `proxy/promexport.go`.

**3. Grafana provisioned both dashboards.** Must list exactly these two uids:

```console
$ curl -s -u admin:"$GRAFANA_ADMIN_PASSWORD" 'http://127.0.0.1:3000/api/search?type=dash-db' \
    | python3 -c 'import json,sys;[print(d["uid"],"|",d["title"]) for d in json.load(sys.stdin)]'
context-guru | context-guru
context-guru-slo | context-guru service SLO

$ curl -s -u admin:"$GRAFANA_ADMIN_PASSWORD" http://127.0.0.1:3000/api/dashboards/uid/context-guru \
    | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["dashboard"]["uid"],len(d["dashboard"]["panels"]),"panels; provisioned:",d["meta"]["provisioned"])'
context-guru 40 panels; provisioned: True
```

`provisioned: False` means Grafana loaded a hand-saved copy from its database and is
ignoring the file — delete it in the UI and restart.

**4. The panels' own queries return data**, without opening a browser. This walks every
target in both JSONs and flags any that would render "No data":

```bash
python3 - <<'EOF'
import json, urllib.parse, urllib.request, glob
for f in sorted(glob.glob('deploy/grafana/dashboards/*.json')):
    for p in json.load(open(f))['panels']:
        for t in p.get('targets', []):
            q = t['expr'].replace('$tenant', '.*')
            u = 'http://127.0.0.1:9090/api/v1/query?' + urllib.parse.urlencode({'query': q})
            r = json.load(urllib.request.urlopen(u, timeout=15))['data']['result']
            print(('ok  ' if r else 'EMPTY'), p['title'], t['refId'])
EOF
```

## Pointing it at a context-guru somewhere else

`/metrics` is a **service-wide** view: it carries every tenant's month-to-date cost. It is
open on loopback because Prometheus normally runs beside the proxy; from anywhere else it
needs a bearer token.

On the proxy host, set the token in the site drop-in (never in the shipped unit — the
installer replaces that on every run):

```bash
# Generate a value without it reaching a log. Copy it out of the terminal, then paste it
# into the editor — do not put it on a command line, where it lands in shell history.
openssl rand -hex 32

sudoedit /etc/systemd/system/context-guru.service.d/20-local.conf
#   uncomment and fill in:  Environment=METRICS_TOKEN=<the value>
sudo systemctl daemon-reload && sudo systemctl restart context-guru
```

That drop-in is created once by the installer and never overwritten, so the token survives
`install.sh install`.

On the Prometheus host, put the same value in a file readable only by Prometheus and
reference it — **not** inline in `prometheus.yml`, which is world-readable on most installs
and ends up in configuration management:

```bash
sudo install -m0600 -o prometheus -g prometheus /dev/null /etc/prometheus/context-guru.token
sudoedit /etc/prometheus/context-guru.token      # paste the same value, nothing else
```

Then enable the second job in `prometheus-scrape.yml` (it is commented out there) and point
it at the nginx front end, not port 4000 — the token crosses the network on every scrape,
so it needs TLS:

```yaml
- job_name: context-guru-remote
  scheme: https
  authorization:
    type: Bearer
    credentials_file: /etc/prometheus/context-guru.token
  static_configs:
    - targets: ['cg.example.ibm.com:443']
      labels:
        service: context-guru
```

Verify from the Prometheus host — a 403 here means the token does not match, and the body
says so plainly:

```console
$ curl -s -H "Authorization: Bearer $(sudo cat /etc/prometheus/context-guru.token)" \
    https://cg.example.ibm.com/metrics | grep '^cg_requests_total'
cg_requests_total 66
```

The dashboards need no change for a remote proxy: they query the datasource, and the
datasource is whatever Prometheus you provisioned. If you scrape several proxies into one
Prometheus, add `instance` to the panels you want split — every series already carries it.

## Reading the dashboards

**`context-guru`** — six rows, in the order you would actually ask the questions:

| Row | Answers |
|---|---|
| Is it up and healthy? | scrape reachability, running build, request rate, compaction rate, added latency |
| Am I saving tokens and money? | actual against baseline spend, token counts before/after, billed tiers, what compaction itself cost |
| Which components earn their place? | tokens removed, hit rate and time spent, per component — a component at 0% hit rate is dead weight |
| Who is using it? | spend against cap, request rate and a per-tenant table |
| Is storage healthy? | local database against cold storage, filesystem headroom, archive reachability, dropped captures |
| Is anything failing? | refusals by reason and by tenant, compaction-model failures, cache-write churn, wasted tokens, expand bounces, buffered streams, disabled tenants, tenants near their cap |

**`context-guru-slo`** — availability, the latency the service is actually responsible for,
whether the observability path itself is lossy, and the **HTTP error-rate SLI**: refused
requests over all requests, with the by-reason breakdown beside it. The denominator is
`refused + processed`, not `cg_requests_total` alone, because a refused request never
reaches the aggregator and so was never counted as processed. Above 1% somebody's agent is
failing repeatedly; above 5% it is an outage for whoever is being refused.

Every panel carries a description (hover the ⓘ) that says what a *bad* value looks like.

### Colour

Series colours are pinned per series, never auto-assigned. Grafana's classic palette cycles
hues and repaints the survivors when a series disappears, so a reader who learned "blue is
actual" gets lied to after a filter.

| Hex | Role |
|---|---|
| `#3987e5` blue | what actually happened |
| `#d95926` orange | the comparison to read it against |
| `#199e70` aqua | a healthy absolute |
| `#c98500` / `#e66767` | warning / critical status only, never a series |

**Stat and gauge panels follow one rule, and a new panel must pick a side.** The traffic
light (`#199e70` / `#c98500` / `#e66767`) means **act on this**: the panel carries a
threshold an operator should respond to — availability, added latency, filesystem in use,
buffered streams, capture loss, tenants at their cap, up/down. Our own **magnitudes carry no
verdict** and are pinned `#3987e5` blue: build, request rate, tokens removed, sessions
archived, a positive saving, tokens removed per component. So green here always means "a
threshold was met that someone might otherwise have had to act on", never merely "this
number is large" — which is why `Filesystem in use` at 68% is green and `Saved this month`
is blue. The one deliberate exception is `Time spent in each component`, pinned `#d95926`
orange because it is the cost to read the blue benefit against.

The corollary: **a threshold must never fire on a metric the upstream does not populate.**
`Cache hit ratio` reads exactly 0 against an upstream that reports no cache tiers, so it
renders a neutral `n/a` at zero rather than red. A false alarm is how a reader learns to
ignore the dashboard.

The three series hues validate colourblind-safe on Grafana's dark surface: worst all-pairs
CVD ΔE 9.4, normal-vision 20.9, all three above 3:1 contrast, in both light and dark. **No
panel uses two y-axes.** Where two measures differ in scale they get two panels — which is
why added latency and upstream latency are not on one chart: 11ms against 19s on one axis
renders the number you care about as a flat line on the floor.

Per-tenant panels are the exception to pinned colour: the tenant set is not known when
the JSON is written, so they use Grafana's `palette-classic-by-name`, which hashes the
series *name*. The hues are off-palette but stable per tenant — filtering one out never
repaints the rest. The two **by-reason refusal breakdowns** use it for the same property
rather than for the same reason: there are seven refusal reasons and only five palette
hexes, and painting five of them in the warning and critical hues would use status colour
as series colour. Name-hashing keeps each reason's hue stable while leaving the traffic
light to mean "act on this" — which on those panels is what a non-zero bar means anyway.

## Known gaps

- **These panels will not agree with the dashboard's own numbers, and that is not a bug.**
  Every `cg_*` series outside `cg_tenant_*`/`cg_dash_*`/`cg_archive_*` is counted **in this
  process, since it started, summed over every tenant**. The dashboard reads the persistent
  database and scopes to one account. So `cg_requests_total` restarts from 0 on every deploy
  (a `rate()` handles that) and will differ from the dashboard for the same window — on a
  single-tenant box only by the restart, on a shared one by the tenant scope as well. The
  `/metrics` HELP text says so per series; use `cg_tenant_*` for the persistent per-tenant
  figures. Do not reconcile the two by eye and conclude something is dropping events —
  check `cg_dash_events_total{outcome="dropped"}` instead, which is what a real capture drop
  looks like.

- **Refusals are counted, but not every 4xx is a refusal.** `cg_refused_requests_total`
  covers the ways the proxy deliberately turns a request away — 429 rate limit, 429
  concurrency, 402 spend cap, 401/403 auth, 502 (no upstream configured, or the upstream
  call failed). It does NOT count a malformed body (400) or an oversized one (413), and it
  does not cover the control-plane endpoints under `/api/`, which write their status
  directly rather than through `failAuth`. So the error-rate SLI is a *proxy-path* error
  rate; a client sending garbage does not appear in it.
- **The per-tenant breakdown is bounded at 2048 tenant×reason series.** Past that the
  process-wide totals keep counting and the per-tenant detail stops. The registry already
  bounds the tenant set, so this is a backstop rather than the real bound.
- **`cg_cache_hit_ratio` reads exactly 0** against upstreams that report no cache tiers in
  their usage block (IBM LiteLLM does not). That is the upstream's silence, not a cache
  miss, so the panel renders a neutral `n/a` there instead of a red zero. The cost is that a
  genuine collapse to exactly 0 would also read `n/a`; the metric cannot tell the two apart,
  and only the upstream can fix that.
- **`Availability, 30 days` is meaningless until Prometheus has retained 30 days.**
  `avg_over_time(up[30d])` averages the samples that exist, so a Prometheus started an hour
  ago reports a flattering 100%. The panel description says so; there is no way to make the
  query itself honest about a window it has no data for.
- **Per-tenant series are month-to-date rollups** declared as counters. They reset at the
  first of the month; `rate()` handles the reset, but a `increase()` over a window spanning
  the boundary undercounts.
- **No alert rules are provisioned.** The thresholds live in the panels, which is where an
  operator reads them, not where a pager reads them. The panels most worth alerting on are
  cache hit ratio, dropped capture events and filesystem in use.
