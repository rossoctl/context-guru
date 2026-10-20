# e2e/gen/inventory.py
"""Manifest -> an installed inventory: plugin skills, marketplace skills, and MCP servers.

Bundled skills and built-in tools are deliberately NOT written: they exist only in the prompt."""
from __future__ import annotations

import argparse
import json
import pathlib
import sys

from e2e.gen import manifest as mf


def _skill(dir_: pathlib.Path, name: str) -> None:
    dir_.mkdir(parents=True, exist_ok=True)
    (dir_ / "SKILL.md").write_text(f"---\nname: {name}\ndescription: e2e fixture skill {name}\n---\n")


def write(m: mf.Manifest, home: str) -> dict:
    h = pathlib.Path(home)
    n_plug = n_mkt = 0
    servers = {}
    for c in m.capabilities:
        if not c.on_disk:
            continue
        if c.kind == "plugin_skill":
            plugin, name = c.name.split(":", 1)
            _skill(h / ".claude/plugins/cache/e2e-market" / plugin / "1.0.0/skills" / name, name)
            n_plug += 1
        elif c.kind == "marketplace_skill":
            _skill(h / ".claude/plugins/marketplaces" / c.owner / "skills" / c.name, c.name)
            n_mkt += 1
        elif c.kind == "mcp_tool":
            servers[c.owner] = {"type": "stdio", "command": "/bin/true", "args": []}
    cfg_path = h / ".claude.json"
    cfg = json.loads(cfg_path.read_text()) if cfg_path.exists() else {}
    cfg["mcpServers"] = servers
    cfg_path.write_text(json.dumps(cfg, indent=1, sort_keys=True))
    return {"plugin_skills": n_plug, "marketplace_skills": n_mkt, "mcp_servers": len(servers)}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tier", required=True)
    ap.add_argument("--home", required=True)
    a = ap.parse_args(argv)
    print(json.dumps(write(mf.build(a.tier, home=a.home), a.home)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
