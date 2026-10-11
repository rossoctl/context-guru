#!/usr/bin/env python3
"""Codex front end for the shared insights engine (insights_core.py).

Same collectors, ranking, pricing, refusals and `key=value` output as the Claude plugin's
/context-guru:insights; only the host-specific parts differ: where the port and options come
from (the Codex install record and proxy.yaml), and the wording of fix commands.
"""

import os
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import insights_core as core  # noqa: E402
import settings  # noqa: E402
from codex_plugin import read_proxy_options, read_record, proxy_config  # noqa: E402

_REWRITES = [
    (r'"\$\{CLAUDE_PLUGIN_ROOT\}/scripts/start-proxy\.sh" --unrouted',
     lambda m: str(Path(settings.state_dir()) / "context-guru-codex")),
    (r"/context-guru:install", lambda m: "$context-guru-setup"),
    (r"/context-guru:([a-z-]+)", lambda m: f"$context-guru-{m.group(1)}"),
    (r"Claude Code", lambda m: "Codex"),
]


def codexify(text):
    if not isinstance(text, str):
        return text
    for pattern, new in _REWRITES:
        text = re.sub(pattern, new, text)
    return text


def _port():
    port = read_record().get("port")
    return (str(port), "codex install record") if isinstance(port, int) and port else (None, "")


def _options():
    preset, keepalive = read_proxy_options()
    options = {"preset": preset, "cache_strategy": "30-min-ping" if keepalive else "none"}
    return options, {key: str(proxy_config()) for key in options}


core._resolve_port = _port
core._configured_options = _options


class CodexReport(core.Report):
    def fact(self, key, value):
        if key in ("session_base_url", "session_routed_through_us", "idle_exit"):
            return
        if key == "upstream" and value == "(anthropic)":
            value = settings.redact_url(read_record().get("upstream") or "") or "(openai)"
        super().fact(key, value)

    def emit(self, as_json):
        self.facts["session_routed"] = os.environ.get("CONTEXT_GURU_ROUTED") == "1"
        self.facts = {k: codexify(v) for k, v in self.facts.items()}
        for finding in self.findings:
            for attr in ("title", "evidence", "fix", "basis"):
                setattr(finding, attr, codexify(getattr(finding, attr)))
        super().emit(as_json)


core.Report = CodexReport
os.environ.pop("ANTHROPIC_BASE_URL", None)  # a Claude setting must not read as a Codex routing fault

if __name__ == "__main__":
    try:
        sys.exit(core.main())
    except KeyboardInterrupt:
        sys.exit(130)
