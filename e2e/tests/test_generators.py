# e2e/tests/test_generators.py
import json
import pathlib
import tempfile
import unittest

from e2e.gen import inventory, manifest as mf, shape, transcripts

REF = json.loads((pathlib.Path(__file__).resolve().parents[1] / "gen" / "reference_keys.json").read_text())


class TestGenerators(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = self.tmp.name
        self.m = mf.build("full", home=self.home)

    def tearDown(self):
        self.tmp.cleanup()

    def test_one_file_per_session_in_its_project_dir(self):
        r = transcripts.write(self.m, self.home)
        self.assertEqual(r["files"], len(self.m.sessions))
        dirs = {p.name for p in (pathlib.Path(self.home) / ".claude" / "projects").iterdir()}
        self.assertEqual(len(dirs), 12)

    def test_post_install_turns_take_their_stamped_time_and_pre_install_do_not(self):
        post = [(x, t) for x in self.m.sessions for t in x.turns if mf.measured(t, self.m)]
        stamps = {f"{x.id}@{t.ts}": 1_800_000_000_000 + i for i, (x, t) in enumerate(post)}
        transcripts.write(self.m, self.home, stamps)
        cut = self.m.sessions[mf.TRUNCATED_SESSION_INDEX]  # its last assistant line is half-written
        cut_key = f"{cut.id}@{cut.turns[-1].ts}"
        stamped = {transcripts._iso(v) for k, v in stamps.items() if k != cut_key}
        seen = set()
        for f in (pathlib.Path(self.home) / ".claude" / "projects").glob("*/*.jsonl"):
            for l in f.read_text().splitlines():
                try:
                    e = json.loads(l)
                except ValueError:
                    continue  # the deliberately truncated line
                if e.get("type") == "assistant":
                    seen.add(e["timestamp"])
        self.assertTrue(stamped <= seen, "every post-install turn carries its stamp")
        pre = {transcripts._iso(t.ts) for x in self.m.sessions for t in x.turns
               if not mf.measured(t, self.m) and not (x is cut and t is cut.turns[-1])}
        self.assertTrue(pre <= seen, "pre-install turns keep the manifest time")

    def test_post_install_turn_without_a_stamp_raises(self):
        with self.assertRaises(KeyError):
            transcripts.write(self.m, self.home, {})

    def test_generator_emits_one_truncated_final_line(self):
        # Review Focus 4: a live session's last line is often cut off.
        r = transcripts.write(self.m, self.home)
        self.assertEqual(r["truncated_lines"], 1)

    def test_assistant_turn_has_the_fields_the_importer_reads(self):
        transcripts.write(self.m, self.home)
        f = next((pathlib.Path(self.home) / ".claude" / "projects").glob("*/*.jsonl"))
        e = next(json.loads(l) for l in f.read_text().splitlines() if '"assistant"' in l)
        for k in ("type", "sessionId", "cwd", "timestamp", "message"):
            self.assertIn(k, e)
        self.assertIn("usage", e["message"])

    def test_skill_invocation_uses_the_Skill_tool_input(self):
        transcripts.write(self.m, self.home)
        text = "".join(p.read_text() for p in (pathlib.Path(self.home) / ".claude" / "projects").glob("*/*.jsonl"))
        self.assertIn('"name": "Skill", "input": {"skill": "toolkit:planner"}', text)

    def test_generated_data_matches_the_real_shape(self):
        transcripts.write(self.m, self.home)
        self.assertEqual(shape.check(self.home, REF), [])

    def test_shape_check_can_fail(self):
        transcripts.write(self.m, self.home)
        f = next((pathlib.Path(self.home) / ".claude" / "projects").glob("*/*.jsonl"))
        f.write_text('{"type":"assistant","bogusKey":1}\n')
        self.assertTrue(shape.check(self.home, REF))

    def test_inventory_writes_every_on_disk_skill_and_mcp_server(self):
        r = inventory.write(self.m, self.home)
        disk = [c for c in self.m.capabilities if c.on_disk]
        self.assertEqual(r["plugin_skills"], sum(c.kind == "plugin_skill" for c in disk))
        self.assertEqual(r["marketplace_skills"], sum(c.kind == "marketplace_skill" for c in disk))
        cfg = json.loads((pathlib.Path(self.home) / ".claude.json").read_text())
        self.assertEqual(len(cfg["mcpServers"]), r["mcp_servers"])
        self.assertFalse(any(pathlib.Path(self.home).rglob("chartmaker")), "bundled skill must not be on disk")

    def test_deleted_project_has_transcripts_but_no_folder(self):
        transcripts.write(self.m, self.home)
        inventory.write(self.m, self.home)
        d = [p for p in self.m.projects if p.key == "deleted-proj"][0]
        self.assertFalse(pathlib.Path(d.root).exists())
        self.assertTrue((pathlib.Path(self.home) / ".claude" / "projects" / d.transcript_dir).exists())

    def test_truncated_session_loses_exactly_its_final_assistant_turn(self):
        transcripts.write(self.m, self.home)
        s = self.m.sessions[mf.TRUNCATED_SESSION_INDEX]
        d = [p for p in self.m.projects if p.key == s.project][0].transcript_dir
        f = pathlib.Path(self.home) / ".claude" / "projects" / d / f"{s.id}.jsonl"
        lines = f.read_text().split("\n")
        with self.assertRaises(ValueError):
            json.loads(lines[-1])
        ok = 0
        for l in lines:
            try:
                e = json.loads(l)
            except ValueError:
                continue
            ok += e.get("type") == "assistant"
        self.assertEqual(ok, len(s.turns) - 1)

    def test_pre_install_only_writes_exactly_the_turns_install_will_import(self):
        # Install imports before any traffic, so whatever is on disk then is what it imports: every
        # pre-install turn, the truncated one minus its cut line, and no post-install turn at all.
        from e2e import oracle
        for tier in ("smoke", "full"):
            with self.subTest(tier=tier), tempfile.TemporaryDirectory() as home:
                m = mf.build(tier, home=home)
                transcripts.write(m, home, pre_install_only=True)
                n = post = 0
                for f in (pathlib.Path(home) / ".claude" / "projects").glob("*/*.jsonl"):
                    for l in f.read_text().splitlines():
                        try:
                            e = json.loads(l)
                        except ValueError:
                            continue
                        if e.get("type") == "assistant":
                            n += 1
                            post += e["timestamp"] >= transcripts._iso(m.install_ms)
                self.assertEqual((n, post), (oracle.expected(m)["import"]["valid_pre_install_turns"], 0))

    def test_assistant_line_message_model_is_priced_model(self):
        """Assistant line's message.model should use the priced model constant."""
        transcripts.write(self.m, self.home)
        f = next((pathlib.Path(self.home) / ".claude" / "projects").glob("*/*.jsonl"))
        for line in f.read_text().splitlines():
            e = json.loads(line)
            if e.get("type") == "assistant":
                self.assertEqual(e["message"]["model"], mf.MODEL)
                break


if __name__ == "__main__":
    unittest.main()
