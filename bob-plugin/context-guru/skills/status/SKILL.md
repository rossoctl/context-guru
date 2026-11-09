---
name: context-guru-status
description: Check if IBM Bob goes through context-guru - the startup-file block, the proxy health and recent Bob traffic. Use when asked about context-guru status, health or troubleshooting.
---

# Check context-guru for Bob

Run this command. It only reads:

```sh
python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" status
```

Report these facts in plain words:

- `routed=`: the startup-file block is there with the right port.
- `proxy_up=`: the proxy answers on `/healthz`.
- `hooks_installed=`: our hooks are in Bob's settings.
- `last_bob_request_age_s=`: how long ago the newest Bob request reached the proxy.
- `result=ok`: all of these are good, and Bob traffic came in the last 24 hours.
- `result=no_bob_traffic`: the setup is there, but Bob does not use it. Give `hint=`. Bob may have
  started from a terminal or after a self-update. Quit Bob and open it from the Dock.
- `result=not_ready`: name the fact that is false.
- `result=not_installed`: tell the user to run the context-guru-setup skill.

Tell the user they can open the dashboard at `dashboard=`.

Always add the `note=`: after setup, and after uninstall, the user logs in to Bob once.

Token figures are true. Dollar figures for Bob are not yet correct: Bob bills in Bobcoins, and the
proxy does not price them. Do not quote dollar savings for Bob.
