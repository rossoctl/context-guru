#!/usr/bin/env python3
"""SWE-bench Verified benchmark harness: run baseline (off) vs a context-guru config
LIVE through the proxy with the claude-code agent, and collect the FULL metric set
per trial — reward, steps, wall-time, and cache-aware token/cost accounting — so we
can compare a config against baseline honestly (including whether offload preserves
the prompt-cache hit rate, the subtle way CE can *raise* cost while cutting content).

Per config it: (1) starts the proxy on :4000 with that preset (+ optional stream
capture for later per-component replay, + optional DUMP for real change logs,
+ INJECT_EXPAND=auto so offload is reversible); (2) runs harbor over the task list;
(3) parses each trial's result.json (reward, cost, timings) and
agent/trajectory.json final_metrics (prompt/cached/creation/read/completion tokens,
cost, steps). Writes a per-trial CSV + a per-config summary JSON, and prints a
baseline-vs-config table.

Metrics come from the claude-code agent's own trajectory (cache-aware). We also
recompute a normalized $ from tokens × the live sonnet-5 price so cost is comparable
even if the agent's internal pricing differs.

Usage: swebench.py --tasks /tmp/cg-runs/swe3.txt --configs off general --jobs-root /tmp/cg-runs/swebench --n 3
"""
import argparse, glob, json, os, socket, subprocess, sys, time, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
HB = Path("/home/vpcuser/projects/context-engineering/harbor")
BIN = "/tmp/cg-runs/cg-proxy-d1"
# Overridable: 4000 is NOT free on a box that also runs the hosted service
# (deploy/service/ binds 127.0.0.1:4000 permanently). A clash killed the benchmark proxy
# with "bind: address already in use", gave every container "Connection refused", and was
# reported as solved=0 — indistinguishable from a model failure. See require_free_port().
PORT = int(os.environ.get("CG_BENCH_PORT") or 4000)
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
CHEAP_MODEL = "aws/claude-haiku-4-5"  # fast/cheap model for CG's own compaction LLM (extract_llm)


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


