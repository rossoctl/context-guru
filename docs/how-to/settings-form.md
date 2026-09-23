# The settings form (field descriptors)

The dashboard's **Settings** page edits a tenant's configuration as *fields*, not as YAML
text. Every field on the page comes from the component it configures, so the form always
matches what the running server actually accepts — you can't fill in a key that gets
silently ignored, and you can't miss one that exists.

## What's on the form

Fields are grouped by component (`dedup`, `extract_llm`, `cachesplit`, and so on), plus the
top-level `pipeline` (which components run, and in what order) and `mode`. Within a
component, each field is a dotted key such as `min_request_tokens` or `model.api_key`, and
every field has a one-line hint explaining what it does and, for a component that is
configured but does nothing, *why*.

Not everything lives here: `preset`, `store.*` and `observe.*` are deployment-shaped
settings, not per-component knobs, and are configured on the account page instead.

The `pipeline` list controls both which components are active and the order they run in —
components process the request in the order they're listed, so reordering the list changes
behavior even if no individual field changes.

## Making a change

1. Find the component's section (it's collapsed by default) and expand it.
2. Edit the field. For a checkbox, tick or untick it. For a number, text, or enum field,
   type a value or pick from the dropdown — picking the empty enum option, or clearing a
   text/number field, removes your override and hands the setting back to the component's
   default.
3. Save. The page shows the value you just set on reload, so you can confirm it took effect.

If a component isn't in the `pipeline` list yet, add it there first — a field you configure
on a component that isn't in the pipeline is saved but has no effect until the component is
enabled.

## Field types

| Type | Control | What to enter |
|---|---|---|
| `bool` | checkbox | on/off |
| `int`, `float` | number input | a number, optionally bounded by a minimum (see below) |
| `enum` | dropdown | one of a fixed list of choices, plus an empty "— default (x) —" option |
| `string`, `strings` | text input | free text or a comma-separated list |
| secret (a credential) | password input | write-only — see "Secrets" below |

A component with no configurable fields shows no section at all.

A few examples of what you'll actually see:

- `extract_llm.strategy` — an enum: `auto`, `code`, `single`, `rlm`, or `deterministic`.
  Defaults to `code` when left unset.
- `extract_llm.min_tokens` — an int with a minimum of 1. Defaults to 300 if left blank.
- `dedup.min_tokens` — an int with a minimum of 1. Only replaces a repeated tool output
  above this many tokens; defaults to 100.
- `model.api_key` — a secret. Write-only, as described below.
- `trigger.min_request_tokens` — an int shared across every component with a `trigger`
  block.

## Defaults vs. recommended values

There are two different "defaults" on this page, and they answer different questions:

- **The field's default** is what the component does when a key is left blank — e.g. if
  `min_tokens` is unset, the component uses its own built-in value. Leaving a field empty is
  not the same as typing `0`; it means "let the component decide."
- **The recommended button** fills in a starter set of values for someone turning a
  component on for the first time (e.g. a cheap model, sane caps). It only appears for
  components that have a recommendation, fills the fields in, and does not save anything by
  itself — you still have to save.

## Minimum values

Some numeric fields have a minimum, shown next to the input and enforced before you can
save. A minimum of 0 and a minimum of 1 mean different things:

- **Minimum 0** means the field is a cap, and 0 itself is a valid choice meaning "unlimited"
  (e.g. `llm_max_per_session: 0`).
- **Minimum 1** means the field is a size threshold, and 0 is not a real setting — it would
  mean "no threshold at all," which the server refuses.

If you enter a value below the minimum, the page refuses to save and shows why.

## Behavior worth knowing before you save

- **A checkbox can't show "unset."** Unlike a number or text field, a checkbox always shows
  the component's default value and doesn't count as a change until you actually flip it.
  Read the hint text if you're unsure what an unticked box currently means.
- **A blank field is not zero.** An empty number or text field shows the component's default
  as greyed-out placeholder text, not as its actual value. If a field already holds a real
  `0`, it displays as `0`, not blank — clearing it removes the setting entirely and hands
  control back to the component's default.
- **Turning a component off still sends it on save**, and that's what causes the server to
  clear its configured keys. A component you never touch is left completely alone.
- **Enabling/disabling a component is done via the pipeline list**, not by adding or removing
  its configuration block — a component with an empty block can still be in the pipeline and
  running with all defaults.
- **Secrets are never shown.** A credential field (e.g. an API key) is always blank on load,
  with a placeholder saying a credential is stored. Type a new value to replace it; leave it
  blank to keep the existing one unchanged. There is no button to clear a stored credential
  from this page — that's done on the account page, since an accidental blank save would
  otherwise wipe it.
- **A parse error disables the whole form.** If your tenant's stored configuration can't be
  parsed strictly, every control is disabled until it's fixed, and saving from that state is
  rejected.
- **A renamed or removed key comes back as an error naming its replacement**, not a generic
  failure — for example, an old `per_output` or `cold_cache.*` key now belongs to a separate
  `extract_llm_sweep` component, and trying to set the old key tells you so.
- **Unknown keys in your stored config survive a save**, but comments and key order in the
  underlying document do not, since the page always round-trips through fields now.
