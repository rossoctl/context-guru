// Package datadep builds docs/data-dependence.md from one file per table row.
//
// Why: all rows used to live in one Markdown table, so two PRs that each added a row conflicted
// in that file (#1088). Now each row is its own file under docs/data-dependence/<section>/, and
// this package builds the page from them and a template.
//
// Row file format. It is NOT YAML. The standard library parses it:
//
//	---
//	key: value        (one line each; split at the first ": ")
//	order: 30         (optional; sorts rows; default 1000; ties sort by file name)
//	---
//
//	The reason, in plain words, on any number of lines. Newlines become spaces in the table.
package datadep

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Section describes one table: its directory, and its cells in column order. The key "name" is
// the first cell. The body of the file is the cell "reason".
type Section struct {
	Dir          string   // under docs/data-dependence/
	Placeholder  string   // the line in the template that the rows replace
	Keys         []string // front matter keys, in the order they are allowed to be written
	Columns      []string // cells, in column order; "reason" is the body; "name" may be wrapped
	BacktickName bool     // render the name cell as `name`
}

var (
	Components = Section{
		Dir: "components", Placeholder: "{{component_rows}}",
		Keys:         []string{"name", "kind", "where", "off", "metadata_only", "everything", "decided_by", "order"},
		Columns:      []string{"name", "kind", "where", "off", "metadata_only", "everything", "reason", "decided_by"},
		BacktickName: true,
	}
	Features = Section{
		Dir: "features", Placeholder: "{{feature_rows}}",
		Keys:    []string{"name", "kind", "off", "metadata_only", "everything", "order"},
		Columns: []string{"name", "kind", "off", "metadata_only", "everything", "reason"},
	}
	Sections = []Section{Components, Features}
)

// Row is one parsed row file.
type Row struct {
	File   string // path of the file, for messages
	Stem   string // file name without ".md"
	Fields map[string]string
	Order  int
}

const defaultOrder = 1000

// Load reads every .md file in <root>/docs/data-dependence/<s.Dir>, sorted by (order, file name).
// It fails on an unknown or missing key, a duplicate key, a bad order, or a file without the
// front matter. It does not judge the content of a cell; the tests do that.
func Load(root string, s Section) ([]Row, error) {
	dir := filepath.Join(root, "docs", "data-dependence", s.Dir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, k := range s.Keys {
		allowed[k] = true
	}
	var rows []Row
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		r, err := parse(string(raw), allowed, s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		r.File = path
		r.Stem = strings.TrimSuffix(e.Name(), ".md")
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Order != rows[j].Order {
			return rows[i].Order < rows[j].Order
		}
		return rows[i].Stem < rows[j].Stem
	})
	return rows, nil
}

func parse(text string, allowed map[string]bool, s Section) (Row, error) {
	r := Row{Fields: map[string]string{}, Order: defaultOrder}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return r, fmt.Errorf("must start with a line of ---")
	}
	i := 1
	for ; i < len(lines) && lines[i] != "---"; i++ {
		k, v, ok := strings.Cut(lines[i], ": ")
		if !ok {
			k, v, ok = strings.Cut(lines[i], ":") // "key:" with an empty value
			if !ok || v != "" {
				return r, fmt.Errorf("line %d: want \"key: value\", got %q", i+1, lines[i])
			}
		}
		if !allowed[k] {
			return r, fmt.Errorf("line %d: unknown key %q (allowed: %s)", i+1, k, strings.Join(s.Keys, ", "))
		}
		if _, dup := r.Fields[k]; dup {
			return r, fmt.Errorf("line %d: key %q twice", i+1, k)
		}
		r.Fields[k] = strings.TrimSpace(v)
	}
	if i == len(lines) {
		return r, fmt.Errorf("the front matter has no closing ---")
	}
	if o, ok := r.Fields["order"]; ok {
		n, err := strconv.Atoi(o)
		if err != nil {
			return r, fmt.Errorf("order %q is not a number", o)
		}
		r.Order = n
		delete(r.Fields, "order")
	}
	for _, c := range s.Columns {
		if c == "reason" {
			continue
		}
		if _, ok := r.Fields[c]; !ok {
			return r, fmt.Errorf("missing key %q", c)
		}
	}
	r.Fields["reason"] = strings.Join(strings.Fields(strings.Join(lines[i+1:], "\n")), " ")
	return r, nil
}

// Line renders the row as a Markdown table row.
func (r Row) Line(s Section) string {
	cells := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		v := r.Fields[c]
		if c == "name" && s.BacktickName {
			v = "`" + v + "`"
		}
		cells[i] = strings.ReplaceAll(v, "|", `\|`)
	}
	return "| " + strings.Join(cells, " | ") + " |"
}

// Build returns the page: the template with each placeholder line replaced by its rows.
func Build(root string) (string, error) {
	tmpl, err := os.ReadFile(filepath.Join(root, "docs", "data-dependence", "page.md.tmpl"))
	if err != nil {
		return "", err
	}
	out := string(tmpl)
	for _, s := range Sections {
		rows, err := Load(root, s)
		if err != nil {
			return "", err
		}
		if strings.Count(out, s.Placeholder) != 1 {
			return "", fmt.Errorf("page.md.tmpl must hold %s exactly once", s.Placeholder)
		}
		lines := make([]string, len(rows))
		for i, r := range rows {
			lines[i] = r.Line(s)
		}
		out = strings.Replace(out, s.Placeholder, strings.Join(lines, "\n"), 1)
	}
	return out, nil
}

// PagePath is the generated page, relative to the repo root.
const PagePath = "docs/data-dependence.md"

// RegenCommand is the command that rewrites the page. Tests print it.
const RegenCommand = "go run ./cmd/gen-datadep"
