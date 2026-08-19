# The settings form (field descriptors)

The dashboard's **Settings** page edits a tenant's configuration as *fields*, not as YAML
text. This page documents the mechanism behind that — where the fields come from, what the
server accepts, and what a component author has to do when they add a knob.

## Why it is not hand-written

The first version of the fields form was hand-written twice: once as a Go struct, once as a
table of controls in JavaScript. It reached **18 keys of about a hundred** and one component
of fifteen; the other fourteen were hand-edit-only. Worse, the two copies had already
drifted:

- the browser carried its own table of defaults, duplicating the server's;
- the strategy list was missing `deterministic`, a value the engine accepts — so a stored
  `strategy: deterministic` was not matched, the form fell back to `code`, and the next save
  **wrote `code` over it**, silently converting an LLM-free configuration into one that makes
  model calls;
- and per-component blocks were unmarshalled *non-strictly*, so `min_tokns: 5000` on any
  component parsed fine, changed nothing, and reported nothing.

Nothing in the tree could notice any of that. So the fields are **declared beside the config
struct that reads them**, and every layer — the form, the API, validation, the delete list —
is a walk over those declarations.

## The descriptor

```go
// components/registry.go
type Field struct {
    Key     string   // DOTTED path inside the block: "cold_cache.min_tokens", "model.source"
    Type    string   // bool | int | float | enum | string | strings
    Default any      // what an ABSENT key means (nil = the type's zero)
    Options []string // the permitted set, for enum
    Hint    string   // one line of prose, from the doc comment already on the struct
    Secret  bool     // a credential: write-only, never echoed back
    Min     int      // smallest accepted number — see "0 is not one thing" below
}
```

Each component declares its list from the same `init()` that registers it, next to the
config struct it describes:

```go
func init() {
    components.RegisterFields("dedup", dedupConfig{}, []components.Field{
        {Key: "min_tokens", Type: components.FieldInt, Default: 100, Min: 1,
            Hint: "Only replace a repeated tool output above this many tokens…"},
        markerModeField(),
    })
}
```

The zero-value config struct is passed in on purpose: the anti-drift test reflects over its
`yaml` tags and fails when the declared keys and the struct's keys disagree in **either**
direction. Nested blocks (`model:`, `trigger:`, `cold_cache:`) need no nesting in the
descriptor — they are dotted paths, and `components.TriggerFields("trigger")` /
`modelFields("model")` declare the shared ones once.

Descriptors are **form metadata only**. No constructor reads them, and no component behaves
differently because its fields are declared.

## Coverage

97 keys across 14 components today (99 across 15 with `-tags cg_skeleton`), plus `pipeline`
and `mode`. What is deliberately *not* on the form: `preset:`, `store.*` and `observe.*` —
deployment-shaped keys that are not per-component and are set on the account page, which
still takes a whole document.

## Two different notions of "default"

| | Answers | Lives in |
|---|---|---|
| `Field.Default` | What does an **absent key** mean? (`min_tokens` 300, cold cache off) | the descriptor, beside the struct |
| the recommended prefill | What should the page **offer** somebody switching this on for the first time? (cold sweep on, hot path off, a cheap model, caps 2/20) | `config.RecommendedComponents()`, served at `/api/options` |

They are separate on purpose. Conflating them is how a form ends up writing its own opinion
over an operator's deliberate value.

## 0 is not one thing

`Field.Min` carries real semantics that used to live in two hand-written maps of field names:

- `Min: 0` — a **cap**. `llm_max_per_session: 0` means *unlimited* to the component, so 0 is
  a legitimate choice and only a negative number is refused.
- `Min: 1` — a **size threshold**. `min_tokens: 0` is not a setting, it is a removed brake:
  every candidate output clears it, and under `fire_on: size` that is the only content gate
  there is.

## The API contract

`GET /api/options` serves the descriptors next to the component names:

