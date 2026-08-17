# Grafana for context-guru

Three provisioned dashboards — two over the proxy's own `/metrics`, one over its logs —
plus the Prometheus job and the Loki/promtail pair that feed them. Everything here is
**file-provisioned**: nothing is clicked into Grafana, so a redeploy cannot lose it and a
review can read it.

```
prometheus-scrape.yml                     the scrape job (merge into your prometheus.yml)
loki.yml                                  the log store: single binary, filesystem, 30d
promtail.yml                              tails the JSON log sink into Loki
context-guru.logrotate                    rotation for that sink (copytruncate; see the file)
provisioning/datasources/prometheus.yml   the datasource, uid pinned to cg-prom
provisioning/datasources/loki.yml         the datasource, uid pinned to cg-loki
provisioning/dashboards/context-guru.yml  where Grafana looks for the JSON
dashboards/context-guru.json              the service dashboard, 6 rows
dashboards/context-guru-slo.json          the SLO dashboard, 4 rows
dashboards/context-guru-logs.json         the log dashboard, filterable by tenant/session/level/component
```

## Why Loki

Grafana stores no logs of its own, so something had to. Loki wins on one property that
matters more than any feature comparison: **it is read through the Grafana that is already
running here**, so an operator who sees a latency spike on the service dashboard clicks
across to the log lines for the same tenant and the same minute without changing tool,
credential or mental model — and there is exactly one thing to secure, back up and explain.
It is also the only candidate whose storage model matches what we actually have: a
single-binary process indexing nothing but a handful of labels and keeping compressed
chunks on the local filesystem, which is the same shape as the Prometheus beside it and
costs one more container. The alternatives lose on that same axis, not on capability.
Elasticsearch (or OpenSearch) is the strongest search engine of the three and would
full-text index every line — it also wants a JVM, several gigabytes of resident memory and
an index lifecycle policy, on a box whose job is serving agent traffic, and it puts the
logs behind a second UI. ClickHouse is a better analytical store than Loki will ever be,
but reading logs is not an analytics workload and it would still need a Grafana datasource
plus a schema we maintain. Plain files with `journalctl` and `grep` — the genuinely lazy
option, and the one we keep as the fallback — cannot answer "every line for this tenant's
session across the last three days" without an ssh session and a human, which is precisely
the question this exists for. Loki is chosen for being the smallest thing that answers it,
and the ranking would flip the day we needed real full-text search over log bodies.

## The fast path on the box that runs the proxy

```bash
sudo deploy/service/install.sh grafana
```

That is the whole install: it needs `docker` (or `podman`), pulls Prometheus, Grafana, Loki
and promtail, writes the config under `/etc/context-guru/observability`, installs the
logrotate snippet, and starts all four bound to **loopback only**. It is idempotent —
re-run it after editing a dashboard JSON and it re-copies the files and recreates the
containers, which is how the edit takes effect. The TSDB, the log store and Grafana's
database live outside the containers, so nothing is lost. Then:

```
https://<host>/grafana/d/context-guru/context-guru            # through the front end, manager only
https://<host>/grafana/d/context-guru-logs/context-guru-logs  # the logs
http://127.0.0.1:3000/grafana/d/context-guru/context-guru     # or ssh -L 3000:127.0.0.1:3000
```

### Reaching it: the manager gate

Grafana binds loopback and nginx is the only thing that talks to it. The front end
publishes it at **`/grafana/`**, behind two doors:

1. **Ours.** nginx `auth_request` asks the proxy's `GET /api/authz/grafana` first, which
   answers 204 only for a `cg_dash` browser session belonging to a **manager** — the same
   `webPrincipal` + `IsManager()` the control plane uses, so there is no second notion of
   who an administrator is. Anyone else gets nginx's own 401/403 and Grafana never sees the
   request. Not even its login page is reachable, which is the point: that form would
   otherwise be a password-guessing target in front of every tenant's spend. A proxy token
   does not work here — cookie only, like every other control-plane route.
2. **Grafana's.** Its own login, still enabled, with `GF_AUTH_ANONYMOUS_ENABLED=false`.

So a manager needs to be signed in at `/dashboard/` *and* have Grafana's admin password.
Sub-path support (`GF_SERVER_ROOT_URL`, `GF_SERVER_SERVE_FROM_SUB_PATH=true`) is set by the
installer; without both, every asset Grafana generates 404s.

`/api/authz/grafana` ships with the nginx config but only takes effect when the proxy
restarts, and the gate fails **closed** until it does: a manager sees 401 too. Check with
`grep -c api/authz/grafana /usr/local/bin/context-guru-proxy`.

### The admin password

