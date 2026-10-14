# `/context-guru:adapt cache`: design

The shell (slash command, rendering, confirm) is the plugin's `/context-guru:adapt`; this is the backend it calls (`proxy/adaptcache.go`,
`proxy/adaptreplay.go`), on the shared adapt principal, rate limit and audit of `proxy/adaptapply.go`.

## What the user gets

For the calling account, over its own history in the dashboard DB:

1. the decision, first: the best option against the default and against the current setting, held-out, paired by session (`relative`, `summary`);
2. a table of options on that same held-out slice:
   - `never`: no pings (the zero every net is measured against);
   - `default`: the shipped default, K2 at 270 s, prefix >= 80K tokens, <= $0.50 a ping, no ping after a thread's first request, plus SessionEnd;
   - `current`: the account's own policy as configured now (or its live rules campaign);
   - `K1`, `K2`, `K3`: ping-everyone blankets, for comparison;
   - adapted: `auto` (one plan for the account, shrunk toward the fleet: the auto campaign builder) and one plan per segment of
     `by_dow`, `by_hour`, `by_prefix`, `by_model`, `by_client`, `by_stop_reason` (shrunk toward the fleet the same way);
3. what the keep-alive actually did (`observed`: real pings, their cost, the misses they avoided, from the dashboard's ledger);
4. a choice: keep the default, or enforce an adapted option through the campaign builder.

## Replay

Decision points are finished, priced, cost-known responses with a cached prefix. Each is linked to the next request of the same conversation and
thread (`KVCacheOptions.ByThread`; rows written before threads existed link per conversation, as before). Responses whose outcome is not known
by the window end are not scored, so they are never counted as savings.

Two replays of the same plans:

- **Money** (`saved_usd`, `spend_usd`, `net_usd`, gateway rates): the closed form in expectation over the model parameters in
  `adaptreplay.go`. Ping j starts j x 270 s after the request start (+1 s lateness) and fires while the keeper entry it refreshes (one per thread of a
  session) has seen no new request and, with SessionEnd, before the end of the session's last response. A ping can rewrite the prefix instead of
  refreshing it; a rewrite is billed and stops the span. A request only a ping kept alive can still miss. The cliff has a spread. A rescue is worth
  the prefix the next request shares, less the head a missed request reads anyway, times (write - read), and only by the share of requests that
  would not have hit anyway after a long gap (h, fitted on the account's own history).
- **Gate units** (`engine_usd`): the builder's closed form (deterministic outcome, scaled rescue and ping values). The ship gate decides in these units.

The money column is an **upper bound** and always labelled so (`upper_bound`, a warning, `calibration`): it does not follow thread lineage
inside a conversation, nor a structural-miss rule. Agreement with `engine_usd` is no check: the calibrated closed form can run high as well.
SessionEnd is scored as always delivered at once, also an upper bound.

## Evaluation

- held-out (the table): plans are tuned on the fleet's rows resolved before the account's 70% point and scored on the account's last 30%;
  fixed options are scored on the same rows. Paired differences with 95% intervals by bootstrap over sessions (B = 1000).
- in-sample (`in_sample` of every option): the whole window, plans fitted on the same rows; optimistic for adapted plans.
- the builder gate, per adapted option (`held_out.builder_gate`): `shipGate`, the function the builder itself uses, on the same rows and units:
  net > 0 and a paired interval above zero against the default, blanket K3 and the fleet plan.
- `recommendation` is set only for an adapted option that passes the gate; otherwise `summary` says plainly that nothing beats the default, or
  what the leading option fails against, and to keep the default. `disagreement` is one line when money and the gate disagree on the leader.

## Enforcement

- `default` (or `revert`, no basis needed) switches the account's live campaign off: its own policy is then in force.
- an adapted option runs the auto build for the account on the report's window, in-process and synchronously (capped at 110 s, then 202), and
  the builder's gate decides: `ship` -> active; otherwise shadow (it decides on live traffic and changes nothing), `ok: false`,
  `force_available: true`; too little history -> off.
- `"force": true` (apply anyway, which the shell asks for separately) activates the account's OWN tuned plan (`own_policy` of the build), not the
  fleet plan a non-ship build carries. A forced campaign is not rebuilt daily: the refresh changes a live campaign only on a passing gate.
- every apply needs the basis of the account's own latest report (an empty basis means the latest; it expires after an hour), goes through the
  shared rate limit, and is audited (hosted: an audit row naming dashboard session or proxy token).

Builder changes for this: `segment_by` (one tuner scope per segment value, the policy one rules segment per value, fallback the fleet plan; the
refresh keeps it), the shipped default in the gate, `own_policy` in every evaluated spec, runtime facts `hour_utc` / `dow_utc`, SessionEnd
cancelling pings after the session's last response in any model, and successors per thread.

## Routes

Mounted beside `/api/*` when the dashboard is on. Scope: the caller's own account (`adaptPrincipal`): hosted, the dashboard cookie (a manager may
pass `?tenant=`) or the account's own proxy token; single-tenant, direct loopback only (no forwarding headers), as `/stats`.

- `GET /api/adapt/cache?since=&until=` (or `days=N`; `session_end=0`) -> `kind`, `version`, `summary`, `relative`, `class`, `method`, `basis_id`,
  `window`, `current`, `default`, `best`, `options[]`, `recommendation`, `disagreement`, `warnings`, `upper_bound`, `calibration`, `observed`,
  `population`, `assumptions`, `held_out`.
- `POST /api/adapt/cache/apply {"option", "basis", "force"}` -> `ok`, `applied`, `previous`, `effective`, `note`, `undo`, `campaign_id`,
  `recommendation`, `force_available`; 400 for an option shown for comparison only, 409 for a stale basis, 429 over the rate limit.

One report or apply runs at a time per process (429 otherwise).

## Not done

- Segment crosses (account x daypart, hour-of-week): finer segments lose most on held-out data; one feature at a time only.
- Account time zones: segments are UTC.
- Manager time-window strategies and per-session overrides are not replayed in `current`; a model campaign is reported as not replayable.
