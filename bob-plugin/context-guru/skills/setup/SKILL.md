---
name: context-guru-setup
description: Set up context-guru for IBM Bob on this Mac. Use when asked to install, enable, set up or start context-guru for Bob.
---

# Set up context-guru for Bob

1. Run this command. It writes nothing:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" setup --plan
   ```

2. Show the user what it found: `rc_file=`, `settings=`, `service_file=`, `preset=`, `preset_effects=` and `note=`. If it printed
   `rc_advice=`, say that setup cannot edit the startup file and will print a block to add by hand.
3. Tell the user what the `preset=` does, from `preset_effects=`. Then ask the `consent_question=` as one yes/no question. A missing or unclear answer is no. Stop there
   on a no.
4. Only after a clear yes, run:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" setup --i-consent-to-traffic-interception
   ```

5. Report the result in plain words:
   - `result=installed`: it works. `rc_probe=ok` means a login shell started the way Bob starts it
     sets the address.
   - `result=installed_not_verified`: the probe did not see the address. Show `detail=`.
   - `result=installed_route_by_hand`: show the printed block. The user adds it to the startup file
     that the login shell reads.
   - `result=refused` or `result=*_failed`: show `reason=` and `detail=`. Setup changed nothing, or
     undid what it changed.
   - `statusbar=installed`: Bob shows a `CG` item in its status bar after the restart.
     `statusbar=skipped`: say why from `statusbar_reason=`. For `no_vsix`, tell the user that
     the status bar needs `make bob-statusbar` in the context-guru repo before setup, until the
     install script ships it. Then copy the plugin folder again and run setup again. The rest of
     setup still worked.
   - `binary_skipped=`: setup did not use that proxy (it is too old: v0.7.1 is needed to list cache
     strategies, and v0.7.2 for `bob-ultrahigh`) and downloaded the latest release. Say so, and
     say why from the line. If the download failed too (`result=binary_install_failed`), say that
     setup needs the network once, or a newer proxy on PATH.
6. Always end with the two steps setup printed: **quit and reopen Bob from the Dock**, then **log in
   to Bob once**. Bob keeps its login per backend address, so the new address needs one new login.
   This is expected and is not a fault.

Bob asks you to approve each command. Setup writes files and starts a service, so the user must
approve it each time. Do not add an allow rule for it.
