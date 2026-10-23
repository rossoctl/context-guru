"""Tests for costly.py and the `settings` words of cg.py. Every test uses temp dirs for HOME, CLAUDE_CONFIG_DIR, the project and
the state: no real user file is read or written. Run: python3 -I scripts/test_costly.py"""

import io
import json
import os
import stat
import sys
import tempfile
import unittest
from contextlib import redirect_stdout

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import costly as S  # noqa: E402
import cg  # noqa: E402
import tvlib as T  # noqa: E402

BETAS = "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"
ID1, ID2 = "disable-experimental-betas", "session-recap"
ID3 = "small-model-rejects-server-check"
SMALL, HAIKU = "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"
OLD, NEW = "claude-haiku-4-5", "claude-haiku-5-5"
ENV_KEYS = [SMALL, HAIKU, "ANTHROPIC_BASE_URL", "CG_URL", BETAS, "CLAUDE_CODE_ENABLE_AWAY_SUMMARY", "CLAUDE_CONFIG_DIR", "CLAUDE_PROJECT_DIR", "CONTEXT_GURU_STATE", "CG_MANAGED_SETTINGS"]


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        t = self.tmp.name
        self.user, self.proj, self.state = os.path.join(t, "user"), os.path.join(t, "proj"), os.path.join(t, "state")
        self.managed = os.path.join(t, "managed.json")
        for d in (self.user, os.path.join(self.proj, ".claude")):
            os.makedirs(d)
        self.saved = {k: os.environ.get(k) for k in ENV_KEYS}
        for k in ENV_KEYS:
            os.environ.pop(k, None)
        os.environ.update(CLAUDE_CONFIG_DIR=self.user, CLAUDE_PROJECT_DIR=self.proj, CONTEXT_GURU_STATE=self.state, CG_MANAGED_SETTINGS=self.managed)
        self.addCleanup(self.restore)

    def restore(self):
        for k, v in self.saved.items():
            os.environ.pop(k, None)
            if v is not None:
                os.environ[k] = v

    def put(self, where, doc, mode=0o600):
        path = {"user": os.path.join(self.user, "settings.json"), "user-local": os.path.join(self.user, "settings.local.json"),
                "project": os.path.join(self.proj, ".claude", "settings.json"),
                "project-local": os.path.join(self.proj, ".claude", "settings.local.json"), "managed": self.managed}[where]
        with open(path, "w") as f:
            json.dump(doc, f, indent=2)
        os.chmod(path, mode)
        return path

    def clean_recap(self):  # a settings file that turns the recap off, so only the betas check can fire
        return self.put("user-local", {"awaySummaryEnabled": False})

    def ids(self):
        return [f["check"]["id"] for f in S.findings()]


class Detect(Base):
    def test_clean_settings_fire_nothing_but_the_recap_default(self):
        self.assertEqual(self.ids(), [ID2])
        self.clean_recap()
        self.assertEqual(self.ids(), [])

    def test_betas_in_each_file(self):
        self.clean_recap()
        for where in ("user", "user-local", "project", "project-local", "managed"):
            path = self.put(where, {"env": {BETAS: "1"}, **({"awaySummaryEnabled": False} if where == "user-local" else {})})
            fs = S.findings()
            self.assertEqual([f["check"]["id"] for f in fs], [ID1], where)
            loc = fs[0]["locations"][0]
            self.assertEqual((loc["source"], loc["path"], loc["editable"]), (where, path, where != "managed"))
            os.unlink(path)
            self.clean_recap()

    def test_betas_in_process_env(self):
        self.clean_recap()
        os.environ[BETAS] = "true"
        loc = S.findings()[0]["locations"][0]
        self.assertEqual((loc["source"], loc["path"], loc["editable"]), ("process", None, False))

    def test_betas_off_values_do_not_fire(self):
        self.clean_recap()
        for v in ("0", "false", "", "no"):
            self.put("user", {"env": {BETAS: v}})
            self.assertEqual(self.ids(), [], v)

    def test_recap_reads(self):
        self.put("user", {"awaySummaryEnabled": True})
        self.assertEqual(self.ids(), [ID2])
        self.put("user", {"awaySummaryEnabled": False})
        self.assertEqual(self.ids(), [])
        os.environ["CLAUDE_CODE_ENABLE_AWAY_SUMMARY"] = "0"
        self.put("user", {})
        self.assertEqual(self.ids(), [])
        os.environ["CLAUDE_CODE_ENABLE_AWAY_SUMMARY"] = "1"
        self.assertEqual(self.ids(), [ID2])

    def test_recap_higher_precedence_file_wins(self):
        self.put("user", {"awaySummaryEnabled": False})
        self.put("project-local", {"awaySummaryEnabled": True})
        self.assertEqual(self.ids(), [ID2])

    def test_reads_only_named_keys(self):
        self.put("user", {"env": {BETAS: "1", "ANTHROPIC_AUTH_TOKEN": "sk-secret-123"}, "apiKeyHelper": "secret-helper"})
        seen = S.read_named([BETAS], [])
        self.assertNotIn("sk-secret-123", json.dumps(seen))
        self.assertNotIn("secret-helper", json.dumps(seen))
        out = "\n".join(S.render(S.findings(), None, 30, cg.dollars, cg.card, "/x", T.Env()))
        self.assertNotIn("sk-secret", out)

    def test_broken_file_is_skipped(self):
        with open(os.path.join(self.user, "settings.json"), "w") as f:
            f.write("{not json")
        self.assertEqual(self.ids(), [ID2])


