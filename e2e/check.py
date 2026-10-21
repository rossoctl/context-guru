"""expected.json vs the collected actuals -> one verdict per check.

Every family here has a test that makes it FAIL, so a check that cannot fail is caught before the
harness is trusted. KNOWN is not assigned here: run.sh re-runs a failure on main to decide it.
A check that cannot be judged (no priced items, no probe, no DB) is BLOCKED, never PASS or FAIL: a
property over an empty set proves nothing, and a FAIL is a finding against the PR."""
from __future__ import annotations

import argparse
import json
import pathlib
import re
import sqlite3
import sys

UNATTRIBUTED = ("", "(unattributed)", "none", "(none)")


def _rec(out, id_, area, ok, exp, act, detail="", snapshot="", blocked=False):
    out.append({"id": id_, "area": area, "tier": "", "verdict": "BLOCKED" if blocked else ("PASS" if ok else "FAIL"),
                "expected": exp, "actual": act, "detail": detail, "snapshot": snapshot})


def _unused(items):
    return [i for i in items if not i.get("invoked")]


def _shutdown_rows(db, sid):
    """(request rows, tool_declarations rows) for sid, or None if the DB cannot be read."""
    if not db or not pathlib.Path(db).exists():
        return None
    try:
        c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
        try:
            q = lambda t: c.execute(f"SELECT COUNT(*) FROM {t} WHERE session_id = ?", (sid,)).fetchone()[0]
            return q("requests"), q("tool_declarations")
        finally:
            c.close()
    except sqlite3.Error:
        return None


