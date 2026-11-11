# Predictive keep-alive campaigns

A learned policy for the keep-alive: when a response ends, estimate how likely the conversation is to come back inside each
ping window, and buy exactly as many pings as pay for themselves. Built per tenant from that tenant's own history, tried in
**shadow** (decides on live traffic, changes nothing), then switched to **active**.

Off by default. Nothing runs until a manager builds a campaign for a tenant and switches it to `shadow` or `active`.

## Turning it on

1. Build a campaign for one tenant (manager session, audited): `POST /api/cache/campaigns/build` with
   `{"scope":"tenant","id":"<tenant id>","window":"30d","builder":"auto"}`. `builder:"auto"` needs nothing beyond the proxy;
   `builder:"model"` (the default) runs the Python training pipeline and needs `CG_CAMPAIGN_PYTHON` pointing at a Python with numpy, pandas, lightgbm and scikit-learn.
2. Read the result: `GET /api/cache/campaigns/{id}`. `recommendation` is `ship`, `baseline` or `insufficient_data`.
3. Switch it on: `POST /api/cache/campaigns/{id}/mode` with `{"mode":"shadow"}` to score live decisions without changing anything, then
   `{"mode":"active"}` to let it set the ping count. `active` is refused unless the build recommended `ship` (or `"force":true`).

| Setting | Default | Meaning |
|---|---|---|
| `CG_CAMPAIGN_DIR` | `campaigns/` beside the dashboard database | where built campaigns are stored; no directory means no campaigns |
| `CG_CAMPAIGN_PYTHON` | `python3` | interpreter for `builder:"model"` builds |
| `CG_CAMPAIGN_SCRIPT` | `scripts/campaign/build_campaign.py` beside the binary | the model build pipeline |
| `CG_CAMPAIGN_AUTO_REFRESH` | `24h` | rebuild live `auto` campaigns in place; `off` disables |

**How it fails safe.** Shadow mode changes nothing. Active mode only sets how many pings a tenant's existing keep-alive sends: it never turns the
keep-alive on for a tenant that has it off, never overrides a manager strategy or a per-session override, and never widens the per-ping dollar ceiling.
The kill switch (`CONTEXT_GURU_KEEPALIVE=off`) stops campaigns with everything else. A panic inside a campaign is recovered and means "no campaign" for
that response. A model file whose SHA-256 does not match its spec is refused. Purging or deleting a tenant erases its campaigns.

## The policy

Two layers, deliberately separate:

1. **A model** turns what the proxy knows at the end of a response into a survival curve `S(t)` of the time to the next request. The model is pluggable
   (`predictive.Model`, one method); two are shipped.
   * `lgb-discrete-hazard` (default): the research LightGBM discrete-time hazard. Time is cut into 20 bins; a gradient-boosted classifier gives the hazard of each bin from 72 request
     inputs plus the bin and its edge; `S(edge) = prod (1 - h)`. 300 trees x 127 leaves by default. Its inputs are the research "meta" family (request columns plus
     thread and session features): no tenant-wide history, no content, nothing that needs the future of the thread. Trees are exported to JSON and evaluated in Go (`lgb.go`).
   * `weibull3-mixture-mlp`: a ~5.6K-parameter network with a censored Weibull-mixture output (37 inputs). Kept as the second implementation of the interface and as a parity target.
2. **The rule**, model independent and with no tuned constant (`Decide`):

       k* = argmax_{k <= kmax}  sum_{j<=k}  stake * (S(x(j-1)+Tc) - S(xj+Tc))  -  pingCost * S(xj)       stake = prefix * (write5m - read),  x = 280 s,  Tc = 295 s,  kmax = 4

   The j-th ping is paid only if the user has not returned by `xj` (the `S(xj)` factor) and earns `stake` if the return falls in the window it opens. `k* = 0` lets the cache lapse.

| piece | where |
|---|---|
| inference, the rule, the live feature tracker, spec v2 types | `predictive/` (Go, no dependencies; the default 300-tree LightGBM model about 0.5-2 ms per decision (1.7 ms measured on a host at load 77), the MLP ~30 us; both run after the response is complete) |
| training, replay, held-out comparison, JSON export | `scripts/campaign/` (Python: numpy, pandas, lightgbm, scikit-learn; torch only for the MLP; build time only) |
| runtime (shadow and active) | `proxy/predictive.go`, hooked into `keeper.arrive` / `keeper.record` |
| build and lifecycle endpoints | `proxy/campaignbuild.go` |

## Parity with the Python reference

