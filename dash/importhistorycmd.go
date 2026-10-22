package dash

// RunImportHistory is the history import as a command: `context-guru-proxy import-history`, which
// install.sh runs once per install, and cmd/cg-import-history, which runs it by hand. It recovers
// request history from Claude Code's own session transcripts for traffic that predates this proxy.
//
// WHAT IT CANNOT RECOVER, which is the whole reason to read its output rather than trust it: a
// transcript records what each request COST and what it CALLED, but not the tools it DECLARED. So
// an imported row can answer "what did I spend" and can never answer "what was I carrying and not
// using". Imported rows inform the inventory; they never produce a verdict.
//
// Dry by default; -apply writes. Exit 0 on success, 1 on an error, 2 on a usage error.

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"time"
)

func RunImportHistory(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import-history", flag.ContinueOnError)
	fs.SetOutput(stderr)

	db := fs.String("db", "", "path to the dashboard database (required)")
	home := fs.String("home", "", "override the home directory holding .claude/projects")
	apply := fs.Bool("apply", false, "write the changes; omit for a dry run")
	sinceDays := fs.Int("since-days", 0, "ignore history older than this many days (0 = all)")
	// -backfill-projects repairs the FOLDER on invocations imported before tool_uses.project
	// existed. Its own flag rather than part of the import, because the import dedupes against
	// the request rows it already wrote -- correctly, so a second run cannot double-count spend
	// -- and that dedupe skips exactly the turns whose invocations need a folder. Measured on a
	// live store after the upgrade: every invocation row carried an unknown folder and a
	// re-import skipped all of them, leaving every per-project list empty.
	backfill := fs.Bool("backfill-projects", false,
		"repair the project folder on already-imported invocations; writes no request rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *db == "" {
		fmt.Fprintln(stderr,
			"usage: import-history -db <dashboard.db> [-home DIR] [-since-days N] [-apply]")
		return 2
	}
	d, err := Open(*db)
	if err != nil {
		fmt.Fprintf(stderr, "opening %s: %v\n", *db, err)
		return 1
	}
	defer d.Close()

	if *backfill {
		n, err := d.BackfillUseProjects(*home, *apply)
		if err != nil {
			fmt.Fprintf(stderr, "backfill: %v\n", err)
			return 1
		}
		if *apply {
			fmt.Fprintf(stdout, "BACKFILL APPLIED — %d invocations re-attributed to the folder they happened in\n", n)
			fmt.Fprintln(stdout, "no request rows were written, so nothing about spend changed")
		} else {
			fmt.Fprintf(stdout, "DRY RUN — %d invocations would be re-attributed; nothing was written (add -apply)\n", n)
		}
		return 0
	}

	opt := ImportOptions{Apply: *apply}
	if *sinceDays > 0 {
		opt.Since = time.Now().AddDate(0, 0, -*sinceDays).UnixMilli()
	}

	out, err := d.ImportHistory(*home, opt)
	if err != nil {
		fmt.Fprintf(stderr, "import: %v\n", err)
		return 1
	}

	if *apply {
		fmt.Fprintf(stdout, "APPLIED — %d requests imported\n", out.Imported)
	} else {
		fmt.Fprintf(stdout, "DRY RUN — %d requests would be imported, nothing was written\n", out.Importable)
	}
	fmt.Fprintf(stdout, "transcripts read            : %d\n", out.Files)
	if out.FirstTS > 0 {
		fmt.Fprintf(stdout, "span recovered              : %s .. %s\n",
			importDay(out.FirstTS), importDay(out.LastTS))
	}
	// Every skip is named. A skip that is only subtracted looks like a transcript that was short,
	// and the reader would not know to ask why their history is missing.
	if out.SkippedOverlap > 0 {
		fmt.Fprintf(stdout, "skipped, already measured   : %d (the proxy saw these itself)\n",
			out.SkippedOverlap)
	}
	if out.SkippedAlready > 0 {
		fmt.Fprintf(stdout, "skipped, already imported   : %d (a previous run)\n", out.SkippedAlready)
	}
	if out.Truncated > 0 {
		fmt.Fprintf(stdout, "WARNING: %d transcript(s) were cut short by an oversized line; their later turns are not counted\n", out.Truncated)
	}
	if out.SkippedOld > 0 {
		fmt.Fprintf(stdout, "skipped, older than -since  : %d\n", out.SkippedOld)
	}

	if len(out.Projects) > 0 {
		fmt.Fprintln(stdout, "\nby project:")
		for _, p := range importSortedByCount(out.Projects) {
			fmt.Fprintf(stdout, "  %7d  %s\n", out.Projects[p], p)
		}
	}

	// THE FINDING THIS IMPORT IS FOR. The cost of carrying an unused skill is not recoverable
	// from a transcript, but whether it was ever used is — and so is the inventory on disk. That
	// pairing needs no inference and is available the day the proxy is installed.
	if len(out.Called) > 0 {
		fmt.Fprintf(stdout, "\ncapabilities invoked in that span: %d\n", len(out.Called))
		for _, n := range importSortedByCount(out.Called) {
			fmt.Fprintf(stdout, "  %7d  %s\n", out.Called[n], n)
		}
	}
	never := NeverCalled(*home, out.Called)
	if len(never) > 0 {
		fmt.Fprintf(stdout, "\ninstalled but NEVER invoked in that span: %d\n", len(never))
		for _, n := range never {
			fmt.Fprintf(stdout, "           %s\n", n)
		}
		fmt.Fprintln(stdout, "\nThese were installed and never called across the history above. What")
		fmt.Fprintln(stdout, "carrying them COST is not in a transcript — only the proxy sees the tools a")
		fmt.Fprintln(stdout, "request declared — so this names candidates rather than pricing them.")
	}
	return 0
}

func importDay(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02") }

func importSortedByCount(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
