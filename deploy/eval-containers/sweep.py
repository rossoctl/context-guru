#!/usr/bin/env python3
"""Resumable SWE-bench sweep: baseline vs context-guru (per-component + combined)
vs competitors, through the eval-containers compose stack against the IBM litellm
upstream (claude-sonnet-4-6).

One "cell" = (task, config). Each cell runs the eval-containers stack with the
right gateway image / preset / agent, waits for the runner to exit, then records
reward + wall-clock (from the output volume) and context-guru's own token-savings
(from the gateway /stats) into a CSV. Resumable: cells already in the CSV are
skipped, so it can be re-run/continued freely.

Config selector grammar (col 3):
  cg:off               our gateway, empty pipeline (passthrough baseline)
  cg:<a,b,c>           our gateway, CONTEXT_GURU_PIPELINE=a,b,c
  cg:preset=balanced   our gateway, a named preset
  headroom             headroom gateway (wired via a separate override)

Usage:
  sweep.py --tasks t1 t2 ...   (defaults to the built-in 10)
  sweep.py --only baseline cg-dedup   (subset of configs, e.g. for validation)
  sweep.py --one-task t1              (run every config on a single task)
"""
import argparse, csv, json, os, subprocess, sys, time

ROOT = os.environ.get("EVAL_ROOT", os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..")))   # holds eval-containers/ and this repo
SWE = f"{ROOT}/eval-containers/containers/benchmarks/swe-bench"
DEPLOY = f"{ROOT}/lab-context-engineering/deploy/eval-containers"
CG_OVERRIDE = f"{DEPLOY}/compose.contextguru.yaml"
HEADROOM_OVERRIDE = f"{DEPLOY}/compose.headroom.yaml"
RESULTS = f"{DEPLOY}/sweep-results.csv"
POLL_SECS, POLL_MAX = 15, 320  # up to ~80 min/cell backstop

TASKS = [
    "sympy__sympy-13647", "sympy__sympy-16766", "sympy__sympy-20438",
    "sphinx-doc__sphinx-7910", "sphinx-doc__sphinx-9320",
    "scikit-learn__scikit-learn-12973", "scikit-learn__scikit-learn-25931",
    "pydata__xarray-4629", "django__django-11820", "django__django-14089",
]
# name, agent, selector. Every context-guru component is run ALONE (default
# config, no task-specific tuning) so we can see which actually fire on real
# Claude Code traffic, plus the combined preset and competitors.
CONFIGS = [
    ("baseline", "claude-code", "cg:off"),
    # lossless / provably-unneeded
    ("cg-format", "claude-code", "cg:format"),
    ("cg-dedup", "claude-code", "cg:dedup"),
    ("cg-cmdfilter", "claude-code", "cg:cmdfilter"),
    ("cg-cacheinject", "claude-code", "cg:cacheinject"),
    ("cg-failed_run", "claude-code", "cg:failed_run"),
    # lossy offloaders (run without expand yet — measures savings AND reward impact)
    ("cg-skeleton", "claude-code", "cg:skeleton"),
    ("cg-collapse", "claude-code", "cg:collapse"),
    ("cg-mask", "claude-code", "cg:mask"),
    ("cg-smartcrush", "claude-code", "cg:smartcrush"),
    ("cg-extract", "claude-code", "cg:extract"),
    ("cg-phi_evict", "claude-code", "cg:phi_evict"),
    # combined + competitors
    ("cg-balanced", "claude-code", "cg:preset=balanced"),
    ("rtk", "claude-code-rtk", "cg:off"),
    ("headroom", "claude-code", "headroom"),
    # LLM-based (model.source: incoming -> same model as the agent, claude-sonnet-4-6)
    ("cg-extract-code", "claude-code", "yaml:extract-code"),
    ("cg-summarize", "claude-code", "yaml:summarize"),
]

# Full config documents for LLM-based configs (per-component config the simple
# CONTEXT_GURU_PIPELINE list can't express). incoming source = the agent's own model.
FULL_CONFIGS = {
    # LLM components are gated by a `trigger` (don't fire every turn) and reuse
    # prior compactions from session state (don't re-call the model on re-sent
    # content). extract fires only on large tool outputs in a large request;
    # summarize only on a large/deep transcript, then reuses its summary until the
    # un-summarized tail grows past resummarize_tokens.
    "extract-code": "pipeline: [extract]\ncomponents:\n"
                    "  extract: {strategy: code, model: {source: incoming},\n"
                    "            trigger: {min_output_tokens: 700, min_request_tokens: 4000}}\n",
    "summarize": "pipeline: [summarize]\ncomponents:\n"
                 "  summarize: {keep_last: 3, min_tokens: 1000, resummarize_tokens: 5000,\n"
                 "              model: {source: incoming},\n"
                 "              trigger: {min_request_tokens: 6000, min_messages: 10}}\n",
}
FIELDS = ["task", "config", "agent", "reward", "passed", "wall_s",
          "gw_requests", "gw_before", "gw_after", "gw_saved", "gw_pct", "note"]


def creds():
    s = json.load(open(os.path.expanduser("~/.claude/settings.json")))["env"]
    return s["ANTHROPIC_BASE_URL"], s["ANTHROPIC_AUTH_TOKEN"]


def sh(cmd, **kw):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True, **kw)


def done_cells():
    if not os.path.exists(RESULTS):
        return set()
    with open(RESULTS) as f:
        return {(r["task"], r["config"]) for r in csv.DictReader(f)}


