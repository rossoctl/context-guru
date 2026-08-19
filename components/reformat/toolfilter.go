package reformat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
)

func init() { components.Register("toolfilter", newToolfilter) }

// toolfilterConfig is the account's opt-in list, and it is the WHOLE of the component's
// authority: nothing is ever removed that is not named here. There is deliberately no
// `auto`, no threshold and no "remove what looked unused" mode — the inventory can only ever
// show that a declaration was not used in the sessions it captured, and an unused tool is
// not the same thing as an unwanted one.
type toolfilterConfig struct {
	// Remove names the tool and MCP-server declarations to stop sending. Exact declaration
	// names as the inventory reports them, plus the bare form `mcp__<server>` for a whole
	// MCP server. An empty list is a no-op, which is what the shipped default is.
	Remove []string `yaml:"remove"`
}

// Toolfilter is a marker component holding the opt-in list: `tools` is a top-level body
// field the pipeline never sees (components operate on `messages`), so the rewrite lives in
// apply — the same arrangement toolschema and cachesplit use. apply reads the list back
// through Removed().
type Toolfilter struct{ remove []string }

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
	return Toolfilter{remove: out}, nil
}

// validDeclName accepts the charset a tool, MCP or skill name is drawn from. No globs, no
// wildcards: a pattern that matches more tomorrow than it did today is exactly the
// non-deterministic removal this component refuses to offer.
func validDeclName(n string) bool {
	if len(n) > 128 {
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

// Reformat is a no-op: the rewrite already happened in apply, which reports it here through
// Ctx.FilteredDecls so this component does not read "declined" on the requests where it just
// acted — the mistake cachesplit's doc records.
func (Toolfilter) Reformat(_ *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) error {
	rep.Skipped = c == nil || c.FilteredDecls == 0
	return nil
}

func init() {
	components.RegisterFields("toolfilter", toolfilterConfig{}, []components.Field{
		{Key: "remove", Type: components.FieldStrings,
			Hint: "declaration names to stop sending (`mcp__<server>` removes a whole MCP server); " +
				"a name still described in the system prompt's prose is kept regardless"},
	})
}
