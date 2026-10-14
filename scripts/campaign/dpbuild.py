"""Decision-point table (one row per 2xx non-ping response) from a dash `requests` table, read-only.
Mirrors the research dpset builder: threads, next compatible request, outcome labels, ping/rebuild costs at the given per-model rates."""
import json, sqlite3
import numpy as np, pandas as pd

FRESH = 7        # a ping bills ~7 fresh input tokens + 1 output token on top of the cached read (measured law)
MAXOPEN = 12     # open threads kept per (tenant, session, model, tools, system_blocks)

def assign_threads(d):
    key = (d.t.astype(str) + "|" + d.s.astype(str) + "|" + d.model.astype(str) + "|" + d.tools.astype(str) + "|" + d.sb.astype(str)).values
    msgs = d.messages.values; cached = d.cached.values.astype(float)
    tid = np.empty(len(d), np.int64); nxt = 0; open_ = {}
    for i in np.lexsort((d.id.values, d.ts.values)):
        L = open_.setdefault(key[i], []); best = -1; bc = -1.0
        for k, (t, lm, lc) in enumerate(L):
            if msgs[i] >= lm and lc <= cached[i] * 1.02 + 500 and lc > bc: best, bc = k, lc
        if best < 0:
            L.append([nxt, msgs[i], cached[i]]); t = nxt; nxt += 1
            if len(L) > MAXOPEN: L.pop(0)
        else:
            L[best][1] = msgs[i]; L[best][2] = cached[i]; t = L[best][0]
        tid[i] = t
    return tid

def load_requests(db, since_ms, until_ms, tenant=None):
    c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    q = ("select id, ts, tenant_id t, session_id s, model, agent, provider, messages, tools, system_blocks sb, stop_reason sr, thinking_mode th, stream, "
         "fresh_input fi, cache_read cr, cache_write cw, output_tokens ot, cost_usd cost, cg_latency_ms cgl, upstream_ms up, token_accounting acc, keepalive ka, "
         "status, cache_ttl, ttfb_ms, reasoning_effort, max_tokens "
         "from requests where status between 200 and 299 and ts >= ? and ts < ?")
    args = [since_ms, until_ms]
    if tenant is not None: q += " and tenant_id = ?"; args.append(tenant)
    return pd.read_sql(q, c, params=args)

def build_dp(d, prices):
    """d: rows of load_requests; prices: {model: {in, out, cache_read, cache_write_5m}} ($ per token)."""
    d = d[d.ka == 0].sort_values(["ts", "id"]).reset_index(drop=True)
    d["cached"] = d.cr + d.cw
    d["tid"] = assign_threads(d)
    d["t_end_ms"] = d.ts + d.up.fillna(0); d["t_dec_ms"] = d.t_end_ms + d.cgl.fillna(0)
    pm = pd.DataFrame(prices).T.reindex(d.model.unique())
    for col, k in (("pin", "in"), ("pout", "out"), ("pr", "cache_read"), ("pw", "cache_write_5m")): d[col] = d.model.map(pm[k])
    d = d.sort_values(["tid", "ts", "id"]).reset_index(drop=True)
    n = len(d); nxt_ts = np.full(n, -1, np.int64); nxt_id = np.full(n, -1, np.int64); nxt_c = np.zeros(n)
    tid, ts, te, ids, cv = d.tid.values, d.ts.values, d.t_end_ms.values, d.id.values, d.cached.values
    for a, b in zip(*(lambda s: (s[:-1], s[1:]))(np.r_[0, np.where(np.diff(tid) != 0)[0] + 1, n])):
        if b - a < 2: continue
        pos = np.maximum(np.searchsorted(ts[a:b], te[a:b], side="left") + a, np.arange(a, b) + 1)
        ok = pos < b; p = pos[ok]; i = np.arange(a, b)[ok]; nxt_ts[i] = ts[p]; nxt_id[i] = ids[p]; nxt_c[i] = cv[p]
    d["next_id"] = nxt_id; d["compat"] = (nxt_id >= 0).astype(int)
    d["dt_next_s"] = np.where(d.compat == 1, (nxt_ts - d.t_end_ms.values) / 1000.0, np.nan)
    d["reusable_next_tok"] = np.where(d.compat == 1, np.minimum(d.cached.values, nxt_c), 0.0)
    d = d.sort_values(["t", "s", "ts", "id"]).reset_index(drop=True)
    sk = (d.t.astype(str) + "|" + d.s.astype(str)).values; tsv, tev = d.ts.values, d.t_end_ms.values
    any_next = np.full(len(d), np.nan)
    for a, b in zip(*(lambda s: (s[:-1], s[1:]))(np.r_[0, np.where(sk[1:] != sk[:-1])[0] + 1, len(d)])):
        pos = np.maximum(np.searchsorted(tsv[a:b], tev[a:b], side="left") + a, np.arange(a, b) + 1); ok = pos < b
        any_next[np.arange(a, b)[ok]] = (tsv[pos[ok]] - tev[a:b][ok]) / 1000.0
    d["dt_next_any_s"] = any_next
    d["session_turn"] = d.groupby(["t", "s"]).cumcount()
    d = d.sort_values(["ts", "id"]).reset_index(drop=True)
    we = float(d.ts.max()); d["window_end_ms"] = we
    d["cens_time_s"] = np.where(d.compat == 0, np.maximum((we - d.t_end_ms) / 1000.0, 0), np.nan)
    d["turn_in_thread"] = d.groupby("tid").cumcount()
    d["prefix_tok"] = d.cached
    d["ping_cost"] = d.cached * d.pr + FRESH * d.pin + d.pout
    d["stake_next"] = d.reusable_next_tok * (d.pw - d.pr)
    t = pd.to_datetime(d.t_end_ms, unit="ms", utc=True); d["hour_utc"] = t.dt.hour; d["dow"] = t.dt.dayofweek
    m = d.model.str.replace("aws/", "", regex=False).str.lower()
    d["family"] = np.select([m.str.contains("opus"), m.str.contains("sonnet"), m.str.contains("haiku")], ["opus", "sonnet", "haiku"], "other")
    d["agent_type"] = d.agent.fillna("").replace("", "unknown")
    d["dp_ok"] = ((d.acc == "complete") & (d.cached > 0) & d.pin.notna()).astype(int)
    d["thread_id"] = d.tid
    d["read_cost"] = d.cached * d.pr; d["rebuild_cost"] = d.cached * d.pw
    d = d.rename(columns={"id": "dp_id", "ts": "ts_ms", "t": "tenant", "s": "session", "sr": "stop_reason", "th": "thinking", "fi": "fresh_input", "ot": "output_tok",
                          "pr": "p_read", "pw": "p_write5m", "pin": "p_in", "pout": "p_out", "cr": "cache_read", "cw": "cache_write", "cost": "cost_usd", "up": "upstream_ms", "sb": "system_blocks"})
    # the names the research feature extractor (and scripts/campaign/gbsfeat.py) use
    d["id"], d["ts"], d["tenant_id"], d["session_id"] = d.dp_id, d.ts_ms, d.tenant, d.session
    d["thinking_mode"], d["output_tokens"] = d.thinking, d.output_tok
    return d
