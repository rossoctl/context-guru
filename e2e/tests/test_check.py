import contextlib
import copy
import io
import json
import sqlite3
import subprocess
import sys
import tempfile
import unittest
import unittest.mock
from pathlib import Path

from e2e import check


def item(name, usd=0.4, invoked=False, priced=True, scope="server"):
    return {"name": name, "usd": usd, "invoked": invoked, "priced": priced, "scope": scope}


EXP = {
    "history": {"installed": 3, "never_used": ["a:x", "b"], "used_installed": 1,
                "owners": {"a": 1, "acme": 1}, "owner_total": 2, "self_excluded": 2,
                "bundled_absent": ["chartmaker"]},
    "cleanup": {"picker": ["/s/alpha"], "not_in_picker": ["/s/hist"],
                "unused": {"/s/alpha": ["mcp__d__s"]}, "used": {"/s/alpha": ["a:y"]},
                "unused_pooled": ["mcp__d__s"]},
    "stats": {"requests": {"/s/alpha": 10}},
    "import": {"valid_pre_install_turns": 11},
    "skill": {"port": 8788},
}
API = {
    "history": {"installed_skills": 3, "never_used": ["a:x", "b"], "used_installed": 1,
                "imported_requests": 11,
                "never_used_owners": [{"owner": "a", "skills": ["a:x"]}, {"owner": "acme", "skills": ["b"]}],
                "self_excluded": {"items": 2}, "used": [{"name": "a:y", "skill": True}]},
    "facets_measured": {"project": ["/s/alpha"]},
    "insights": {"/s/hist": {"items": [], "timeline": {"buckets": []}},
                 "": {"items": [item("mcp__d__s"), item("Bash", usd=0, scope="none", invoked=True)], "removable_usd": 0.4},
                 "/s/alpha": {"items": [item("mcp__d__s"), item("Bash", usd=0, scope="none", invoked=True)],
                              "removable_usd": 0.4, "timeline": {"buckets": [1]}}},
    "stats": {"/s/alpha": {"requests": 12}},
    "stats_pooled": {"requests": 12},
    "unpriced_probe": {"items": [item("mcp__d__s", usd=0, priced=False)]},
    "skill": {"exit": 0, "stdout": "  http://127.0.0.1:8788/dashboard/#behaviour/history\n"},
}
PROBES = {"shutdown": {"session_id": "alpha-s01-x-shutdown-probe", "root": "/s/alpha"},
          "unpriced": {"session_id": "alpha-s01-x-unpriced-probe", "root": "/s/alpha"}}


def make_db(rows_for=(), requests_for=None):
    """requests_for defaults to rows_for' complement-free case: the probe session HAS a request row."""
    f = tempfile.NamedTemporaryFile(suffix=".db", delete=False)
    f.close()
    c = sqlite3.connect(f.name)
    c.execute("CREATE TABLE tool_declarations (session_id TEXT, name TEXT)")
    c.execute("CREATE TABLE requests (session_id TEXT)")
    for s_ in (PROBES["shutdown"]["session_id"],) if requests_for is None else requests_for:
        c.execute("INSERT INTO requests VALUES (?)", (s_,))
    for s in rows_for:
        c.execute("INSERT INTO tool_declarations VALUES (?, 'Bash')", (s,))
    c.commit()
    c.close()
    return f.name


INSTALLS = {"install": {"result": "routed", "history_import": "done"},
            "reinstall": {"result": "routed", "history_import": "already_done"}}


def verdicts(api=API, exp=EXP, ui=None, tier="full", probes=PROBES, db=None, installs=INSTALLS):
    return {r["id"]: r["verdict"] for r in check.run(exp, api, ui, tier, probes=probes, db=db, installs=installs)}