def append_row(row):
    new = not os.path.exists(RESULTS)
    with open(RESULTS, "a", newline="") as f:
        w = csv.DictWriter(f, fieldnames=FIELDS)
        if new:
            w.writeheader()
        w.writerow(row)


def vol_json(proj, path):
    r = sh(f"docker run --rm -v {proj}_output:/o alpine cat /o/{path} 2>/dev/null")
    try:
        return json.loads(r.stdout)
    except Exception:
        return {}


CG_IMAGE = "context-guru:local"
CG_TAR = "/tmp/context-guru-local.tar"


def ensure_cg_image():
    """Insurance against Rancher Desktop's disk-pressure image GC evicting our
    gateway mid-sweep: keep a tar and reload it if the tag goes missing."""
    have = sh(f"docker image inspect {CG_IMAGE} >/dev/null 2>&1").returncode == 0
    if have and not os.path.exists(CG_TAR):
        sh(f"docker save -o {CG_TAR} {CG_IMAGE}")
    elif not have and os.path.exists(CG_TAR):
        print("    (gateway image was evicted — reloading from tar)", flush=True)
        sh(f"docker load -i {CG_TAR}")


def run_cell(task, name, agent, selector, base, token):
    if selector != "headroom":
        ensure_cg_image()
    proj = f"sw-{name}".lower().replace("_", "-")
    env = dict(os.environ)
    env.update(
        ANTHROPIC_API_BASE=base, ANTHROPIC_API_KEY=token,
        OPENAI_API_KEY="unused", OPENAI_API_BASE="http://unused.invalid/v1",
        EVAL_MODEL="anthropic/claude-sonnet-4-6", EVAL_TIMEOUT="1800",
        EVAL_TASK_ID=task, EVAL_AGENT=agent, EVAL_GATEWAY_LABEL=name,
    )
    files = f"-f {SWE}/compose.yaml"
    if selector == "headroom":
        files += f" -f {HEADROOM_OVERRIDE}"
    else:
        files += f" -f {CG_OVERRIDE}"
        if selector == "cg:off":
            env["CONTEXT_GURU_PIPELINE"] = ""
        elif selector.startswith("cg:preset="):
            env["CONTEXT_GURU_PRESET"] = selector.split("=", 1)[1]
        elif selector.startswith("cg:"):
            env["CONTEXT_GURU_PIPELINE"] = selector[3:]
        elif selector.startswith("yaml:"):
            env["CONTEXT_GURU_CONFIG_YAML"] = FULL_CONFIGS[selector[5:]]

    sh(f"docker compose -p {proj} down -v")
    up = sh(f"docker compose -p {proj} {files} up -d", cwd=SWE, env=env)
    if up.returncode != 0:
        note = ("up-failed: " + up.stderr.strip().replace("\n", " ")[-200:])
        return {"task": task, "config": name, "agent": agent, "note": note}

    status = "timeout"
    for _ in range(POLL_MAX):
        st = sh(f"docker inspect -f '{{{{.State.Status}}}}' {proj}-runner-1").stdout.strip()
        if st == "exited":
            status = "exited"
            break
        time.sleep(POLL_SECS)

    task_res = vol_json(proj, "task/result.json")
    ag = vol_json(proj, "agent/result.json")
    stats_raw = sh(f"docker exec {proj}-gateway-1 sh -c 'curl -s localhost:4000/stats || wget -qO- localhost:4000/stats' 2>/dev/null").stdout
    try:
        stats = json.loads(stats_raw)
    except Exception:
        stats = {}
    wall = ""
    try:
        from datetime import datetime
        fmt = "%Y-%m-%dT%H:%M:%SZ"
        wall = int((datetime.strptime(ag["ended_at"], fmt) - datetime.strptime(ag["started_at"], fmt)).total_seconds())
    except Exception:
        pass
    row = {
        "task": task, "config": name, "agent": agent,
        "reward": task_res.get("reward", ""), "passed": task_res.get("passed", ""),
        "wall_s": wall, "gw_requests": stats.get("requests", ""),
        "gw_before": stats.get("tokens_before", ""), "gw_after": stats.get("tokens_after", ""),
        "gw_saved": stats.get("saved_tokens", ""), "gw_pct": stats.get("savings_pct", ""),
        "note": status if task_res else status + "/no-result",
    }
    sh(f"docker compose -p {proj} down -v")
    return row


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", nargs="*", default=TASKS)
    ap.add_argument("--only", nargs="*", default=None, help="config names to include")
    ap.add_argument("--one-task", default=None)
    a = ap.parse_args()
    tasks = [a.one_task] if a.one_task else a.tasks
    configs = [c for c in CONFIGS if (a.only is None or c[0] in a.only)]
    base, token = creds()
    done = done_cells()
    total = len(tasks) * len(configs)
    n = 0
    for task in tasks:
        for name, agent, sel in configs:
            n += 1
            if (task, name) in done:
                print(f"[{n}/{total}] skip {task}/{name} (done)", flush=True)
                continue
            print(f"[{n}/{total}] RUN {task}/{name} ({agent}, {sel})", flush=True)
            t0 = time.time()
            row = run_cell(task, name, agent, sel, base, token)
            append_row(row)
            print(f"    -> reward={row.get('reward')} wall={row.get('wall_s')} "
                  f"gw_saved={row.get('gw_saved')} note={row.get('note')} "
                  f"({int(time.time()-t0)}s cell)", flush=True)


if __name__ == "__main__":
    main()
