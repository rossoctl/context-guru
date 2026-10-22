import ast
import pathlib
import unittest

from e2e import oracle
from e2e.gen import manifest as mf

ROOT = pathlib.Path(__file__).resolve().parents[1]


class TestOracle(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.m = mf.build("full")
        cls.e = oracle.expected(cls.m)

    def test_imports_nothing_from_the_pr(self):
        tree = ast.parse((ROOT / "oracle.py").read_text())
        mods = {n.module if isinstance(n, ast.ImportFrom) else a.name
                for n in ast.walk(tree) if isinstance(n, (ast.Import, ast.ImportFrom))
                for a in (n.names if isinstance(n, ast.Import) else [n])}
        self.assertTrue(mods <= {"json", "argparse", "sys", "e2e.gen", "e2e.gen.manifest", "__future__"},
                        f"oracle imports {mods}; it must not depend on PR code or output")

    def test_never_used_excludes_self_owned_and_bundled(self):
        for n in self.e["history"]["never_used"]:
            self.assertFalse(n.startswith(("context-guru:", "cg:")), n)
            self.assertNotEqual(n, "chartmaker")

    def test_never_used_plus_used_partitions_installed(self):
        h = self.e["history"]
        self.assertEqual(len(h["never_used"]) + h["used_installed"], h["installed"])

    def test_owner_total_equals_never_used(self):
        h = self.e["history"]
        self.assertEqual(sum(h["owners"].values()), len(h["never_used"]))
        self.assertEqual(h["owner_total"], len(h["never_used"]))

    def test_known_never_used_skill(self):
        self.assertIn("toolkit:never-a", self.e["history"]["never_used"])
        self.assertNotIn("toolkit:planner", self.e["history"]["never_used"])
        self.assertNotIn("toolkit:tester", self.e["history"]["never_used"],
                         "used before install only: still used, so not never-used")

    def test_picker_has_only_measured_roots(self):
        picker = self.e["cleanup"]["picker"]
        roots = {p.key: p.root for p in self.m.projects}
        self.assertIn(roots["alpha"], picker)
        self.assertIn(roots["mixed"], picker)
        for k in ("history-only", "deleted-proj", "scratch-1"):
            self.assertNotIn(roots[k], picker)
            self.assertIn(roots[k], self.e["cleanup"]["not_in_picker"])

    def test_gamma_subfolders_are_one_picker_entry(self):
        gamma = [p for p in self.m.projects if p.key == "gamma"][0]
        self.assertEqual([r for r in self.e["cleanup"]["picker"] if r.startswith(gamma.root)], [gamma.root])

    def test_wanderer_requests_include_tmp(self):
        w = [p for p in self.m.projects if p.key == "wanderer"][0]
        own = sum(1 for s in self.m.sessions if s.project == "wanderer" for t in s.turns)
        self.assertEqual(self.e["stats"]["requests"][w.root], own,
                         "every turn of the project's sessions, /tmp excursion included")

    def test_wanderer_is_filed_under_its_root_not_its_tmp_excursion(self):
        w = [p for p in self.m.projects if p.key == "wanderer"][0]
        self.assertEqual((w.root in self.e["cleanup"]["picker"], "/tmp/scratch-w" in self.e["stats"]["requests"]),
                         (True, False))

    def test_requests_exclude_the_pre_install_turn_the_transcript_cut_removes(self):
        # Smoke is the tier whose cut turn is pre-install; there the importer never sees it.
        m = mf.build("smoke")
        last_s = m.sessions[mf.TRUNCATED_SESSION_INDEX]
        self.assertFalse(mf.measured(last_s.turns[-1], m), "precondition: the cut turn is pre-install")
        root = {p.key: p.root for p in m.projects}[last_s.project]
        own = sum(len(s.turns) for s in m.sessions if s.project == last_s.project)
        self.assertEqual(oracle.expected(m)["stats"]["requests"][root], own - 1)

    def test_cleanup_unused_never_lists_builtin_or_self(self):
        for root, names in self.e["cleanup"]["unused"].items():
            for n in names:
                self.assertNotIn(n, ("Bash", "Read", "Edit", "Write", "Grep"), root)
                self.assertFalse(n.startswith(("context-guru:", "cg:")), (root, n))

    def test_pooled_unused_is_what_no_measured_project_used(self):
        # All projects together: an item is unused only if NO measured project used it. Used in one project
        # and unused in another, it is used in the pooled view.
        c = self.e["cleanup"]
        want = set.intersection(*(set(v) for v in c["unused"].values()))
        self.assertEqual(set(c["unused_pooled"]), want)
        used_somewhere = set().union(*(set(v) for v in c["used"].values()))
        self.assertFalse(set(c["unused_pooled"]) & used_somewhere)

    def test_projects_differ_so_cross_project_mixing_is_visible(self):
        # The data must make a leak detectable: two projects with different unused lists, and an item used in
        # one project and unused in another. Identical projects would let a leak pass.
        c = self.e["cleanup"]
        lists = {tuple(v) for v in c["unused"].values()}
        self.assertGreater(len(lists), 1, "every project has the same unused list")
        split = [n for n in set().union(*(set(v) for v in c["unused"].values()))
                 if any(n in v for v in c["used"].values())]
        self.assertTrue(split, "no item is used in one project and unused in another")

    def test_cli_writes_json(self):
        import json, subprocess, tempfile
        with tempfile.TemporaryDirectory() as d:
            out = pathlib.Path(d) / "e.json"
            subprocess.run(["python3", "-m", "e2e.oracle", "--tier", "smoke", "--out", str(out)],
                           check=True, cwd=ROOT.parent)
            self.assertIn("history", json.loads(out.read_text()))

    def test_import_count_smoke(self):
        """Hard-coded: smoke tier has 12 pre-install turns, the cut turn is pre-install, so 11."""
        m_smoke = mf.build("smoke")
        e_smoke = oracle.expected(m_smoke)
        self.assertEqual(e_smoke["import"]["valid_pre_install_turns"], 11)

    def test_import_count_full(self):
        """Hard-coded: full tier has 6418 pre-install turns, the cut turn is pre-install, so 6417."""
        self.assertEqual(self.e["import"]["valid_pre_install_turns"], 6417)


if __name__ == "__main__":
    unittest.main()