Nothing here has a default password and `admin/admin` is never set.

- **First install:** the installer generates a password and **prints it once**, as
  `admin / <value>`. It is not written to any file that outlives the install — copy it out
  of that output. Or set `GRAFANA_ADMIN_PASSWORD` in the environment of the
  `install.sh grafana` call and it uses yours instead.
- **How it is passed, and why not `-e`:** through `docker run --env-file`, a 0600
  root-owned file on the `/run` tmpfs that is deleted the moment the container exists.
  `-e GF_SECURITY_ADMIN_PASSWORD=…` would put the password in the installer's own argv,
  and `/proc/<pid>/cmdline` is world-readable — any local user running `ps auxww` during
  the install would read it, and Grafana's admin sees every tenant's month-to-date cost.
  If an install predating this ever ran on your box, treat that password as disclosed and
  rotate it below.
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
sudo install -d -m0755 -o 10001 -g 10001 "$ST/loki" "$ST/promtail"  # both run as 10001
sudo install -d -m0750 -o cg    -g cg    /var/log/context-guru      # the JSON log sink

# 3. our files. install -m0644 rather than cp: root's umask leaves cp output 0640, and a
#    container user that cannot read its own config crash-loops with "permission denied".
sudo install -m0644 deploy/grafana/prometheus-scrape.yml "$ETC/prometheus.yml"
sudo install -m0644 deploy/grafana/loki.yml "$ETC/loki.yml"
sudo install -m0644 deploy/grafana/promtail.yml "$ETC/promtail.yml"
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

# 5. Loki, the log store. loki.yml pins its listen address to loopback, which host
#    networking would otherwise leave on every interface.
sudo docker run -d --name cg-loki --network=host --restart=unless-stopped \
  -v "$ETC/loki.yml:/etc/loki/local-config.yaml:ro,Z" \
  -v "$ST/loki:/loki:Z" \
  docker.io/grafana/loki:3.4.2 -config.file=/etc/loki/local-config.yaml

# 6. promtail, which tails the proxy's JSON sink into Loki. The log directory read-only;
#    its own state directory writable, for the positions file that stops a restarted
#    promtail re-shipping the whole log.
sudo docker run -d --name cg-promtail --network=host --restart=unless-stopped \
  -v "$ETC/promtail.yml:/etc/promtail/config.yml:ro,Z" \
  -v "$ST/promtail:/var/lib/promtail:Z" \
  -v /var/log/context-guru:/var/log/context-guru:ro,Z \
  docker.io/grafana/promtail:3.4.2 -config.file=/etc/promtail/config.yml

# 7. Grafana. GF_SERVER_HTTP_ADDR keeps it on loopback — nginx publishes it at /grafana/,
#    and ROOT_URL + SERVE_FROM_SUB_PATH are what make its redirects and asset URLs agree
#    with that path. Only the FIRST run needs a password; drop that line on later runs,
#    because after this it lives in Grafana's database under $ST/grafana.
#
#    Every variable goes through an env-FILE, and that is about the one secret among them:
#    `-e GF_SECURITY_ADMIN_PASSWORD=…` puts the password in this shell's argv, and
#    /proc/<pid>/cmdline is world-readable. 0600 on the /run tmpfs instead, deleted as soon
#    as the container exists — the runtime has copied the values into it by then.
read -rs GRAFANA_ADMIN_PASSWORD
EF=$(sudo mktemp /run/cg-grafana-env.XXXXXX) && sudo chmod 0600 "$EF"
sudo tee "$EF" >/dev/null <<EOF
GF_SERVER_HTTP_ADDR=127.0.0.1
GF_SERVER_HTTP_PORT=3000
GF_SERVER_ROOT_URL=https://$(hostname -f)/grafana/
GF_SERVER_SERVE_FROM_SUB_PATH=true
GF_AUTH_ANONYMOUS_ENABLED=false
GF_ANALYTICS_REPORTING_ENABLED=false
GF_SECURITY_ADMIN_PASSWORD=$GRAFANA_ADMIN_PASSWORD
EOF
unset GRAFANA_ADMIN_PASSWORD
sudo docker run -d --name cg-grafana --network=host --restart=unless-stopped \
  --env-file "$EF" \
  -v "$ETC/grafana/provisioning:/etc/grafana/provisioning:ro,Z" \
  -v "$ETC/grafana/dashboards:/var/lib/grafana/dashboards/context-guru:ro,Z" \
  -v "$ST/grafana:/var/lib/grafana:Z" \
  docker.io/grafana/grafana:11.6.0
