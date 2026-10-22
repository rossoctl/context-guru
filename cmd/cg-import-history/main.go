// Command cg-import-history recovers request history from Claude Code's own session transcripts,
// for traffic that predates this proxy's installation.
//
// WHY IT EXISTS. A user installs context-guru after months of working with Claude Code and the
// dashboard starts from zero: every request before install day is invisible, so the first thing
// they see is an empty page. Claude Code writes a transcript of every session under
// ~/.claude/projects/, and those files exist whether or not this proxy ever ran.
//
// ONCE PER INSTALL, NEVER ON STARTUP. install.sh runs the same import as `context-guru-proxy
// import-history` once, when the plugin is installed (the release ships only that binary). The
// proxy never runs it by itself: it reads the user's filesystem, a different source from the
// traffic every other figure comes from, and only works on the machine the transcripts live on.
// This command is the same import, for running it again by hand.
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
	"os"

	"github.com/rossoctl/context-guru/dash"
)

func main() { os.Exit(dash.RunImportHistory(os.Args[1:], os.Stdout, os.Stderr)) }
