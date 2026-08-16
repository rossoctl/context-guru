#!/usr/bin/env python3
"""SWE-bench Verified harness for HEADROOM — mirror of context-guru's
deploy/harbor/swebench.py so results are directly comparable.

Per config it: (1) starts the headroom proxy on :4010 pointed at the IBM
LiteLLM gateway (upstream via ANTHROPIC_TARGET_API_URL + a bearer token
injected through --anthropic-extra-headers so the containerized claude-code
agent can send a dummy key); (2) runs harbor over the task list with the
claude-code agent on aws/claude-sonnet-5; (3) parses each trial's result.json
(reward, timings) and agent/trajectory.json final_metrics (cache-aware tokens,
cost, steps) — IDENTICAL accounting to the context-guru harness; (4) reads the
headroom /stats + /metrics endpoints for savings %, token before/after,
per-transform breakdown, proxy-added latency, and CCR retrieval (bounce)
counts. Writes rows-<cfg>.json + summary.json under the jobs-root.

The `off` baseline is NOT re-run here — reuse context-guru's validated
/tmp/cg-runs/final50/rows-off.json. Use config `hdoff` (headroom passthrough,
--no-optimize) only to validate proxy wiring end-to-end.

Usage:
  swebench_headroom.py --tasks /tmp/cg-runs/swe3-verify.txt \
      --configs hd-cache --jobs-root /tmp/tb-runs/swe3 --n 2
"""
import argparse, glob, json, os, re, subprocess, sys, time, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

HD = Path("/home/vpcuser/projects/context-engineering/headroom")
HB = Path("/home/vpcuser/projects/context-engineering/harbor")
HEADROOM_BIN = os.path.expanduser("~/.local/bin/headroom")
PORT = 4010
# The address containers reach this host on. Set CG_LAN to this box's LAN IP;
# 127.0.0.1 only works when the agent runs on the host network. Warn loudly on the
# default: a container that cannot reach the proxy fails EVERY task, and a run of all
# failures reads as "the preset is bad" rather than "nothing could connect".
LAN = os.environ.get("CG_LAN") or "127.0.0.1"
if not os.environ.get("CG_LAN"):
    print(f"WARNING: CG_LAN is unset, using {LAN}. Containers on a bridge network "
          "cannot reach the proxy there and every task will fail. Set CG_LAN to this "
          "host's LAN IP (`hostname -I`) unless the agent runs with --network host.",
          file=sys.stderr)
PRICES_URL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
MODEL = "aws/claude-sonnet-5"

# Headroom proxy configs. Each is (extra CLI args, extra env). All route to the
# gateway; the difference is the compaction policy.
#   hd-cache : cache mode (freeze prior turns; only newest turn mutable) +
#              code-aware AST compression on — the fair analog of context-guru's
#              cache-aware `codesmart`. This is what a coding-agent user gets.
#   hd-token : token mode (max compression, prior history may be rewritten).
#   hdoff    : passthrough (--no-optimize) — wiring sanity / matched control.
#   NOTE: claude-code always streams (SSE). Headroom's CCR response-interception
#   buffers+re-emits the stream to catch `headroom_retrieve` calls, and that
#   re-emission corrupts the content-block sequence ("API Error: Content block
#   not found" -> agent aborts turn 1). Headroom's --help says --no-ccr is
#   "right for streaming", so the streaming-safe config disables CCR (compression
#   stays fully active; only the reversible retrieve tool is dropped -> no
#   restoration/bounce metric, by design).
CONFIGS = {
    "hd-cache": (["--mode", "cache", "--code-aware", "--no-ccr"], {}),
    "hd-token": (["--mode", "token", "--code-aware", "--no-ccr"], {}),
    "hd-ccr":   (["--mode", "cache", "--code-aware"], {}),  # CCR on (breaks streaming); reference only
    "hdoff":    (["--no-optimize"], {}),
}


def price(model):
    fb = (2e-6, 1e-5, 2e-7, 2.5e-6)  # in, out, cache_read, cache_write
    try:
        d = json.load(urllib.request.urlopen(PRICES_URL, timeout=15))
        c = d.get(model) or d.get(model.split("/")[-1])
        if c:
            return (c.get("input_cost_per_token") or fb[0], c.get("output_cost_per_token") or fb[1],
                    c.get("cache_read_input_token_cost") or fb[2],
                    c.get("cache_creation_input_token_cost") or (c.get("input_cost_per_token") or fb[0]) * 1.25)
    except Exception as e:
        print(f"[price] {e}; fallback", file=sys.stderr)
    return fb