class TestCheck(unittest.TestCase):
    def test_all_pass_on_matching_data(self):
        v = verdicts()
        self.assertNotIn("FAIL", v.values(), v)

    def test_never_used_count_can_fail(self):
        a = copy.deepcopy(API); a["history"]["never_used"].append("c")
        self.assertEqual(verdicts(a)["S4.history_never_used_count"], "FAIL")

    def test_picker_can_fail_on_history_only(self):
        a = copy.deepcopy(API); a["facets_measured"]["project"].append("/s/hist")
        self.assertEqual(verdicts(a)["S5.picker_excludes_history_only"], "FAIL")

    def test_picker_folds_subfolder_can_fail(self):
        a = copy.deepcopy(API); a["facets_measured"]["project"].append("/s/alpha/sub")
        self.assertEqual(verdicts(a)["S5.picker_folds_subfolder"], "FAIL")

    def test_picker_folds_subfolder_passes_for_a_subfolder_the_oracle_lists_as_a_root(self):
        a, e = copy.deepcopy(API), copy.deepcopy(EXP)
        a["facets_measured"]["project"].append("/s/alpha/sub"); e["cleanup"]["picker"].append("/s/alpha/sub")
        a["insights"]["/s/alpha/sub"] = copy.deepcopy(a["insights"]["/s/alpha"])
        for k in ("unused", "used"):
            e["cleanup"][k]["/s/alpha/sub"] = e["cleanup"][k]["/s/alpha"]
        self.assertEqual(verdicts(a, e)["S5.picker_folds_subfolder"], "PASS")

    def test_picker_has_no_unattributed_entry_can_fail(self):
        a = copy.deepcopy(API); a["facets_measured"]["project"].append("(unattributed)")
        self.assertEqual(verdicts(a)["C.picker_has_no_unattributed_entry"], "FAIL")

    def test_bundled_in_used_list_fails(self):
        a = copy.deepcopy(API); a["history"]["used"].append({"name": "chartmaker", "skill": True})
        self.assertEqual(verdicts(a)["H.bundled_not_used"], "FAIL")

    def test_bundled_in_used_list_fails_whatever_its_skill_flag(self):
        for extra in ({"name": "chartmaker", "skill": False}, {"name": "chartmaker"}):
            a = copy.deepcopy(API); a["history"]["used"].append(extra)
            self.assertEqual(verdicts(a)["H.bundled_not_used"], "FAIL", extra)

    def test_never_used_set_and_owner_counts_can_fail(self):
        a = copy.deepcopy(API); a["history"]["never_used"] = ["a:x", "z"]
        self.assertEqual(verdicts(a)["H.never_used_set"], "FAIL")
        a = copy.deepcopy(API); a["history"]["never_used_owners"][0]["skills"].append("q")
        self.assertEqual(verdicts(a)["H.owner_counts"], "FAIL")

    def test_partition_can_fail(self):
        a = copy.deepcopy(API); a["history"]["used_installed"] = 5
        self.assertEqual(verdicts(a)["H.partition"], "FAIL")

    def test_self_excluded_can_fail(self):
        a = copy.deepcopy(API); a["history"]["self_excluded"]["items"] = 0
        self.assertEqual(verdicts(a)["H.self_excluded"], "FAIL")

    def test_unused_set_and_used_absent_can_fail(self):
        a = copy.deepcopy(API); a["insights"]["/s/alpha"]["items"][0]["invoked"] = True
        self.assertEqual(verdicts(a)["C.unused_set./s/alpha"], "FAIL")
        a = copy.deepcopy(API); a["insights"]["/s/alpha"]["items"].append(item("a:y"))
        self.assertEqual(verdicts(a)["C.used_absent./s/alpha"], "FAIL")

    def test_unused_set_is_exact_superset_subset_equal(self):
        extra = copy.deepcopy(API)  # a USED capability wrongly listed as unused
        for r in ("", "/s/alpha"):
            extra["insights"][r]["items"].append(item("a:y"))
        self.assertEqual(verdicts(extra)["C.unused_set./s/alpha"], "FAIL")
        missing = copy.deepcopy(API); missing["insights"]["/s/alpha"]["items"][0]["invoked"] = True
        self.assertEqual(verdicts(missing)["C.unused_set./s/alpha"], "FAIL")
        self.assertEqual(verdicts()["C.unused_set./s/alpha"], "PASS")

    def test_unused_positive_blocked_when_no_priced_unused_item(self):
        a = copy.deepcopy(API)
        for i in a["insights"]["/s/alpha"]["items"]:
            if not i["invoked"]:
                i["priced"] = False; i["usd"] = 0
        a["insights"]["/s/alpha"]["items"].append(item("used", invoked=True))  # priced, but used
        a["insights"]["/s/alpha"]["removable_usd"] = 0
        self.assertEqual(verdicts(a)["C.money.unused_positive./s/alpha"], "BLOCKED")

    def test_owner_total_rendered_and_picker_set_can_fail(self):
        a = copy.deepcopy(API); a["history"]["never_used_owners"].pop()
        self.assertEqual(verdicts(a)["H.owner_total_rendered"], "FAIL")
        a = copy.deepcopy(API); a["facets_measured"]["project"] = []
        self.assertEqual(verdicts(a)["C.picker_set"], "FAIL")

    def test_partition_requires_the_oracle_installed_count(self):
        a = copy.deepcopy(API)  # self-consistent (1 + 2 == 3 -> 2 + 2 == 4) but not what was installed
        a["history"]["installed_skills"] = 4; a["history"]["used_installed"] = 2
        self.assertEqual(verdicts(a)["H.partition"], "FAIL")

    def test_ui_page_checks_can_fail(self):
        base = {"errors": [], "values": {"headline_never": 2}, "overflow": [], "contrast": {}}
        self.assertEqual(verdicts(ui={"pages": {"p": base}})["U.headline_matches_api.p"], "PASS")
        bad = dict(base, errors=["boom"]); self.assertEqual(verdicts(ui={"pages": {"p": bad}})["U.no_js_errors.p"], "FAIL")
        bad = dict(base, overflow=["#x"]); self.assertEqual(verdicts(ui={"pages": {"p": bad}})["U.no_overflow.p"], "FAIL")
        bad = dict(base, values={"headline_never": 9})
        self.assertEqual(verdicts(ui={"pages": {"p": bad}})["U.headline_matches_api.p"], "FAIL")

    def test_money_unused_positive_can_fail_for_a_priced_item(self):
        a = copy.deepcopy(API); a["insights"]["/s/alpha"]["items"][0]["usd"] = 0
        v = verdicts(a)
        self.assertEqual(v["C.money.unused_positive./s/alpha"], "FAIL")

    def test_total_is_sum_excludes_scope_none_and_can_fail(self):
        # an UNUSED scope-none item has usd but is not part of removable_usd (dash/insightsshape.go)
        a = copy.deepcopy(API); e = copy.deepcopy(EXP)
        a["insights"]["/s/alpha"]["items"].append(item("Grep", usd=0.1, scope="none"))
        e["cleanup"]["unused"]["/s/alpha"].append("Grep")
        self.assertEqual(verdicts(a, e)["C.money.total_is_sum./s/alpha"], "PASS")
        a["insights"]["/s/alpha"]["removable_usd"] = 0.5  # the scope-none usd wrongly folded in
        self.assertEqual(verdicts(a, e)["C.money.total_is_sum./s/alpha"], "FAIL")
        a = copy.deepcopy(API); a["insights"]["/s/alpha"]["removable_usd"] = 0.5
        self.assertEqual(verdicts(a)["C.money.total_is_sum./s/alpha"], "FAIL")

    def test_money_checks_blocked_when_nothing_is_priced(self):
        a = copy.deepcopy(API)
        for d in a["insights"].values():
            for i in d["items"]:
                i["priced"] = False; i["usd"] = 0
        d = a["insights"]["/s/alpha"]; d["removable_usd"] = 0
        v = verdicts(a)
        for k in ("C.money.unused_positive./s/alpha", "C.money.total_is_sum./s/alpha", "C.money.pooled_ge_project"):
            self.assertEqual(v[k], "BLOCKED", k)

    def test_used_is_zero_fails_for_an_invoked_item_with_a_cost(self):
        a = copy.deepcopy(API); a["insights"]["/s/alpha"]["items"][1]["usd"] = 0.1
        r = {x["id"]: x for x in check.run(EXP, a, None, "full", probes=PROBES)}["C.money.used_is_zero./s/alpha"]
        self.assertEqual((r["verdict"], r["actual"]), ("FAIL", ["Bash"]))

    def test_used_is_zero_passes_for_zero_or_absent_usd(self):
        a = copy.deepcopy(API); del a["insights"]["/s/alpha"]["items"][1]["usd"]
        self.assertEqual(verdicts()["C.money.used_is_zero./s/alpha"], "PASS")
        self.assertEqual(verdicts(a)["C.money.used_is_zero./s/alpha"], "PASS")

    def test_used_is_zero_blocked_when_nothing_was_invoked(self):
        a = copy.deepcopy(API); a["insights"]["/s/alpha"]["items"] = [item("mcp__d__s")]
        r = {x["id"]: x for x in check.run(EXP, a, None, "full", probes=PROBES)}["C.money.used_is_zero./s/alpha"]
        self.assertEqual((r["verdict"], r["detail"]), ("BLOCKED", "no invoked items"))

    def test_pooled_less_than_project_fails(self):
        a = copy.deepcopy(API); a["insights"][""]["removable_usd"] = 0.1
        self.assertEqual(verdicts(a)["C.money.pooled_ge_project"], "FAIL")

    def test_unpriced_probe_pass_fail_blocked(self):
        self.assertEqual(verdicts()["C.money.unpriced_is_not_zero"], "PASS")
        a = copy.deepcopy(API); a["unpriced_probe"]["items"][0]["priced"] = True
        self.assertEqual(verdicts(a)["C.money.unpriced_is_not_zero"], "FAIL")
        a = copy.deepcopy(API); a["unpriced_probe"]["items"][0]["usd"] = 0.2
        self.assertEqual(verdicts(a)["C.money.unpriced_is_not_zero"], "FAIL")
        a = copy.deepcopy(API); a["unpriced_probe"]["items"] = []
        self.assertEqual(verdicts(a)["C.money.unpriced_is_not_zero"], "BLOCKED")
        self.assertEqual(verdicts(probes={})["C.money.unpriced_is_not_zero"], "BLOCKED")

    def test_requests_count_includes_each_probe_and_can_fail(self):
        self.assertEqual(verdicts()["R.requests./s/alpha"], "PASS")  # 10 + 2 probes == 12
        self.assertEqual(verdicts(probes={"shutdown": PROBES["shutdown"]})["R.requests./s/alpha"], "FAIL")
        a = copy.deepcopy(API); a["stats"]["/s/alpha"]["requests"] = 10
        self.assertEqual(verdicts(a)["R.requests./s/alpha"], "FAIL")

    def test_requests_ignore_a_probe_sent_after_the_stats_read(self):
        p = {"shutdown": PROBES["shutdown"], "unpriced": dict(PROBES["unpriced"], counted=False)}
        a = copy.deepcopy(API); a["stats"]["/s/alpha"]["requests"] = 11
        self.assertEqual(verdicts(a, probes=p)["R.requests./s/alpha"], "PASS")
        self.assertEqual(verdicts(probes=p)["R.requests./s/alpha"], "FAIL")  # 12 would now be one too many

    def test_requests_blocked_when_probe_root_unknown(self):
        p = {"shutdown": {"session_id": "x"}}
        self.assertEqual(verdicts(probes=p)["R.requests./s/alpha"], "BLOCKED")

    def test_import_count_can_fail(self):
        self.assertEqual(verdicts()["R.import_count_equals_valid_turns"], "PASS")
        a = copy.deepcopy(API); a["history"]["imported_requests"] = 10
        self.assertEqual(verdicts(a)["R.import_count_equals_valid_turns"], "FAIL")

    def test_inventory_survives_shutdown_never_passes(self):
        sid = PROBES["shutdown"]["session_id"]
        self.assertEqual(verdicts(db=make_db([sid]))["K.inventory_survives_shutdown"], "BLOCKED")
        self.assertEqual(verdicts(db=make_db([]))["K.inventory_survives_shutdown"], "FAIL")
        # no request row for the probe: the probe never reached the recorder, a harness reason
        self.assertEqual(verdicts(db=make_db([], requests_for=[]))["K.inventory_survives_shutdown"], "BLOCKED")
        self.assertEqual(verdicts(db=None)["K.inventory_survives_shutdown"], "BLOCKED")
        self.assertEqual(verdicts(db="/nonexistent/x.db")["K.inventory_survives_shutdown"], "BLOCKED")

    def test_inventory_survives_shutdown_says_the_harness_probe_is_supporting_evidence_only(self):
        why = "verified outside the harness (Go reproducer, 22/50 runs lost the inventory); in-harness probe is supporting evidence only"
        sid = PROBES["shutdown"]["session_id"]
        for db in (make_db([sid]), make_db([])):
            r = {x["id"]: x for x in check.run(EXP, API, None, "full", probes=PROBES, db=db)}["K.inventory_survives_shutdown"]
            self.assertIn(why, r["detail"])

    def test_skill_port_can_fail(self):
        a = copy.deepcopy(API); a["skill"]["stdout"] = "http://127.0.0.1:8787/dashboard/"
        self.assertEqual(verdicts(a)["S7.skill_port"], "FAIL")

    def test_skill_port_fails_on_nonzero_exit_even_with_right_port(self):
        a = copy.deepcopy(API); a["skill"]["exit"] = 1
        self.assertEqual(verdicts(a)["S7.skill_port"], "FAIL")

    def test_timeline_buckets_array_passes_null_fails_missing_blocks(self):
        i = "C.timeline_buckets_is_array./s/hist"
        self.assertEqual(verdicts()[i], "PASS")
        a = copy.deepcopy(API); a["insights"]["/s/hist"]["timeline"]["buckets"] = None
        self.assertEqual(verdicts(a)[i], "FAIL")
        a = copy.deepcopy(API); del a["insights"]["/s/hist"]["timeline"]["buckets"]
        self.assertEqual(verdicts(a)[i], "FAIL")
        a = copy.deepcopy(API); del a["insights"]["/s/hist"]
        self.assertEqual(verdicts(a)[i], "BLOCKED")

    def test_skill_port_fails_when_no_url_printed(self):
        a = copy.deepcopy(API); a["skill"]["stdout"] = "nothing"
        self.assertEqual(verdicts(a)["S7.skill_port"], "FAIL")

    def test_ui_checks_blocked_without_ui_data(self):
        self.assertEqual(verdicts()["S6.ui_no_js_errors"], "BLOCKED")

    def test_ui_js_error_fails(self):
        ui = {"pages": {"history-pooled-light-desktop": {"errors": ["TypeError: x"], "values": {},
                                                          "overflow": [], "contrast": {}}}}
        self.assertEqual(verdicts(ui=ui)["S6.ui_no_js_errors"], "FAIL")

    def test_undefined_text_fails_passes_and_blocks(self):
        def run(values):
            ui = {"pages": {"p": {"errors": [], "values": values, "overflow": [], "contrast": {}}}}
            return {r["id"]: r for r in check.run(EXP, API, ui, "full", probes=PROBES)}["U.no_undefined_text.p"]
        ctx = "docs-search removable undefined tools"
        fail = run({"bad_tokens": [ctx]})
        self.assertEqual((fail["verdict"], fail["detail"]), ("FAIL", ctx))
        self.assertEqual(run({"bad_tokens": []})["verdict"], "PASS")
        self.assertEqual(run({})["verdict"], "BLOCKED")

    def test_empty_contrast_is_blocked_not_pass(self):
        ui = {"pages": {"p": {"errors": [], "values": {}, "overflow": [], "contrast": {}}}}
        r = {x["id"]: x for x in check.run(EXP, API, ui, "full", probes=PROBES)}["U.contrast.p"]
        self.assertEqual((r["verdict"], r["detail"]), ("BLOCKED", "no contrast samples"))

    def test_history_page_without_headline_is_blocked(self):
        page = {"errors": [], "values": {}, "overflow": [], "contrast": {}}
        v = verdicts(ui={"pages": {"history-pooled-light-desktop": page, "cleanup-pooled-light-desktop": page}})
        self.assertEqual(v["U.headline_matches_api.history-pooled-light-desktop"], "BLOCKED")
        self.assertNotIn("U.headline_matches_api.cleanup-pooled-light-desktop", v)

    def test_a_page_that_did_not_render_blocks_its_content_checks(self):
        # A page that never rendered has no overflow, no undefined text and no headline: that proves nothing.
        page = {"errors": ["render timeout: #insights-body .panel h2"], "values": {"bad_tokens": [], "headline_never": 2},
                "overflow": [], "contrast": {}}
        got = {r["id"]: r for r in check.run(EXP, API, {"pages": {"history-p": page}}, "full", probes=PROBES)}
        for cid in ("U.no_overflow.history-p", "U.no_undefined_text.history-p", "U.headline_matches_api.history-p"):
            self.assertEqual((got[cid]["verdict"], got[cid]["detail"]), ("BLOCKED", "page did not render"), cid)
        self.assertEqual(got["U.no_js_errors.history-p"]["verdict"], "FAIL")

    def test_a_timed_out_page_with_real_defects_still_fails(self):
        # Rendered, then timed out after the project change: what it did show is evidence, so a defect stays FAIL.
        page = {"errors": ["render timeout: #insights-body .panel h2 after project change"],
                "values": {"bad_tokens": ["undefined"], "headline_never": 5}, "overflow": ["ins-why [742>351]"], "contrast": {}}
        got = {r["id"]: r["verdict"] for r in check.run(EXP, API, {"pages": {"history-p": page}}, "full", probes=PROBES)}
        self.assertEqual([got[c] for c in ("U.no_overflow.history-p", "U.no_undefined_text.history-p",
                                           "U.headline_matches_api.history-p")], ["FAIL", "FAIL", "FAIL"])

    def test_low_contrast_fails(self):
        ui = {"pages": {"p": {"errors": [], "values": {}, "overflow": [], "contrast": {"headline": 2.1}}}}
        self.assertEqual(verdicts(ui=ui)["U.contrast.p"], "FAIL")

