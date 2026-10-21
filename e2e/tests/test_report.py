import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

from e2e import report


def _v(id_, verdict, exp=1, act=1, snap=""):
    return {"id": id_, "area": "x", "verdict": verdict, "expected": exp, "actual": act, "detail": "", "snapshot": snap}


class TestReport(unittest.TestCase):
    def test_summary_lists_every_failure_with_both_values(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([
                _v("A", "PASS"), _v("B", "FAIL", 3, 4), _v("C", "KNOWN", 0, 1)]))
            (p / "phases.jsonl").write_text('{"phase":"generate","status":"ok","seconds":3}\n')
            report.build(d)
            s = (p / "summary.md").read_text()
            self.assertIn("FAIL 1", s)
            self.assertIn("KNOWN 1", s)
            self.assertIn("| B |", s)
            self.assertIn("3", s)
            self.assertIn("4", s)
            h = (p / "report.html").read_text()
            for scheme in ("http://", "https://"):
                self.assertNotIn(scheme, h.replace("http://127.0.0.1", ""), "report must load nothing remote")

    def test_verdict_is_fail_when_any_check_fails(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("B", "FAIL", 1, 2)]))
            (p / "phases.jsonl").write_text("")
            report.build(d)
            self.assertTrue((p / "summary.md").read_text().startswith("# PR #578 e2e review: FAIL"))

    def test_unknown_fails_lists_only_fails_no_known_finding_explains(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("U.x.a", "FAIL", act=["undefined tools"]), _v("Z", "FAIL"),
                                                         _v("Y", "PASS")]))
            kf = p / "known.txt"; kf.write_text("U.x.* | undefined  # known\n")
            self.assertEqual(report.unknown_fails(d, str(kf)), ["Z"])

    def test_a_fail_with_the_right_id_but_other_evidence_is_unknown(self):
        # A new defect under an old check id must not ride through the gate on the old finding's line.
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("U.x.a", "FAIL", act=["undefined tools"]),
                                                         _v("U.x.b", "FAIL", act=["NaN tok/req"])]))
            kf = p / "known.txt"; kf.write_text("U.x.* | undefined  # known\n")
            self.assertEqual(report.unknown_fails(d, str(kf)), ["U.x.b"])

    def test_explains_requires_the_id_and_the_evidence(self):
        f = ("U.x.* R.y", "scratch-w", "why")
        self.assertEqual([report.explains(f, _v(i, "FAIL", act=a)) for i, a in
                          (("U.x.a", ["/tmp/scratch-w"]), ("R.y", "/tmp/scratch-w"), ("U.x.a", ["/tmp/other"]),
                           ("Q.z", ["/tmp/scratch-w"]))],
                         [True, True, False, False])

    def test_numeric_evidence_matches_the_whole_value_not_a_substring(self):
        f = ("R.y", "329", "why")
        self.assertEqual([report.explains(f, _v("R.y", "FAIL", act=a)) for a in (329, 3290, 1329, 329.5)],
                         [True, False, False, False])

    def test_a_known_findings_line_without_evidence_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            kf = pathlib.Path(d) / "known.txt"; kf.write_text("U.x.*  # no evidence\n")
            with self.assertRaises(ValueError):
                report.known(kf)

    def test_unknown_fails_cli_exits_nonzero_when_it_cannot_judge(self):
        # The smoke gate reads this; an exception must close the gate, never print nothing and open it.
        with tempfile.TemporaryDirectory() as d:
            r = subprocess.run([sys.executable, "-m", "e2e.report", "--unknown-fails", d],
                               capture_output=True, text=True)
            self.assertEqual((r.returncode != 0, r.stdout), (True, ""))

    def test_verdict_is_pass_with_only_known_and_by_design_blocked(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("C", "KNOWN"), _v("K.inventory_survives_shutdown", "BLOCKED")]))
            (p / "phases.jsonl").write_text("")
            report.build(d)
            self.assertTrue((p / "summary.md").read_text().startswith("# PR #578 e2e review: PASS"))

    def test_headline_is_incomplete_when_a_check_could_not_be_judged(self):
        self.assertEqual(report.headline([_v("A", "PASS"), _v("D", "BLOCKED")], []), "INCOMPLETE")

    def test_headline_fail_wins_over_blocked_and_a_crashed_phase_is_fail(self):
        self.assertEqual((report.headline([_v("B", "FAIL"), _v("D", "BLOCKED")], []),
                          report.headline([_v("A", "PASS")], [{"phase": "ui", "status": "crashed"}])),
                         ("FAIL", "FAIL"))

    def test_incomplete_headline_reaches_the_summary(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("A", "PASS"), _v("D", "BLOCKED")]))
            (p / "phases.jsonl").write_text("")
            report.build(d)
            self.assertTrue((p / "summary.md").read_text().startswith("# PR #578 e2e review: INCOMPLETE"))

    def test_known_findings_section_rendered_from_file(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("A", "PASS")]))
            (p / "phases.jsonl").write_text("")
            kf = p / "known.txt"
            kf.write_text("U.x.y | undefined  # undefined text in cleanup card\n# comment only\n")
            report.build(d, known_findings=str(kf))
            s = (p / "summary.md").read_text()
            self.assertIn("## Known PR findings", s)
            self.assertIn("| undefined text in cleanup card | U.x.y | undefined | 0 |", s)

    def test_a_known_finding_lists_its_globs_and_the_checks_it_explains(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([
                _v("U.x.a", "FAIL", act="e"), _v("U.x.b", "FAIL", act="e"), _v("R.y", "KNOWN", act="e"),
                _v("U.x.c", "PASS", act="e"), _v("Z", "FAIL", act="e")]))
            (p / "phases.jsonl").write_text("")
            kf = p / "known.txt"
            kf.write_text("U.x.* R.y | e  # one finding, two kinds of check\n")
            report.build(d, known_findings=str(kf))
            s = (p / "summary.md").read_text()
            self.assertIn("| one finding, two kinds of check | U.x.* R.y | e | 3 |", s)
            self.assertIn("Not explained by a known finding: Z", s)

    def test_notes_md_in_the_results_dir_is_included_verbatim(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("A", "PASS")]))
            (p / "phases.jsonl").write_text("")
            (p / "notes.md").write_text("## Why the stress run hung\n\nThe VM clock stepped.\n")
            report.build(d)
            self.assertIn("## Why the stress run hung\n\nThe VM clock stepped.", (p / "summary.md").read_text())
            self.assertIn("The VM clock stepped.", (p / "report.html").read_text())

    def test_a_snapshot_shared_by_many_checks_is_embedded_once(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d); (p / "snapshots").mkdir()
            (p / "snapshots" / "page.png").write_bytes(b"\x89PNG-fake")
            (p / "verdicts.json").write_text(json.dumps([_v(f"C{i}", "FAIL", snap="snapshots/page.png") for i in range(3)]))
            (p / "phases.jsonl").write_text("")
            report.build(d)
            h = (p / "report.html").read_text()
            self.assertEqual(h.count("data:image/png;base64,"), 1)
            self.assertEqual(h.count('href="#snap-page"'), 3)

    def test_mutation_round_included_from_sibling_dir(self):
        with tempfile.TemporaryDirectory() as root:
            r = pathlib.Path(root)
            (r / "run").mkdir()
            (r / "z-mutation").mkdir()
            (r / "z-mutation" / "mutation.json").write_text(json.dumps(
                {"baseline": "b", "mutations": [{"id": "m1", "caught": True, "turned_red": ["S7.x"]}]}))
            (r / "run" / "verdicts.json").write_text(json.dumps([_v("A", "PASS")]))
            (r / "run" / "phases.jsonl").write_text("")
            report.build(str(r / "run"))
            s = (r / "run" / "summary.md").read_text()
            self.assertIn("## Mutation round", s)
            self.assertIn("| m1 | True | S7.x |", s)

    def test_timings_json_rendered(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("A", "PASS")]))
            (p / "phases.jsonl").write_text("")
            (p / "timings.json").write_text(json.dumps({"phases": [], "stress": {"import_s": 12, "replay_s": 34}}))
            report.build(d)
            s = (p / "summary.md").read_text()
            self.assertIn("## Stress timings", s)
            self.assertIn("| import_s | 12 |", s)


if __name__ == "__main__":
    unittest.main()
