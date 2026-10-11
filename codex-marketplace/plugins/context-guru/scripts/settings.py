"""The five names insights_core.py takes from the Claude plugin's settings.py, for Codex.

insights_core.py is a verbatim copy of context-guru-plugin/scripts/insights.py (a drift test
enforces it) because this plugin ships as a sparse checkout that cannot import from the Claude
plugin. Everything host-specific is supplied here or overridden in insights.py.
"""

import os

DEFAULT_PRESET = "off"
DEFAULT_STRATEGY = "30-min-ping"


def state_dir():
    base = os.environ.get("XDG_STATE_HOME") or os.path.join(os.path.expanduser("~"), ".local/state")
    return os.path.join(base, "context-guru-codex")


def redact_url(value):
    if not isinstance(value, str) or "://" not in value:
        return value
    scheme, _, rest = value.partition("://")
    host, slash, tail = rest.partition("/")
    if "@" in host:
        host = "<credentials not shown>@" + host.rsplit("@", 1)[1]
    return f"{scheme}://{host}{slash}{tail}".split("?", 1)[0]
