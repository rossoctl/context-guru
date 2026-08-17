#!/usr/bin/env python3
"""Terminal-Bench 2.0 benchmark harness: run the baseline (`off` transparent
passthrough) — and, later, context-guru/headroom/rtk arms — LIVE through the proxy
with the claude-code agent on aws/claude-sonnet-5, collecting the SAME full metric
set as the SWE-bench study (reward, steps, wall-time, cache-aware token/cost
accounting, cache-hit rate).

This is a thin adaptation of `swebench.py`: the ONLY benchmark-specific change is the
Harbor dataset (`terminal-bench@2.0` instead of `swebench-verified@1.0`) and the
default jobs-root. The claude-code trajectory parser, the cache-aware cost model, and
the summarizer are agent-specific (not benchmark-specific), so every number is
computed identically to the SWE arms and is directly comparable in methodology.

Baseline routing note: like the SWE baseline arm, "baseline" runs through the `off`
passthrough proxy on :4000 (transparent — no compaction) so routing/model-forcing is
byte-identical to how the compaction arms will later run. The only difference between
baseline and a framework arm is the compaction, never the plumbing.

Usage:
  # 1-task smoke:
  python3 deploy/harbor/terminalbench.py --tasks /tmp/tb-runs/tb1.txt --configs off \
     --jobs-root /tmp/tb-runs/smoke --n 1
  # full 89-task baseline:
  python3 deploy/harbor/terminalbench.py --tasks /tmp/tb-runs/tb89.txt --configs off \
     --jobs-root /tmp/tb-runs/tb89 --n 2
"""
import argparse, glob, json, os, socket, subprocess, sys, time, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
HB = Path("/home/vpcuser/projects/context-engineering/harbor")
# Overridable because two harnesses must not share a binary BASENAME: stop_proxy kills
# by exact process name, so a second run using the same copy kills the first one's proxy
# mid-task and the trials that follow record connection refusals as model failures.
BIN = os.environ.get("CG_BENCH_BIN") or "/tmp/cg-runs/cg-proxy-d1"
# The benchmark proxy's port. Overridable because 4000 is NO LONGER FREE on a box that
# also runs the hosted service (deploy/service/), which binds 127.0.0.1:4000
# permanently. When that clashed, the benchmark proxy died with "bind: address already
# in use", every container got "API Error: Connection refused", and the summary reported
# `solved: 0, exceptions: 3` — which reads as "the model failed" rather than "the proxy
# never started". Three tasks burned ~20 minutes producing a number that meant nothing.
# Hence both the override and the hard preflight in require_free_port().
PORT = int(os.environ.get("CG_BENCH_PORT") or 4000)
# The address containers reach this host on. Set CG_LAN to this box's LAN IP;
# 127.0.0.1 only works when the agent runs on the host network. Warn loudly on the
# default: a container that cannot reach the proxy fails EVERY task, and a run of all
# failures reads as "the preset is bad" rather than "nothing could connect".
LAN = os.environ.get("CG_LAN") or "127.0.0.1"
# An ALREADY-RUNNING proxy to measure, instead of starting one. Set CG_PROXY_URL to its
# root (with or without the trailing /anthropic).
#
# This is the only honest way to benchmark a HOSTED proxy: there, the pipeline is a
# property of the authenticated tenant's own configuration, not of a --preset flag this
# harness could pass. So with CG_PROXY_URL the --configs values are LABELS only — they
# name the jobs directory and the summary row, and the pipeline is whatever that proxy
# serves the tenant whose CG_TOKEN is set.
EXT = (os.environ.get("CG_PROXY_URL") or "").rstrip("/")
if EXT.endswith("/anthropic"):
    EXT = EXT[: -len("/anthropic")]
if not os.environ.get("CG_LAN") and not EXT:
    print(f"WARNING: CG_LAN is unset, using {LAN}. Containers on a bridge network "
          "cannot reach the proxy there and every task will fail. Set CG_LAN to this "
          "host's LAN IP (`hostname -I`) unless the agent runs with --network host.",
          file=sys.stderr)


