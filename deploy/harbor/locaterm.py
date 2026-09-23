#!/usr/bin/env python3
"""How each LOCA episode ENDED, keyed by environment, for one pass.

WHY THIS EXISTS. `Final reward (accuracy)` conflates two events that need opposite responses:

  1. the agent worked the task and got it wrong, and
  2. the HARNESS killed the episode because one response hit `max_tokens`.

Measured on iteration 024's ten passes: 63 of 150 episodes (42%) ended on a truncated response, and NONE
of those 63 ever scored, while 82 of the 87 survivors (94%) did. On iteration 028 seed 1, 9 of 15 in each
sonnet pass. So the metric everything has been read from is close to `1 - kill_rate`, and a pass can move
several accuracy points without the agent's task performance changing at all.

That also makes the two arms' kill rates a CONFOUND rather than an outcome: iteration 024's arms differed
by 37/75 killed against 26/75, which is 14.7 points of accuracy available before any content-selection
effect is counted.

The stop_reason of the LAST response of an episode is the discriminator, and it is only in the pass's own
LOCA log -- results.json records the reward, not how the episode ended. Both readout028.py and
perenv028.py import this so the accounting cannot drift between them, which is the same reason the
degenerate list should live here rather than in two files.
"""
import re
from pathlib import Path

# Constant zero across all nineteen iteration-024 passes. KNOWN TO BE PARTLY AN ARTIFACT: all three are
# in the truncation-killed set in both iteration-028 sonnet passes, and haiku -- which killed no episodes
# at all -- solved CanvasArrangeExam outright, 35/35 courses matched. Kept as a list of environments that
# have never scored ON SONNET AT THIS OUTPUT BUDGET, which is a narrower claim than the name suggests.
DEGENERATE = {"CanvasArrangeExamS2LEnv", "CanvasListTestS2LEnv", "WoocommerceNewWelcomeS2LEnv"}

# The harness ends an episode when a response hits the output cap. It is not a task outcome.
KILLED_BY = "max_tokens"


def _log(tag: str) -> Path:
    return Path.home() / "cg-loca" / f"i022loca-{tag}.log"


def episodes(tag: str) -> dict:
    """{env_name: {"end": stop_reason, "acc": float, "steps": int, "responses": int, "truncated": int}}

    Returns {} when the log is absent. Raises nothing: callers decide whether a missing pass is fatal.
    """
    p = _log(tag)
    if not p.exists():
        return {}
    t = p.read_text(errors="replace")
    env, acc, steps, end = {}, {}, {}, {}
    resp, trunc = {}, {}
    for m in re.finditer(r"\[Task(\d+)-Config\d+-Run0\] Environment: \S*?([A-Za-z0-9]+Env)", t):
        env[m.group(1)] = m.group(2)
    # The LAST stop_reason wins: this loop walks the log in order, so the final assignment is the
    # response the episode ended on.
    for m in re.finditer(r"\[Task(\d+)-Config\d+-Run0\] Claude API response - stop_reason: (\w+)", t):
        k = m.group(1)
        end[k] = m.group(2)
        resp[k] = resp.get(k, 0) + 1
        if m.group(2) == KILLED_BY:
            trunc[k] = trunc.get(k, 0) + 1
    for m in re.finditer(r"\[Task(\d+)-Config\d+-Run0\] Final reward \(accuracy\): ([0-9.]+)", t):
        acc[m.group(1)] = float(m.group(2))
    for m in re.finditer(r"\[Task(\d+)-Config\d+-Run0\] Total steps: (\d+)", t):
        steps[m.group(1)] = int(m.group(2))
    return {env[k]: {"end": end.get(k, "?"), "acc": acc.get(k, float("nan")),
                     "steps": steps.get(k, 0), "responses": resp.get(k, 0),
                     "truncated": trunc.get(k, 0)} for k in env}


def kills(eps: dict) -> tuple:
    """(killed_envs, survivor_envs). Killed = the episode's final response was truncated."""
    killed = sorted(e for e, v in eps.items() if v["end"] == KILLED_BY)
    surv = sorted(e for e, v in eps.items() if v["end"] != KILLED_BY)
    return killed, surv


def kill_line(tag: str, eps: dict) -> str:
    """One line stating the kill rate and what each group scored. Empty string when there is no pass."""
    if not eps:
        return ""
    killed, surv = kills(eps)
    ks = sum(1 for e in killed if eps[e]["acc"] > 0)
    ss = sum(1 for e in surv if eps[e]["acc"] > 0)
    return ("  truncation-killed %d/%d episodes (%.0f%%), %d of them scored; "
            "survivors solved %d/%d" % (len(killed), len(eps), 100.0 * len(killed) / len(eps),
                                        ks, ss, len(surv)))