class HaikuVersion(unittest.TestCase):
    def test_table(self):
        for model, want in [("claude-haiku-4-5", (4, 5)), ("claude-haiku-4-5-20251001", (4, 5)), ("aws/claude-haiku-4-5", (4, 5)),
                            ("us.anthropic.claude-haiku-4-5-20251001-v1:0", (4, 5)), ("claude-3-5-haiku-20241022", (3, 5)),
                            ("claude-3-haiku-20240307", (3, 0)), ("claude-haiku-5-5", (5, 5)), ("claude-haiku-5-5[1m]", (5, 5)),
                            ("CLAUDE-HAIKU-4-5", (4, 5)), ("claude-haiku-5", (5, 0)), ("claude-haiku-6", (6, 0)), ("claude-haiku-4.5", (4, 5)),
                            ("claude-haiku-5-10", (5, 10)),
                            ("claude-sonnet-4-5", None), ("claude-opus-4-1", None), ("garbage", None), ("", None), ("haiku", None),
                            ("claude-haiku-latest", None), ("my-haiku-20251001", None), (None, None), (5, None)]:
            self.assertEqual(S.haiku_version(model), want, model)


class SmallModel(Base):
    def setUp(self):
        super().setUp()
        self.clean_recap()

    def test_each_source_and_key(self):
        for where in ("user", "project", "project-local", "managed"):
            for key in (SMALL, HAIKU):
                path = self.put(where, {"env": {key: OLD}})
                fs = S.findings()
                self.assertEqual([f["check"]["id"] for f in fs], [ID3], (where, key))
                self.assertEqual(fs[0]["locations"][0]["editable"], where != "managed")
                self.assertEqual(fs[0]["values"], [OLD])
                os.unlink(path)

    def test_process_env(self):
        os.environ[SMALL] = OLD
        fs = S.findings()
        self.assertEqual([f["check"]["id"] for f in fs], [ID3])
        self.assertEqual(fs[0]["locations"][0]["source"], "process")

    def test_fires_with_or_without_the_betas_setting(self):
        self.put("user", {"env": {SMALL: OLD}})
        self.assertEqual(self.ids(), [ID3])
        self.put("user", {"env": {SMALL: OLD, BETAS: "1"}})
        self.assertEqual(self.ids(), [ID1, ID3])

    def test_new_unset_and_foreign_models_do_not_fire(self):
        self.assertEqual(self.ids(), [])  # unset: Claude Code's own default is not judged
        for v in (NEW, "claude-haiku-5-5[1m]", "claude-sonnet-4-5", "garbage", "", "claude-haiku-6"):
            self.put("user", {"env": {SMALL: v, HAIKU: v}})
            self.assertEqual(self.ids(), [], v)

    def test_higher_file_overrides_a_lower_one(self):
        self.put("user", {"env": {SMALL: OLD}})
        self.put("project-local", {"env": {SMALL: NEW}})
        self.assertEqual(self.ids(), [])
        self.put("project-local", {"env": {SMALL: OLD}})
        self.put("user", {"env": {SMALL: NEW}})
        self.assertEqual(self.ids(), [ID3])

    def test_a_file_beats_the_process_env(self):
        os.environ[SMALL] = OLD
        self.put("user", {"env": {SMALL: NEW}})
        self.assertEqual(self.ids(), [])

    def card(self, rows=None):
        return "\n".join(S.render(S.findings(), rows, 30, cg.dollars, cg.card, "/x", T.Env(color=False, width=120)))

    def test_card_text_with_and_without_the_betas_check(self):
        self.put("user", {"env": {SMALL: OLD}})
        out = self.card()
        self.assertIn(f"Small model {OLD} rejects the free auto-mode server check", out)
        self.assertIn("The betas setting is off, so the small model can cause billed classifier requests now.", out)
        self.assertNotIn("after you remove", out)
        self.assertIn("Check that your gateway serves claude-haiku-5-5 first.", out)
        self.put("user", {"env": {SMALL: OLD, BETAS: "1"}})
        out = self.card()
        self.assertIn("This matters after you remove the betas setting above.", out)
        self.assertNotIn("can cause billed classifier requests now", out)

    def test_cost_line_notes_the_double_count(self):
        self.put("user", {"env": {SMALL: OLD}})
        row = {"id": ID3, "measured": True, "requests": 4, "cost_usd": 1.5, "also": []}
        out = self.card({ID3: row})
        self.assertIn("about $1.50 over the last 30 days (4 requests)", out)
        self.assertIn("(also counted under the betas setting while it is on)", out)
        self.assertNotIn("(also counted", self.card(None))

    def test_apply_sets_both_keys_in_the_right_files_only(self):
        user = self.put("user", {"model": "opus", "env": {SMALL: OLD, "KEEP": "x"}}, mode=0o600)
        proj = self.put("project-local", {"env": {HAIKU: OLD}}, mode=0o644)
        other = self.put("project", {"env": {HAIKU: NEW}})
        other_before = open(other).read()
        user_before = open(user).read()
        out, rc = S.apply(ID3)
        self.assertEqual(rc, 0, out)
        self.assertEqual(json.load(open(user)), {"model": "opus", "env": {SMALL: NEW, "KEEP": "x"}})  # HAIKU is not set here: not added
        self.assertEqual(json.load(open(proj)), {"env": {HAIKU: NEW}})
        self.assertEqual(open(other).read(), other_before)
        self.assertEqual(stat.S_IMODE(os.stat(user).st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(os.stat(proj).st_mode), 0o644)
        baks = [f for f in os.listdir(self.user) if f.endswith(".bak")]
        self.assertEqual(len(baks), 1)
        self.assertEqual(open(os.path.join(self.user, baks[0])).read(), user_before)
        self.assertEqual(self.ids(), [])

    def test_both_keys_in_one_file_one_backup_two_records(self):
        path = self.put("user", {"env": {SMALL: OLD, HAIKU: "claude-3-5-haiku-20241022"}})
        out, rc = S.apply(ID3)
        self.assertEqual(rc, 0, out)
        self.assertEqual(json.load(open(path)), {"env": {SMALL: NEW, HAIKU: NEW}})
        self.assertEqual(len([f for f in os.listdir(self.user) if f.endswith(".bak")]), 1)
        self.assertEqual(len(S.load_record()[ID3]["entries"]), 2)

    def test_revert_restores_both_exactly(self):
        path = self.put("user", {"env": {SMALL: OLD, HAIKU: "claude-3-5-haiku-20241022", "KEEP": "x"}})
        before = json.load(open(path))
        S.apply(ID3)
        out, rc = S.revert(ID3)
        self.assertEqual(rc, 0, out)
        self.assertEqual(json.load(open(path)), before)
        self.assertEqual(S.revert(ID3)[1], 1)

    def test_revert_refuses_after_an_outside_change_of_either_key(self):
        for key in (SMALL, HAIKU):
            path = self.put("user", {"env": {SMALL: OLD, HAIKU: OLD}})
            S.apply(ID3)
            doc = json.load(open(path))
            doc["env"][key] = "claude-haiku-5-6"
            json.dump(doc, open(path, "w"))
            snap = open(path).read()
            out, rc = S.revert(ID3)
            self.assertEqual(rc, 1, key)
            self.assertIn("Not undone", out[0])
            self.assertEqual(open(path).read(), snap)
            os.unlink(os.path.join(self.state, "costly-settings.json"))

    def test_managed_only_and_process_only_are_not_applicable(self):
        path = self.put("managed", {"env": {SMALL: OLD}})
        before = open(path).read()
        out, rc = S.apply(ID3)
        self.assertEqual(rc, 1)
        self.assertIn("administrator", out[0])
        self.assertEqual(open(path).read(), before)
        self.assertNotIn("Apply it?", self.card())
        os.unlink(path)
        os.environ[SMALL] = OLD
        out, rc = S.apply(ID3)
        self.assertEqual(rc, 1)
        self.assertIn("not in a file", out[0])

    def test_a_read_never_writes(self):
        self.put("user", {"env": {SMALL: OLD}})
        snap = {f: open(os.path.join(self.user, f)).read() for f in os.listdir(self.user)}
        S.findings()
        self.card()
        self.assertEqual({f: open(os.path.join(self.user, f)).read() for f in os.listdir(self.user)}, snap)
        self.assertFalse(os.path.exists(self.state))


class Cost(Base):
    ROWS = {ID1: {"id": ID1, "measured": True, "requests": 6016, "cost_usd": 249.52,
                  "also": [{"id": "classifier-stage-2", "requests": 12, "cost_usd": 3.5}]},
            ID2: {"id": ID2, "measured": False, "requests": 0, "cost_usd": None, "also": []}}

    def card(self, rows, ascii_only=False):
        return "\n".join(S.render(S.findings(), rows, 30, cg.dollars, cg.card, "/context-guru:save-more", T.Env(color=False, ascii_only=ascii_only, width=100)))

    def test_cost_in_hindsight_and_not_measured(self):
        self.put("user", {"env": {BETAS: "1"}})
        out = self.card(self.ROWS)
        self.assertIn("about $249.52 over the last 30 days (6,016 requests)", out)
        self.assertIn("probably", out)
        self.assertIn("not added to the figure above", out)
        self.assertEqual(out.count("Cost: not measured yet"), 1)  # the recap

    def test_old_proxy_shows_settings_with_not_measured(self):
        self.put("user", {"env": {BETAS: "1"}})
        out = self.card(None)
        self.assertIn(BETAS, out)
        self.assertEqual(out.count("Cost: not measured yet"), 2)
        self.assertNotIn("$0", out)

    def test_costs_returns_none_on_404(self):
        self.assertIsNone(S.costs(cg.Api("", {}), 0, 1))  # fixture without the route: 404
        self.assertIsNone(S.costs(None, 0, 1))
        self.assertEqual(S.costs(cg.Api("", {"/api/adapt/settings": {"checks": [self.ROWS[ID1]]}}), 0, 1)[ID1]["cost_usd"], 249.52)

    def test_ascii_and_unicode(self):
        self.put("user", {"env": {BETAS: "1"}})
        uni, asc = self.card(self.ROWS), self.card(self.ROWS, ascii_only=True)
        self.assertIn("╭", uni)
        self.assertTrue(asc.isascii(), [c for c in asc if ord(c) > 127])
        self.assertIn("+- Settings that cost you money", asc)

    def test_managed_value_says_it_cannot_be_changed(self):
        self.clean_recap()
        self.put("managed", {"env": {BETAS: "1"}})
        out = self.card(self.ROWS)
        self.assertIn("administrator", out)
        self.assertNotIn("Apply it?", out)

    def test_apply_line_is_not_wrapped(self):
        self.clean_recap()
        self.put("user", {"env": {BETAS: "1"}})
        last = self.card(self.ROWS).splitlines()[-1]
        self.assertEqual(last, f"Apply it? Say yes, or run: /context-guru:save-more apply settings {ID1}")


class Change(Base):
    def test_apply_edits_only_the_key_backs_up_keeps_mode(self):
        self.clean_recap()
        path = self.put("user", {"model": "opus", "env": {BETAS: "1", "KEEP": "x"}, "hooks": {"a": [1]}}, mode=0o600)
        before = open(path).read()
        out, rc = S.apply(ID1)
        self.assertEqual(rc, 0, out)
        doc = json.load(open(path))
        self.assertEqual(doc, {"model": "opus", "env": {"KEEP": "x"}, "hooks": {"a": [1]}})
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
        baks = [f for f in os.listdir(self.user) if f.startswith("settings.json.") and f.endswith(".bak")]
        self.assertEqual(len(baks), 1)
        self.assertEqual(open(os.path.join(self.user, baks[0])).read(), before)
        self.assertEqual(stat.S_IMODE(os.stat(os.path.join(self.user, baks[0])).st_mode), 0o600)
        self.assertEqual([f for f in os.listdir(self.user) if f.endswith(".tmp")], [])

    def test_mode_0644_is_kept_too(self):
        self.clean_recap()
        path = self.put("user", {"env": {BETAS: "1"}}, mode=0o644)
        S.apply(ID1)
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o644)

    def test_revert_restores_the_previous_value(self):
        self.clean_recap()
        path = self.put("user", {"env": {BETAS: "true", "KEEP": "x"}})
        S.apply(ID1)
        out, rc = S.revert(ID1)
        self.assertEqual(rc, 0, out)
        self.assertEqual(json.load(open(path)), {"env": {"KEEP": "x", BETAS: "true"}})
        self.assertEqual(S.revert(ID1)[1], 1)  # nothing left to undo

    def test_revert_refuses_after_an_outside_change(self):
        self.clean_recap()
        path = self.put("user", {"env": {BETAS: "1"}})
        S.apply(ID1)
        doc = json.load(open(path))
        doc["env"][BETAS] = "1"  # the user, or a tool, set the key again
        json.dump(doc, open(path, "w"))
        snapshot = open(path).read()
        out, rc = S.revert(ID1)
        self.assertEqual(rc, 1)
        self.assertIn("Not undone", out[0])
        self.assertEqual(open(path).read(), snapshot)

    def test_recap_apply_and_revert_when_the_key_was_unset(self):
        path = self.put("user", {"model": "opus"})
        out, rc = S.apply(ID2)
        self.assertEqual(rc, 0, out)
        self.assertEqual(json.load(open(path)), {"model": "opus", "awaySummaryEnabled": False})
        self.assertEqual(S.findings(), [])
        S.revert(ID2)
        self.assertEqual(json.load(open(path)), {"model": "opus"})

    def test_recap_apply_creates_the_user_file_and_revert_removes_it(self):
        path = os.path.join(self.user, "settings.json")
        out, rc = S.apply(ID2)
        self.assertEqual((rc, json.load(open(path))), (0, {"awaySummaryEnabled": False}))
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
        S.revert(ID2)
        self.assertFalse(os.path.exists(path))

    def test_recap_set_true_in_user_file_goes_to_false(self):
        path = self.put("user", {"awaySummaryEnabled": True})
        S.apply(ID2)
        self.assertEqual(json.load(open(path)), {"awaySummaryEnabled": False})
        S.revert(ID2)
        self.assertEqual(json.load(open(path)), {"awaySummaryEnabled": True})

    def test_managed_setting_cannot_be_applied(self):
        self.clean_recap()
        path = self.put("managed", {"env": {BETAS: "1"}})
        before = open(path).read()
        out, rc = S.apply(ID1)
        self.assertEqual(rc, 1)
        self.assertIn("administrator", out[0])
        self.assertEqual(open(path).read(), before)
        self.assertEqual(os.listdir(self.state) if os.path.isdir(self.state) else [], [])

    def test_process_env_only_cannot_be_applied(self):
        self.clean_recap()
        os.environ[BETAS] = "1"
        out, rc = S.apply(ID1)
        self.assertEqual(rc, 1)
        self.assertIn("not in a file", out[0])

    def test_managed_and_user_both_set_only_user_changes(self):
        self.clean_recap()
        user = self.put("user", {"env": {BETAS: "1"}})
        man = self.put("managed", {"env": {BETAS: "1"}})
        S.apply(ID1)
        self.assertEqual(json.load(open(user)), {"env": {}})
        self.assertEqual(json.load(open(man)), {"env": {BETAS: "1"}})

    def test_unknown_id_and_clean_settings(self):
        self.assertEqual(S.apply("nope")[1], 1)
        self.clean_recap()
        out, rc = S.apply(ID1)
        self.assertEqual(rc, 0)
        self.assertIn("Nothing to change", out[0])

    def test_symlinked_settings_file_keeps_its_link(self):
        self.clean_recap()
        real = os.path.join(self.tmp.name, "dotfiles.json")
        json.dump({"env": {BETAS: "1"}}, open(real, "w"))
        link = os.path.join(self.user, "settings.json")
        os.symlink(real, link)
        S.apply(ID1)
        self.assertTrue(os.path.islink(link))
        self.assertEqual(json.load(open(real)), {"env": {}})