_PROXY = {"p": None}


def stop_proxy():
    # Kill the tracked proxy by its process GROUP (setsid), then free the port with
    # fuser. Never use `pkill -f <pattern>` where <pattern> appears in this command
    # line — it self-matches the killing shell and can leave the proxy orphaned.
    import signal
    p = _PROXY.get("p")
    if p is not None and p.poll() is None:
        try:
            os.killpg(os.getpgid(p.pid), signal.SIGTERM)
        except Exception:
            pass
    for _ in range(4):
        # fuser operates on the socket, not on cmdline text -> no self-match.
        subprocess.run(f"fuser -k {PORT}/tcp >/dev/null 2>&1 || true", shell=True)
        time.sleep(1)
        r = subprocess.run(f"fuser {PORT}/tcp 2>/dev/null", shell=True, capture_output=True)
        if not r.stdout.strip():
            break
    _PROXY["p"] = None
    time.sleep(2)


def start_proxy(cfg, base, token):
    stop_proxy()
    extra_args, extra_env = CONFIGS[cfg]
    # Upstream = IBM gateway. base already looks like https://host[/path]; headroom
    # appends /v1/messages to ANTHROPIC_TARGET_API_URL. The gateway expects a bearer
    # token; inject it via extra-headers so the container can send a dummy key.
    # inject BOTH forms — LiteLLM accepts Authorization: Bearer and x-api-key.
    hdrs = json.dumps({"Authorization": f"Bearer {token}", "x-api-key": token})
    state = f"/tmp/tb-runs/hdstate-{cfg}"
    subprocess.run(f"rm -rf {state} && mkdir -p {state}", shell=True)
    env = dict(os.environ,
               ANTHROPIC_TARGET_API_URL=base,
               HEADROOM_WORKSPACE_DIR=state,
               HEADROOM_CONFIG_DIR=f"{state}/config",
               HEADROOM_TELEMETRY="off",
               HEADROOM_UPDATE_CHECK="off",
               # output shaper OFF (default) so we compare INPUT compaction fairly vs CG
               HEADROOM_OUTPUT_SHAPER="0",
               # CRITICAL: disable server-side Tool Search deferral. Headroom auto-enables
               # it for the detected claude-code client and treats our gateway as first-party
               # Anthropic, injecting first-party-only tool_search_tool_* / defer_loading.
               # The Bedrock-backed gateway can't honor them -> deferred tools become
               # unreachable -> claude-code aborts turn 1 with "API Error: Content block
               # not found". Force it off; also disable tool-schema/desc mutation and
               # memory-tool injection so headroom does not alter the tool surface.
               HEADROOM_TOOL_SEARCH="0",
               HEADROOM_TOOL_DESC_MAX_CHARS="0",
               HEADROOM_TOOL_DESC_STRIP_SEMANTIC="0",
               HEADROOM_NO_MEMORY_TOOLS="1")
    env.update(extra_env)
    args = [HEADROOM_BIN, "proxy", "--host", "0.0.0.0", "--port", str(PORT),
            "--anthropic-api-url", base,
            "--anthropic-extra-headers", hdrs] + extra_args
    log = open(f"/tmp/tb-runs/proxy-{cfg}.log", "w")
    p = subprocess.Popen(args, cwd=str(HD), stdout=log, stderr=log, env=env, preexec_fn=os.setsid)
    _PROXY["p"] = p
    for _ in range(60):
        try:
            urllib.request.urlopen(f"http://localhost:{PORT}/livez", timeout=3).read()
            return p
        except Exception:
            if p.poll() is not None:
                raise RuntimeError(f"headroom proxy for {cfg} exited early; see /tmp/tb-runs/proxy-{cfg}.log")
            time.sleep(1)
    raise RuntimeError(f"headroom proxy for {cfg} did not come up; see /tmp/tb-runs/proxy-{cfg}.log")


