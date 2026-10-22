"""Expected values for the e2e harness, from the manifest alone.

INDEPENDENT BY CONSTRUCTION: this imports nothing from the PR and reads none of its output. It
re-states the rules the PR is meant to follow, so a disagreement between this and the dashboard is
a finding rather than the code agreeing with itself. A test pins the import list.
"""
from __future__ import annotations

import argparse
import json
import sys

from e2e.gen import manifest as mf

SKILL_KINDS = ("plugin_skill", "marketplace_skill")


def _root_of(m: mf.Manifest) -> dict:
    """Every session id -> its project's root. Claude Code's grouping: a session belongs to the
    project it was filed under, whichever folder each turn ran in."""
    roots = {p.key: p.root for p in m.projects}
    return {s.id: roots[s.project] for s in m.sessions}


def expected(m: mf.Manifest) -> dict:
    caps = {c.name: c for c in m.capabilities}
    root_of = _root_of(m)
    invoked_anywhere = {i.cap for s in m.sessions for t in s.turns for i in t.invocations}

    # --- History -------------------------------------------------------------------------------
    installed = sorted(c.name for c in m.capabilities
                       if c.on_disk and not c.self_owned and c.kind in SKILL_KINDS)
    never = sorted(n for n in installed if n not in invoked_anywhere)
    owners: dict = {}
    for n in never:
        owners[caps[n].owner] = owners.get(caps[n].owner, 0) + 1
    self_names = {c.name for c in m.capabilities if c.self_owned}
    self_names |= {n for n in invoked_anywhere if n.startswith(("context-guru:", "cg:"))}

    # --- Cleanup -------------------------------------------------------------------------------
    measured_roots = sorted({root_of[s.id] for s in m.sessions for t in s.turns if mf.measured(t, m)})
    all_roots = sorted({p.root for p in m.projects})
    declared = [c for c in m.capabilities
                if c.declared and not c.self_owned and c.kind != "builtin"]
    unused, used = {}, {}
    for r in measured_roots:
        inv = {i.cap for s in m.sessions if root_of[s.id] == r
               for t in s.turns if mf.measured(t, m) for i in t.invocations}
        used[r] = sorted(inv)
        unused[r] = sorted(c.name for c in declared if c.name not in inv)

    # --- Stats ---------------------------------------------------------------------------------
    # A pre-install turn reaches the DB only through the importer, which cannot read the transcript line
    # the generator cuts (see the Import block below), so that one turn is never a request.
    cut_s = m.sessions[mf.TRUNCATED_SESSION_INDEX]
    cut_turn = 0 if mf.measured(cut_s.turns[-1], m) else 1
    reqs: dict = {}
    for s in m.sessions:
        reqs[root_of[s.id]] = reqs.get(root_of[s.id], 0) + len(s.turns) - (cut_turn if s is cut_s else 0)

    # --- Import (Controller ruling amendment) --------------------------------------------------
    pre_install_turns = sum(1 for s in m.sessions for t in s.turns if not mf.measured(t, m))
    # The transcript generator cuts the final turn of the last session. Only a pre-install turn
    # would have been imported before the cut, so subtract 1 only if that turn is pre-install.
    last = m.sessions[mf.TRUNCATED_SESSION_INDEX].turns[-1]
    cut = 0 if mf.measured(last, m) else 1
    valid_pre_install_turns = pre_install_turns - cut

    return {
        "history": {
            "installed": len(installed),
            "never_used": never,
            "used_installed": len(installed) - len(never),
            "owners": owners,
            "owner_total": len(never),
            "self_excluded": len(self_names),
            "bundled_absent": sorted(c.name for c in m.capabilities if c.kind == "bundled_skill"),
        },
        "cleanup": {
            "picker": measured_roots,
            "not_in_picker": sorted(set(all_roots) - set(measured_roots)),
            "unused": unused,
            "used": used,
        },
        "import": {
            "valid_pre_install_turns": valid_pre_install_turns,
        },
        "stats": {"requests": reqs},
        "skill": {"port": 8788},
    }


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tier", required=True, choices=("smoke", "full", "stress"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--home", default="/sandbox")
    a = ap.parse_args(argv)
    with open(a.out, "w") as f:
        json.dump(expected(mf.build(a.tier, home=a.home)), f, sort_keys=True, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
