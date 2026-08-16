#!/usr/bin/env python3
"""SWE-bench Verified harness for RTK (Rust Token Killer) — a 4th arm for the
three-way study, using IDENTICAL trajectory accounting to swebench.py so results
are directly comparable to baseline / context-guru / headroom.

rtk is NOT a request-stream proxy. It is a Claude Code ``PreToolUse`` hook that
rewrites Bash commands (``pytest`` -> ``rtk pytest``, ``cat`` -> ``rtk read``,
``git status`` -> ``rtk git status``) INSIDE the task container, compressing bash
output at the shell before it enters the model context. So there is nothing to
proxy for compaction — model routing is made IDENTICAL to the baseline by
running the same context-guru ``off`` passthrough proxy on :4000. The ONLY
difference from baseline is the in-container bash compression, delivered by the
custom ``claude-code-rtk`` Harbor agent (see
harbor/src/harbor/agents/installed/claude_code_rtk.py).

Per config it: (1) starts cg-proxy-d1 with preset ``off`` (pure passthrough,
FORCE_MODEL=sonnet-5) — same routing as the baseline ``rows-off.json``;
(2) runs harbor with ``-a claude-code-rtk`` (uploads the rtk binary + installs
the PreToolUse hook in-container), passing RTK_BIN_HOST so the agent finds the
host-built static-musl binary; (3) parses each trial's result.json + trajectory
final_metrics for reward/steps/cache-aware tokens/cost (identical to swebench.py);
(4) reads each trial's /logs/agent/rtk-gain.json for rtk's OWN bash-output
savings ledger. Writes rows-<cfg>.json + summary.json under the jobs-root.

The ``off`` baseline is NOT re-run — reuse /tmp/cg-runs/final50/rows-off.json.

Usage:
  swebench_rtk.py --tasks /tmp/cg-runs/swe3-verify.txt --jobs-root /tmp/rtk-runs/swe3 --n 2
"""
import argparse, glob, json, os, subprocess, sys, time, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
HB = Path("/home/vpcuser/projects/context-engineering/harbor")
BIN = "/tmp/cg-runs/cg-proxy-d1"          # context-guru proxy (off = passthrough)
RTK_BIN = os.environ.get("RTK_BIN_HOST", "/tmp/rtk-runs/rtk")  # host static-musl rtk
PORT = 4000
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


def stop_proxy():
    for _ in range(3):
        subprocess.run(f"pkill -x {Path(BIN).name}", shell=True)
        time.sleep(1)
        r = subprocess.run(f"pgrep -x {Path(BIN).name}", shell=True, capture_output=True)
        if not r.stdout.strip():
            return
    time.sleep(2)


def start_proxy(base, token):
    """Start cg-proxy-d1 as a pure passthrough (preset off) — identical routing
    to the baseline, so the only variable is rtk's in-container compression."""
    stop_proxy()
    env = dict(os.environ, ANTHROPIC_UPSTREAM=base, ANTHROPIC_API_KEY=token,
               OPENAI_UPSTREAM=base, OPENAI_API_KEY=token, FORCE_MODEL=MODEL,
               LISTEN_ADDR=f":{PORT}")
    log = open("/tmp/tb-runs/proxy-rtk-off.log", "w")
    p = subprocess.Popen([BIN, "--preset", "off"], cwd=str(CG), stdout=log, stderr=log,
                         env=env, preexec_fn=os.setsid)
    for _ in range(30):
        try:
            urllib.request.urlopen(f"http://localhost:{PORT}/healthz", timeout=3).read()
            return p
        except Exception:
            time.sleep(0.5)
    raise RuntimeError("off proxy did not come up")


def run_harbor(tasks, jobs_dir, n, setup_mult, build_mult, agent_mult, max_retries=4):
    proxy_url = f"http://{LAN}:{PORT}/anthropic"
    inc = " ".join(f"-i {t}" for t in tasks)
    home = os.path.expanduser("~")
    abs_path = f"{home}/.local/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    # RTK_BIN_HOST tells the claude-code-rtk agent where the host rtk binary is.
    # The agent's credentials. Hosted: its own gateway key in the auth slots and the
    # tenant token in x-context-guru-token. Local: the sk-proxy placeholder, because a
    # single-tenant proxy injects its own upstream key. Both arrive as ${VAR} templates
    # that harbor expands from the environment below, so neither value is written to
    # this command line, to the run log, or to the jobs-dir run config.
    overlay, ae_auth = cgenv.agent_auth()
    cmd = (f"cd {HB} && ANTHROPIC_BASE_URL='{proxy_url}' ANTHROPIC_API_KEY=\"${{CG_AGENT_KEY}}\" "
           f"ANTHROPIC_AUTH_TOKEN=\"${{CG_AGENT_KEY}}\" RTK_BIN_HOST='{RTK_BIN}' PATH='{abs_path}' HOME='{home}' "
           f"{home}/.local/bin/uv run harbor run -y -d terminal-bench@2.0 -a claude-code-rtk -m '{MODEL}' "
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


def read_rtk_gain(tdir):
    """rtk's OWN bash-output savings ledger for this trial (bytes/4 estimate)."""
    gp = tdir / "agent" / "rtk-gain.json"
    if not gp.exists():
        return None
    try:
        g = json.load(open(gp))
    except Exception:
        return None
    s = g.get("summary") or {}
    by_cmd = g.get("byCommand") or g.get("by_command") or []
    return dict(commands=s.get("total_commands"), input_tokens=s.get("total_input"),
                output_tokens=s.get("total_output"), saved_tokens=s.get("total_saved"),
                savings_pct=s.get("avg_savings_pct"), total_time_ms=s.get("total_time_ms"),
                by_command=by_cmd)


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
                         exception=bool(d.get("exception_info")),
                         rtk=read_rtk_gain(tdir)))
    return rows