def anthropic_url():
    """The Anthropic-dialect base URL the agent under test is pointed at."""
    return (EXT + "/anthropic") if EXT else f"http://{LAN}:{PORT}/anthropic"


def stats_url():
    """Where to read the proxy's own rollup.

    Loopback by default, and CG_STATS_URL overrides it, because /stats is a
    service-wide aggregate and the proxy only trusts it unauthenticated from a loopback
    peer (proxy.statsTrusted). A CG_PROXY_URL naming this box's LAN address is NOT a
    loopback peer even when it is the same machine, so pointing the stats read at the
    same address would 403 and the summary would silently report zeros.
    """
    if u := os.environ.get("CG_STATS_URL"):
        return u
    return (EXT + "/stats") if EXT else f"http://localhost:{PORT}/stats"


DATASET = "terminal-bench@2.0"  # <-- the only benchmark-specific difference vs swebench.py
PRICES_URL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
MODEL = "aws/claude-sonnet-5"
CHEAP_MODEL = "aws/claude-haiku-4-5"  # for CG's own compaction LLM (extract_llm) in later arms


def price(model):
    fb = (2e-6, 1e-5, 2e-7, 2.5e-6)  # in, out, cache_read, cache_write(≈1.25×in)
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


def stop_proxy():
    # Kill by the basename of the binary WE started, not a hardcoded one: with
    # CG_BENCH_BIN a hardcoded name kills somebody else's proxy and leaves ours running.
    name = Path(BIN).name
    for _ in range(3):
        subprocess.run(f"pkill -x {name}", shell=True)
        time.sleep(1)
        r = subprocess.run(f"pgrep -x {name}", shell=True, capture_output=True)
        if not r.stdout.strip():
            return
    time.sleep(2)  # let the port fully release (avoids bind race on restart)


# Custom (non-preset) configs for the LATER framework arms — kept identical to the SWE
# harness so a terminal-bench framework run is a one-flag change. Baseline uses `off`.
CUSTOM_CONFIGS = {
    # cacheinject ALONE. Removes no content tokens, so any delta vs `off` is purely
    # cache mechanics (breakpoint placement + the cross-session prefix repairs that
    # apply/prefixorder.go gates on this component). Confirmed by
    # proxy_tokens_before == proxy_tokens_after in the summary.
    "cacheonly": "pipeline: [cacheinject]\n",
    "codesmart": (
        "pipeline: [format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit]\n"
        "components:\n"
        "  extract:\n"
        "    min_tokens: 400\n"
        "  extract_llm:\n"
        "    strategy: code\n"
        "    model:\n"
        "      source: config\n"
        "    min_tokens: 3000\n"
        "    trigger:\n"
        "      min_request_tokens: 3000\n"
        "    llm_every_n_requests: 1\n"
        "    llm_max_per_request: 4\n"
    ),
    # codesmart with extract_llm FORCED ON. Terminal-bench runs claude-code, which is a
    # caching backend, and extract_llm hard-declines there by default — see
    # components/offload/extract_econ.go: the component measured net-negative on every
    # caching workload tested (break-even ~30,500 tokens per output against a
    # largest-observed 2,053), so the default is a shipping decision, not a threshold.
    #
    # That means no amount of trigger tuning makes the component fire on this benchmark:
    # in the `codesmart` arm above it records runs>0 and acted=0 with the gate reason
    # `cached_prefix`. This arm exists so the claim is MEASURED on terminal-bench rather
    # than inherited from another workload's numbers. Compare it against `codesmart` on
    # cost and cache-write, not on saved_tokens alone — the whole question is whether the
    # tokens it removes are worth the cache rewrite that removing them forces.
    "codesmart_llm": (
        "pipeline: [format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit]\n"
        "components:\n"
        "  extract:\n"
        "    min_tokens: 400\n"
        "  extract_llm:\n"
        "    strategy: code\n"
        "    model:\n"
        "      source: config\n"
        "    min_tokens: 3000\n"
        "    allow_on_caching_backend: true\n"
        "    trigger:\n"
        "      min_request_tokens: 3000\n"
        "    llm_every_n_requests: 1\n"
        "    llm_max_per_request: 4\n"
    ),
}


