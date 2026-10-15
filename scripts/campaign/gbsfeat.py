"""The 72 per-request / per-thread inputs of the LightGBM discrete-time hazard (research family "meta": 22 request columns + 50 m_* thread and session features).
Every one is a function of the finished request and of earlier requests of its thread / session; none reads an outcome or the thread's future length.
m_features is the research extractor's m_* block (feat-meta/extract.py) so the two stay comparable; the Go tracker (predictive/gbsfeatures.go) is tested against it."""
import warnings
import numpy as np, pandas as pd
warnings.filterwarnings("ignore", category=RuntimeWarning)

OWN_NUM = ["messages", "tools", "system_blocks", "stream", "session_turn", "turn_in_thread", "hour_utc", "dow", "prefix_tok", "output_tok", "read_cost", "rebuild_cost", "ping_cost", "cost_usd", "fresh_input"]
OWN_CAT = ["family", "agent_type", "provider", "stop_reason", "thinking"]
M_NAMES = ["m_n_req_thread", "m_n_req_session", "m_session_age_s", "m_thread_age_s", "m_ctx_tokens", "m_ctx_window", "m_ctx_fill", "m_cache_read_ratio", "m_cache_write_ratio", "m_fresh_ratio",
           "m_rewrite", "m_rewrites_last5", "m_reqs_since_rewrite", "m_ctx_growth", "m_ctx_growth_ratio", "m_gap1", "m_gap2", "m_gap3", "m_gap_mean5", "m_gap_median5", "m_gap_std5", "m_gap_max10",
           "m_gap_trend5", "m_gap_ewma", "m_is_user_turn", "m_since_user_turn_s", "m_tool_streak", "m_reqs_since_user_turn", "m_messages", "m_messages_delta", "m_reqs_since_compaction", "m_compactions",
           "m_stop_reason", "m_prev_stop_reason", "m_model", "m_agent", "m_cache_ttl", "m_tools", "m_system_blocks", "m_out_tokens", "m_out_ratio", "m_resp_s", "m_ttfb_s", "m_resp_mean5", "m_thinking",
           "m_effort", "m_max_tokens", "m_status_ok", "m_cost_req", "m_cost_session_cum"]
NAMES = OWN_NUM + ["stake", "ping_over_stake"] + OWN_CAT + M_NAMES
CATS = ["family", "agent_type", "provider", "stop_reason", "thinking", "m_stop_reason", "m_prev_stop_reason", "m_model", "m_agent", "m_cache_ttl", "m_thinking", "m_effort"]
assert len(NAMES) == 72
SIZES = [128_000, 200_000, 400_000, 1_000_000]

def window_table(dp):
    """served window per model: the smallest standard size >= the largest context seen for it."""
    mx = (dp.fresh_input + dp.cache_read + dp.cache_write).groupby(dp.model).max()
    return {m: next((s for s in SIZES if s >= v), SIZES[-1]) for m, v in mx.items()}

