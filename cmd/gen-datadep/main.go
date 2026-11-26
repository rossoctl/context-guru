// Command gen-datadep writes docs/data-dependence.md from the row files under
// docs/data-dependence/ and the template page.md.tmpl there. Run it from the repo root, or
// pass -root. See internal/datadep.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rossoctl/context-guru/internal/datadep"
)

func main() {
	root := flag.String("root", ".", "repo root")
	flag.Parse()
	page, err := datadep.Build(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-datadep:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(*root, datadep.PagePath), []byte(page), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-datadep:", err)
		os.Exit(1)
	}
}