def require_free_port():
    """Refuse to start a run whose proxy cannot bind.

    Checked once, up front, because the alternative is what actually happened: the proxy
    failed to bind, and the run continued for 20 minutes recording exception rows that
    were indistinguishable from model failures. A benchmark that reports a number when
    its own instrument is not running is worse than one that refuses to start.
    """
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            s.bind(("", PORT))
            return
        except OSError as e:
            holder = ""
            try:
                out = subprocess.run(["ss", "-ltnp"], capture_output=True, text=True).stdout
                holder = next((l.strip() for l in out.splitlines()
                               if f":{PORT} " in l or l.rstrip().endswith(f":{PORT}")), "")
            except Exception:
                pass
            sys.exit(
                f"FATAL: cannot bind port {PORT} for the benchmark proxy ({e}).\n"
                f"  holder: {holder or 'unknown'}\n"
                "  On a box that also runs the hosted service, 4000 is permanently taken.\n"
                f"  Re-run with a free port, e.g.:  CG_BENCH_PORT=4010 {' '.join(sys.argv)}\n"
                "  Refusing to continue: without the proxy every task fails with\n"
                "  'Connection refused' and the summary would report it as solved=0."
            )


def start_proxy(preset, base, token, capture=None, dump=None):
    stop_proxy()
    env = dict(os.environ, ANTHROPIC_UPSTREAM=base, ANTHROPIC_API_KEY=token,
               OPENAI_UPSTREAM=base, OPENAI_API_KEY=token, FORCE_MODEL=MODEL,
               LISTEN_ADDR=f":{PORT}", INJECT_EXPAND="auto", CONTEXT_GURU_DEBUG="1",
               CHEAP_MODEL=CHEAP_MODEL, CHEAP_MODEL_PROVIDER="anthropic",
               CHEAP_MODEL_BASE=base, CHEAP_MODEL_KEY=token, CHEAP_MODEL_AUTH="bearer")
    if capture:
        env["CONTEXT_GURU_CAPTURE"] = capture
        Path(capture).unlink(missing_ok=True)
    if dump:
        env["CONTEXT_GURU_DUMP"] = dump
        Path(dump).unlink(missing_ok=True)
    log = open(f"/tmp/tb-runs/proxy-terminalbench-{preset}.log", "w")
    PRESETS = {"off", "safe", "balanced", "aggressive", "coding", "mcp", "agent", "general"}
    if preset in CUSTOM_CONFIGS:
        cfgp = f"/tmp/tb-runs/cfg-{preset}.yaml"
        Path(cfgp).write_text(CUSTOM_CONFIGS[preset])
        args = [BIN, "--config", cfgp]
    elif preset in PRESETS:
        args = [BIN, "--preset", preset]
    else:  # single component name -> one-component pipeline
        cfgp = f"/tmp/tb-runs/cfg-{preset}.yaml"
        Path(cfgp).write_text(f"pipeline: [{preset}]\n")
        args = [BIN, "--config", cfgp]
    p = subprocess.Popen(args, cwd=str(CG), stdout=log, stderr=log,
                         env=env, preexec_fn=os.setsid)
    for _ in range(30):
        try:
            urllib.request.urlopen(f"http://localhost:{PORT}/healthz", timeout=3).read()
            return p
        except Exception:
            time.sleep(0.5)
    raise RuntimeError(f"proxy for {preset} did not come up")