def m_features(df, windows):
    """df: one row per decision point with the dash `requests` column names plus thread_id, t_end_ms; returns the m_* frame in df order."""
    order = np.lexsort((df.id.values, df.ts.values, pd.factorize(df.session_id)[0], pd.factorize(df.tenant_id)[0]))
    inv = np.empty_like(order); inv[order] = np.arange(len(order)); df = df.iloc[order].reset_index(drop=True)
    F = pd.DataFrame({"dp_id": df.id})
    ts = df.ts.values.astype(np.int64)
    ctx = (df.fresh_input + df.cache_read + df.cache_write).values.astype(float); ctxp = np.where(ctx > 0, ctx, np.nan)
    gt = df.groupby("thread_id", sort=False); gs = df.groupby(["tenant_id", "session_id"], sort=False)
    sh = lambda s, k=1: s.groupby(df.thread_id, sort=False).shift(k)
    end = df.t_end_ms.values.astype(np.int64)
    F["m_n_req_thread"] = gt.cumcount() + 1; F["m_n_req_session"] = gs.cumcount() + 1
    F["m_session_age_s"] = (ts - gs.ts.transform("first").values) / 1e3; F["m_thread_age_s"] = (ts - gt.ts.transform("first").values) / 1e3
    F["m_ctx_tokens"] = ctx; F["m_ctx_window"] = df.model.map(windows).astype(float); F["m_ctx_fill"] = ctx / F.m_ctx_window
    F["m_cache_read_ratio"] = df.cache_read / ctxp; F["m_cache_write_ratio"] = df.cache_write / ctxp; F["m_fresh_ratio"] = df.fresh_input / ctxp
    rew = (df.cache_write >= 0.8 * ctxp).astype(float); rew_nf = rew.where(F.m_n_req_thread > 1)
    F["m_rewrite"] = rew
    F["m_rewrites_last5"] = sum(sh(rew_nf, k).fillna(0) for k in range(1, 5)) + rew_nf.fillna(0)
    lastrew = pd.Series(np.where(rew_nf == 1, F.m_n_req_thread, np.nan)).groupby(df.thread_id, sort=False).ffill()
    F["m_reqs_since_rewrite"] = F.m_n_req_thread - lastrew
    prev_ctx = sh(pd.Series(ctx)); F["m_ctx_growth"] = ctx - prev_ctx; F["m_ctx_growth_ratio"] = ctx / prev_ctx
    gap = pd.Series((ts - sh(pd.Series(end)).values) / 1e3).clip(lower=0); gap[F.m_n_req_thread.values == 1] = np.nan
    G = np.column_stack([sh(gap, k).values if k else gap.values for k in range(10)])
    F["m_gap1"], F["m_gap2"], F["m_gap3"] = G[:, 0], G[:, 1], G[:, 2]
    G5 = G[:, :5]; n5 = (~np.isnan(G5)).sum(1)
    with np.errstate(all="ignore"):
        F["m_gap_mean5"] = np.where(n5 >= 1, np.nanmean(G5, 1), np.nan); F["m_gap_median5"] = np.where(n5 >= 1, np.nanmedian(G5, 1), np.nan)
        F["m_gap_std5"] = np.where(n5 >= 2, np.nanstd(G5, 1, ddof=1), np.nan); F["m_gap_max10"] = np.where((~np.isnan(G)).sum(1) >= 1, np.nanmax(G, 1), np.nan)
    L5 = np.log1p(G5); F["m_gap_trend5"] = np.where(n5 == 5, (L5[:, 0] * 2 + L5[:, 1] - L5[:, 3] - 2 * L5[:, 4]) / 10, np.nan)
    lg = pd.Series(np.log1p(gap.values)); F["m_gap_ewma"] = np.expm1(lg.groupby(df.thread_id, sort=False).transform(lambda s: s.ewm(alpha=.3).mean())).values
    ptool = sh(df.stop_reason.eq("tool_use").astype(float)).fillna(0) == 1; user_turn = (~ptool).astype(float); F["m_is_user_turn"] = user_turn
    turn_ts = pd.Series(np.where(user_turn == 1, ts, np.nan)).groupby(df.thread_id, sort=False).ffill(); F["m_since_user_turn_s"] = (ts - turn_ts) / 1e3
    st = (df.stop_reason == "tool_use").astype(int); b = (st == 0).groupby(df.thread_id, sort=False).cumsum(); F["m_tool_streak"] = st.groupby([df.thread_id, b], sort=False).cumsum()
    F["m_reqs_since_user_turn"] = F.m_n_req_thread - gt.cumcount().where(user_turn == 1).groupby(df.thread_id, sort=False).ffill() - 1
    msgs = df.messages.astype(float); pm = sh(msgs); F["m_messages"] = msgs; F["m_messages_delta"] = msgs - pm
    comp = (msgs < 0.7 * pm) | (ctx < 0.6 * prev_ctx)
    cidx = pd.Series(np.where(comp, F.m_n_req_thread, np.nan)).groupby(df.thread_id, sort=False).ffill()
    F["m_reqs_since_compaction"] = F.m_n_req_thread - cidx.fillna(1); F["m_compactions"] = comp.astype(float).groupby(df.thread_id, sort=False).cumsum()
    F["m_stop_reason"] = df.stop_reason.replace("", "none"); F["m_prev_stop_reason"] = sh(df.stop_reason).fillna("first").replace("", "none")
    F["m_model"] = df.model.str.replace(r"^(aws|azure|Azure)/", "", regex=True); F["m_agent"] = df.agent
    F["m_cache_ttl"] = df.cache_ttl.replace("", "none"); F["m_tools"] = df.tools; F["m_system_blocks"] = df.system_blocks
    F["m_out_tokens"] = df.output_tokens; F["m_out_ratio"] = df.output_tokens / ctxp
    F["m_resp_s"] = df.upstream_ms / 1e3; F["m_ttfb_s"] = df.ttfb_ms / 1e3
    F["m_resp_mean5"] = pd.Series(df.upstream_ms.values / 1e3).groupby(df.thread_id, sort=False).transform(lambda s: s.rolling(5, min_periods=1).mean()).values
    F["m_thinking"] = df.thinking_mode.replace("", "none"); F["m_effort"] = df.reasoning_effort.replace("", "none"); F["m_max_tokens"] = df.max_tokens
    F["m_status_ok"] = (df.status == 200).astype(int); F["m_cost_req"] = df.cost_usd
    # Billed cost of the session's requests that the proxy had finished with (t_dec) by this request's t_dec, including it. The research definition sums every earlier-STARTED request, which with parallel
    # threads includes requests still running, i.e. costs not yet known at the decision.
    B = np.int64(4_000_000_000_000); gid = pd.factorize(df.tenant_id.astype(str) + "|" + df.session_id.astype(str))[0].astype(np.int64)
    dec = df.t_dec_ms.values.astype(np.int64); key = gid * B + dec; o = np.argsort(key, kind="stable"); cs = np.concatenate([[0.0], np.cumsum(df.cost_usd.values[o])])
    F["m_cost_session_cum"] = cs[np.searchsorted(key[o], key, "right")] - cs[np.searchsorted(key[o], gid * B, "left")]
    return F.replace([np.inf, -np.inf], np.nan).iloc[inv].reset_index(drop=True)

def raw_frame(dp, windows):
    """(N, 72) frame of the 72 inputs; categoricals still strings. Row order = dp."""
    m = m_features(dp, windows); X = pd.DataFrame(index=range(len(dp)))
    for c in OWN_NUM: X[c] = dp[c].astype(float).values
    stake = (dp.rebuild_cost - dp.read_cost).values
    X["stake"] = stake; X["ping_over_stake"] = dp.ping_cost.values / np.where(stake != 0, stake, np.nan)
    for c in OWN_CAT: X[c] = dp[c].values
    for c in M_NAMES: X[c] = m[c].values
    return X[NAMES]

def encode(X, enc):
    """strings -> float codes with the training-time mapping (unseen / missing -> NaN)."""
    out = X.copy()
    for c in CATS:
        mp = {v: i for i, v in enumerate(enc[c])}; out[c] = out[c].map(mp).astype(float)
    return out.astype(np.float32)

def fit_encoders(X): return {c: sorted(X[c].dropna().astype(str).unique().tolist()) for c in CATS}
