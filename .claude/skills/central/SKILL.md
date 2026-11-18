---
name: central
description: Deploy and manage the central metrics stack (OpenTelemetry Collector, Prometheus, Alertmanager, Grafana) on the RHEL 9 server. Use when asked to deploy, update, check or restart the central, set up its TLS, read its logs, or find out why a proxy's metrics do not arrive.
---

# Manage the central metrics stack

The stack lives in `deploy/central/`. `deploy/central/deploy.sh` runs it on the server over ssh. `install.sh`
in the same folder does the work on the server. Read `deploy/central/README.md` for the design.

Run every command from `deploy/central/` in the repo checkout, on the user's laptop.

## Before you start

1. Ask the user for the ssh target (`user@host`) if you do not know it. Use the same one for the whole session.
   If the user says not to use ssh, do not run `deploy.sh`. Give the user the commands to run instead.
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
| Is it reachable from here? | `CENTRAL_HOST=... ./deploy.sh USER@HOST check` (expect 200 on both paths) |
| Logs | `./deploy.sh USER@HOST logs collector\|prometheus\|alertmanager\|grafana` |
| Copy only, no install | `./deploy.sh USER@HOST stage` |
| Stop the stack | `./deploy.sh USER@HOST remove` (keeps the data) |

A deploy checks the folder first (the shell script, the dashboard JSON against its generator, and `promtool` if
it is installed). It stops if a check fails. Do not skip a failed check. Fix the cause.

## Rules

1. **Ask before you change the server.** `deploy`, `nginx` and `remove` change a shared server. Say what you will
   run, and wait for a yes. `status`, `check`, `logs` and `--dry-run` are safe to run without asking.
2. **Run `--dry-run` first** for `deploy` and `nginx`, and show the user what it will do.
3. **Never delete data.** Do not remove `/var/lib/central-metrics`, a container volume, or run
   `docker compose down -v` against the server. `remove` keeps the data on purpose.
4. **After a deploy, run `status`.** Report each line that has a cross. If `status` exits non-zero, the deploy is
   not done.
5. **Authentication is off.** The collector accepts any push that reaches it, so do not tell the user that a
   push proves which machine sent it. The `token` commands are off. The work to switch authentication on is
   tracked in rossoctl/context-guru#1130. Do not make tokens by hand.
6. **A redeploy restarts the collector** (about 1.5 seconds). A push in that gap fails, and the next push
   repairs it because proxies send running totals. Tell the user.

## Giving a machine its settings

An install needs only the URL:

| Proxy | URL | Setting |
|---|---|---|
| context-guru | `https://CENTRAL_HOST/context-guru` | `--metrics-push-url` or `METRICS_PUSH_URL` |
| Cortex | `https://CENTRAL_HOST/cortex` | the same setting (the Cortex side is not built yet) |

The proxy refuses plain `http` to a remote host. It trusts the system certificate store, so a company CA must
be installed on the machine.

## When metrics do not arrive

Work down this list and stop at the first cause.

1. `check` shows a TLS error: the certificate chain is incomplete, or the machine does not trust the CA.
2. `check` shows something other than 200: nginx is not forwarding. Run `nginx` again, then read the nginx error log.
3. The proxy log says `metrics push failed` with a reason: read the reason. `unauthorized` means the server
   still checks tokens, because it runs an older release. Redeploy.
4. The proxy log says nothing about metrics: the URL is empty. With no URL the proxy logs one warning and sends
   nothing.
5. Prometheus has the machine but a dashboard is empty: widen the time range. The `rate()` panels need about
   5 minutes of traffic.
6. A dashboard says "Dashboard not found": the dashboard folder on the server is empty or stale. Run `deploy`
   again, which recreates the containers.

## Files

- `deploy/central/deploy.sh`: the laptop side. It copies a release to `~/central-metrics-src/releases/<time>` on
  the server, keeps the last 5, and runs `install.sh`.
- `deploy/central/install.sh`: the server side (`install`, `status`, `nginx`, `remove`).
- `deploy/central/grafana/gen_dashboards.py`: change a dashboard here, run it, and commit the JSON.
- State: `/var/lib/central-metrics`. Config: `/etc/central-metrics`.
