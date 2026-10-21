// Command cg-import-history recovers request history from Claude Code's own session transcripts,
// for traffic that predates this proxy's installation.
//
// WHY IT EXISTS. A user installs context-guru after months of working with Claude Code and the
// dashboard starts from zero: every request before install day is invisible, so the first thing
// they see is an empty page. Claude Code writes a transcript of every session under
// ~/.claude/projects/, and those files exist whether or not this proxy ever ran.
//
// A ONE-OFF, RUN BY HAND. It reads the user's filesystem, which is a different source from
// everything else the dashboard reports — those figures all come from traffic that passed through
// the proxy. A migration that silently rewrites history from outside the request path is not
// something to do on startup, and it only works on the machine the transcripts live on.
//
// WHAT IT CANNOT RECOVER, which is the whole reason to read its output rather than trust it: a
// transcript records what each request COST and what it CALLED, but not the tools it DECLARED. So
// an imported row can answer "what did I spend" and can never answer "what was I carrying and not
// using" — the waste finding the insights page is built around. Imported rows inform the
// inventory; they never produce a verdict.
//
// Dry by default. Pass -apply to write.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/dash"
)

func main() {
	db := flag.String("db", "", "path to the dashboard database (required)")
	home := flag.String("home", "", "override the home directory holding .claude/projects")
	apply := flag.Bool("apply", false, "write the changes; omit for a dry run")
	sinceDays := flag.Int("since-days", 0, "ignore history older than this many days (0 = all)")
	// -backfill-projects repairs the FOLDER on invocations imported before tool_uses.project
	// existed. Its own flag rather than part of the import, because the import dedupes against
	// the request rows it already wrote -- correctly, so a second run cannot double-count spend
	// -- and that dedupe skips exactly the turns whose invocations need a folder. Measured on a
	// live store after the upgrade: every invocation row carried an unknown folder and a
	// re-import skipped all of them, leaving every per-project list empty.
	backfill := flag.Bool("backfill-projects", false,
		"repair the project folder on already-imported invocations; writes no request rows")
	flag.Parse()

	if *db == "" {
		fmt.Fprintln(os.Stderr,
			"usage: cg-import-history -db <dashboard.db> [-home DIR] [-since-days N] [-apply]")
		os.Exit(2)
	}
	d, err := dash.Open(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opening %s: %v\n", *db, err)
		os.Exit(1)
	}
	defer d.Close()

	if *backfill {
		n, err := d.BackfillUseProjects(*home, *apply)
		if err != nil {
			fmt.Fprintf(os.Stderr, "backfill: %v\n", err)
			os.Exit(1)
		}
		if *apply {
			fmt.Printf("BACKFILL APPLIED — %d invocations re-attributed to the folder they happened in\n", n)
			fmt.Println("no request rows were written, so nothing about spend changed")
		} else {
			fmt.Printf("DRY RUN — %d invocations would be re-attributed; nothing was written (add -apply)\n", n)
		}
		return
	}

	opt := dash.ImportOptions{Apply: *apply}
	if *sinceDays > 0 {
		opt.Since = time.Now().AddDate(0, 0, -*sinceDays).UnixMilli()
	}

	out, err := d.ImportHistory(*home, opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		os.Exit(1)
	}

	if *apply {
		fmt.Printf("APPLIED — %d requests imported\n", out.Imported)
	} else {
		fmt.Printf("DRY RUN — %d requests would be imported, nothing was written\n", out.Importable)
	}
	fmt.Printf("transcripts read            : %d\n", out.Files)
	if out.FirstTS > 0 {
		fmt.Printf("span recovered              : %s .. %s\n",
			day(out.FirstTS), day(out.LastTS))
	}
	// Every skip is named. A skip that is only subtracted looks like a transcript that was short,
	// and the reader would not know to ask why their history is missing.
	if out.SkippedOverlap > 0 {
		fmt.Printf("skipped, already measured   : %d (the proxy saw these itself)\n",
			out.SkippedOverlap)
	}
	if out.SkippedAlready > 0 {
		fmt.Printf("skipped, already imported   : %d (a previous run)\n", out.SkippedAlready)
	}
	if out.Truncated > 0 {
		fmt.Printf("WARNING: %d transcript(s) were cut short by an oversized line; their later turns are not counted\n", out.Truncated)
	}
	if out.SkippedOld > 0 {
		fmt.Printf("skipped, older than -since  : %d\n", out.SkippedOld)
	}

	if len(out.Projects) > 0 {
		fmt.Println("\nby project:")
		for _, p := range sortedByCount(out.Projects) {
			fmt.Printf("  %7d  %s\n", out.Projects[p], p)
		}
	}

	// THE FINDING THIS IMPORT IS FOR. The cost of carrying an unused skill is not recoverable
	// from a transcript, but whether it was ever used is — and so is the inventory on disk. That
	// pairing needs no inference and is available the day the proxy is installed.
	if len(out.Called) > 0 {
		fmt.Printf("\ncapabilities invoked in that span: %d\n", len(out.Called))
		for _, n := range sortedByCount(out.Called) {
			fmt.Printf("  %7d  %s\n", out.Called[n], n)
		}
	}
	never := dash.NeverCalled(*home, out.Called)
	if len(never) > 0 {
		fmt.Printf("\ninstalled but NEVER invoked in that span: %d\n", len(never))
		for _, n := range never {
			fmt.Printf("           %s\n", n)
		}
		fmt.Println("\nThese were installed and never called across the history above. What")
		fmt.Println("carrying them COST is not in a transcript — only the proxy sees the tools a")
		fmt.Println("request declared — so this names candidates rather than pricing them.")
	}
}

func day(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02") }

func sortedByCount(m map[string]int) []string {
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