def run_harbor(tasks, jobs_dir, n, setup_mult, build_mult, agent_mult, max_retries=4):
    # headroom serves the Anthropic messages endpoint at the ROOT (/v1/messages),
    # so the client base URL is just http://host:port (no /anthropic suffix).
    proxy_url = f"http://{LAN}:{PORT}"
    inc = " ".join(f"-i {t}" for t in tasks)
    home = os.path.expanduser("~")
    abs_path = f"{home}/.local/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    # The agent's credentials. Hosted: its own gateway key in the auth slots and the
    # tenant token in x-context-guru-token. Local: the sk-proxy placeholder, because a
    # single-tenant proxy injects its own upstream key. Both arrive as ${VAR} templates
    # that harbor expands from the environment below, so neither value is written to
    # this command line, to the run log, or to the jobs-dir run config.
    overlay, ae_auth = cgenv.agent_auth()
    cmd = (f"cd {HB} && ANTHROPIC_BASE_URL='{proxy_url}' ANTHROPIC_API_KEY=\"${{CG_AGENT_KEY}}\" "
           f"ANTHROPIC_AUTH_TOKEN=\"${{CG_AGENT_KEY}}\" PATH='{abs_path}' HOME='{home}' "
           f"{home}/.local/bin/uv run harbor run -y -d terminal-bench@2.0 -a claude-code -m '{MODEL}' "
           f"--env docker {inc} -n {n} --jobs-dir '{jobs_dir}' --no-delete "
           f"--agent-setup-timeout-multiplier {setup_mult} --environment-build-timeout-multiplier {build_mult} "
           f"--agent-timeout-multiplier {agent_mult} --max-retries {max_retries} "
           f"--ae ANTHROPIC_BASE_URL='{proxy_url}' {ae_auth}")
    log = f"/tmp/tb-runs/run-{Path(jobs_dir).name}.log"
    with open(log, "w") as f:
        subprocess.run(["sg", "docker", "-c", cmd], stdout=f, stderr=f,
                       env=dict(os.environ, **overlay))
    return log


def iso(s):
    from datetime import datetime
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def parse_trials(jobs_dir, pr):
    rows = []
    for rf in glob.glob(f"{jobs_dir}/*/*/result.json"):
        try:
            d = json.load(open(rf))
        except Exception:
            continue
        if "verifier_result" not in d:
            continue
        tdir = Path(rf).parent
        reward = ((d.get("verifier_result") or {}).get("rewards") or {}).get("reward")
        fm = {}
        traj = tdir / "agent" / "trajectory.json"
        if traj.exists():
            try:
                fm = (json.load(open(traj)) or {}).get("final_metrics") or {}
            except Exception:
                fm = {}
        ex = fm.get("extra") or {}
        pt = fm.get("total_prompt_tokens") or 0
        ct = fm.get("total_completion_tokens") or 0
        cached = fm.get("total_cached_tokens") or 0
        cwrite = ex.get("total_cache_creation_input_tokens") or 0
        cread = ex.get("total_cache_read_input_tokens") or cached
        fresh = max(pt - cread - cwrite, 0)
        norm_cost = fresh * pr[0] + ct * pr[1] + cread * pr[2] + cwrite * pr[3]
        wall = None
        try:
            wall = iso(d["finished_at"]) - iso(d["started_at"])
        except Exception:
            pass
        agent_wall = None
        try:
            ae = d.get("agent_execution") or {}
            agent_wall = iso(ae["finished_at"]) - iso(ae["started_at"])
        except Exception:
            pass
        rows.append(dict(task=d.get("task_name", tdir.parent.name), reward=reward,
                         steps=fm.get("total_steps"), prompt_tokens=pt, completion_tokens=ct,
                         cached_tokens=cached, cache_read=cread, cache_write=cwrite, fresh_input=fresh,
                         agent_cost=fm.get("total_cost_usd"), norm_cost=round(norm_cost, 5),
                         wall_s=round(wall, 1) if wall else None,
                         agent_wall_s=round(agent_wall, 1) if agent_wall else None,
                         exception=bool(d.get("exception_info"))))
    return rows