class TestUnusedBuiltins(unittest.TestCase):
    def test_unused_builtin_with_scope_none_is_not_an_extra_item(self):
        api = copy.deepcopy(API)
        for k in ("", "/s/alpha"):
            api["insights"][k]["items"].append(dict(item("Grep", usd=0.01, scope="none"), kind="tool"))
        v = verdicts(api)
        self.assertEqual(v["C.unused_set./s/alpha"], "PASS")

    def test_builtin_never_offered_passes_for_an_unused_builtin_tool(self):
        api = copy.deepcopy(API)
        api["insights"]["/s/alpha"]["items"].append(dict(item("Grep", scope="none"), kind="tool"))
        self.assertEqual(verdicts(api)["C.builtin_never_offered./s/alpha"], "PASS")

    def test_builtin_never_offered_fails_for_an_unused_scope_none_skill(self):
        api = copy.deepcopy(API)
        api["insights"]["/s/alpha"]["items"].append(dict(item("bundled-x", scope="none"), kind="skill"))
        self.assertEqual(verdicts(api)["C.builtin_never_offered./s/alpha"], "FAIL")

    def test_unused_removable_extra_still_fails(self):
        api = copy.deepcopy(API)
        api["insights"]["/s/alpha"]["items"].append(item("mcp__extra__x"))
        self.assertEqual(verdicts(api)["C.unused_set./s/alpha"], "FAIL")


