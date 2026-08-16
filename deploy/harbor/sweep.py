#!/usr/bin/env python3
"""Portable harbor sweep for context-guru (Linux, IBM LiteLLM gateway).

One cell = (benchmark, task, config). For each config it (re)starts the
context-guru proxy on a port with that pipeline (fresh /stats), then runs each
task's agent through the proxy and records reward + within-run token savings.

Replaces the macOS/eval-containers sweep.py: no docker-compose, no eval-containers
repo — it drives `harbor run` directly and points the agent's ANTHROPIC_BASE_URL at
the proxy. Resumable: cells already in the CSV are skipped.

Model: aws/claude-sonnet-5 via the IBM LiteLLM gateway (creds from ~/.claude/settings.json).
Agent: claude-code (Anthropic dialect -> proxy /anthropic route).

Usage:
  sweep.py --matrix validate            # small baseline+agent validation matrix
  sweep.py --configs off agent mask extract --benchmark terminal-bench-sample@2.0 --n-tasks 2
  sweep.py --benchmark swebench-verified@1.0 --configs off agent --n-tasks 1 --timeout 5400
"""
import argparse, csv, json, os, re, shutil, signal, socket, subprocess, sys, time, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
HARBOR = Path("/home/vpcuser/projects/context-engineering/harbor")
BIN = "/tmp/cg-runs/cg-proxy-d1"          # stable snapshot; override with --bin
RUNS = Path("/tmp/cg-runs")
CSV_PATH = RUNS / "sweep-results.csv"
PORT = 4010                                # dedicated sweep proxy port
MODEL = "aws/claude-sonnet-5"

# config name -> pipeline (comma list) | "off" (passthrough) | "preset:<name>"
CONFIGS = {
    "off":        "off",
    "agent":      "preset:agent",
    "balanced":   "preset:balanced",
    "format":     "format",
    "dedup":      "dedup",
    "failed_run": "failed_run",
    "cmdfilter":  "cmdfilter",
    "cacheinject":"cacheinject",
    "mask":       "mask",
    "collapse":   "collapse",
    "extract":    "extract",
    "smartcrush": "smartcrush",
    "skeleton":   "skeleton",
    "phi_evict":  "phi_evict",
    "toon":       "toon",
}
FIELDS = ["benchmark", "task", "config", "reward", "wall_s", "gw_requests",
          "gw_before", "gw_after", "gw_saved", "gw_pct", "per_component", "note"]




def host_ip():
    return subprocess.run("hostname -I", shell=True, capture_output=True, text=True).stdout.split()[0]


def stats(port):
    try:
        with urllib.request.urlopen(f"http://localhost:{port}/stats", timeout=5) as r:
            return json.load(r)
    except Exception:
        return {}


def start_proxy(config, base, token, dump):
    """(Re)start the proxy for a config; returns the Popen. Fresh process => fresh /stats."""
    subprocess.run(f"pkill -x {Path(BIN).name}", shell=True); time.sleep(1.0)
    spec = CONFIGS[config]
    env = dict(os.environ,
               ANTHROPIC_UPSTREAM=base, ANTHROPIC_API_KEY=token,
               OPENAI_UPSTREAM=base, OPENAI_API_KEY=token,
               FORCE_MODEL=MODEL, LISTEN_ADDR=f":{PORT}",
               CONTEXT_GURU_DEBUG="1", CONTEXT_GURU_DUMP=dump)
    if spec == "off":
        args = [BIN, "--preset", "off"]
    elif spec.startswith("preset:"):
        args = [BIN, "--preset", spec.split(":", 1)[1]]
    else:
        cfg = RUNS / f"cfg-{config}.yaml"
        cfg.write_text(f"pipeline: [{spec}]\n")
        args = [BIN, "--config", str(cfg)]
    p = subprocess.Popen(args, cwd=str(CG), stdout=open(RUNS / f"proxy-{config}.log", "w"),
                         stderr=subprocess.STDOUT, env=env, preexec_fn=os.setsid)
    for _ in range(30):
        if stats(PORT) != {} or _healthz(PORT):
            return p
        time.sleep(0.5)
    return p


def _healthz(port):
    try:
        with urllib.request.urlopen(f"http://localhost:{port}/healthz", timeout=3) as r:
            return r.status == 200
    except Exception:
        return False


def latest_reward(jobs_dir):
    """Find the most recent trial reward.txt under jobs_dir."""
    cands = sorted(Path(jobs_dir).glob("*/*/verifier/reward.txt"), key=lambda p: p.stat().st_mtime)
    if not cands:
        return None
    txt = cands[-1].read_text().strip()
    try:
        return float(txt)
    except ValueError:
        return txt


