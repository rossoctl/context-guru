package datadep

import (
	"os"
	"path/filepath"
	"testing"
)

const repoRoot = "../.."

// The checked-in page must equal what the generator makes from the row files.
func TestGeneratedPageIsUpToDate(t *testing.T) {
	want, err := Build(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot, PagePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s is out of date with docs/data-dependence/. Run: %s\n"+
			"Do not edit the page by hand, and do not resolve a merge conflict in it by hand: re-run the command.",
			PagePath, RegenCommand)
	}
}

func TestEveryRowIsNamedAfterItsFile(t *testing.T) {
	for _, s := range Sections {
		rows, err := Load(repoRoot, s)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Errorf("no rows in docs/data-dependence/%s", s.Dir)
		}
		for _, r := range rows {
			if r.Fields["name"] == "" {
				t.Errorf("%s: empty name", r.File)
			}
			if s.BacktickName && r.Fields["name"] != r.Stem {
				t.Errorf("%s: name %q must equal the file name %q", r.File, r.Fields["name"], r.Stem)
			}
		}
	}
}

func TestParseRejectsBadFiles(t *testing.T) {
	allowed := map[string]bool{"name": true, "kind": true, "off": true, "metadata_only": true, "everything": true, "order": true}
	for name, text := range map[string]string{
		"no front matter": "name: x\n",
		"unclosed":        "---\nname: x\n",
		"unknown key":     "---\nnmae: x\n---\n",
		"missing key":     "---\nname: x\n---\nreason\n",
		"bad order":       "---\nname: x\nkind: a\noff: b\nmetadata_only: c\neverything: d\norder: z\n---\n",
		"duplicate key":   "---\nname: x\nname: y\n---\n",
	} {
		if _, err := parse(text, allowed, Features); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	r, err := parse("---\nname: x\nkind: a\noff: b\nmetadata_only: c\neverything: d\n---\n\nOne\ntwo.\n", allowed, Features)
	if err != nil || r.Fields["reason"] != "One two." || r.Order != defaultOrder {
		t.Errorf("good file: %+v %v", r, err)
	}
}