sudo rm -f "$EF"; unset EF
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
you set `http_addr = 127.0.0.1`, `root_url` and `serve_from_sub_path = true` in
`/etc/grafana/grafana.ini` instead of the env vars.

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
cg_refused_requests_total{reason="auth"} 2
cg_refused_requests_total{reason="no_provider_key"} 0
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

**3. Grafana provisioned every dashboard.** Note the `/grafana` prefix on its API too —
`SERVE_FROM_SUB_PATH` moves the whole application, not just the UI, and the bare paths
answer 301:

```console
$ curl -s -u admin:"$GRAFANA_ADMIN_PASSWORD" 'http://127.0.0.1:3000/grafana/api/search?type=dash-db' \
    | python3 -c 'import json,sys;[print(d["uid"],"|",d["title"]) for d in json.load(sys.stdin)]'
context-guru | context-guru
context-guru-logs | context-guru logs
context-guru-slo | context-guru service SLO

$ curl -s -u admin:"$GRAFANA_ADMIN_PASSWORD" http://127.0.0.1:3000/grafana/api/dashboards/uid/context-guru \
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

## The logs

### Turning DEBUG on for this deployment

The whole point of DEBUG here is the per-component decision line: which gate declined a
component, on what numbers, for which tenant and session. It is about 8x the line count of
INFO — measured, not guessed: the same agent traffic produced 337 lines at DEBUG and 40 at
INFO through an 8-component pipeline. So it is the level to run while investigating, not
the level to leave on.

**The exact command.** Use the drop-in, not the shipped unit — the installer replaces the
unit on every run and would silently undo an edit there:

```bash
sudoedit /etc/systemd/system/context-guru.service.d/20-local.conf
#   add:  Environment=CG_LOG_LEVEL=debug
sudo systemctl daemon-reload && sudo systemctl restart context-guru
```

Confirm it took, without guessing:

```console
$ systemctl show -p Environment context-guru | tr ' ' '\n' | grep CG_LOG
CG_LOG_FILE=/var/log/context-guru/proxy.jsonl
CG_LOG_LEVEL=debug

$ journalctl -u context-guru -n1 -o cat | python3 -c 'import json,sys; print(json.load(sys.stdin)["logs"])'
stderr + /var/log/context-guru/proxy.jsonl, level DEBUG, format json
```

Back to normal: delete the line, `daemon-reload`, `restart`. There is no live level
switch — a restart costs every active tenant one cold cache prefix, which is a real cost
but a smaller one than a runtime endpoint that can turn on 8x logging without an audit
trail.

Running the proxy by hand needs no unit at all:

```bash
CG_LOG_LEVEL=debug context-guru-proxy --preset codesmart      # text on stderr, nothing shipped
```

### The four questions, as LogQL

Paste these into Explore, or use the provisioned dashboard's Tenant / Level / Session /
Component fields, which build them for you.

```logql
# 1. Everything one user's agent did, newest first.
{job="context-guru", tenant="eab4d2de01e89d63"}

# 2. One conversation, end to end. In hosted mode the resolved session id is
#    tenant-scoped (`<tenant>:<agent session>`), so match on a substring.
{job="context-guru"} | json | session=~".*sess-alpha.*"

# 3. Why did this component do nothing? The gate names and counts are the same
#    Report.Gates map /stats sums into components.<name>.gates.
{job="context-guru", level="DEBUG"} | json | msg="cg.component", component="extract_llm"
  | line_format "{{.session}} {{.verdict}} saved={{.saved}} gates={{.gates}}"

# 4. What are we refusing, and to whom?
sum by (reason) (count_over_time({job="context-guru"} | json | msg="cg.refused" [5m]))
```

`tenant` and `level` are Loki **labels**; `session` and `component` deliberately are not.
Every distinct label-value combination is a stream, so a session label would mint one
stream per agent conversation and eventually take the index down. They are read out of the
JSON at query time instead — which is what the two textbox fields on the dashboard do.

Single-tenant proxies log `tenant="local"`: there is no tenant id, and an empty label is
dropped by promtail, which would leave the selector empty for exactly the people running
this on a laptop.

### Nothing leaves the box unless you ask

The proxy never talks to Loki. It writes lines to the journal always, and to
`CG_LOG_FILE` as JSON when that is set; promtail is what ships. So a local proxy with no
containers is fully logged and ships nothing, and `CG_LOG_PLAIN=1` opts out of all of it —
plain `log/slog` text on stderr, no file, and (say it plainly) **no credential
scrubbing**, because that is what "use the standard logger instead" means.

### Verify the log path

Three checks, in the order that isolates a failure.

**1. The proxy is writing JSON.** Must print a line, and `level` must be `DEBUG` if you
turned it on:

```console
$ sudo tail -1 /var/log/context-guru/proxy.jsonl | python3 -m json.tool | head -6
{
    "time": "2026-08-16T19:13:18.581054577Z",
    "level": "INFO",
    "msg": "cg.request",
    "tenant": "1b3aa926c98b6b85",
```

**2. Loki has the lines, and knows the tenants.** A count of 0 with traffic flowing means
promtail is not shipping — `docker logs cg-promtail`:

```console
$ curl -sG http://127.0.0.1:3100/loki/api/v1/query \
    --data-urlencode 'query=sum(count_over_time({job="context-guru"}[10m]))' \
    | python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else "EMPTY")'