def run(exp: dict, api: dict, ui, tier: str, probes: dict | None = None, db: str | None = None) -> list:
    probes = probes or {}
    out = []
    h, he = api.get("history", {}), exp["history"]
    picker = api.get("facets_measured", {}).get("project") or []
    ins = api.get("insights", {})

    # --- smoke --------------------------------------------------------------------------------
    _rec(out, "S4.history_never_used_count", "history", len(h.get("never_used", [])) == len(he["never_used"]),
         len(he["never_used"]), len(h.get("never_used", [])))
    _rec(out, "S5.picker_excludes_history_only", "cleanup",
         not set(picker) & set(exp["cleanup"]["not_in_picker"]),
         "absent: " + ", ".join(exp["cleanup"]["not_in_picker"]), picker)
    _rec(out, "S5.picker_folds_subfolder", "cleanup",
         all(not any(p != r and p.startswith(r + "/") and p not in exp["cleanup"]["picker"] for p in picker)
             for r in exp["cleanup"]["picker"]),
         "no subfolder of a root listed (unless the oracle lists it as its own root)", picker)
    port = re.findall(r"127\.0\.0\.1:(\d+)/dashboard", api.get("skill", {}).get("stdout", ""))
    sk = api.get("skill", {})
    _rec(out, "S7.skill_port", "skill",
         sk.get("exit") == 0 and bool(port) and all(p == str(exp["skill"]["port"]) for p in port),
         f"exit 0, port {exp['skill']['port']}", {"exit": sk.get("exit"), "ports": port})
    # The timeline must be a JSON array for every root, including ones with no measured traffic: null
    # there threw in the chart. Read at the API, where the bug lives.
    for root in sorted(set(exp["cleanup"]["picker"]) | set(exp["cleanup"].get("not_in_picker", []))):
        doc = ins.get(root)
        tl = (doc or {}).get("timeline") if isinstance(doc, dict) else None
        got = tl.get("buckets", "<absent>") if isinstance(tl, dict) else "<no timeline>"
        _rec(out, f"C.timeline_buckets_is_array.{root}", "cleanup", isinstance(got, list),
             "buckets is a JSON array", got if not isinstance(got, list) else "array",
             blocked=not isinstance(doc, dict))
    pages = (ui or {}).get("pages", {})
    _rec(out, "S6.ui_no_js_errors", "ui", bool(pages) and not any(p["errors"] for p in pages.values()),
         "no JS errors on any page", {k: v["errors"] for k, v in pages.items() if v["errors"]},
         blocked=not pages)

    # --- history ------------------------------------------------------------------------------
    _rec(out, "H.never_used_set", "history", sorted(h.get("never_used", [])) == sorted(he["never_used"]),
         sorted(he["never_used"]), sorted(h.get("never_used", [])))
    _rec(out, "H.partition", "history",
         h.get("used_installed", -1) + len(h.get("never_used", [])) == h.get("installed_skills", -2)
         and h.get("installed_skills") == he["installed"],
         f"used + never == installed == {he['installed']} (the oracle's count)",
         [h.get("used_installed"), len(h.get("never_used", [])), h.get("installed_skills")])
    got_owners = {g["owner"]: len(g["skills"]) for g in h.get("never_used_owners", [])}
    _rec(out, "H.owner_counts", "history", got_owners == he["owners"], he["owners"], got_owners)
    _rec(out, "H.owner_total_rendered", "history", sum(got_owners.values()) == he["owner_total"],
         he["owner_total"], sum(got_owners.values()))
    _rec(out, "H.self_excluded", "history", (h.get("self_excluded") or {}).get("items") == he["self_excluded"],
         he["self_excluded"], (h.get("self_excluded") or {}).get("items"))
    used_names = {u["name"] for u in h.get("used", []) if u.get("skill")}
    _rec(out, "H.bundled_not_used", "history", not used_names & set(he["bundled_absent"]),
         "absent: " + ", ".join(he["bundled_absent"]), sorted(used_names & set(he["bundled_absent"])))

    # --- cleanup ------------------------------------------------------------------------------
    _rec(out, "C.picker_set", "cleanup", sorted(picker) == sorted(exp["cleanup"]["picker"]),
         sorted(exp["cleanup"]["picker"]), sorted(picker))
    _rec(out, "C.picker_has_no_unattributed_entry", "cleanup", not set(picker) & set(UNATTRIBUTED),
         "no unattributed entry", [p for p in picker if p in UNATTRIBUTED])
    totals = {}
    for root in exp["cleanup"]["picker"]:
        doc = ins.get(root, {})
        items = doc.get("items", [])
        # Unused built-ins are listed by design with scope "none" (not removable at any scope); the oracle's
        # unused set is what the user CAN remove, so judge those, and judge the built-ins separately.
        unused = sorted(i["name"] for i in _unused(items) if i.get("scope") != "none")
        offered = sorted(i["name"] for i in _unused(items) if i.get("scope") == "none" and i.get("kind") != "tool")
        _rec(out, f"C.builtin_never_offered.{root}", "cleanup", not offered, "no removable-looking built-in", offered)
        _rec(out, f"C.unused_set.{root}", "cleanup", sorted(exp["cleanup"]["unused"][root]) == unused,
             sorted(exp["cleanup"]["unused"][root]), unused,
             "exact: a missing item hides waste, an extra one accuses the user of waste for something they use")
        _rec(out, f"C.used_absent.{root}", "cleanup",
             not set(exp["cleanup"]["used"][root]) & set(unused), "none listed as unused",
             sorted(set(exp["cleanup"]["used"][root]) & set(unused)))
        priced = [i for i in items if i.get("priced")]
        no_priced = not priced
        no_priced_unused = not _unused(priced)
        # unpriced items are judged by C.money.unpriced_is_not_zero, not here
        _rec(out, f"C.money.unused_positive.{root}", "money",
             all(i.get("usd", 0) > 0 for i in _unused(priced)), "usd > 0 for every priced unused item",
             [i["name"] for i in _unused(priced) if not i.get("usd", 0) > 0],
             detail="no priced unused items" if no_priced_unused else "", blocked=no_priced_unused)
        # dash/insightsshape.go: removable_usd only accumulates items whose scope != "none"
        total = round(sum(i.get("usd", 0) for i in _unused(items) if i.get("scope") != "none"), 6)
        _rec(out, f"C.money.total_is_sum.{root}", "money", abs(total - doc.get("removable_usd", 0)) < 1e-6,
             total, doc.get("removable_usd"),
             detail="no priced items" if no_priced else "sum of usd over unused items with scope != none",
             blocked=no_priced)
        totals[root] = (doc.get("removable_usd", 0), no_priced)
    pooled_doc = ins.get("", {})
    pooled_unpriced = not any(i.get("priced") for i in pooled_doc.get("items", []))
    blocked = pooled_unpriced or all(v[1] for v in totals.values())
    pooled = pooled_doc.get("removable_usd", 0)
    mx = max([v[0] for v in totals.values()] or [0])
    _rec(out, "C.money.pooled_ge_project", "money", pooled + 1e-9 >= mx, f">= max project ({mx})", pooled,
         detail="no priced items" if blocked else "", blocked=blocked)

    probe = api.get("unpriced_probe") or {}
    pitems = probe.get("items") or []
    bad = [i["name"] for i in pitems if i.get("priced") or i.get("usd", 0) > 0]
    pb = "unpriced" not in probes or not pitems
    _rec(out, "C.money.unpriced_is_not_zero", "money", not bad, "every item priced=false and usd=0",
         bad if pitems else "no items",
         detail=("no unpriced probe recorded" if "unpriced" not in probes else "probe session has no items") if pb else "",
         blocked=pb)

    # --- rollups ------------------------------------------------------------------------------
    for root, n in exp["stats"]["requests"].items():
        unknown = [k for k, p in probes.items() if not p.get("root")]
        want = n + sum(1 for p in probes.values() if p.get("root") == root and p.get("counted", True))
        got = (api.get("stats", {}).get(root) or {}).get("requests")
        _rec(out, f"R.requests.{root}", "rollup", got == want, want, got,
             detail=f"{n} manifest turns + probes sent before the stats read",
             blocked=bool(unknown))
    _rec(out, "R.import_count_equals_valid_turns", "rollup",
         h.get("imported_requests") == exp["import"]["valid_pre_install_turns"],
         exp["import"]["valid_pre_install_turns"], h.get("imported_requests"))

    # --- known finding #1 ---------------------------------------------------------------------
    sp = probes.get("shutdown")
    got = _shutdown_rows(db, sp["session_id"]) if sp else None
    if got is None:
        _rec(out, "K.inventory_survives_shutdown", "known", False, "tool_declarations rows for the probe session",
             "n/a", detail="no shutdown probe or no DB copy", blocked=True)
    elif got[0] == 0:
        _rec(out, "K.inventory_survives_shutdown", "known", False, "a request row for the probe session", 0,
             detail="probe request not recorded", blocked=True)
    elif got[1] > 0:
        _rec(out, "K.inventory_survives_shutdown", "known", False, "rows present", got[1],
             detail="race not hit; see the Go reproducer", blocked=True)
    else:
        _rec(out, "K.inventory_survives_shutdown", "known", False, "tool_declarations rows for the probe session", 0,
             detail="request row present, inventory writer not drained on Close (dash/toolinventory.go)")

    # --- ui -----------------------------------------------------------------------------------
    for name, p in pages.items():
        # A page snap.mjs gave up waiting for has no overflow, no bad text and no headline to read: BLOCKED, not PASS.
        unrendered = any("render timeout" in e for e in p["errors"])
        nr = "page did not render" if unrendered else ""
        _rec(out, f"U.no_js_errors.{name}", "ui", not p["errors"], [], p["errors"], snapshot=f"snapshots/{name}.png")
        _rec(out, f"U.no_overflow.{name}", "ui", not p["overflow"], [], p["overflow"], detail=nr,
             snapshot=f"snapshots/{name}.png", blocked=unrendered)
        samples = p.get("contrast", {})
        low = {k: v for k, v in samples.items() if v < 3.0}
        _rec(out, f"U.contrast.{name}", "ui", not low, ">= 3:1", low, snapshot=f"snapshots/{name}.png",
             detail="" if samples else "no contrast samples", blocked=not samples)
        bad = p.get("values", {}).get("bad_tokens")
        _rec(out, f"U.no_undefined_text.{name}", "ui", not bad, "no undefined/NaN/null/[object Object] in visible text",
             bad or [], detail=nr or "; ".join(bad or []), snapshot=f"snapshots/{name}.png",
             blocked=unrendered or bad is None)
        if name.startswith("history-") and "headline_never" not in p.get("values", {}):
            _rec(out, f"U.headline_matches_api.{name}", "ui", False, len(h.get("never_used", [])), None,
                 detail="headline not read", snapshot=f"snapshots/{name}.png", blocked=True)
        elif "headline_never" in p.get("values", {}):
            _rec(out, f"U.headline_matches_api.{name}", "ui",
                 p["values"]["headline_never"] == len(h.get("never_used", [])),
                 len(h.get("never_used", [])), p["values"]["headline_never"], detail=nr,
                 snapshot=f"snapshots/{name}.png", blocked=unrendered)
    for r in out:
        r["tier"] = tier
    return out