```json
{
  "components": ["agentdiet", "cacheinject", "cachesplit", "…"],
  "component_fields": {
    "extract_llm": [
      {"key": "per_output", "type": "bool", "default": true, "hint": "The HOT-PATH pass…"},
      {"key": "strategy", "type": "enum", "default": "code",
       "options": ["auto", "code", "single", "rlm", "deterministic"], "hint": "…"},
      {"key": "min_tokens", "type": "int", "default": 300, "min": 1, "hint": "…"},
      {"key": "cold_cache.min_tokens", "type": "int", "default": 1000, "min": 1, "hint": "…"},
      {"key": "model.api_key", "type": "string", "secret": true, "hint": "…"}
    ],
    "cachesplit": []
  },
  "recommended": {"extract_llm": {"per_output": false, "cold_cache.enabled": true, "…": "…"}}
}
```

Fields appear in declaration order. `default`, `options`, `min` and `secret` are omitted when
they are the zero value; an omitted `default` means "the type's zero". Every name in
`components` has an entry in `component_fields`, empty for a component that takes no
configuration.

The form itself is posted to `PUT /api/me` (or `PATCH /api/tenants/{id}`) as:

```json
{"config": {
  "pipeline": ["format", "dedup", "extract_llm", "cachesplit"],
  "mode": "sync",
  "components": {"extract_llm": {"per_output": true, "cold_cache.min_tokens": 800}}
}}
```

and read back from the same shape on `tenant.effective_config`. The keys inside a component
are the **dotted YAML keys**, so the page cannot post a field name the server does not read.

## What the server does with it

- **Absent means default.** Only keys the document really states are on the form, and a key
  the form leaves out is *deleted* from the block, handing the decision back to the
  component's own default. A form that prefilled defaults could not tell a stored
  `llm_max_per_session: 0` from an unset key, and wrote 20 over a deliberate "no cap".
- **Enablement is pipeline membership.** A block is configuration, not enablement. A bare
  `preset: general` has `extract_llm` in the pipeline with no block at all.
- **A component the form does not send is not touched.** A save that mentions `extract` does
  not disturb the `collapse` block a preset tuned.
- **Switching off clears exactly the declared keys**, leaf by leaf, never a parent block.
- **Unknown keys survive**: the document is round-tripped through `map[string]any`. Comments
  and key order do **not** survive — accepted, because the page is fields now.
- **Secrets are write-only**: `model.api_key` is never read into the form, so "absent" cannot
  mean "cleared" — an explicit empty string clears it, anything else leaves it alone.
- **One coupling, named.** `extract_llm` with `per_output: false` and the cold sweep off is a
  combination its own constructor refuses ("nothing to do"), so the form takes it out of the
  pipeline instead. `fire_on` is *never* derived from `per_output`: deriving it once meant
  ticking a checkbox quietly turned the spending brakes advisory.

## Strict per-component blocks

`config.LoadBytes` has always rejected unknown keys at the top level. Since this change each
component's block is decoded with `components.Decode`, which is `KnownFields(true)` — a typo
is an error at load time rather than silence. That is what makes a descriptor-driven form
trustworthy: the declared keys and the accepted keys are provably the same set. It also means
a document with a mistyped component key **loads but does not build**, and the settings page
reports it and refuses to save over it, exactly as for a top-level typo.

`components.cachesplit` takes no configuration and now says so: its constructor used to
receive the raw block and discard it.

## The rules the tests pin

Each of these came from a production failure, and each has a test named after it in
`config/form_test.go`.

| | Rule |
|---|---|
| **R1** | `ParseForm` is best-effort — strict load, else a non-strict fallback that records `ParseError`. Saving from a guess is refused server-side. |
| **R2** | Enablement comes from pipeline membership, never block presence. |
| **R3** | Only stated keys reach the form, so a meaningful zero is displayed and preserved. |
| **R4** | A key written by merging into an existing block is never deleted as its parent. |
| **R5** | Keys the form does not know about survive the round trip; comments and order do not. |
| **R7** | The delete list is *derived* from the declared keys, so the enable and disable paths cannot disagree. The hand-written list named the whole `cold_cache` block, so disabling deleted `min_idle_seconds` and `max_calls` that enabling had carefully preserved. |
| **R8** | Enum options are read from the same constant the engine parses (`extract.Modes`), never retyped. |
| **R11** | The descriptor carries the component default; the recommended prefill is a separate policy layer. |

## Adding a knob