class TestMain(unittest.TestCase):
    def _main(self, rows):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d); (p / "logs").mkdir()
            out = io.StringIO()
            with contextlib.redirect_stdout(out), unittest.mock.patch.object(check, "_judge", return_value=rows):
                rc = check.main(["--results", d, "--tier", "smoke"])
            return rc, out.getvalue()

    def test_main_prints_incomplete_and_exits_zero_when_a_check_is_blocked(self):
        rc, out = self._main([{"id": "U.contrast.p", "verdict": "BLOCKED"}, {"id": "A", "verdict": "PASS"}])
        self.assertEqual((rc, out), (0, "INCOMPLETE: PASS 1 FAIL 0 BLOCKED 1\n"))

    def test_main_exits_one_on_a_fail(self):
        rc, out = self._main([{"id": "A", "verdict": "FAIL"}, {"id": "U.contrast.p", "verdict": "BLOCKED"}])
        self.assertEqual((rc, out), (1, "FAIL: PASS 0 FAIL 1 BLOCKED 1\n"))

    def test_a_judge_traceback_exits_three_not_one(self):
        # 1 is FAIL, a judged result; p_assert must be able to tell a crashed judge from it.
        with tempfile.TemporaryDirectory() as d:
            r = subprocess.run([sys.executable, "-m", "e2e.check", "--results", d, "--tier", "smoke"],
                               capture_output=True, text=True)
        self.assertEqual((r.returncode, "FileNotFoundError" in r.stderr), (3, True))


