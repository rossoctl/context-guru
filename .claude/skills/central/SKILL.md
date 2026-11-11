---
name: central
description: Deploy and manage the central metrics stack (OpenTelemetry Collector, Prometheus, Alertmanager, Grafana) on the RHEL 9 server. Use when asked to deploy, update, check or restart the central, set up its TLS, add or revoke a machine's push token, read its logs, or find out why a proxy's metrics do not arrive.
---

# Manage the central metrics stack

The stack lives in `deploy/central/`. `deploy/central/deploy.sh` runs it on the server over ssh. `install.sh`
in the same folder does the work on the server. Read `deploy/central/README.md` for the design.

Run every command from `deploy/central/` in the repo checkout, on the user's laptop.

## Before you start

1. Ask the user for the ssh target (`user@host`) if you do not know it. Use the same one for the whole session.
2. For TLS, you need `CENTRAL_HOST` (the name the proxies use), and `TLS_CERT` and `TLS_KEY` (paths **on the
   server**, certificate chain first). The company certificate is the expected source. Never create a
   self-signed one unless the user asks.
3. The ssh user needs sudo on the server. `deploy.sh` uses `ssh -t`, so a sudo password prompt reaches the user.

## Commands

| Task | Command |
|---|---|
| First install or an update | `./deploy.sh --dry-run USER@HOST deploy`, show the user, then `./deploy.sh USER@HOST deploy` |
| TLS front end | `CENTRAL_HOST=... TLS_CERT=... TLS_KEY=... ./deploy.sh USER@HOST nginx` |
| Is it healthy? | `./deploy.sh USER@HOST status` |
| Is it reachable from here? | `CENTRAL_HOST=... ./deploy.sh USER@HOST check` (expect 401 on both paths) |
| Add a machine | `./deploy.sh USER@HOST token add cortex\|context-guru NAME` |
| Remove a machine | `./deploy.sh USER@HOST token revoke cortex\|context-guru NAME` |
| List machines | `./deploy.sh USER@HOST token list` |
| Logs | `./deploy.sh USER@HOST logs collector\|prometheus\|alertmanager\|grafana` |
| Copy only, no install | `./deploy.sh USER@HOST stage` |
| Stop the stack | `./deploy.sh USER@HOST remove` (keeps the data) |

A deploy checks the folder first (the shell script, the dashboard JSON against its generator, and `promtool` if
it is installed). It stops if a check fails. Do not skip a failed check. Fix the cause.

## Rules

1. **Ask before you change the server.** `deploy`, `nginx`, `token add`, `token revoke` and `remove` change a
   shared server. Say what you will run, and wait for a yes. `status`, `check`, `logs`, `token list` and
   `--dry-run` are safe to run without asking.
2. **Run `--dry-run` first** for `deploy` and `nginx`, and show the user what it will do.
3. **A token is printed once, by `token add`.** Give it to the user in your reply so they can hand it to the
   install. Do not write it to a file, a commit, a PR or a log. Do not echo it again later. If it is lost, revoke
   it and add a new one.
4. **Never delete data.** Do not remove `/var/lib/central-metrics`, a container volume, or run
   `docker compose down -v` against the server. `remove` keeps the data on purpose.
5. **Revoking restarts the collector** (about 1.5 seconds). A push in that gap fails, and the next push repairs
   it because proxies send running totals. Tell the user.
6. **After a deploy, run `status`.** Report each line that has a cross. If `status` exits non-zero, the deploy is
   not done.
7. **A token does not tie a sender to one machine.** The `machine` label is the sender's claim. Do not tell the
   user that a token proves which machine sent a metric.

## Giving a machine its settings

After `token add`, tell the user what the install needs:

| Proxy | URL | Settings |
|---|---|---|
| context-guru | `https://CENTRAL_HOST/context-guru` | `--metrics-push-url`, `--metrics-push-token` (or `METRICS_PUSH_URL`, `METRICS_PUSH_TOKEN`) |
| Cortex | `https://CENTRAL_HOST/cortex` | the same two settings |

The proxy refuses plain `http` to a remote host. It trusts the system certificate store, so a company CA must
be installed on the machine.

## When metrics do not arrive

Work down this list and stop at the first cause.

1. `check` shows a TLS error: the certificate chain is incomplete, or the machine does not trust the CA.
2. `check` shows something other than 401: nginx is not forwarding. Run `nginx` again, then read the nginx error log.
3. The proxy log says `metrics push is on` but Prometheus has no `llm_proxy_info{machine="..."}`: the token is
   wrong or was revoked. The collector log shows `401`. Make a new token.
4. The proxy log says nothing about metrics: the URL is empty. With no URL the proxy logs one warning and sends
   nothing.
5. Prometheus has the machine but a dashboard is empty: widen the time range. The `rate()` panels need about
   5 minutes of traffic.
6. A dashboard says "Dashboard not found": the dashboard folder on the server is empty or stale. Run `deploy`
   again, which recreates the containers.

## Files

- `deploy/central/deploy.sh`: the laptop side. It copies a release to `~/central-metrics-src/releases/<time>` on
  the server, keeps the last 5, and runs `install.sh`.
- `deploy/central/install.sh`: the server side (`install`, `status`, `token`, `nginx`, `remove`).
- `deploy/central/grafana/gen_dashboards.py`: change a dashboard here, run it, and commit the JSON.
- State: `/var/lib/central-metrics`. Config: `/etc/central-metrics`. The token files are there, mode 0600.
