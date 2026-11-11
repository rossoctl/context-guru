package reformat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/skills"
)

func init() { components.Register("toolfilter", newToolfilter) }

// toolfilterConfig is the account's opt-in list, and it is the WHOLE of the component's
// authority to REMOVE: nothing is ever removed that is not named here. There is deliberately no
// "remove what looked unused" mode — the inventory can only ever show that a declaration was not
// used in the sessions it captured, and an unused tool is not the same thing as an unwanted one.
// `defer: auto` is not removal: a deferred tool stays declared and the model can load it.
type toolfilterConfig struct {
	// Remove names the declarations to stop sending, in three shapes, all of them exact and
	// none of them a pattern:
	//
	//   - `<tool>` — one tool or MCP tool, by the name the inventory reports;
	//   - `mcp__<server>` — a whole MCP server, every tool it declares;
	//   - `skill__<skill>` — one skill, removed from the listing prose rather than from `tools`.
	//
	// An empty list is a no-op, which is what the shipped default is.
	Remove []string `yaml:"remove"`
	// Defer turns on tool deferral (#485, package tooldefer): `auto` defers the tools the user's
	// own history says are unused, with `defer_loading` and the API's search tool, so none is
	// removed. On a model that ignores `defer_loading` (GPT, GLM, Kimi; #825) the tool is removed
	// and comes back through our own search tool. `off` (the default, and the account opt-out)
	// defers nothing. The `ultrahigh` preset and the Bob plugin's `bob-ultrahigh` preset set `auto`.
	Defer string `yaml:"defer"`
	// Keep names tools deferral must never touch (the account keep list).
	Keep []string `yaml:"keep"`
	// AlwaysDefer names tools `defer: auto` defers in every new thread, whatever the history says
	// (#878): a tool name, or `mcp__<server>` for a whole server. The keep list wins over it.
	AlwaysDefer []string `yaml:"always_defer"`
}

// Toolfilter is a marker component holding the opt-in list: `tools` is a top-level body
// field the pipeline never sees (components operate on `messages`), so the rewrite lives in
// apply — the same arrangement toolschema and cachesplit use. apply reads the list back
// through Removed().
//
// The skills half lives there too, for a different reason: the listing IS in `messages`, but it
// sits in the cached prefix, and every rewrite of the prefix has to happen before any byte offset
// into the body is taken. Same place, same list, same counters. See apply.filterSkillListing.
type Toolfilter struct {
	remove []string
	defers bool
	keep   []string
	always []string
}

func newToolfilter(raw []byte) (components.Component, error) {
	var cfg toolfilterConfig
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, fmt.Errorf("toolfilter: %w", err)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(cfg.Remove))
	for _, n := range cfg.Remove {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !validDeclName(n) {
			// Rejected at WRITE time, not at request time: config.Validate builds the
			// pipeline, so a junk name is a 400 on the settings page instead of a filter that
			// silently matches nothing forever.
			return nil, fmt.Errorf("toolfilter: %q is not a declaration name", n)
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	// Sorted so the stored list, the audit entry and the settings page agree on an order,
	// and so no map iteration can leak into anything a reader compares.
	sort.Strings(out)
	tf := Toolfilter{remove: out}
	switch cfg.Defer {
	case "", "off":
	case "auto":
		tf.defers = true
	default:
		return nil, fmt.Errorf("toolfilter: defer is %q; want auto or off", cfg.Defer)
	}
	for _, n := range cfg.Keep {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if !validDeclName(n) {
			return nil, fmt.Errorf("toolfilter: keep %q is not a tool name", n)
		}
		tf.keep = append(tf.keep, n)
	}
	sort.Strings(tf.keep)
	for _, n := range cfg.AlwaysDefer {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if !validDeclName(n) || strings.HasPrefix(n, skills.RemovePrefix) {
			return nil, fmt.Errorf("toolfilter: always_defer %q is not a tool name", n)
		}
		tf.always = append(tf.always, n)
	}
	sort.Strings(tf.always)
	return tf, nil
}

// validDeclName accepts the charset a tool, MCP or skill name is drawn from, plus the two
// prefixes that select a mechanism. No globs, no wildcards: a pattern that matches more tomorrow
// than it did today is exactly the non-deterministic removal this component refuses to offer.
//
// The length bound is applied to the NAME, after the prefix, so `skill__` does not eat seven
// characters of a legitimate one.
func validDeclName(n string) bool {
	n = strings.TrimPrefix(n, skills.RemovePrefix)
	if n == "" || len(n) > 128 {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '-' || r == '/':
		default:
			return false
		}
	}
	return true
}

func (Toolfilter) Name() string { return "toolfilter" }

func (Toolfilter) Enabled(*components.Ctx) bool { return true }

// Removed is the opt-in list, sorted. apply calls it; nothing else should.
func (t Toolfilter) Removed() []string { return t.remove }

// Defers reports whether tool deferral is on (`defer: auto`). The host reads it.
func (t Toolfilter) Defers() bool { return t.defers }

// Keep is the account keep list for deferral, sorted.
func (t Toolfilter) Keep() []string { return t.keep }

// AlwaysDefer is the account always-defer list for deferral, sorted (#878).
func (t Toolfilter) AlwaysDefer() []string { return t.always }

// Reformat is a no-op: the rewrite already happened in apply, which reports it here through
// Ctx.FilteredDecls so this component does not read "declined" on the requests where it just
// acted — the mistake cachesplit's doc records.
func (Toolfilter) Reformat(_ *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) error {
	rep.Skipped = c == nil || c.FilteredDecls == 0
	return nil
}

// ponytail: this component records NO gate, so a configured name the prose gate declines is
// invisible in /stats — the one inconsistency GROUND-TRUTH already notes against toolfilter
// ("no gates recorded"). rep.Gate("prose_referenced") is the right home for it and the plumbing is
// the cost: the decision is made in apply, so the count has to reach Ctx the way FilteredDecls
// does, which means a fourth return value on filterDeclarations and filterSkillListing and their
// 31 call sites, 28 of them in tests. Deferred deliberately rather than duplicating
// proseReferenced at a second site, which is how the two would drift. The user-facing half is
// already handled: the page states the condition instead of promising an outcome it has not
// checked (dash/ui/tools.js, the opted-out row and the skills panel).

func init() {
	components.RegisterFields("toolfilter", toolfilterConfig{}, []components.Field{
		{Key: "defer", Type: components.FieldEnum, Options: []string{"off", "auto"}, Default: "off",
			Hint: "auto defers the tools your own history shows you do not use: each stays in the request with " +
				"defer_loading and loads through the API's search tool when the model needs it; on other models (GPT, GLM, " +
				"Kimi) the tool is removed and loads through Context Guru's own search tool, then stays for the thread; " +
				"off is the account opt-out"},
		{Key: "keep", Type: components.FieldStrings,
			Hint: "tool names deferral never touches"},
		{Key: "always_defer", Type: components.FieldStrings,
			Hint: "tool names, or `mcp__<server>` for a whole MCP server, that defer: auto defers in every new " +
				"thread whatever your history says; a name on the keep list stays loaded"},
		{Key: "remove", Type: components.FieldStrings,
			Hint: "declaration names to stop sending: a tool name, `mcp__<server>` for a whole MCP " +
				"server, or `skill__<skill>` for one skill's listing entry; a name still described " +
				"in the system prompt's prose is kept regardless"},
	})
}
