package main

import "testing"

func TestAccountantStatePath(t *testing.T) {
	for _, tc := range []struct {
		db        string
		dashboard bool
		want      string
	}{
		{"/var/cg/dash.db", true, "/var/cg/dash.db.accountant-state.json"},
		{"/var/cg/dash.db", false, ""},
		{"", true, ""},
		{":memory:", true, ""},
		{"file::memory:?cache=shared", true, ""},
	} {
		if got := accountantStatePath(tc.db, tc.dashboard); got != tc.want {
			t.Errorf("accountantStatePath(%q, %v) = %q, want %q", tc.db, tc.dashboard, got, tc.want)
		}
	}
}
