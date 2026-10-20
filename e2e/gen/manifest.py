"""The ground truth for the PR #578 end-to-end review.

Every other file in e2e/ derives from this: the transcripts, the installed inventory, the replayed
traffic, and the expected values. Nothing here imports PR code. Seeded, so a run is reproducible.
"""
from __future__ import annotations

import json
import random
from dataclasses import asdict, dataclass

SEED = 20261020
T0 = 1788393600000  # 2026-09-07 00:00 UTC
DAYS = 43
INSTALL_DAY = 28
DAY = 86_400_000
TRUNCATED_SESSION_INDEX = -1  # the session whose final assistant turn the transcript generator cuts short
MODEL = "claude-sonnet-4-5"          # priced in LiteLLM's list, so the money checks test real pricing
# No LiteLLM key may CONTAIN this id: LiteLLM.Price falls back to a contains-scan, so a real-looking id such as
# "claude-opus-5" still resolves (to "anthropic.claude-opus-5") and is NOT unpriced.
UNPRICED_MODEL = "e2e-unpriced-model"


def day_ms(day: int, hour: int = 0) -> int:
    return T0 + day * DAY + hour * 3_600_000


@dataclass(frozen=True)
class Capability:
    name: str
    kind: str
    owner: str
    on_disk: bool
    declared: bool
    tokens: int
    self_owned: bool


@dataclass(frozen=True)
class Project:
    key: str
    root: str
    folders: tuple
    exists: bool
    measured: bool
    transcript_dir: str


@dataclass(frozen=True)
class Invocation:
    cap: str


@dataclass(frozen=True)
class Turn:
    ts: int
    cwd: str
    fresh: int
    cache_read: int
    cache_write: int
    output: int
    invocations: tuple
    stream: bool
    truncate: bool


@dataclass(frozen=True)
class Session:
    id: str
    project: str
    turns: tuple


@dataclass(frozen=True)
class Manifest:
    tier: str
    seed: int
    install_ms: int
    projects: tuple
    capabilities: tuple
    sessions: tuple
    home: str


def measured(t: Turn, m: Manifest) -> bool:
    return t.ts >= m.install_ms


# --- capabilities -----------------------------------------------------------------------------

def _caps(full: bool) -> list:
    c = []

    def add(name, kind, owner, on_disk=True, declared=True, tokens=300, self_owned=False):
        c.append(Capability(name, kind, owner, on_disk, declared, tokens, self_owned))

    # MCP servers, three tools each. States: never / one-project / stale / post-install-only.
    for server in (("docs-search", "notes-db", "ticketer", "metrics") if full else ("docs-search",)):
        for tool in ("search", "fetch", "list"):
            add(f"mcp__{server}__{tool}", "mcp_tool", server, tokens=420)
    # Plugin skills: every state.
    plug = {
        "toolkit": ("planner", "reviewer", "debugger", "tester", "writer", "never-a", "never-b"),
        "helpers": ("formatter", "linter", "old-trick", "late-adds"),
    } if full else {"toolkit": ("planner", "never-a")}
    for p, names in plug.items():
        for n in names:
            add(f"{p}:{n}", "plugin_skill", p, tokens=180)
    # Marketplace skills (bare names): never / one-project / pre-install-only.
    for n in (("pdf", "xlsx", "slides", "canvas", "brand", "memo") if full else ("pdf",)):
        add(n, "marketplace_skill", "acme-agent-skills", tokens=160)
    # Bundled: invoked, on disk nowhere, declared in the prompt.
    add("chartmaker", "bundled_skill", "(bundled)", on_disk=False, tokens=140)
    # Built-in tools: used in several projects, never removable.
    for n in (("Bash", "Read", "Edit", "Write", "Grep") if full else ("Bash",)):
        add(n, "builtin", "(builtin)", on_disk=False, tokens=900)
    # This tool's own skills, under both names it has shipped as.
    if full:
        for n in ("insights", "status", "dashboard"):
            add(f"context-guru:{n}", "plugin_skill", "context-guru", tokens=200, self_owned=True)
        add("cg:insights", "plugin_skill", "cg", tokens=200, self_owned=True)
    else:
        # Smoke carries one, installed and never invoked: the only state in which a broken self-exclusion
        # shows (the skill leaks into the installed inventory and the never-used list).
        add("context-guru:insights", "plugin_skill", "context-guru", tokens=200, self_owned=True)
    return c


# The (kind, state) cells the spec's matrix requires. `coverage` must contain all of them.
REQUIRED_CELLS = {
    "mcp_tool": ("never", "one", "stale", "post_only"),
    "plugin_skill": ("never", "one", "several", "stale", "pre_only", "post_only"),
    "marketplace_skill": ("never", "one", "pre_only"),
    "bundled_skill": ("one",),
    "builtin": ("several",),
    "self_owned": ("never", "one", "pre_only"),
}