def summarize(cfg, rows):
    got = [r for r in rows if r["reward"] is not None]
    solved = sum(1 for r in got if r["reward"] and r["reward"] >= 1)
    def avg(k):
        vs = [r[k] for r in rows if isinstance(r.get(k), (int, float))]
        return round(sum(vs) / len(vs), 3) if vs else None
    tot = lambda k: sum(r[k] for r in rows if isinstance(r.get(k), (int, float)))
    cacheable = tot("cache_read") + tot("fresh_input") + tot("cache_write")
    return dict(config=cfg, trials=len(rows), scored=len(got), solved=solved,
                solve_rate=round(solved / len(got), 3) if got else None,
                exceptions=sum(1 for r in rows if r["exception"]),
                mean_steps=avg("steps"), mean_prompt_tokens=avg("prompt_tokens"),
                mean_completion_tokens=avg("completion_tokens"),
                cache_hit_rate=round(tot("cache_read") / cacheable, 4) if cacheable else None,
                total_fresh_input=tot("fresh_input"), total_cache_read=tot("cache_read"),
                total_cache_write=tot("cache_write"), total_completion=tot("completion_tokens"),
                total_norm_cost=round(tot("norm_cost"), 4), mean_norm_cost=avg("norm_cost"),
                mean_agent_cost=avg("agent_cost"), mean_wall_s=avg("wall_s"),
                mean_agent_wall_s=avg("agent_wall_s"))


def fetch(path):
    try:
        return urllib.request.urlopen(f"http://localhost:{PORT}{path}", timeout=8).read().decode()
    except Exception as e:
        return None


def deep_find(obj, keys):
    """Return the first value under any of `keys` anywhere in a nested dict/list."""
    out = {}
    def walk(o):
        if isinstance(o, dict):
            for k, v in o.items():
                if k in keys and k not in out and isinstance(v, (int, float)):
                    out[k] = v
                walk(v)
        elif isinstance(o, list):
            for v in o:
                walk(v)
    walk(obj)
    return out


