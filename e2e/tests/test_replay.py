import dataclasses
import json
import threading
import unittest
from unittest import mock

from e2e import replay, upstream
from e2e.gen import manifest as mf


class TestReplay(unittest.TestCase):
    def setUp(self):
        self.m = mf.build("smoke")
        self.s = [s for s in self.m.sessions if any(mf.measured(t, self.m) for t in s.turns)][0]
        self.t = [t for t in self.s.turns if mf.measured(t, self.m)][0]

    def test_body_carries_the_project_marker(self):
        b = replay.body_for(self.t, self.s, self.m)
        self.assertIn(f"Primary working directory: {self.t.cwd}", json.dumps(b))

    def test_replay_body_without_marker_is_marked_unattributed(self):
        # Review Focus 1: a body the proxy cannot attribute must carry no marker at all.
        b = replay.body_for(self.t, self.s, self.m, with_marker=False)
        self.assertNotIn("Primary working directory", json.dumps(b))

    def test_body_declares_mcp_tools_and_lists_skills(self):
        b = replay.body_for(self.t, self.s, self.m)
        names = {x["name"] for x in b["tools"]}
        self.assertIn("mcp__docs-search__search", names)
        self.assertIn("Bash", names)
        msgs = json.dumps(b["messages"])
        self.assertIn("The following skills are available for use with the Skill tool:", msgs)
        self.assertIn("- toolkit:planner:", msgs)
        self.assertIn("- chartmaker:", msgs, "bundled skills are declared in the prompt")

    def test_skills_listing_is_its_own_system_message(self):
        b = replay.body_for(self.t, self.s, self.m)
        sys_msgs = [x for x in b["messages"] if x["role"] == "system"]
        self.assertEqual(len(sys_msgs), 1)
        self.assertIn("The following skills are available for use with the Skill tool:",
                      sys_msgs[0]["content"][0]["text"])
        self.assertNotIn("The following skills", json.dumps(b["messages"][0]))

    def test_session_id_uses_claude_codes_object_shape(self):
        b = replay.body_for(self.t, self.s, self.m)
        self.assertEqual(json.loads(b["metadata"]["user_id"]), {"session_id": self.s.id})

    def test_run_sends_every_post_install_turn(self):
        srv = upstream.serve(0)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            r = replay.run(self.m, f"http://127.0.0.1:{srv.server_address[1]}")
        finally:
            srv.shutdown()
            srv.server_close()
        want = sum(1 for s in self.m.sessions for t in s.turns if mf.measured(t, self.m))
        self.assertEqual(r["sent"], want)
        self.assertEqual(r["failed"], 0)

    def test_run_records_one_send_time_per_sent_turn(self):
        import time
        t0 = int(time.time() * 1000)
        srv = upstream.serve(0)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        stamps = {}
        try:
            r = replay.run(self.m, f"http://127.0.0.1:{srv.server_address[1]}", stamps=stamps)
        finally:
            srv.shutdown()
            srv.server_close()
        self.assertEqual(len(stamps), r["sent"])
        self.assertTrue(all(v >= t0 for v in stamps.values()))
        self.assertTrue(all("@" in k for k in stamps))

    def test_probe_sends_one_turn_under_a_fresh_session_and_records_no_stamp(self):
        srv = upstream.serve(0)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        stamps = {}
        try:
            r = replay.run(self.m, f"http://127.0.0.1:{srv.server_address[1]}", stamps=stamps, probe="-shutdown-probe")
        finally:
            srv.shutdown()
            srv.server_close()
        self.assertEqual((r["sent"], r["ok"]), (1, 1))
        self.assertEqual(stamps, {})

    def test_run_counts_an_upstream_400_as_failed(self):
        bad = dataclasses.replace(self.t, fresh=-1)  # upstream rejects negative usage with 400
        srv = upstream.serve(0)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            with mock.patch.object(replay, "_turns", return_value=[(self.s, bad)]):
                r = replay.run(self.m, f"http://127.0.0.1:{srv.server_address[1]}", limit=1)
        finally:
            srv.shutdown()
            srv.server_close()
        self.assertEqual(r["sent"], 1)
        self.assertEqual(r["ok"], 0)
        self.assertEqual(r["failed"], 1)

    def test_body_uses_the_priced_model(self):
        """body_for() should use the priced model constant by default."""
        b = replay.body_for(self.t, self.s, self.m)
        self.assertEqual(b["model"], mf.MODEL)

    def test_model_override_is_used(self):
        """body_for() with model_override should use the provided model instead of default."""
        b = replay.body_for(self.t, self.s, self.m, model_override=mf.UNPRICED_MODEL)
        self.assertEqual(b["model"], mf.UNPRICED_MODEL)


if __name__ == "__main__":
    unittest.main()