def two_projects():
    """Two measured projects with different usage, plus the pooled view: what cross-project checks need."""
    exp, api = copy.deepcopy(EXP), copy.deepcopy(API)
    exp["cleanup"]["picker"] = ["/s/alpha", "/s/beta"]
    exp["cleanup"]["unused"] = {"/s/alpha": ["mcp__d__s", "mcp__d__t"], "/s/beta": ["mcp__d__s"]}
    exp["cleanup"]["used"] = {"/s/alpha": ["a:y"], "/s/beta": ["mcp__d__t"]}
    exp["cleanup"]["unused_pooled"] = ["mcp__d__s"]
    exp["stats"]["requests"] = {"/s/alpha": 10, "/s/beta": 5}
    a = dict(item("mcp__d__s", usd=0.3), reads=300)
    t = dict(item("mcp__d__t", usd=0.2), reads=200)
    b = dict(item("mcp__d__s", usd=0.1), reads=100)
    api["insights"] = {
        "/s/hist": {"items": [], "timeline": {"buckets": []}},
        "/s/alpha": {"items": [a, t], "removable_usd": 0.5, "timeline": {"buckets": [1]}},
        "/s/beta": {"items": [b, dict(item("mcp__d__t"), invoked=True, usd=0)], "removable_usd": 0.1,
                    "timeline": {"buckets": [1]}},
        # pooled: s is unused everywhere (reads 300+100); t was used in beta, so it is used pooled
        "": {"items": [dict(item("mcp__d__s", usd=0.4), reads=400), dict(item("mcp__d__t"), invoked=True, usd=0)],
             "removable_usd": 0.4, "timeline": {"buckets": [1]}},
    }
    api["facets_measured"]["project"] = ["/s/alpha", "/s/beta"]
    api["stats"] = {"/s/alpha": {"requests": 12}, "/s/beta": {"requests": 5}}
    api["stats_pooled"] = {"requests": 17}
    return exp, api