def require_free_port():
    """Refuse to start a run whose proxy cannot bind, rather than reporting solved=0."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            s.bind(("", PORT))
            return
        except OSError as e:
            holder = ""
            try:
                out = subprocess.run(["ss", "-ltnp"], capture_output=True, text=True).stdout
                holder = next((l.strip() for l in out.splitlines() if f":{PORT} " in l), "")
            except Exception:
                pass
            sys.exit(
                f"FATAL: cannot bind port {PORT} for the benchmark proxy ({e}).\n"
                f"  holder: {holder or 'unknown'}\n"
                f"  Re-run with a free port, e.g.:  CG_BENCH_PORT=4010 {' '.join(sys.argv)}\n"
                "  Refusing to continue: every task would fail with 'Connection refused'\n"
                "  and the summary would report it as solved=0."
            )


def stop_proxy():
    for _ in range(3):
        subprocess.run(f"pkill -x {Path(BIN).name}", shell=True)
        time.sleep(1)
        r = subprocess.run(f"pgrep -x {Path(BIN).name}", shell=True, capture_output=True)
        if not r.stdout.strip():
            return
    time.sleep(2)  # let the port fully release (avoids bind race on restart)


# Custom (non-preset) configs, referenced by name on --configs. `codesmart` is the
# coding-appropriate best-effort config: NO blind `mask` (which hid re-referenced
# outputs and tripled steps on SWE), instead the LLM extract:code strategy does
# relevance-aware, deletion-only trimming, plus safe deterministic components.
CUSTOM_CONFIGS = {
    "codesmart": (
        # Split extract: the DETERMINISTIC `extract` runs every step (cheap, conservative
        # noise collapse), then the LLM-driven `extract_llm` does relevance-aware rewriting
        # on what's still large. NO `collapse` (blind head/tail can drop needed content),
        # NO `mask` (age-based, harmful on coding traffic). Cache-aware by default
        # (supersession/age offloaders stay in the uncached tail).
        # extract_llm BEFORE the deterministic extract: the LLM gets first crack at LARGE
        # new outputs (relevance skeletonization — the big win), marking them; the
        # deterministic extract then strips ANSI/noise on the smaller outputs the LLM left
        # untouched. (When extract ran first it marked outputs so extract_llm skipped them
        # via HasPlaceholder → the LLM never fired.)
        "pipeline: [format, textclean, searchfold, dedup, failed_run, cmdfilter, extract_llm, extract, linecap, cachesplit]\n"
        "components:\n"
        "  extract:\n"
        "    min_tokens: 400\n"  # deterministic, zero-latency: catches obvious noise every step
        "  extract_llm:\n"
        "    strategy: code\n"
        "    model:\n"
        "      source: config\n"
        # LLM compaction is cache-safe by construction: it only ever NEWLY compacts the
        # single NEWEST tool output (the one not yet in the provider cache), freezes it,
        # and replays it byte-identically on later turns — so it shrinks this turn's
        # cache-WRITE with zero prefix mutation. Fires only on MEDIUM/LARGE outputs
        # (>=1200 tok) so most turns make no model call at all (low latency + low CG cost).
        # File reads: skip_file_reads unset = AUTO (skipped on cached agents where they
        # already bill cheap; skeletonized on non-caching backends). failed_run likewise
        # auto-skips NEW collapses on cached agents (superseded runs already bill cheap).
        # Threshold raised to 1500: the LLM fires only on genuinely LARGE outputs (which
        # hold the bulk of the token mass), so most turns make no model call — cutting
        # latency and CG cost — while the free deterministic `extract` handles smaller
        # noise. Parallel per-output calls (independent, ~1 call wall-time per turn).
        # Absolute floor (NO min_output_frac: OutputFloor returns the fraction *instead of*
        # the absolute, and sonnet-5's window resolves to 1M → 0.0075*1M=7500 → almost
        # nothing qualified → 0 LLM calls. Absolute 3000 is predictable and fires on large
        # file reads / big logs — the token mass — without a flood of medium-output calls
        # (bounds latency). Parallel per-output calls keep a turn's batch to ~1 call's wall.
        "    min_tokens: 3000\n"
        "    trigger:\n"
        "      min_request_tokens: 3000\n"
        "    llm_every_n_requests: 1\n"
        "    llm_max_per_request: 4\n"
    ),
    # cacheinject ALONE. Isolates the prompt-cache lever from token reduction: no
    # component here removes a single content token, so any cost delta vs `off` is
    # purely breakpoint placement. This is the arm that tests whether cacheinject
    # earns its place — the offline model says placement headroom against
    # claude-code's own breakpoints is ~0%, and this is the live check of that.
    "cacheonly": "pipeline: [cacheinject]\n",
    # conservative deterministic-only (no LLM, no mask): safe control
    "codesafe": (
        "pipeline: [format, textclean, searchfold, dedup, failed_run, cmdfilter, extract, collapse, linecap, cachesplit]\n"
        "components:\n"
        "  collapse:\n"
        "    max_tokens: 3000\n"
    ),
}


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
    log = open(f"/tmp/cg-runs/proxy-swebench-{preset}.log", "w")
    PRESETS = {"off", "safe", "balanced", "aggressive", "coding", "mcp", "agent", "general"}
    if preset in CUSTOM_CONFIGS:
        cfgp = f"/tmp/cg-runs/cfg-{preset}.yaml"
        Path(cfgp).write_text(CUSTOM_CONFIGS[preset])
        args = [BIN, "--config", cfgp]
    elif preset in PRESETS:
        args = [BIN, "--preset", preset]
    else:  # single component name -> one-component pipeline
        cfgp = f"/tmp/cg-runs/cfg-{preset}.yaml"
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


def run_harbor(tasks, jobs_dir, n, setup_mult, build_mult, agent_mult):
    proxy_url = f"http://{LAN}:{PORT}/anthropic"
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
           f"{home}/.local/bin/uv run harbor run -y -d swebench-verified@1.0 -a claude-code -m '{MODEL}' "
           f"--env docker {inc} -n {n} --jobs-dir '{jobs_dir}' "
           # --no-delete keeps each task's image after the trial so it is NOT re-pulled
           # on later runs — avoids exhausting the Docker Hub anonymous pull quota (the
           # gotcha that made a back-to-back off+codesmart run fail ~30 env builds).
           f"--no-delete "
           f"--agent-setup-timeout-multiplier {setup_mult} --environment-build-timeout-multiplier {build_mult} "
           f"--agent-timeout-multiplier {agent_mult} --max-retries 2 "
           f"--ae ANTHROPIC_BASE_URL='{proxy_url}' {ae_auth}")
    log = f"/tmp/cg-runs/run-swebench-{Path(jobs_dir).name}.log"
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
        # normalized cache-aware cost from live sonnet-5 pricing
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
                total_norm_cost=round(tot("norm_cost"), 4), mean_norm_cost=avg("norm_cost"),
                mean_agent_cost=avg("agent_cost"), mean_wall_s=avg("wall_s"),
                mean_agent_wall_s=avg("agent_wall_s"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", required=True)
    ap.add_argument("--configs", nargs="+", default=["off", "general"])
    ap.add_argument("--jobs-root", default="/tmp/cg-runs/swebench")
    ap.add_argument("--n", type=int, default=3, help="harbor concurrency")
    ap.add_argument("--setup-mult", type=float, default=4.0, help="agent-setup timeout multiplier")
    ap.add_argument("--build-mult", type=float, default=4.0, help="environment-build timeout multiplier")
    ap.add_argument("--agent-mult", type=float, default=1.5, help="agent-execution timeout multiplier")
    ap.add_argument("--capture-config", default="off", help="which config also captures the stream for replay")
    ap.add_argument("--dump-configs", nargs="*", default=["general"], help="configs that DUMP before→after change logs")
    a = ap.parse_args()
    require_free_port()  # before any container is built or token spent
    base, token = cgenv.gateway()
    pr = price(MODEL)
    cpr = price(CHEAP_MODEL)  # CG's OWN compaction LLM is the CHEAP model — price it at ITS rate
    tasks = [t.strip() for t in open(a.tasks) if t.strip()]
    print(f"SWE-bench: {len(tasks)} tasks × {len(a.configs)} configs (n={a.n}) | "
          f"price in=${pr[0]*1e6:.2f} out=${pr[1]*1e6:.2f} cread=${pr[2]*1e6:.2f} cwrite=${pr[3]*1e6:.2f} /M\n")
    all_summ = []
    for cfg in a.configs:
        jobs = f"{a.jobs_root}/{cfg}"
        subprocess.run(f"rm -rf {jobs}", shell=True)
        # Captures go under the RUN's jobs_root, not a shared /tmp path. A fixed path
        # is silently destructive: start_proxy unlinks it, so launching any new run
        # truncates the capture an earlier analysis was computed from — which is
        # exactly how the 472-request capture behind docs/cache-optimization.md was
        # lost mid-analysis. Run-scoped means a new run can never clobber an old one.
        cap = f"{a.jobs_root}/capture-{cfg}.jsonl" if cfg == a.capture_config else None
        dump = f"{a.jobs_root}/dump-{cfg}.jsonl" if cfg in a.dump_configs else None
        print(f"### config={cfg} (capture={'yes' if cap else 'no'} dump={'yes' if dump else 'no'}) ...", flush=True)
        start_proxy(cfg, base, token, capture=cap, dump=dump)
        t0 = time.time()
        run_harbor(tasks, jobs, a.n, a.setup_mult, a.build_mult, a.agent_mult)
        rows = parse_trials(jobs, pr)
        Path(f"{a.jobs_root}/rows-{cfg}.json").write_text(json.dumps(rows, indent=1))
        st = {}
        try:
            st = json.load(urllib.request.urlopen(f"http://localhost:{PORT}/stats", timeout=5))
        except Exception:
            pass
        stop_proxy()
        s = summarize(cfg, rows)
        s["proxy_savings_pct"] = round(st.get("savings_pct", 0), 2)
        s["proxy_bounces"] = st.get("bounces")
        s["proxy_tokens_before"] = st.get("tokens_before")
        s["proxy_tokens_after"] = st.get("tokens_after")
        s["wall_total_min"] = round((time.time() - t0) / 60, 1)
        # Per-component CG metrics: savings, invocations, and the component's OWN
        # latency (duration_ms) — the cost CG itself adds, separate from the agent.
        comps = st.get("components", {}) or {}
        s["per_component"] = {k: {"runs": v.get("runs"), "acted": v.get("acted"),
                                  "saved_tokens": v.get("saved_tokens"),
                                  "saved_tokens_unique": v.get("saved_tokens_unique"),
                                  "overcount_ratio": v.get("overcount_ratio"),
                                  "duration_ms": round(v.get("duration_ms", 0), 1)}
                              for k, v in comps.items()}
        s["cg_added_ms_avg"] = st.get("cg_added_ms_avg")
        s["upstream_ms_avg"] = st.get("upstream_ms_avg")
        s["upstream_ms_avg_bypassed"] = st.get("upstream_ms_avg_bypassed")
        # CG components' OWN LLM cost (cheap-model calls, e.g. extract:code) — priced
        # like the agent (input/output). This is spend CG adds on top of the agent.
        lc, li, lo = st.get("llm_calls", 0), st.get("llm_input_tokens", 0), st.get("llm_output_tokens", 0)
        s["cg_llm_calls"] = lc
        s["cg_llm_input_tokens"] = li
        s["cg_llm_output_tokens"] = lo
        s["cg_llm_cost"] = round(li * cpr[0] + lo * cpr[1], 4)  # cheap-model rate, not agent rate
        s["cg_total_latency_s"] = round(sum(v.get("duration_ms", 0) for v in comps.values()) / 1000, 1)
        # honest total cost = agent cost (normalized) + CG's own LLM cost
        if s.get("total_norm_cost") is not None:
            s["total_cost_incl_cg"] = round(s["total_norm_cost"] + s["cg_llm_cost"], 4)
        all_summ.append(s)
        print(json.dumps(s, indent=1), flush=True)
    Path(f"{a.jobs_root}/summary.json").write_text(json.dumps(dict(model=MODEL, price=pr, tasks=len(tasks), configs=all_summ), indent=1))
    # comparison table
    print("\n==== SUMMARY ====")
    hdr = (f"{'config':<10}{'solved':>8}{'rate':>7}{'steps':>7}{'cache_hit':>10}"
           f"{'agent$/t':>9}{'cg_llm$':>8}{'save%':>7}{'cg_lat_s':>9}{'bounces':>8}")
    print(hdr)
    for s in all_summ:
        print(f"{s['config']:<10}{str(s['solved'])+'/'+str(s['scored']):>8}{str(s['solve_rate']):>7}"
              f"{str(s['mean_steps']):>7}{str(s['cache_hit_rate']):>10}{str(s['mean_norm_cost']):>9}"
              f"{str(s.get('cg_llm_cost')):>8}{str(s['proxy_savings_pct']):>7}{str(s.get('cg_total_latency_s')):>9}{str(s['proxy_bounces']):>8}")
    # per-component breakdown per config
    print("\n---- per-component (saved_tokens / runs / own-latency_s) ----")
    for s in all_summ:
        pc = s.get("per_component", {})
        if not pc:
            continue
        parts = [f"{k}:{v['saved_tokens']}tok/{v['runs']}r/{round((v['duration_ms'] or 0)/1000,1)}s" for k, v in sorted(pc.items(), key=lambda x: -(x[1]['saved_tokens'] or 0))]
        print(f"  {s['config']}: " + " | ".join(parts))
        if s.get("cg_llm_calls"):
            print(f"    cg_llm: {s['cg_llm_calls']} calls, in={s['cg_llm_input_tokens']} out={s['cg_llm_output_tokens']} tokens, cost=${s['cg_llm_cost']}")
    print(f"\nwrote {a.jobs_root}/summary.json + rows-*.json")


if __name__ == "__main__":
    main()