def suite_checks(log, rc) -> list:
    """The PR's own `go test` result, one check per failing test (per package when no test is named, e.g. a
    build failure). A red package is a finding against the PR, not a crashed harness phase. None means the
    suite was not run (smoke)."""
    if log is None or rc is None:
        return []
    ids, tests = [], []
    for l in log.splitlines():
        if l.startswith("--- FAIL: "):  # top-level only; subtests are indented
            tests.append(l.split()[2])
        elif l.startswith("FAIL\t") or l.startswith("FAIL ") and len(l.split()) > 1 and "/" in l.split()[1]:
            pkg = l.split()[1]
            ids += [f"{pkg}.{t}" for t in tests] or [pkg]
            tests = []
    if ids:
        return [{"id": f"G.go_test.{i}", "area": "suite", "verdict": "FAIL", "expected": "ok", "actual": "FAIL",
                 "detail": "see logs/suite.log", "snapshot": ""} for i in sorted(set(ids))]
    return [{"id": "G.go_test.suite", "area": "suite", "verdict": "PASS" if rc == 0 else "FAIL",
             "expected": 0, "actual": rc, "detail": "make test exit code", "snapshot": ""}]


def _load(path):
    return json.loads(path.read_text()) if path.exists() else None


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--results", required=True)
    ap.add_argument("--tier", required=True)
    a = ap.parse_args(argv)
    d = pathlib.Path(a.results)
    sl, sx = d / "logs" / "suite.log", d / "suite.exit"
    suite = suite_checks(sl.read_text() if sl.exists() and sx.exists() else None,
                         int(sx.read_text().strip()) if sx.exists() else None)
    if a.tier == "suite":  # `make test` only (the KNOWN baseline on main, which has no History tab to judge)
        v = suite
    else:
        v = _judge(d, a.tier) + suite
    for r in v:
        r["tier"] = a.tier
    (d / "verdicts.json").write_text(json.dumps(v, indent=1))
    c = {k: sum(r["verdict"] == k for r in v) for k in ("PASS", "FAIL", "BLOCKED")}
    print(" ".join(f"{k} {n}" for k, n in c.items()))
    return 1 if c["FAIL"] else 0


def _judge(d: pathlib.Path, tier: str) -> list:
    probes = {k: v for k, v in (("shutdown", _load(d / "shutdown-probe.json")),
                                ("unpriced", _load(d / "unpriced-probe.json"))) if v}
    db = d / "db" / "dashboard-8788.db"
    return run(json.loads((d / "expected.json").read_text()), json.loads((d / "actual-api.json").read_text()),
               _load(d / "actual-ui.json"), tier, probes=probes, db=str(db) if db.exists() else None)


if __name__ == "__main__":
    sys.exit(main())