def run_harbor(tasks, jobs_dir, n, setup_mult, build_mult, agent_mult, max_retries=2):
    proxy_url = anthropic_url()
    inc = " ".join(f"-i {t}" for t in tasks)
    home = os.path.expanduser("~")
    abs_path = f"{home}/.local/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    # The agent's credentials. Hosted: its own gateway key in the auth slots and the
    # tenant token in x-context-guru-token. Local: the sk-proxy placeholder, because a
    # single-tenant proxy injects its own upstream key. Both arrive as ${VAR} templates
    # that harbor expands from the environment below, so neither value is written to
    # this command line, to the run log, or to the jobs-dir run config.
    overlay, ae_auth = cgenv.agent_auth()
    cmd = (f"cd {HB} && ANTHROPIC_BASE_URL='{proxy_url}' "
           f"ANTHROPIC_API_KEY=\"${{{cgenv.KEY_VAR}}}\" ANTHROPIC_AUTH_TOKEN=\"${{{cgenv.KEY_VAR}}}\" "
           f"PATH='{abs_path}' HOME='{home}' "
           f"{home}/.local/bin/uv run harbor run -y -d {DATASET} -a claude-code -m '{MODEL}' "
           f"--env docker {inc} -n {n} --jobs-dir '{jobs_dir}' "
           # --no-delete keeps each task's image after the trial so its base layers are
           # NOT re-pulled on later runs — avoids exhausting the Docker Hub anonymous quota.
           f"--no-delete "
           f"--agent-setup-timeout-multiplier {setup_mult} --environment-build-timeout-multiplier {build_mult} "
           f"--agent-timeout-multiplier {agent_mult} --max-retries {max_retries} "
           f"--ae ANTHROPIC_BASE_URL='{proxy_url}' {ae_auth}")
    log = f"/tmp/tb-runs/run-terminalbench-{Path(jobs_dir).name}.log"
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
            continue  # job-summary file, skip
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
        fresh = max(pt - cread - cwrite, 0)  # uncached fresh input
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
    n = len(rows)
    got = [r for r in rows if r["reward"] is not None]
    solved = sum(1 for r in got if r["reward"] and r["reward"] >= 1)
    def avg(k):
        vs = [r[k] for r in rows if isinstance(r.get(k), (int, float))]
        return round(sum(vs) / len(vs), 3) if vs else None
    tot = lambda k: sum(r[k] for r in rows if isinstance(r.get(k), (int, float)))
    cacheable = tot("cache_read") + tot("fresh_input") + tot("cache_write")
    return dict(config=cfg, trials=n, scored=len(got), solved=solved,
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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", required=True)
    ap.add_argument("--configs", nargs="+", default=["off"])
    ap.add_argument("--jobs-root", default="/tmp/tb-runs/tb89")
    ap.add_argument("--n", type=int, default=2, help="harbor concurrency")
    ap.add_argument("--setup-mult", type=float, default=4.0, help="agent-setup timeout multiplier")
    ap.add_argument("--build-mult", type=float, default=4.0, help="environment-build timeout multiplier")
    # keep methodology identical to the SWE study (setup 4 / build 4 / agent 1.5) so the two
    # benchmarks are directly comparable; the task.yaml's own max_agent_timeout_sec still applies.
    ap.add_argument("--agent-mult", type=float, default=1.5, help="agent-execution timeout multiplier")
    ap.add_argument("--max-retries", type=int, default=2, help="harbor per-trial retries (bump under high concurrency to absorb transient 429s)")
    ap.add_argument("--capture-config", default=None, help="which config also captures the stream for replay")
    ap.add_argument("--dump-configs", nargs="*", default=[], help="configs that DUMP before→after change logs")
    a = ap.parse_args()
    # Before any container is built or any token is spent.
    if EXT:
        print(f"### measuring the ALREADY-RUNNING proxy at {EXT} — its pipeline is its "
              f"own; --configs {a.configs} are labels only", flush=True)
    else:
        require_free_port()
    base, token = cgenv.gateway()
    pr = price(MODEL)
    cpr = price(CHEAP_MODEL)
    tasks = [t.strip() for t in open(a.tasks) if t.strip()]
    print(f"Terminal-Bench 2.0: {len(tasks)} tasks × {len(a.configs)} configs (n={a.n}) | "
          f"price in=${pr[0]*1e6:.2f} out=${pr[1]*1e6:.2f} cread=${pr[2]*1e6:.2f} cwrite=${pr[3]*1e6:.2f} /M\n")
    all_summ = []
    for cfg in a.configs:
        jobs = f"{a.jobs_root}/{cfg}"
        subprocess.run(f"rm -rf {jobs}", shell=True)
        cap = f"/tmp/tb-runs/capture-terminalbench.jsonl" if cfg == a.capture_config else None
        dump = f"/tmp/tb-runs/dump-terminalbench-{cfg}.jsonl" if cfg in a.dump_configs else None
        print(f"### config={cfg} (capture={'yes' if cap else 'no'} dump={'yes' if dump else 'no'}) ...", flush=True)
        if not EXT:
            start_proxy(cfg, base, token, capture=cap, dump=dump)
        t0 = time.time()
        run_harbor(tasks, jobs, a.n, a.setup_mult, a.build_mult, a.agent_mult, a.max_retries)
        rows = parse_trials(jobs, pr)
        Path(f"{a.jobs_root}/rows-{cfg}.json").write_text(json.dumps(rows, indent=1))
        st = {}
        try:
            st = json.load(urllib.request.urlopen(cgenv.stats_request(stats_url()), timeout=5))
        except Exception as e:
            print(f"[stats] {stats_url()}: {e}", file=sys.stderr)
        if not EXT:
            stop_proxy()
        s = summarize(cfg, rows)
        s["proxy_savings_pct"] = round(st.get("savings_pct", 0), 2)
        s["proxy_bounces"] = st.get("bounces")
        s["wall_total_min"] = round((time.time() - t0) / 60, 1)
        comps = st.get("components", {}) or {}
        s["per_component"] = {k: {"runs": v.get("runs"), "acted": v.get("acted"),
                                  "saved_tokens": v.get("saved_tokens"),
                                  "saved_tokens_unique": v.get("saved_tokens_unique"),
                                  "overcount_ratio": v.get("overcount_ratio"),
                                  "duration_ms": round(v.get("duration_ms", 0), 1)}
                              for k, v in comps.items()}
        s["cg_added_ms_avg"] = st.get("cg_added_ms_avg")
        s["upstream_ms_avg"] = st.get("upstream_ms_avg")
        lc, li, lo = st.get("llm_calls", 0), st.get("llm_input_tokens", 0), st.get("llm_output_tokens", 0)
        s["cg_llm_calls"] = lc
        s["cg_llm_cost"] = round(li * cpr[0] + lo * cpr[1], 4)
        s["cg_total_latency_s"] = round(sum(v.get("duration_ms", 0) for v in comps.values()) / 1000, 1)
        if s.get("total_norm_cost") is not None:
            s["total_cost_incl_cg"] = round(s["total_norm_cost"] + s["cg_llm_cost"], 4)
        all_summ.append(s)
        print(json.dumps(s, indent=1), flush=True)
    Path(f"{a.jobs_root}/summary.json").write_text(json.dumps(dict(model=MODEL, price=pr, dataset=DATASET, tasks=len(tasks), configs=all_summ), indent=1))
    print("\n==== SUMMARY ====")
    hdr = (f"{'config':<10}{'solved':>8}{'rate':>7}{'steps':>7}{'cache_hit':>10}"
           f"{'agent$/t':>9}{'wall_s/t':>9}{'excs':>6}")
    print(hdr)
    for s in all_summ:
        print(f"{s['config']:<10}{str(s['solved'])+'/'+str(s['scored']):>8}{str(s['solve_rate']):>7}"
              f"{str(s['mean_steps']):>7}{str(s['cache_hit_rate']):>10}{str(s['mean_norm_cost']):>9}"
              f"{str(s['mean_agent_wall_s']):>9}{str(s['exceptions']):>6}")
    print(f"\nwrote {a.jobs_root}/summary.json + rows-*.json")


if __name__ == "__main__":
    main()
