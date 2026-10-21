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
            (p / "verdicts.json").write_text(json.dumps([_v("U.x.a", "FAIL"), _v("Z", "FAIL"), _v("Y", "PASS")]))
            kf = p / "known.txt"; kf.write_text("U.x.*  # known\n")
            self.assertEqual(report.unknown_fails(d, str(kf)), ["Z"])

    def test_unknown_fails_cli_exits_nonzero_when_it_cannot_judge(self):
        # The smoke gate reads this; an exception must close the gate, never print nothing and open it.
        with tempfile.TemporaryDirectory() as d:
            r = subprocess.run([sys.executable, "-m", "e2e.report", "--unknown-fails", d],
                               capture_output=True, text=True)
            self.assertEqual((r.returncode != 0, r.stdout), (True, ""))

    def test_verdict_is_pass_with_only_known_and_blocked(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("C", "KNOWN"), _v("D", "BLOCKED")]))
            (p / "phases.jsonl").write_text("")
            report.build(d)
            self.assertTrue((p / "summary.md").read_text().startswith("# PR #578 e2e review: PASS"))

    def test_known_findings_section_rendered_from_file(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([_v("A", "PASS")]))
            (p / "phases.jsonl").write_text("")
            kf = p / "known.txt"
            kf.write_text("U.x.y  # undefined text in cleanup card\n# comment only\n")
            report.build(d, known_findings=str(kf))
            s = (p / "summary.md").read_text()
            self.assertIn("## Known PR findings", s)
            self.assertIn("| undefined text in cleanup card | U.x.y | 0 |", s)

    def test_a_known_finding_lists_its_globs_and_the_checks_it_explains(self):
        with tempfile.TemporaryDirectory() as d:
            p = pathlib.Path(d)
            (p / "verdicts.json").write_text(json.dumps([
                _v("U.x.a", "FAIL"), _v("U.x.b", "FAIL"), _v("R.y", "KNOWN"), _v("U.x.c", "PASS"), _v("Z", "FAIL")]))
            (p / "phases.jsonl").write_text("")
            kf = p / "known.txt"
            kf.write_text("U.x.* R.y  # one finding, two kinds of check\n")
            report.build(d, known_findings=str(kf))
            s = (p / "summary.md").read_text()
            self.assertIn("| one finding, two kinds of check | U.x.* R.y | 3 |", s)
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
