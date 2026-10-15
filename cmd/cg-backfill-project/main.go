// Command cg-backfill-project fills the project column on requests recorded before that column
// existed, reading each session's working directory out of the agent's own transcripts.
//
// A ONE-OFF, RUN BY HAND. It reads ~/.claude/projects/, which is a different source from
// everything else the dashboard reports — those figures all come from traffic that passed through
// the proxy. A migration that silently rewrites historical rows from the filesystem is not
// something to do on startup, and it only works on the machine the transcripts live on.
//
// Dry by default. Pass -apply to write.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/rossoctl/context-guru/dash"
)

func main() {
	db := flag.String("db", "", "path to the dashboard database (required)")
	home := flag.String("home", "", "override the home directory holding .claude/projects")
	apply := flag.Bool("apply", false, "write the changes; omit for a dry run")
	flag.Parse()

	if *db == "" {
		fmt.Fprintln(os.Stderr, "usage: cg-backfill-project -db <dashboard.db> [-home DIR] [-apply]")
		os.Exit(2)
	}
	d, err := dash.Open(*db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opening %s: %v\n", *db, err)
		os.Exit(1)
	}
	defer d.Close()

	out, err := d.BackfillProjects(*home, *apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "back-fill: %v\n", err)
		os.Exit(1)
	}

	mode := "DRY RUN — nothing was written"
	if *apply {
		mode = fmt.Sprintf("APPLIED — %d rows updated", out.Updated)
	}
	fmt.Println(mode)
	fmt.Printf("sessions with unattributed rows : %d (%d had a transcript)\n",
		out.Sessions, out.SessionsMatched)
	fmt.Printf("unattributed rows               : %d\n", out.Rows)
	fmt.Printf("rows a directory was found for  : %d\n", out.RowsFound)

	if len(out.ByProject) > 0 {
		fmt.Println("\nby project:")
		keys := make([]string, 0, len(out.ByProject))
		for k := range out.ByProject {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return out.ByProject[keys[i]] > out.ByProject[keys[j]] })
		for _, k := range keys {
			fmt.Printf("  %6d  %s\n", out.ByProject[k], k)
		}
	}
	// Named rather than rounded away: "3,002 of 3,003" needs the one explained.
	if len(out.Unmatched) > 0 {
		fmt.Printf("\n%d session(s) had no transcript and stay unattributed:\n", len(out.Unmatched))
		for _, s := range out.Unmatched {
			fmt.Printf("  %s\n", s)
		}
	}
}