class Cli(Base):
    """The cg.py words. A read never writes; apply and revert are their own commands."""

    def run_cg(self, *argv):
        buf = io.StringIO()
        old = sys.argv
        sys.argv = ["cg.py"]
        try:
            with redirect_stdout(buf):
                rc = cg.main(list(argv) + ["--now", "2026-10-08T15:30:00Z", "--tz-offset-min", "0"])
        finally:
            sys.argv = old
        return buf.getvalue(), rc

    def tree(self):
        out = {}
        for root, _, files in os.walk(self.tmp.name):
            for f in files:
                p = os.path.join(root, f)
                out[p] = open(p, "rb").read()
        return out

    def test_a_read_never_writes(self):
        self.put("user", {"env": {BETAS: "1"}})
        os.environ["CG_URL"] = "http://127.0.0.1:1"  # nothing listens: the cost is not measured
        self.addCleanup(os.environ.pop, "CG_URL", None)
        before = self.tree()
        out, rc = self.run_cg("save-more", "settings")
        self.assertEqual(rc, 0)
        self.assertIn("Settings that cost you money", out)
        self.assertIn("Cost: not measured yet", out)
        self.assertEqual(self.tree(), before)

    def test_settings_read_needs_no_proxy(self):
        self.put("user", {"env": {BETAS: "1"}})
        os.environ.pop("ANTHROPIC_BASE_URL", None)
        out, rc = self.run_cg("save-more", "settings")
        self.assertEqual(rc, 0)
        self.assertIn(BETAS, out)

    def test_nothing_found_says_so(self):
        self.clean_recap()
        out, rc = self.run_cg("save-more", "settings")
        self.assertEqual((rc, out.strip()), (0, "No settings that cost you money were found."))

    def test_apply_and_revert_words(self):
        self.clean_recap()
        path = self.put("user", {"env": {BETAS: "1"}})
        out, rc = self.run_cg("apply", "settings", ID1)
        self.assertEqual(rc, 0, out)
        self.assertEqual(json.load(open(path)), {"env": {}})
        out, rc = self.run_cg("revert", "settings", ID1)
        self.assertEqual(rc, 0, out)
        self.assertEqual(json.load(open(path)), {"env": {BETAS: "1"}})

    def test_a_change_under_a_read_command_is_refused_with_the_exact_command(self):
        self.put("user", {"env": {BETAS: "1"}})
        os.environ["CG_URL"] = "http://127.0.0.1:1"
        self.addCleanup(os.environ.pop, "CG_URL", None)
        before = self.tree()
        out, rc = self.run_cg("save-more", "apply", "settings", ID1)
        self.assertEqual(rc, 1)
        self.assertIn("Run this instead", out)
        self.assertIn(f"apply settings {ID1}", out)
        self.assertEqual(self.tree(), before)

    def test_revert_needs_an_id(self):
        out, rc = self.run_cg("revert", "settings")
        self.assertEqual(rc, 1)
        self.assertIn("Usage", out)


if __name__ == "__main__":
    unittest.main()