class TestCrossProject(unittest.TestCase):
    """All projects together must be the projects added up, and no project may carry another's data."""

    def v(self, exp, api):
        return verdicts(api, exp)

    def test_correct_two_project_data_passes(self):
        got = self.v(*two_projects())
        self.assertEqual([got[k] for k in ("C.unused_set.pooled", "C.pooled_requests_are_the_sum",
                                           "C.pooled_carry_is_the_sum")], ["PASS"] * 3)

    def test_pooled_list_with_an_item_used_in_another_project_fails(self):
        exp, api = two_projects()
        api["insights"][""]["items"][1] = dict(item("mcp__d__t", usd=0.2), reads=200)  # t shown unused pooled
        self.assertEqual(self.v(exp, api)["C.unused_set.pooled"], "FAIL")

    def test_pooled_requests_that_are_not_the_sum_fail(self):
        exp, api = two_projects()
        api["stats_pooled"]["requests"] = 16  # one request lost between the projects and the pool
        self.assertEqual(self.v(exp, api)["C.pooled_requests_are_the_sum"], "FAIL")

    def test_one_project_counting_anothers_traffic_fails(self):
        # beta's figure for s includes alpha's 300 reads: beta + alpha now exceed the pool.
        exp, api = two_projects()
        api["insights"]["/s/beta"]["items"][0] = dict(item("mcp__d__s", usd=0.4), reads=400)
        self.assertEqual(self.v(exp, api)["C.pooled_carry_is_the_sum"], "FAIL")

    def test_pooled_dropping_a_project_fails(self):
        exp, api = two_projects()
        api["insights"][""]["items"][0] = dict(item("mcp__d__s", usd=0.3), reads=300)  # beta's 100 missing
        self.assertEqual(self.v(exp, api)["C.pooled_carry_is_the_sum"], "FAIL")

    def test_without_the_pooled_stats_the_sum_check_blocks(self):
        exp, api = two_projects()
        del api["stats_pooled"]
        self.assertEqual(self.v(exp, api)["C.pooled_requests_are_the_sum"], "BLOCKED")