def parse_headroom_stats(cfg):
    """Best-effort extraction of headroom proxy metrics from /stats + /metrics.
    Saves the raw dumps for later inspection."""
    stats_txt = fetch("/stats")
    metrics_txt = fetch("/metrics")
    Path(f"/tmp/tb-runs/stats-{cfg}.json").write_text(stats_txt or "")
    Path(f"/tmp/tb-runs/metrics-{cfg}.txt").write_text(metrics_txt or "")
    res = {}
    st = None
    if stats_txt:
        try:
            st = json.loads(stats_txt)
        except Exception:
            st = None
    if st:
        tk = st.get("tokens", {}) if isinstance(st.get("tokens"), dict) else {}
        # headline savings figures (exact keys per proxy/server.py _build_stats_payload)
        res["tokens"] = tk
        res["tokens_saved"] = tk.get("saved")
        res["savings_percent"] = tk.get("savings_percent")            # all layers, whole request
        res["active_savings_percent"] = tk.get("active_savings_percent")  # saved / compressible (headline)
        res["proxy_savings_percent"] = tk.get("proxy_savings_percent")
        res["proxy_compression_saved"] = tk.get("proxy_compression_saved")
        res["total_before_compression"] = tk.get("total_before_compression")
        res["tokens_input"] = tk.get("input")
        res["tokens_output"] = tk.get("output")
        res["output_reduction_percent"] = tk.get("output_reduction_percent")
        # proxy-added latency (Headroom optimization time only) + total + ttfb
        res["overhead"] = st.get("overhead")
        res["latency"] = st.get("latency")
        res["ttfb"] = st.get("ttfb")
        # per-transform timings + per-strategy savings
        res["pipeline_timing"] = st.get("pipeline_timing")
        res["compressions_by_strategy"] = st.get("compressions_by_strategy")
        res["tokens_saved_by_strategy"] = st.get("tokens_saved_by_strategy")
        res["extension_savings"] = st.get("extension_savings")
        # CCR retrieval (bounce / restoration) counters
        comp = st.get("compression", {}) if isinstance(st.get("compression"), dict) else {}
        res["ccr_retrievals"] = comp.get("ccr_retrievals")
        res["ccr_entries"] = comp.get("ccr_entries")
        res["original_tokens_cached"] = comp.get("original_tokens_cached")
        res["compressed_tokens_cached"] = comp.get("compressed_tokens_cached")
        res["compression"] = comp
        res["prefix_cache"] = st.get("prefix_cache")
        res["compression_cache"] = st.get("compression_cache")
        rq = st.get("requests", {}) if isinstance(st.get("requests"), dict) else {}
        res["requests_total"] = rq.get("total")
        res["requests_cached"] = rq.get("cached")
        res["requests_failed"] = rq.get("failed")
        res["requests_by_provider"] = rq.get("by_provider")
    # Prometheus scrape for durable counters
    if metrics_txt:
        prom = {}
        for line in metrics_txt.splitlines():
            if line.startswith("#") or not line.strip():
                continue
            m = re.match(r"^(\S+?)(\{[^}]*\})?\s+([0-9eE.+-]+)$", line.strip())
            if not m:
                continue
            name, labels, val = m.group(1), m.group(2) or "", m.group(3)
            try:
                v = float(val)
            except Exception:
                continue
            key = name + labels
            prom[key] = v
        # pull the interesting ones
        want = [k for k in prom if any(s in k for s in (
            "tokens_saved", "transform_timing", "latency_ms", "overhead",
            "ccr", "retrieve", "expansion", "requests_total", "cache_bust",
            "provider_cache", "ttfb"))]
        res["prom"] = {k: prom[k] for k in want}
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", required=True)
    ap.add_argument("--configs", nargs="+", default=["hd-cache"])
    ap.add_argument("--jobs-root", default="/tmp/tb-runs/hd")
    ap.add_argument("--n", type=int, default=2)
    ap.add_argument("--setup-mult", type=float, default=4.0)
    ap.add_argument("--build-mult", type=float, default=4.0)
    ap.add_argument("--agent-mult", type=float, default=4.0)  # TB: 4x flat budget (matches baseline's long-horizon budget)
    ap.add_argument("--max-retries", type=int, default=4)
    a = ap.parse_args()
    base, token = cgenv.gateway()
    pr = price(MODEL)
    tasks = [t.strip() for t in open(a.tasks) if t.strip()]
    Path(a.jobs_root).mkdir(parents=True, exist_ok=True)
    print(f"Terminal-Bench(headroom): {len(tasks)} tasks x {len(a.configs)} configs (n={a.n}) | "
          f"price in=${pr[0]*1e6:.2f} out=${pr[1]*1e6:.2f} cread=${pr[2]*1e6:.2f} cwrite=${pr[3]*1e6:.2f}/M\n", flush=True)
    all_summ = []
    for cfg in a.configs:
        jobs = f"{a.jobs_root}/{cfg}"
        subprocess.run(f"rm -rf {jobs}", shell=True)
        print(f"### config={cfg} ...", flush=True)
        start_proxy(cfg, base, token)
        t0 = time.time()
        run_harbor(tasks, jobs, a.n, a.setup_mult, a.build_mult, a.agent_mult, a.max_retries)
        rows = parse_trials(jobs, pr)
        Path(f"{a.jobs_root}/rows-{cfg}.json").write_text(json.dumps(rows, indent=1))
        hd = parse_headroom_stats(cfg)
        stop_proxy()
        s = summarize(cfg, rows)
        s["wall_total_min"] = round((time.time() - t0) / 60, 1)
        s["headroom"] = hd
        all_summ.append(s)
        print(json.dumps(s, indent=1), flush=True)
    Path(f"{a.jobs_root}/summary.json").write_text(json.dumps(
        dict(model=MODEL, price=pr, tasks=len(tasks), configs=all_summ), indent=1))
    print("\n==== SUMMARY ====")
    for s in all_summ:
        hd = s.get("headroom", {})
        print(f"{s['config']:<10} solved={s['solved']}/{s['scored']} rate={s['solve_rate']} "
              f"steps={s['mean_steps']} cache_hit={s['cache_hit_rate']} "
              f"norm$/t={s['mean_norm_cost']} save%={hd.get('savings_percent')} "
              f"saved_tok={hd.get('tokens_saved')}")
    print(f"\nwrote {a.jobs_root}/summary.json + rows-*.json ; raw stats-*.json/metrics-*.txt in /tmp/tb-runs")


if __name__ == "__main__":
    main()