# Which capability plays which state. Fixed, not random: the matrix must be filled every time.
_FULL_ROLES = {
    # cap name -> (state, projects that invoke it, window) ; window in {"pre","post","both"}
    "mcp__docs-search__search": ("one", ("alpha",), "both"),
    "mcp__notes-db__fetch": ("stale", ("beta",), "pre_early"),
    "mcp__ticketer__list": ("post_only", ("alpha",), "post"),
    "toolkit:planner": ("several", ("alpha", "beta", "gamma"), "both"),
    "toolkit:reviewer": ("one", ("beta",), "both"),
    "toolkit:debugger": ("stale", ("deleted-proj",), "pre_early"),
    "toolkit:tester": ("pre_only", ("history-only", "deleted-proj"), "pre"),
    "toolkit:writer": ("post_only", ("my-repo",), "post"),
    "helpers:formatter": ("one", ("gamma",), "both"),
    "helpers:late-adds": ("post_only", ("beta",), "post"),
    "pdf": ("one", ("wanderer",), "both"),
    "xlsx": ("pre_only", ("history-only",), "pre"),
    "chartmaker": ("one", ("alpha",), "both"),
    "Bash": ("several", ("alpha", "beta", "gamma", "my-repo", "mixed", "wanderer", "home"), "both"),
    "Read": ("several", ("alpha", "beta", "gamma"), "both"),
    "Edit": ("several", ("alpha", "gamma"), "both"),
    "context-guru:insights": ("one", ("alpha",), "both"),
    "cg:insights": ("pre_only", ("beta",), "pre"),
}
_SMOKE_ROLES = {
    "mcp__docs-search__search": ("one", ("alpha",), "both"),
    "toolkit:planner": ("one", ("alpha",), "both"),
    "pdf": ("pre_only", ("history-only",), "pre"),
    "chartmaker": ("one", ("alpha",), "both"),
    "Bash": ("several", ("alpha", "gamma", "history-only"), "both"),
}


def roles(m: Manifest) -> dict:
    return _SMOKE_ROLES if m.tier == "smoke" else _FULL_ROLES


def observed_states(m: Manifest) -> dict[str, set[str]]:
    """Compute actual states for each capability from manifest invocations.

    Returns: capability name -> set of states it actually holds
    - "never": 0 invocations anywhere
    - "one": invoked in exactly 1 project
    - "several": invoked in >= 2 projects
    - "pre_only": invoked, and every invocation ts < m.install_ms
    - "post_only": invoked, and every invocation ts >= m.install_ms
    - "stale": invoked, and last invocation >= 14 days before day_ms(DAYS)
    """
    states = {}
    stale_cutoff = day_ms(DAYS) - 14 * DAY

    for cap in m.capabilities:
        projects = set()
        pre_count = 0
        post_count = 0
        last_ts = -1

        for session in m.sessions:
            for turn in session.turns:
                for inv in turn.invocations:
                    if inv.cap == cap.name:
                        projects.add(session.project)
                        if turn.ts < m.install_ms:
                            pre_count += 1
                        else:
                            post_count += 1
                        last_ts = max(last_ts, turn.ts)

        cap_states = set()
        if not projects:
            cap_states.add("never")
        else:
            if len(projects) == 1:
                cap_states.add("one")
            else:
                cap_states.add("several")

            if post_count == 0:
                cap_states.add("pre_only")
            elif pre_count == 0:
                cap_states.add("post_only")

            if last_ts >= 0 and last_ts <= stale_cutoff:
                cap_states.add("stale")

        states[cap.name] = cap_states

    return states


def coverage(m: Manifest) -> set:
    """The (kind, state) cells this manifest actually fills from observed invocations."""
    states = observed_states(m)
    cells = set()

    for cap in m.capabilities:
        kind = "self_owned" if cap.self_owned else cap.kind
        for state in states[cap.name]:
            cells.add((kind, state))

    return cells


# --- projects ---------------------------------------------------------------------------------

def _projects(full: bool, home: str, scale: int) -> list:
    def P(key, root, subs=(), exists=True, measured=True, extra=()):
        tdir = root.replace("/", "-")
        return Project(key, root, (root,) + tuple(f"{root}/{s}" for s in subs) + tuple(extra),
                       exists, measured, tdir)

    if not full:
        return [
            P("alpha", f"{home}/work/alpha"),
            P("gamma", f"{home}/work/gamma", subs=("server",)),
            P("history-only", f"{home}/work/history-only", measured=False),
        ]
    ps = [
        P("alpha", f"{home}/work/alpha"),
        P("beta", f"{home}/work/beta"),
        P("gamma", f"{home}/work/gamma", subs=("server", "docs/adr", "ops/compose", "ops/backup")),
        P("my-repo", f"{home}/work/my-repo"),
        P("deleted-proj", f"{home}/work/deleted-proj", exists=False, measured=False),
        P("history-only", f"{home}/work/history-only", measured=False),
        P("mixed", f"{home}/work/mixed"),
        P("wanderer", f"{home}/work/wanderer", extra=("/tmp/scratch-w",)),
        P("scratch-1", "/tmp/scratch-1", measured=False),
        P("scratch-2", "/tmp/scratch-2", measured=False),
        P("scratch-3", "/tmp/scratch-3"),
        P("home", home),
    ]
    if scale > 1:  # stress: ten copies of each non-home project, distinct roots
        ps = [p for p in ps if p.key == "home"] + [
            Project(f"{p.key}-{i}", f"{p.root}-{i}", tuple(f"{f}-{i}" if f.startswith(p.root) else f
                                                           for f in p.folders),
                    p.exists, p.measured, f"{p.transcript_dir}-{i}")
            for i in range(scale) for p in ps if p.key != "home"]
    return ps