def run_cell(benchmark, task_glob, config, base, token, timeout):
    proxy_url = f"http://{host_ip()}:{PORT}/anthropic"
    dump = str(RUNS / f"dump-{config}-{benchmark.replace('@','_').replace('/','_')}.jsonl")
    open(dump, "w").close()
    start_proxy(config, base, token, dump)
    before = stats(PORT)
    jobs_dir = RUNS / f"jobs-{benchmark.replace('@','_').replace('/','_')}-{config}"
    sel = f"-i '{task_glob}'" if task_glob else "-l 1"
    # Credentials via ${VAR} templates the shell and harbor expand, so nothing lands in
    # argv or in the jobs dir. See cgenv.agent_auth().
    overlay, ae_auth = cgenv.agent_auth()
    cmd = (f"ANTHROPIC_BASE_URL='{proxy_url}' ANTHROPIC_API_KEY=\"${{CG_AGENT_KEY}}\" "
           f"ANTHROPIC_AUTH_TOKEN=\"${{CG_AGENT_KEY}}\" PATH='{os.environ['PATH']}:{os.path.expanduser('~/.local/bin')}' "
           f"uv run harbor run -d {benchmark} -a claude-code -m {MODEL} -n 1 {sel} "
           f"--jobs-dir '{jobs_dir}' --force-build "
           f"--ae ANTHROPIC_BASE_URL='{proxy_url}' {ae_auth}")
    t0 = time.time()
    note = "ok"
    try:
        r = subprocess.run(["sg", "docker", "-c", cmd], cwd=str(HARBOR),
                           capture_output=True, text=True, timeout=timeout,
                           env=dict(os.environ, **overlay))
        if r.returncode != 0:
            note = "run-rc=%d:%s" % (r.returncode, (r.stderr or r.stdout)[-160:].replace("\n", " "))
    except subprocess.TimeoutExpired:
        note = "timeout"
    wall = int(time.time() - t0)
    after = stats(PORT)
    reward = latest_reward(jobs_dir)
    percomp = {k: v.get("saved_tokens", 0) for k, v in after.get("components", {}).items() if v.get("saved_tokens")}
    return {
        "benchmark": benchmark, "task": task_glob or "first", "config": config,
        "reward": reward, "wall_s": wall,
        "gw_requests": after.get("requests", ""), "gw_before": after.get("tokens_before", ""),
        "gw_after": after.get("tokens_after", ""), "gw_saved": after.get("saved_tokens", ""),
        "gw_pct": round(after.get("savings_pct", 0), 2),
        "per_component": json.dumps(percomp), "note": note,
    }


def done_cells():
    if not CSV_PATH.exists():
        return set()
    with open(CSV_PATH) as f:
        return {(r["benchmark"], r["task"], r["config"]) for r in csv.DictReader(f)}


def append(row):
    new = not CSV_PATH.exists()
    with open(CSV_PATH, "a", newline="") as f:
        w = csv.DictWriter(f, fieldnames=FIELDS)
        if new:
            w.writeheader()
        w.writerow(row)


MATRICES = {
    # small end-to-end validation: baseline vs agent preset on two benchmarks
    "validate": [
        ("terminal-bench-sample@2.0", ["chess-best-move", "hello-world"], ["off", "agent"]),
    ],
}


def main():
    global BIN
    ap = argparse.ArgumentParser()
    ap.add_argument("--matrix", choices=list(MATRICES))
    ap.add_argument("--benchmark")
    ap.add_argument("--tasks", nargs="*", help="task-name globs (harbor -i); default first task")
    ap.add_argument("--configs", nargs="*", default=["off", "agent"])
    ap.add_argument("--timeout", type=int, default=3600)
    ap.add_argument("--bin", default=BIN)
    a = ap.parse_args()
    BIN = a.bin
    base, token = cgenv.gateway()
    cells = []
    if a.matrix:
        for bench, tasks, configs in MATRICES[a.matrix]:
            for t in tasks:
                for c in configs:
                    cells.append((bench, t, c))
    else:
        tasks = a.tasks or [""]
        for t in tasks:
            for c in a.configs:
                cells.append((a.benchmark, t, c))
    done = done_cells()
    print(f"{len(cells)} cells; {len(done)} already done", flush=True)
    for bench, task, config in cells:
        if (bench, task, config) in done:
            print(f"skip {bench} {task} {config}", flush=True); continue
        print(f"RUN {bench} | {task} | {config}", flush=True)
        row = run_cell(bench, task, config, base, token, a.timeout)
        append(row)
        print(f"  -> reward={row['reward']} saved%={row['gw_pct']} req={row['gw_requests']} note={row['note']}", flush=True)
    subprocess.run(f"pkill -x {Path(BIN).name}", shell=True)
    print("done", flush=True)


if __name__ == "__main__":
    main()
