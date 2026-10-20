import unittest
from e2e.gen import manifest as mf


class TestManifest(unittest.TestCase):
    def test_same_seed_is_byte_identical(self):
        self.assertEqual(mf.to_json(mf.build("full")), mf.to_json(mf.build("full")))

    def test_different_seed_differs(self):
        self.assertNotEqual(mf.to_json(mf.build("full", seed=1)), mf.to_json(mf.build("full", seed=2)))

    def test_full_tier_matches_the_real_shape(self):
        m = mf.build("full")
        dirs = {p.transcript_dir for p in m.projects}
        self.assertEqual(len(dirs), 12, "12 transcript directories, like the real machine")
        self.assertGreaterEqual(len(m.sessions), 38)
        self.assertLessEqual(len(m.sessions), 44)
        turns = sum(len(s.turns) for s in m.sessions)
        self.assertGreaterEqual(turns, 8000, "~9k assistant turns, like the real 9,006")
        self.assertLessEqual(turns, 10000)

    def test_stress_is_ten_times_full(self):
        full, stress = mf.build("full"), mf.build("stress")
        ratio = sum(len(s.turns) for s in stress.sessions) / sum(len(s.turns) for s in full.sessions)
        self.assertGreaterEqual(ratio, 9.0)
        self.assertLessEqual(ratio, 11.0)

    def test_every_matrix_cell_is_filled(self):
        m = mf.build("full")
        cells = mf.coverage(m)
        for kind, states in mf.REQUIRED_CELLS.items():
            for state in states:
                self.assertIn((kind, state), cells, f"no capability of kind {kind} in state {state}")

    def test_every_edge_case_project_exists(self):
        keys = {p.key for p in mf.build("full").projects}
        for k in ("alpha", "beta", "gamma", "my-repo", "deleted-proj", "history-only",
                  "mixed", "wanderer", "home"):
            self.assertIn(k, keys)

    def test_history_only_has_no_measured_turn(self):
        m = mf.build("full")
        s = [x for x in m.sessions if x.project == "history-only"]
        self.assertTrue(s)
        self.assertFalse(any(mf.measured(t, m) for x in s for t in x.turns))

    def test_mixed_session_spans_the_install(self):
        m = mf.build("full")
        s = [x for x in m.sessions if x.project == "mixed"][0]
        flags = {mf.measured(t, m) for t in s.turns}
        self.assertEqual(flags, {True, False})

    def test_gamma_session_spans_four_subfolders(self):
        m = mf.build("full")
        cwds = {t.cwd for s in m.sessions if s.project == "gamma" for t in s.turns}
        gamma = [p for p in m.projects if p.key == "gamma"][0]
        self.assertGreaterEqual(len([c for c in cwds if c.startswith(gamma.root + "/")]), 4)

    def test_smoke_is_small_and_covers_each_kind_once(self):
        m = mf.build("smoke")
        self.assertEqual(len({p.transcript_dir for p in m.projects}), 3)
        self.assertLessEqual(len(m.sessions), 8)
        self.assertEqual({c.kind for c in m.capabilities},
                         {"mcp_tool", "plugin_skill", "marketplace_skill", "bundled_skill", "builtin"})

    def test_stream_mix_is_about_eighty_percent(self):
        m = mf.build("full")
        post = [t for s in m.sessions for t in s.turns if mf.measured(t, m)]
        share = sum(t.stream for t in post) / len(post)
        self.assertGreaterEqual(share, 0.75)
        self.assertLessEqual(share, 0.85)
        self.assertEqual(sum(t.truncate for t in post), 2, "two truncated streams (Review Focus 2)")

    def test_role_labels_match_observed_states(self):
        """Every role label must be observed in the manifest's actual invocations."""
        for tier in ("smoke", "full"):
            m = mf.build(tier)
            observed = mf.observed_states(m)
            rmap = mf.roles(m)

            for cap_name, (label_state, _projects, _window) in rmap.items():
                cap_observed = observed[cap_name]
                self.assertIn(label_state, cap_observed,
                    f"{tier}: {cap_name} labelled '{label_state}' but observed {cap_observed}")


if __name__ == "__main__":
    unittest.main()
