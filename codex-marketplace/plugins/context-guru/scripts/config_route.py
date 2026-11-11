#!/usr/bin/env python3
"""Surgically install or restore context-guru's default Codex routing."""

import json
import os
from pathlib import Path
import re
import sys

BEGIN = "# context-guru: begin managed provider"
END = "# context-guru: end managed provider"
MODEL = re.compile(r'^(\s*model_provider\s*=\s*).*$')


def _atomic(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".context-guru.tmp")
    temporary.write_text(text)
    temporary.chmod(0o600)
    os.replace(temporary, path)


def _without_block(lines):
    result, inside = [], False
    for line in lines:
        if line.rstrip("\n") == BEGIN:
            if inside:
                raise RuntimeError("nested context-guru provider markers")
            inside = True
            continue
        if line.rstrip("\n") == END:
            if not inside:
                raise RuntimeError("unmatched context-guru provider marker")
            inside = False
            continue
        if not inside:
            result.append(line)
    if inside:
        raise RuntimeError("unterminated context-guru provider marker")
    return result


def install(config_path, state_path, port, provider):
    config_path, state_path = Path(config_path), Path(state_path)
    existed = config_path.exists()
    original = config_path.read_text() if existed else ""
    lines = _without_block(original.splitlines(keepends=True))
    original_line = None
    table_at = next((i for i, line in enumerate(lines) if line.lstrip().startswith("[")), len(lines))
    for index, line in enumerate(lines[:table_at]):
        if MODEL.match(line.rstrip("\n")):
            original_line = line.rstrip("\n")
            lines[index] = 'model_provider = "context-guru"\n'
            break
    else:
        lines.insert(table_at, 'model_provider = "context-guru"\n')

    auth = provider.get("experimental_bearer_token")
    if lines and not lines[-1].endswith("\n"):
        lines[-1] += "\n"
    block = [BEGIN + "\n",
             "[model_providers.context-guru]\n", 'name = "context-guru (local)"\n',
             f'base_url = "http://127.0.0.1:{port}/openai/v1"\n', 'wire_api = "responses"\n',
             f'requires_openai_auth = {str(bool(provider.get("requires_openai_auth", True))).lower()}\n']
    if auth:
        block.append(f"experimental_bearer_token = {json.dumps(auth)}\n")
    block.append(END + "\n")
    rendered = "".join(lines + block)
    state_path.parent.mkdir(parents=True, exist_ok=True)
    if not state_path.exists():
        backup = state_path.with_name("config.toml.before-context-guru")
        _atomic(backup, original)
        _atomic(state_path, json.dumps({"config": str(config_path), "existed": existed,
                                       "original_model_provider_line": original_line}) + "\n")
    _atomic(config_path, rendered)


def restore(state_path):
    state_path = Path(state_path)
    if not state_path.exists():
        return "no_record"
    state = json.loads(state_path.read_text())
    config_path = Path(state["config"])
    lines = _without_block(config_path.read_text().splitlines(keepends=True)) if config_path.exists() else []
    table_at = next((i for i, line in enumerate(lines) if line.lstrip().startswith("[")), len(lines))
    restored = False
    for index, line in enumerate(lines[:table_at]):
        match = MODEL.match(line.rstrip("\n"))
        if match and line.split("=", 1)[1].strip() == '"context-guru"':
            original = state.get("original_model_provider_line")
            if original is None:
                del lines[index]
            else:
                lines[index] = original + "\n"
            restored = True
            break
    rendered = "".join(lines).lstrip("\n")
    if rendered.strip():
        _atomic(config_path, rendered)
    elif state.get("existed"):
        _atomic(config_path, rendered)
    else:
        config_path.unlink(missing_ok=True)
    state_path.unlink()
    return "restored" if restored else "routing_already_changed"


def is_routed(config_path):
    path = Path(config_path)
    if not path.exists():
        return False
    lines = path.read_text().splitlines()
    table_at = next((i for i, line in enumerate(lines) if line.lstrip().startswith("[")), len(lines))
    return any(MODEL.match(line) and line.split("=", 1)[1].strip() == '"context-guru"'
               for line in lines[:table_at]) and BEGIN in lines and END in lines


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] != "restore":
        raise SystemExit("usage: config_route.py restore ROUTING_STATE")
    print("config_restore=" + restore(sys.argv[2]))
