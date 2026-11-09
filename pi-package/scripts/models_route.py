#!/usr/bin/env python3
"""Install or restore context-guru's route in Pi's models.json.

Pi reads `providers.<name>.baseUrl` from models.json to send a BUILT-IN provider somewhere else, while
the provider keeps its own models and authentication. This file sets that one key per routed provider
and nothing else. JSON has no comments, so ownership lives in a routing record next to the proxy state:
the record says which keys we set, what each held before, and whether the file existed. Restore puts
back only a key that still holds our URL; a key the user changed since is left as it is.
"""

import json
import os
from pathlib import Path
import sys

# Provider name in Pi -> the proxy path that serves it. Pi's OpenAI SDK appends `/responses` to the
# base URL, so the OpenAI path ends in `/v1`, as the proxy serves `POST /openai/v1/responses`.
ROUTES = {"anthropic": "/anthropic", "openai": "/openai/v1"}


def base_url(port, provider):
    return f"http://127.0.0.1:{port}{ROUTES[provider]}"


def _atomic(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".context-guru.tmp")
    temporary.write_text(text)
    temporary.chmod(0o600)
    os.replace(temporary, path)


def _load(path):
    """The models.json document, {} when absent. Raises on a file that is not a JSON object, so a
    broken file is reported rather than overwritten."""
    if not path.exists():
        return {}
    text = path.read_text()
    if not text.strip():
        return {}
    document = json.loads(text)
    if not isinstance(document, dict):
        raise ValueError(f"{path} is not a JSON object")
    providers = document.get("providers", {})
    if not isinstance(providers, dict):
        raise ValueError(f"{path}: providers is not a JSON object")
    return document


def _dump(document):
    return json.dumps(document, indent=2) + "\n"


def _same(a, b):
    """URLs compare without trailing slashes, as Pi joins paths onto them."""
    return a is not None and b is not None and a.rstrip("/") == b.rstrip("/")


def current(models_path, provider):
    """The baseUrl models.json sets for this provider, or None."""
    try:
        entry = _load(Path(models_path)).get("providers", {}).get(provider)
    except (OSError, ValueError):
        return None
    if isinstance(entry, dict) and isinstance(entry.get("baseUrl"), str):
        return entry["baseUrl"]
    return None


def is_routed(models_path, port, provider="anthropic"):
    return bool(port) and _same(current(models_path, provider), base_url(port, provider))


def _ours(url, port, provider, ours):
    """True for the URL this port gives the provider, or one of the URLs an earlier setup wrote
    (`ours`, from the install record): a route on an old port is still ours to move."""
    return _same(url, base_url(port, provider)) or any(_same(url, mine) for mine in ours)


def conflicts(models_path, port, providers, ours=()):
    """{provider: url} for every provider whose baseUrl is already set to something that is not ours.
    Setup refuses on any: that URL may be a company gateway, and replacing it breaks its auth."""
    ours = tuple(ours)
    found = {}
    for provider in providers:
        url = current(models_path, provider)
        if url is not None and not _ours(url, port, provider, ours):
            found[provider] = url
    return found


def install(models_path, state_path, port, providers, ours=()):
    """Set each provider's baseUrl to the proxy on `port`. A provider already in the routing record
    keeps its first `previous`, so a re-run (on the same or a new port) never records our own URL as
    the user's value. `ours` names URLs an earlier setup wrote, for the case the record is gone."""
    ours = tuple(ours)
    models_path, state_path = Path(models_path), Path(state_path)
    existed = models_path.exists()
    document = _load(models_path)
    record = {}
    if state_path.exists():
        record = json.loads(state_path.read_text())
    keys = record.get("keys", {})
    table = document.setdefault("providers", {})
    for provider in providers:
        entry = table.get(provider)
        if entry is None:
            entry = table[provider] = {}
            created_entry = True
        elif not isinstance(entry, dict):
            raise ValueError(f"{models_path}: providers.{provider} is not a JSON object")
        else:
            created_entry = False
        if provider not in keys:
            previous = entry.get("baseUrl")
            if previous is not None and _ours(previous, port, provider, ours):
                previous = None  # our own URL is never the user's value to put back
            keys[provider] = {"previous": previous, "created_entry": created_entry}
        entry["baseUrl"] = base_url(port, provider)
    if not state_path.exists():
        # For a person only: restore() works from the routing record, never from this copy.
        backup = state_path.with_name("models.json.before-context-guru")
        if existed:
            _atomic(backup, models_path.read_text())
    record.update(models=str(models_path), existed=record.get("existed", existed), keys=keys,
                  port=int(port))
    _atomic(state_path, json.dumps(record, indent=2) + "\n")
    _atomic(models_path, _dump(document))


def restore(state_path):
    """Undo install. Returns restored | routing_already_changed | no_record."""
    state_path = Path(state_path)
    if not state_path.exists():
        return "no_record"
    record = json.loads(state_path.read_text())
    models_path = Path(record["models"])
    port = record.get("port")
    try:
        document = _load(models_path)
    except (OSError, ValueError):
        return "models_unreadable"
    table = document.get("providers", {})
    restored, changed = 0, 0
    for provider, saved in record.get("keys", {}).items():
        entry = table.get(provider)
        if not isinstance(entry, dict) or not _same(entry.get("baseUrl"), base_url(port, provider)):
            changed += 1
            continue
        if saved.get("previous") is None:
            del entry["baseUrl"]
        else:
            entry["baseUrl"] = saved["previous"]
        if not entry and saved.get("created_entry"):
            del table[provider]
        restored += 1
    if "providers" in document and not document["providers"]:
        del document["providers"]
    if document or record.get("existed"):
        _atomic(models_path, _dump(document))
    else:
        models_path.unlink(missing_ok=True)
    state_path.unlink()
    return "restored" if restored and not changed else "routing_already_changed"


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] != "restore":
        raise SystemExit("usage: models_route.py restore ROUTING_STATE")
    print("models_restore=" + restore(sys.argv[2]))