def summarize(cfg, rows):
    got = [r for r in rows if r["reward"] is not None]
    solved = sum(1 for r in got if r["reward"] and r["reward"] >= 1)
    def avg(k):
        vs = [r[k] for r in rows if isinstance(r.get(k), (int, float))]
        return round(sum(vs) / len(vs), 3) if vs else None
    tot = lambda k: sum(r[k] for r in rows if isinstance(r.get(k), (int, float)))
    cacheable = tot("cache_read") + tot("fresh_input") + tot("cache_write")
    # aggregate rtk's own ledger across trials
    rk = [r["rtk"] for r in rows if r.get("rtk")]
    rtk_before = sum((x.get("input_tokens") or 0) for x in rk)
    rtk_after = sum((x.get("output_tokens") or 0) for x in rk)
    rtk_saved = sum((x.get("saved_tokens") or 0) for x in rk)
    rtk_cmds = sum((x.get("commands") or 0) for x in rk)
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
                mean_agent_wall_s=avg("agent_wall_s"),
                rtk_trials_with_ledger=len(rk), rtk_commands=rtk_cmds,
                rtk_bash_tokens_before=rtk_before, rtk_bash_tokens_after=rtk_after,
                rtk_bash_tokens_saved=rtk_saved,
                rtk_bash_savings_pct=round(100 * rtk_saved / rtk_before, 2) if rtk_before else None)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", required=True)
    ap.add_argument("--config", default="rtk", help="label for this arm")
    ap.add_argument("--jobs-root", default="/tmp/tb-runs/rtk")
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
    print(f"Terminal-Bench(rtk): {len(tasks)} tasks (n={a.n}) | rtk_bin={RTK_BIN} | "
          f"price in=${pr[0]*1e6:.2f} out=${pr[1]*1e6:.2f} cread=${pr[2]*1e6:.2f} cwrite=${pr[3]*1e6:.2f}/M\n", flush=True)
    if not Path(RTK_BIN).exists():
        raise SystemExit(f"rtk binary not found at {RTK_BIN}; set RTK_BIN_HOST")
    cfg = a.config
    jobs = f"{a.jobs_root}/{cfg}"
    subprocess.run(f"rm -rf {jobs}", shell=True)
    print(f"### config={cfg} (rtk in-container hook; off-proxy routing) ...", flush=True)
    start_proxy(base, token)
    t0 = time.time()
    run_harbor(tasks, jobs, a.n, a.setup_mult, a.build_mult, a.agent_mult, a.max_retries)
    rows = parse_trials(jobs, pr)
    Path(f"{a.jobs_root}/rows-{cfg}.json").write_text(json.dumps(rows, indent=1))
    stop_proxy()
    s = summarize(cfg, rows)
    s["wall_total_min"] = round((time.time() - t0) / 60, 1)
    Path(f"{a.jobs_root}/summary.json").write_text(json.dumps(
        dict(model=MODEL, price=pr, tasks=len(tasks), configs=[s]), indent=1))
    print(json.dumps(s, indent=1), flush=True)
    print("\n==== SUMMARY ====")
    print(f"{cfg:<12} solved={s['solved']}/{s['scored']} rate={s['solve_rate']} "
          f"steps={s['mean_steps']} cache_hit={s['cache_hit_rate']} "
          f"norm$/t={s['mean_norm_cost']} exceptions={s['exceptions']}")
    print(f"  rtk ledger: {s['rtk_trials_with_ledger']}/{len(rows)} trials, {s['rtk_commands']} cmds, "
          f"bash tokens {s['rtk_bash_tokens_before']}->{s['rtk_bash_tokens_after']} "
          f"(saved {s['rtk_bash_tokens_saved']} = {s['rtk_bash_savings_pct']}%)")
    print(f"\nwrote {a.jobs_root}/summary.json + rows-{cfg}.json")


if __name__ == "__main__":
    main()