# --- sessions ---------------------------------------------------------------------------------

def _usage(rng: random.Random):
    return (rng.randint(1, 40), rng.randint(20_000, 90_000), rng.choice((0, 0, 0, 4_000, 30_000)),
            rng.randint(100, 2_000))


def build(tier: str, seed: int = SEED, home: str = "/sandbox") -> Manifest:
    if tier not in ("smoke", "full", "stress"):
        raise ValueError(f"unknown tier {tier!r}")
    full = tier != "smoke"
    scale = 10 if tier == "stress" else 1
    rng = random.Random(seed)
    caps = tuple(_caps(full))
    projects = tuple(_projects(full, home, scale))
    install = day_ms(INSTALL_DAY, 12)
    rmap = _SMOKE_ROLES if not full else _FULL_ROLES
    sessions = []
    sess_per_project = 1 if not full else 3
    turns_per_session = 4 if not full else 220
    n_stream, n_trunc = 0, 0
    # Track which (project, capability) pairs have been invoked
    invoked_pairs = set()
    for p in projects:
        base = p.key.rsplit("-", 1)[0] if scale > 1 and p.key != "home" else p.key
        for si in range(sess_per_project + (1 if base in ("alpha", "beta") else 0)):
            sid = f"{p.key}-s{si:02d}-{rng.randrange(16**8):08x}"
            if base == "deleted-proj" and si == 0:
                # First deleted-proj session: early for toolkit:debugger (stale, pre_early)
                start = rng.randint(0, 3)
            elif base == "my-repo" and si == 0:
                # First my-repo session: post-install for toolkit:writer (post_only)
                start = rng.randint(INSTALL_DAY, DAYS - 3)
            elif base == "wanderer" and si == 0:
                # First wanderer session: post-install to exercise /tmp/scratch-w excursion
                start = rng.randint(INSTALL_DAY, DAYS - 3)
            elif base == "history-only" or base == "deleted-proj" or not p.measured:
                start = rng.randint(0, INSTALL_DAY - 4)
            elif base == "mixed" and si == 0:
                start = INSTALL_DAY - 1  # straddles day 28 12:00
            elif base in ("alpha", "beta") and si >= sess_per_project:
                # Extra sessions for alpha/beta: ensure post-install start to satisfy post_only and truncation roles
                start = rng.randint(INSTALL_DAY, DAYS - 3)
            else:
                start = rng.randint(0, DAYS - 3)
            turns = []
            for ti in range(turns_per_session):
                day = min(DAYS - 1, start + (ti * 3) // turns_per_session)
                ts = day_ms(day, 8) + ti * 60_000 + rng.randint(0, 59_000)
                if base == "mixed" and si == 0:
                    ts = install - (turns_per_session // 2 - ti) * 600_000
                if not p.measured and ts >= install:
                    ts = install - 3_600_000 - ti * 60_000
                # For wanderer post-install: ensure turn 0 uses /tmp/scratch-w excursion
                if base == "wanderer" and si == 0 and ts >= install and ti == 0:
                    cwd = "/tmp/scratch-w"
                else:
                    cwd = p.folders[ti % len(p.folders)] if len(p.folders) > 1 else p.root
                invs = []
                for cname, (state, users, window) in rmap.items():
                    if base not in users:
                        continue
                    pre = ts < install
                    ok = (window == "both" or (window == "pre" and pre) or
                          (window == "post" and not pre) or
                          (window == "pre_early" and ts < day_ms(6)))
                    # Ensure each (project, capability) pair gets at least one invocation, then random
                    pair = (base, cname)
                    need_invocation = pair not in invoked_pairs and ok
                    if need_invocation or (ok and rng.random() < 0.20):
                        invs.append(Invocation(cname))
                        invoked_pairs.add(pair)
                fr, cr, cw, out = _usage(rng)
                is_post = ts >= install
                stream = is_post and rng.random() < 0.8
                n_stream += stream
                trunc = False
                if full and stream and n_trunc < 2 and base == "alpha" and ti > 10:
                    trunc, n_trunc = True, n_trunc + 1
                turns.append(Turn(ts, cwd, fr, cr, cw, out, tuple(invs), stream, trunc))
            sessions.append(Session(sid, p.key, tuple(turns)))
    return Manifest(tier, seed, install, projects, caps, tuple(sessions), home)


def to_json(m: Manifest) -> str:
    return json.dumps(asdict(m), sort_keys=True, indent=1)
