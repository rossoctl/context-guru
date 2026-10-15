package main

import (
	"regexp"
	"testing"
	"time"
)

// -h prints each flag's default as the process computed it from the environment, so this
// spans flag definition + envBool, which a helper-level test would not.
func TestLifecycleSignalsFlagDefault(t *testing.T) {
	re := regexp.MustCompile(`(?s)-dashboard-lifecycle-signals.*?\n\s*(.*?)\n  -`)
	for _, tc := range []struct {
		env  []string
		want bool
	}{{nil, true}, {[]string{"DASHBOARD_LIFECYCLE_SIGNALS=0"}, false}, {[]string{"DASHBOARD_LIFECYCLE_SIGNALS=false"}, false}} {
		out, _, _ := runMain(t, 20*time.Second, append([]string{"CG_TEST_MAIN_ARGS=-h"}, tc.env...)...)
		m := re.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("flag not in usage output:\n%s", out)
		}
		// Go prints "(default true)" only for a non-zero default.
		if got := regexp.MustCompile(`\(default true\)`).MatchString(m[0]); got != tc.want {
			t.Errorf("env %v: default on = %v, want %v\n%s", tc.env, got, tc.want, m[0])
		}
	}
}