class TestInstallImportsOnce(unittest.TestCase):
    """The real install imports the pre-install history once; a re-install must not import again."""

    def _v(self, first, again):
        return verdicts(installs={"install": {"result": "routed", "history_import": first},
                                  "reinstall": {"result": "routed", "history_import": again}})

    def test_imported_on_install_and_skipped_on_reinstall_passes(self):
        self.assertEqual(self._v("done", "already_done")["R.install_imports_history_once"], "PASS")

    def test_install_that_did_not_import_fails(self):
        for first in ("failed", "skipped", None):
            self.assertEqual(self._v(first, "already_done")["R.install_imports_history_once"], "FAIL", first)

    def test_reinstall_that_imported_again_fails(self):
        self.assertEqual(self._v("done", "done")["R.install_imports_history_once"], "FAIL")

    def test_facts_reader_keeps_the_last_value_and_splits_only_on_the_first_equals(self):
        with tempfile.TemporaryDirectory() as d:
            f = Path(d) / "install.facts"
            f.write_text("history_import=failed\nnoise line\nhistory_import_retry=x import-history -db=/p\nhistory_import=done\n")
            self.assertEqual(check._facts(f), {"history_import": "done", "history_import_retry": "x import-history -db=/p"})

    def test_missing_install_facts_block(self):
        self.assertEqual(verdicts(installs={})["R.install_imports_history_once"], "BLOCKED")


