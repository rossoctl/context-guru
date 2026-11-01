package main

import (
	"flag"
	"log/slog"
	"strings"
)

// noRecorderWarning is logged once when a record flag is set on a build without the recorder.
const noRecorderWarning = "this build has no session recorder; rebuild with -tags cg_record"

// warnIfRecorderMissing logs noRecorderWarning once when the binary has no recorder and any
// record flag is set: --record or --record-otlp (also from the environment), or any other
// --record-* flag given on the command line. It never fails startup. It reports whether it warned.
func warnIfRecorderMissing(builtIn bool, dir, otlp string, fs *flag.FlagSet) bool {
	if builtIn {
		return false
	}
	set := strings.TrimSpace(dir) != "" || strings.TrimSpace(otlp) != ""
	if !set && fs != nil {
		fs.Visit(func(f *flag.Flag) {
			if strings.HasPrefix(f.Name, "record") {
				set = true
			}
		})
	}
	if set {
		slog.Warn(noRecorderWarning)
	}
	return set
}
