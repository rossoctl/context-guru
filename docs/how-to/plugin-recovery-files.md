# The plugin's recovery files

Wherever `/context-guru:install` writes to a Claude Code settings file — `.claude/settings.local.json`,
`.claude/settings.json`, or `~/.claude/settings.json` for a `--global` install — it also creates a
folder beside that file:

```
.claude/
  settings.local.json
  context-guru-settings-json/
    README.md
    settings.local.pre-install.json
    settings.local.created-by-us
    settings.local.pre-reset-20260924-093000.json
    settings.local.context-guru-backup-20260924-093512-004821.json
```

Every file that holds a copy of your settings ends in `.json` (an editor or `file` treats it as
what it is); `settings.local.` is your file's own name with `.json` removed, so the names still
tell you which settings file each one belongs to.

This is what `/context-guru:uninstall` and the `context-guru-reset` escape hatch restore from. It
used to be a shared, hashed set of copies under `~/.local/state/context-guru/originals/` and
`.../prereset/` — nobody who needed to recover something would think to look there, and worse, that
design was a single ledger nothing ever removed an entry from, so a scope touched once (a
`--scope user` test, say) and cleanly uninstalled since would still get swept up and reverted by a
later, unrelated recovery run. Putting a folder beside each settings file fixes both: it's where you'd
actually look, and there's no shared list left for one scope's history to leak into another's.

## What's in the folder

- **`README.md`** — a short pointer back to this page, written once.
- **`<name>.pre-install.json`** — a one-time copy of the settings file from *before* context-guru's
  very first edit to it. Never overwritten, no matter how many times you install, uninstall or
  reconfigure afterwards. This is what a restore reverts to.
- **`<name>.created-by-us`** — an empty marker, not JSON content, so it is the one file here that
  does not end in `.json`. Its presence means context-guru created that file from nothing (there
  was no `settings.local.json` at all before the install), so undoing it means deleting the file,
  not restoring a copy — there's nothing to restore to.
- **`<name>.pre-reset-<timestamp>.json`** — only present if `context-guru-reset` has run here. A
  safety copy of the file's *routed* content, taken right before the hatch restores or deletes it,
  so running the hatch is itself undoable. The newest 10 are kept per file; older ones are pruned
  automatically.
- **`<name>.context-guru-backup-<timestamp>.json`** — a copy of the file taken immediately before
  *any* edit `/context-guru:install`'s scripts make to it (routing, statusline, cache strategy, and
  so on) — not specific to install/uninstall the way the other three are. The newest 10 are kept
  per file while the plugin is actively editing it; a clean `/context-guru:uninstall` **or** a
  successful `context-guru-reset` run deletes all of them for that file, since at that point
  `.pre-install.json` already answers the only question left (see [Cleaning up](#cleaning-up)).

Exactly one of `.pre-install.json` or `.created-by-us` will exist for a given settings file (or
neither, if it was already routed before this mechanism existed — the hatch reports that case
honestly rather than guessing).

## Is it safe to have in my repo?

A `context-guru-settings-json/` folder can, in principle, hold a credential — the same way
`settings.local.json` itself already can, since nothing stops one living in its `env` block.
`/context-guru:install` closes the obvious risk that follows from that (a full copy of a settings
file sitting in a project's working tree, uncovered by any `.gitignore`, one `git add -A` away from
a commit) automatically and deterministically: it checks whether the folder is already
`.gitignore`'d, and if it isn't, adds the one line that covers it — no question asked, because the
check is entirely mechanical and the fix is fully reversible. If your project has no `.gitignore` (or
isn't a git repository at all), nothing is written; there's nothing to protect against.

You can verify this yourself:

```bash
git check-ignore -v .claude/context-guru-settings-json
```

## Cleaning up

A clean `/context-guru:uninstall`, or a successful `context-guru-reset` run, deletes every
`.context-guru-backup-*.json` for that file on its own — once context-guru's keys are gone (either
because the normal uninstall removed them, or because the hatch just restored the file to its
`.pre-install.json` state), those rolling per-edit checkpoints answer a question nobody has anymore,
and a growing pile of them next to `.pre-install.json` is just something to explain, not something
to recover from.

Everything else it leaves alone, on the theory that "I might still want the original back" outlives
"I just undid the install." If context-guru *created* the file from nothing, uninstalling (through
either path) deletes the file **and** removes the whole folder along with it — there is no
`.pre-install.json` in that case (see `.created-by-us` above), so nothing in it is useful once the
file itself is gone.

The folder is otherwise safe to delete by hand once you're sure you no longer need to recover
anything through it. If you delete it, you also give up the `context-guru-reset` escape hatch for
that file: with no `.pre-install.json` copy, there is nothing to restore from and the hatch will say
so honestly rather than pretending it can help.

See [install-plugin.md](install-plugin.md#troubleshooting) for the escape hatch itself.