class TestSuiteChecks(unittest.TestCase):
    def test_one_fail_per_failing_package(self):
        log = "ok  \tm/a\t1s\nFAIL\tm/b\t2s\nFAIL\tm/c [build failed]\nFAIL\n"
        got = {r["id"]: r["verdict"] for r in check.suite_checks(log, 1)}
        self.assertEqual(got, {"G.go_test.m/b": "FAIL", "G.go_test.m/c": "FAIL"})

    def test_one_fail_per_failing_test_named_under_its_package(self):
        # go test prints a package's --- FAIL lines before that package's FAIL line; a pre-existing failure can
        # then be labelled KNOWN without hiding a new one in the same package.
        log = ("--- FAIL: TestX (0.1s)\n    x_test.go:3: boom\nFAIL\nFAIL\tm/b\t2s\n"
               "ok  \tm/a\t1s\n--- FAIL: TestY (0s)\n--- FAIL: TestZ (0s)\nFAIL\nFAIL\tm/c\t1s\n")
        got = {r["id"]: r["verdict"] for r in check.suite_checks(log, 1)}
        self.assertEqual(got, {"G.go_test.m/b.TestX": "FAIL", "G.go_test.m/c.TestY": "FAIL",
                               "G.go_test.m/c.TestZ": "FAIL"})

    def test_a_failing_test_records_its_own_fail_line_as_evidence(self):
        # known-findings.txt matches on the actual value, so it must name the test, not just say FAIL.
        log = "--- FAIL: TestX (0.1s)\nFAIL\nFAIL\tm/b\t2s\nFAIL\tm/c [build failed]\n"
        got = {r["id"]: r["actual"] for r in check.suite_checks(log, 1)}
        self.assertEqual(got, {"G.go_test.m/b.TestX": "--- FAIL: TestX (0.1s)", "G.go_test.m/c": "FAIL\tm/c [build failed]"})

    def test_nonzero_exit_without_a_package_line_still_fails(self):
        got = check.suite_checks("make: *** boom\n", 2)
        self.assertEqual([(r["id"], r["verdict"]) for r in got], [("G.go_test.suite", "FAIL")])

    def test_clean_run_passes(self):
        got = check.suite_checks("ok  \tm/a\t1s\n", 0)
        self.assertEqual([(r["id"], r["verdict"]) for r in got], [("G.go_test.suite", "PASS")])

    def test_no_suite_run_adds_nothing(self):
        self.assertEqual(check.suite_checks(None, None), [])

    def test_the_suite_tier_judges_only_the_suite_and_needs_no_oracle(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d); (p / "logs").mkdir()
            (p / "logs" / "suite.log").write_text("--- FAIL: TestX (0s)\nFAIL\nFAIL\tm/b\t1s\n")
            (p / "suite.exit").write_text("1\n")
            with contextlib.redirect_stdout(io.StringIO()):
                rc = check.main(["--results", d, "--tier", "suite"])
            got = [(r["id"], r["verdict"]) for r in json.loads((p / "verdicts.json").read_text())]
        self.assertEqual((rc, got), (1, [("G.go_test.m/b.TestX", "FAIL")]))


if __name__ == "__main__":
    unittest.main()