333

$ curl -s 'http://127.0.0.1:3100/loki/api/v1/label/tenant/values'
{"status":"success","data":["1b3aa926c98b6b85","eab4d2de01e89d63"]}
```

`sudo deploy/service/install.sh grafana-status` runs both of these plus the container
states.

**3. The log dashboard's panels return data**, without opening a browser. Same shape as
the Prometheus check below, against Loki:

```bash
python3 - <<'EOF'
import json, time, urllib.parse, urllib.request
end = int(time.time()); start = end - 3600
for p in json.load(open('deploy/grafana/dashboards/context-guru-logs.json'))['panels']:
    for t in p.get('targets', []):
        q = (t['expr'].replace('$tenant', '.*').replace('$level', '.*')
             .replace('$session', '').replace('$component', '.*')
             .replace('$search', '').replace('$__range', '1h'))
        u = 'http://127.0.0.1:3100/loki/api/v1/query_range?' + urllib.parse.urlencode(
            {'query': q, 'start': f'{start}000000000', 'end': f'{end}000000000',
             'limit': 10, 'step': '60'})
        r = json.load(urllib.request.urlopen(u, timeout=30))['data']['result']
        print(('ok   ' if r else 'EMPTY'), p['title'], '| series', len(r))
EOF
```

`Errors` reading EMPTY is the one expected empty: it counts ERROR lines, and a healthy
proxy has none.

## Reading the dashboards

**`context-guru`** — six rows, in the order you would actually ask the questions:

| Row | Answers |
|---|---|
| Is it up and healthy? | scrape reachability, running build, request rate, compaction rate, added latency |
| Am I saving tokens and money? | actual against baseline spend, token counts before/after, billed tiers, what compaction itself cost |
| Which components earn their place? | tokens removed, hit rate and time spent, per component — a component at 0% hit rate is dead weight |
| Who is using it? | month-to-date spend, request rate and a per-tenant table |
| Is storage healthy? | local database against cold storage, filesystem headroom, archive reachability, dropped captures |
| Is anything failing? | refusals by reason and by tenant, compaction-model failures, cache-write churn, wasted tokens, expand bounces, buffered streams, disabled tenants |

**`context-guru-logs`** — the lines themselves, and the one thing the metric dashboards
cannot show: WHY. Four numbers across the top (lines, requests, warnings, errors), the log
rate by level, refusals by reason, component verdicts, a table of **which gate declined
candidates per component**, and the log panel underneath. Tenant and Level are Loki labels;
Session and Component are free-text over the JSON. Most of it needs `CG_LOG_LEVEL=debug` —
see "The logs" above.

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
buffered streams, capture loss, up/down. Our own **magnitudes carry no
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
  concurrency, 401/403 auth, 502 (no upstream configured, or the upstream
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
- **The log dashboard is nearly empty at INFO, and that is not a fault.** Component
  verdicts and the gate table come from DEBUG lines. At INFO you get the one line per
  request, refusals, and whatever degraded — which is the right default, because DEBUG is
  about 8x the volume. The panel descriptions say which need it.

- **Loki's retention is enforced by its compactor, and it deletes by stream age.** A
  30-day `retention_period` means a chunk is dropped once it is 30 days old, not that the
  last 30 days are guaranteed present: the filesystem filling up is still the filesystem
  filling up. `du -sh /var/lib/context-guru/observability/loki` is the number to watch, and
  the logrotate snippet bounds the source file rather than the store.

- **A rotated log file loses a few lines from Loki, never from the journal.** The rotation
  uses `copytruncate` because the proxy holds the file open for its lifetime and has no
  reopen path; lines written between the copy and the truncate are lost from the file. See
  deploy/grafana/context-guru.logrotate for why that trade beats the alternative, which is
  a log that silently stops growing.

- **No alert rules are provisioned.** The thresholds live in the panels, which is where an
  operator reads them, not where a pager reads them. The panels most worth alerting on are
  cache hit ratio, dropped capture events and filesystem in use.