1. Add the field to the component's config struct with its `yaml` tag and a doc comment.
2. Add one `components.Field` to that component's `RegisterFields` call, with the hint taken
   from the comment.

That is the whole change. Skip step 2 and
`TestEveryComponentDeclaresExactlyItsConfigurableKeys` names the key you forgot; get the type
or the enum options wrong and it names that too. The generalized round-trip test then
perturbs your field off its stored value, saves, and asserts it reached the document, that
**nothing else in the document moved**, that re-parsing reads it back, and that the result
still builds.

## The page that draws it

`dash/ui/app.js` renders the whole form from the descriptors and knows no field names. One
function does it:

```js
renderComponentFields(name, fields, values, disabled, { recommended, redraw, opts })
```

It returns one `<details>` per component — `data-testid="cfg-<name>"` — and inside it one
control per declared field, in declaration order, keyed `x-<component>-<key with dots as
dashes>`. The type picks the control:

| `type` | control | posted as |
|---|---|---|
| `bool` | checkbox | `true` / `false` |
| `int`, `float` | `<input type=number min=…>` | number |
| `enum` | `<select>`, the declared options in order plus one empty "— default (x) —" | string |
| `string`, `strings` | `<input type=text>` (`password` when `secret`) | string / list |
| anything else | no control, and a line saying the server is newer than this page | nothing |

A component with no declared fields draws no section at all, and `hint` is rendered verbatim
under every control: it is the only thing on the page that explains why a configured
component did nothing.

The replaced version hand-wrote sixteen control calls for one component, with its own copy of
the default table (`XLLM_DEFAULTS`) and its own enum lists — which is where R8 came from.
`TestSettingsFormSpeaksTheSameFieldNamesAsTheServer` is now INVERTED for that reason: it walks
every declared field and fails if `app.js` names one, with a four-entry allowlist that each
say why. Mentioning a name is no longer evidence of anything; not mentioning one is.

### What the page must get right

- **Absent is not zero, in the browser too.** `cfgState` holds only the keys the document
  states plus the ones edited on the page. A number or text field with no stated value renders
  EMPTY with its default as the `placeholder`; an enum sits on the empty choice. Clearing a
  box deletes the key, so the component's default takes over again. A stated `0` renders as
  `0` — that is the R3 zero, and blanking it would be the bug from the other direction.
  A checkbox cannot draw "absent", so it shows the default's own value and writes nothing
  until it is changed; every hint states what an unset key means, in prose, so the note cannot
  go stale as the control changes.
- **`min` is carried onto the input**, and a value below it is refused in the browser with the
  reason — `min: 1` says 0 removes the brake, `min: 0` says 0 is allowed and means unlimited.
  The refusal never reaches `cfgState`, so the server's 400 is a backstop, not the first the
  user hears of it.
- **Secrets never touch the DOM.** `readBlocks` does not send one, and the page drops declared
  secret keys while seeding `cfgState` anyway. The input is `type=password`, always empty,
  placeholder "stored credential kept — type to replace it", and an empty box is omitted from
  the POST. There is no *clear the stored key* control: an explicit `""` is what the server
  reads as a deletion, and offering that is one keystroke away from a form that deletes a
  credential every time somebody saves. Removing a stored key is a manager edit on the account
  page.
- **Enablement stays the component grid.** A component that is not ticked is drawn read-only
  with the reason, and it is still SENT on save — that is what makes the server clear its
  keys. A component the page never sends is untouched.
- **`recommended` is a button**, `data-testid="cfg-rec-<name>"`, and only for components the
  server recommends anything for. It fills the fields in and saves nothing.
- **`parse_error` disables every control** and the save omits `config` entirely; the server's
  409 is the second line of defence.
- **The coupling is refused, not posted.** Switching off the second of `per_output` /
  `cold_cache.enabled` switches the other back on and says why — the combination the
  constructor refuses cannot leave the page.

The page is checked by driving it under jsdom against the real descriptors: all 97 fields
render, all 25 enums offer exactly their declared options, all 53 numeric inputs carry their
declared `min`, an unstated key is not posted, a `min: 1` field refuses 0, a secret planted in
the payload appears in neither the DOM nor the request body, and an edited value survives the
save and the reload that follows it.