* **Model** (`lgb_parity_test.go`, 1,200 decision points, fixture `testdata/lgb.*`): the Go tree evaluator reproduces LightGBM's survival curve to about 1e-16 (requirement 1e-5) and every
  ping count, in two cost regimes so more than one k is exercised. The MLP (`parity_test.go`) agrees to about 1e-15. The Go evaluator rounds its inputs to
  float32 before comparing them with a threshold, because training and the Python reference see float32 inputs.
* **Features**: the Go `Tracker`, fed only start / end / response events, reproduces the Python feature vectors (and so the decisions) on all rows except those where a
  thread had parallel requests (below) and at most 0.25% of the rest (two requests of one session finishing in the same millisecond).

Every fixture under `testdata/` is synthetic: `scripts/campaign/synth_fixtures.py` generates the traffic (fixed seeds) and runs
`export_fixture_lgb.py` / `export_fixture_mlp.py` and the reference auto tuner on it (needs numpy, pandas, lightgbm, scikit-learn, torch; not run in CI).

## Leak audit of the inputs

Neither model reads `thread_len` (a thread's total length, which with the turn index reveals the last request of the thread) or anything derived from it. Counts of a thread or session are running counts of the
requests seen so far (`turn_in_thread`, `m_n_req_thread`, `session_turn`), never totals. The inputs, in model order:

* **LightGBM (72)**: `messages, tools, system_blocks, stream, session_turn, turn_in_thread, hour_utc, dow, prefix_tok, output_tok, read_cost, rebuild_cost, ping_cost, cost_usd, fresh_input, stake, ping_over_stake, family, agent_type, provider, stop_reason, thinking`
  and `m_n_req_thread, m_n_req_session, m_session_age_s, m_thread_age_s, m_ctx_tokens, m_ctx_window, m_ctx_fill, m_cache_read_ratio, m_cache_write_ratio, m_fresh_ratio, m_rewrite, m_rewrites_last5, m_reqs_since_rewrite, m_ctx_growth, m_ctx_growth_ratio, m_gap1, m_gap2, m_gap3, m_gap_mean5, m_gap_median5, m_gap_std5, m_gap_max10, m_gap_trend5, m_gap_ewma, m_is_user_turn, m_since_user_turn_s, m_tool_streak, m_reqs_since_user_turn, m_messages, m_messages_delta, m_reqs_since_compaction, m_compactions, m_stop_reason, m_prev_stop_reason, m_model, m_agent, m_cache_ttl, m_tools, m_system_blocks, m_out_tokens, m_out_ratio, m_resp_s, m_ttfb_s, m_resp_mean5, m_thinking, m_effort, m_max_tokens, m_status_ok, m_cost_req, m_cost_session_cum`
  (plus the time bin and its edge inside the hazard model). `m_ctx_window` is a per-model constant table read from the training rows only (a model unseen there gets a missing window and a missing category code, which the trees route by their stored default direction, the same in Python and Go; `TestCacheCampaignWithTheLightGBMModel` decides on an unseen model), the only input that uses more than the request's own past.
* **Weibull-mixture network (37)**: `l_prefix, l_out, l_fresh, cw_frac, l_msgs, l_tools, l_turn, l_sturn, l_dur, l_sage, pg_first, pg_pos, pg_neg, pg2, hs, hc, ds, dc, kv_p5, kv_p1, kv_ln, gq_nan, l_gq, think, stream, l_ping, sr_tool_use, sr_stop_sequence, sr_end_turn, sr_stop, sr_tool_calls, fam_opus, fam_sonnet, fam_haiku, ag_claude-cli, ag_openai, ag_asyncopenai`.
  `l_turn` is log1p of the running thread index; `kv_*` and `l_gq` are statistics of return gaps CLOSED before the decision time (a gap counts only once the successor request has started); `pg_*` are idle gaps before this request.

## Every input, and how the proxy computes it

All 72 inputs of the default model are computed live at response end; none is dropped. `Live` below is `LiveFacts`, filled in `proxy.go`'s terminal defer from the
capture that also fills the dashboard row, so the campaign sees the values it was trained on. "Tracker" is `predictive.Tracker` (per campaign, in memory).

| inputs | how |
|---|---|
| `messages` / `m_messages` | `Live.Messages` = the pipeline trace's message count (the dashboard `messages` column) |
| `tools`, `system_blocks`, `stream`, `m_tools`, `m_system_blocks`, `m_max_tokens`, `thinking`, `m_thinking`, `m_effort` | `Live`, from `metaFromBody` on the pristine inbound body (the dashboard's own parse) |
| `m_cache_ttl` | `Live.CacheTTL` = the pipeline trace's cache tier asked for |
| `family`, `m_model`, `m_ctx_window`, `m_ctx_fill` | the model name; the window is the model file's own table (smallest of 128k / 200k / 400k / 1M above the largest context seen in training) |
| `agent_type`, `m_agent` | `dash.AgentFor(User-Agent)`, as stored on the row |
| `provider` | the routed provider name, as stored on the row |
| `prefix_tok`, `fresh_input`, `output_tok`, `m_ctx_tokens`, `m_cache_read_ratio`, `m_cache_write_ratio`, `m_fresh_ratio`, `m_out_tokens`, `m_out_ratio`, `m_rewrite`, `stop_reason`, `m_stop_reason` | the response's usage block and stop reason |
| `read_cost`, `rebuild_cost`, `ping_cost`, `stake`, `ping_over_stake`, `cost_usd`, `m_cost_req` | usage x the model's rates (`modelinfo.Price`); a ping = prefix read + 7 fresh tokens + 1 output token |
| `hour_utc`, `dow` | UTC calendar of the response end |
| `m_resp_s`, `m_resp_mean5` | request start to response end |
| `m_ttfb_s` | `Live.TTFBMs`: time to first byte on streamed responses, 0 on buffered ones, exactly as the dashboard records it |
| `m_status_ok` | constant 1: only successful responses are decision points |
| `session_turn`, `m_n_req_session`, `m_session_age_s` | Tracker, per (tenant, session) |
| `turn_in_thread`, `m_n_req_thread`, `m_thread_age_s`, `m_gap1..3`, `m_gap_mean5`, `m_gap_median5`, `m_gap_std5`, `m_gap_max10`, `m_gap_trend5`, `m_gap_ewma` | Tracker, per thread, from request start times and the previous response's end (known at the request's start; a previous request still running contributes gap 0) |
| `m_ctx_growth`, `m_ctx_growth_ratio`, `m_messages_delta`, `m_prev_stop_reason`, `m_rewrites_last5`, `m_reqs_since_rewrite`, `m_is_user_turn`, `m_since_user_turn_s`, `m_tool_streak`, `m_reqs_since_user_turn`, `m_reqs_since_compaction`, `m_compactions` | Tracker, per thread, from the previous finished request's response (size, stop reason, message count) |
| `m_cost_session_cum` | Tracker: billed cost of the session's requests the proxy had finished with by this one, including it |

Where the live value can differ from the offline research definition (each is measured in the parity test or listed here):

* **Parallel requests on one thread** (subagents, forks that the thread rule keeps together): the offline definition reads the previous request's tokens and stop reason even when it is still running at this
  one's end, which a live system cannot know. The tracker then uses the last finished request. This is a small share of decision points; `Row.Skipped` marks them and the 10 rows after. The LightGBM parity fixture contains such rows; the test bounds how many of their ping counts may differ from the offline definition (at most half, in both cost regimes) and requires the rest to match.
* **Thread identity**: the proxy's own thread id (`threadFor`) stands in for the research thread rule (tools / system blocks / message-count continuity). They agree on the common case, not always.
* **Counters** (`turn_in_thread`, `session_turn`, session age) restart at zero after a process restart or six idle hours.
* **`m_cost_session_cum`** is redefined causally (the research feature sums every earlier-started request, which with parallel threads includes costs still unknown).
* **Response end** is taken when the proxy finishes returning the response (about 40 ms after the provider's end).
* The keeper never pings a conversation's first request (an existing gate), while training rows include them.

Not an input and not available live: the thread's total length (`thread_len`), which would reveal the last request of a thread. It is excluded everywhere.

## Safety properties (tested)

* **Shadow is inert.** The campaign runs in `keeper.record` only after the kill switch and the no-audit-sink refusal, so a switched-off or unaudited keep-alive tracks and retains nothing for it; any panic inside it is recovered and means "no campaign applies" for that response (`TestShadowCampaignIsInert`: a panicking model leaves the keeper's registrations identical to no campaign).
* **Active never overrides a tenant's opt-out.** It acts only when the tenant's own keep-alive policy is on (`TestActiveCampaignNeverOverridesTenantOptOut`); the skipped activations are counted (`active_skipped_tenant_opt_out`). A manager strategy still forces it on, as before; a campaign does not. Precedence is strategy over campaign, per-session override over both, and the per-ping dollar ceiling is never widened.
* **Requests keep their identity.** The tracker's `Arrival` travels with the request (through the capture), not with its thread, and shadow decisions are kept per request: overlapping requests on a thread are counted once each and scored against their own response (`TestOverlappingRequestsOnAThreadKeepTheirOwnState`, `TestOverlappingRequestsAreCountedOnce`).
* **No unbounded history.** A request dropped before its response, or a plan-only campaign that never takes features, cannot grow a thread's predecessor chain (`TestPredecessorChainsStayBounded`, 100,000 requests).
* **The build subprocess gets a minimal environment** (PATH, locale, temp dir, `CG_CAMPAIGN_*`, thread limits): none of the proxy's upstream or gateway credentials; arguments are passed as `--name=value`.

## Campaign spec v2 (`cg.cache.campaign/2`)

A campaign is an ordered list of segments; each says which rule decides for the requests it matches. Predicates are `[fact, op, value]` triples over
`family`, `stop_reason`, `model`, `agent`, `prefix_tok`, `ping_cost`; the first matching segment wins, else `policy.fallback`.

```jsonc
{ "schema": "cg.cache.campaign/2", "version": "2.0.0", "built_at": "...", "scope": {"type": "tenant", "id": "..."}, "window": {...},
  "menu": { "none": {...}, "model": {"gaps_s": [280,...], "max_pings": 4, "note": "K chosen per decision point by the model"}, "u280k2": {"gaps_s": [280,280], ...} },
  "policy": { "kind": "model+segments",                       // or "rules" (no model: plan / none segments only)
              "model": {"ref": "model.json", "sha256": "...", "schema": "cg.cache.predictor/1", "kind": "lgb-discrete-hazard", "n_features": 72, "rule": {"lam":1,"x_s":280,"kmax":4,"tc_s":295}},
              "segments": [ {"id": "family-haiku", "when": [["family","==","haiku"]], "rule": {"kind": "none"}},
                            {"id": "all", "when": [], "rule": {"kind": "model"}} ],     // rule kinds: model | plan (menu name) | none
              "fallback": {"kind": "none"} },
  "segments": [ ...per-segment held-out table... ], "evidence": {...}, "caveats": [...] }
```

The runtime refuses a model file whose SHA-256 differs from the one in the spec (it is not the model that was evaluated).

## Endpoints (manager only, audited)

| | |
|---|---|
| `POST /api/cache/campaigns/build` | body `{"scope":"user"\|"tenant","id":"<tenant id>","window":"30d" \| {"days":N} \| {"since":ms,"until":ms},"objective":"net_usd","builder":"model"\|"auto","session_end":false}` -> `202 {"campaign_id","state":"running","poll","scope","window","objective"}`. One build at a time (`429` otherwise), `404` if the scope has no traffic in the window, `422` if none of its models is priced. |
| `GET /api/cache/campaigns` | `{"campaigns":[view...]}`, newest first, without the spec |
| `GET /api/cache/campaigns/{id}` | view below |
| `POST /api/cache/campaigns/{id}/mode` | body `{"mode":"off"\|"shadow"\|"active","force":false}`. `active` needs the build's recommendation to be `ship` unless `force`. `409` if the build is unfinished or the scope had too little history. |

View: `{"id","state":"running|done|failed","stage","error","created_at","scope":{"type","id"},"window":{"since","until"},"objective","mode","live","recommendation":"ship|baseline|insufficient_data","seconds",`
`"summary":{"model":{row},"best_baseline":{row},"vs_best_baseline":{"diff_usd","ci95"},"oracle_net_usd","split","class"}, "shadow":{...}, "spec":{...}}`.
A table row is `{"strategy","net_usd","ci95","gap_captured","pings","credited_rate","ping_usd","wasted_ping_usd"}`. `shadow` (only while the campaign is live) is
`{"decisions","k_hist":[13],"resolved","returned","would_pings","would_rescues","est_ping_usd","est_rescue_usd","est_net_usd","skipped_no_price","last_decision_ms"}`:
a running closed-form score of the live decisions (a decision resolves when the next request arrives on that thread, or counts as no-return after two hours).

`recommendation` defaults to **`baseline`** (do not run the model; keep the best fixed rule). It is `ship` only if the model's held-out net is positive **and** its paired difference to the best simple strategy
(the best of the six on the same held-out rows, picked in hindsight, so the test is conservative) has a 95% interval entirely above zero. `insufficient_data` below 500 held-out rows, 20 sessions or 50 observed returns.
`shadow` mode stays available whatever the recommendation (it changes nothing), to collect live evidence; `active` needs `ship` or `force`.

## How a build works

1. the server writes the tenant's model prices and runs `scripts/campaign/build_campaign.py` (niced, 2 threads, 45 min limit) against a read-only handle on the dashboard database;
2. decision points are built (threads, next compatible request, outcome labels), causal features computed, the first 70% of the window (by response end) trains the model (LightGBM: 300 rounds on up to 200,000 requests expanded to person-period rows; `--model mlp` trains 3 seeds of the network instead);
3. the last 30% is replayed in closed form for the model and every baseline (never, blanket K1 / K2 / K3, the tuned best single plan, the merged rule K2 at >= 80k prefix and <= $0.50 per ping, stop-reason-gated K2, and a hindsight oracle), with 95% session-bootstrap intervals and paired differences;
4. the shipped model **is the evaluated one** (no refit on the held-out slice), so the table describes exactly what runs.

Trade-off: the build is Python because training and the bootstrap are build-time work that does not belong on the request path, and a Go port would duplicate the
feature code a third time. The cost is a python3 + numpy + pandas + lightgbm + scikit-learn dependency (torch only for `--model mlp`) on the build host (`CG_CAMPAIGN_PYTHON`). Inference, the part that runs per request, is Go.

Dollars are closed-form units calibrated to an offline replay engine (`scripts/campaign/engine_calibration.json`). Compare strategies; do not read the level.

## The auto builder (`"builder":"auto"`)

A plan chooser, not a predictor, and no Python: `predictive/autotune.go` picks the tenant's plan from the menu "ping k times, 240 s apart", k = 0..6, and
`proxy/campaignauto.go` wraps it in a campaign (spec kind `rules`, one segment, the chosen plan).

* Data: the fleet's rows (for the prior) come from one read capped at the dashboard's newest-rows limit; the tenant's own rows are read on
  their own, so a busy fleet never pushes them out. The spec's `evidence.data` and `caveats` say when either read was truncated.
* Every row of every tenant in the window is scored in closed form for each plan (the same arithmetic as the shadow score). With `"session_end":true` a
  session's last response sends no ping (the keeper does this when the client's SessionEnd hook is installed).
* The tenant's score for plan k is `(S_u[k] + kappa * g[k]) / (N_u + kappa)`: its own recency-weighted rows plus `kappa` pseudo-rows of the fleet mean `g`.
  `kappa` (0 ... +Inf, +Inf = the fleet plan) and the recency half-life (1 ... 30 days, or none) are chosen on the training rows only: fit on their first
  70%, score on the rest, safest setting first, so a less shrunk one wins only if it is strictly better on rows it was not fitted on.
* Held-out evidence: tuned on rows resolved before the tenant's 70% point, replayed on the tenant's last 30% against never, blanket K1/K2/K3 and the fleet
  plan, with session-bootstrap intervals. `ship` needs a positive net and a paired 95% interval above zero against BOTH blanket K3 and the fleet plan;
  a tenant whose plan is the fleet plan is therefore always `baseline` (nothing to gain over what it gets anyway). A non-`ship` build runs the fleet plan.
* The shipped plan is the tuner refitted on the whole window (the evidence is for the procedure, not for one fit: recency is most of the value, below).
* **Refresh**: a live (shadow or active) auto campaign is rebuilt in place every `CG_CAMPAIGN_AUTO_REFRESH` (default `24h`, `off` disables) on the same
  window length ending now, from one shared read of the fleet's history per pass; the mode is kept, an ACTIVE campaign changes its plan only on a `ship` rebuild, a rebuild that finds too little history keeps the previous plan, and a new spec replaces the old one only after it parses.
* An active campaign (either builder) sets the ping count; the spacing stays the tenant's configured keep-alive interval (240 s if none).

The tuner makes the same choices as a reference Python implementation (`scripts/campaign/synth_fixtures.py`) on the same rows
(`TestAutoTuneMatchesTheResearchImplementation`, 20,000 synthetic rows of 19 scopes whose return habits drift over four weeks).

## Run it

```
CG_CAMPAIGN_PYTHON=/path/to/venv/bin/python   # numpy, pandas, lightgbm, scikit-learn
CG_CAMPAIGN_DIR=/var/lib/.../campaigns          # default: "campaigns" beside the dashboard database
curl -X POST .../api/cache/campaigns/build -d '{"scope":"user","id":"<tenant>","window":"30d"}'
curl .../api/cache/campaigns/<id>
curl -X POST .../api/cache/campaigns/<id>/mode -d '{"mode":"shadow"}'
```

Auto: `curl -X POST .../api/cache/campaigns/build -d '{"scope":"tenant","id":"<tenant>","window":"30d","builder":"auto","session_end":true}'`, then the same view and mode routes.

Tests: `go test ./predictive ./proxy -run 'Parity|CacheCampaign|Auto'` and `python scripts/campaign/test_pipeline.py`.
