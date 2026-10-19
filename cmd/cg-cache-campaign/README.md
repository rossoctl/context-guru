# cg-cache-campaign

Builds a keep-alive **campaign file** for the dashboard's Campaigns tab, and a **report** of what every
cache policy would have cost on the same history.

## Run it

```sh
go build -o cg-cache-campaign ./cmd/cg-cache-campaign
nice -n 19 ./cg-cache-campaign -db /var/lib/context-guru/cg.db -days 30 \
  -out campaign.json -report report.json -md report.md
```

- It opens the database **read-only** (`dash.OpenReadOnly`). It is safe beside a live proxy.
- Upload `campaign.json` in the dashboard: **Campaigns → upload a file**.
- Read `report.md`.

| Flag | Default | Meaning |
|---|---|---|
| `-db` | — | The dashboard database. Required. |
| `-days` | 30 | History to read. Retention can keep less; the report says what it read. |
| `-holdout` | 0.3 | Share of the history, at its end, used to score the campaign out of sample. 0 = skip. |
| `-baseline` | `fixed-5m` | The arm every saving is measured against. |
| `-prices` | — | A `MODEL_PRICES` table, the same file the proxy reads. |
| `-offline` | false | Do not fetch the public price map. |

## What the report compares

All numbers come from core. The tool only adds cells up.

- **`campaign`**: each (account, hour) cell runs its own best arm that a campaign can run live. This is the upload.
- **One arm everywhere** (`keepalive-5m`, `fixed-1h`, ...): the naive policies.
- **`observed-policy`**: the cache tier each request asked for.
- **`current_keepalive`**: what the proxy's live keep-alive pings measured (core's keep-alive ledger).
- **`optimal`**: the cheapest plan in hindsight. No policy can reach it.
- **`campaign-by-weekday` / `-model` / `-agent`**: the best arm per (account, hour, key). The upload format has no such key, so these show what the key is worth.
- **`campaign-if-every-arm-were-live`**: the campaign if simulation-only arms could run.

Breakdowns: by account, hour (UTC), weekday, model and agent.

## What it refuses to do

- **Write an unpriced upload.** If no model in the history has a price, it stops.
- **Report against a broken ceiling.** If any arm beats `optimal` in a cell, it stops.
- **Write a file the dashboard rejects.** The create route takes at most 256 KiB, so the upload has no per-arm candidates. They are in the report.
- **Name an arm the campaign cannot run.** Such a cell gets the best arm that can run, from core's own per-arm costs.
