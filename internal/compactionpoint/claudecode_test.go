package compactionpoint

import "testing"

func TestClaudeCodeResolve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    ClaudeCode
		model  string
		window int
		want   int // C in tokens; 0 = not ok
	}{
		{"nothing set", ClaudeCode{}, "claude-opus-5", 1_000_000, 0},
		{"window env 200k", ClaudeCode{EnvWindow: "200000"}, "claude-opus-5", 1_000_000, 167_000},
		{"window env shorthand", ClaudeCode{EnvWindow: "200k"}, "claude-opus-5", 1_000_000, 167_000},
		{"window above model window is the model window", ClaudeCode{EnvWindow: "1m"}, "claude-haiku-4-5", 200_000, 167_000},
		{"window below minimum is the minimum", ClaudeCode{EnvWindow: "50k"}, "claude-opus-5", 1_000_000, 67_000},
		{"window not a number is ignored", ClaudeCode{EnvWindow: "lots"}, "claude-opus-5", 1_000_000, 0},
		{"settings window", ClaudeCode{SettingsWindow: "300000"}, "claude-opus-5", 1_000_000, 267_000},
		{"settings window auto is ignored", ClaudeCode{SettingsWindow: "auto"}, "claude-opus-5", 1_000_000, 0},
		{"env beats settings", ClaudeCode{EnvWindow: "200k", SettingsWindow: "500k"}, "claude-opus-5", 1_000_000, 167_000},
		{"model entry beats the top-level one", ClaudeCode{SettingsWindow: "500k",
			ModelWindows: map[string]string{"claude-opus-5": "250k"}}, "claude-opus-5[1m]", 1_000_000, 217_000},
		{"model entry for another model", ClaudeCode{ModelWindows: map[string]string{"claude-haiku-4-5": "150k"}},
			"claude-opus-5", 1_000_000, 0},
		// pct alone, on the model window: floor((200000-20000) x 50 / 100) = 90000
		{"percent override lowers it", ClaudeCode{EnvPct: "50"}, "claude-haiku-4-5", 200_000, 90_000},
		// the percent can only lower: 99% of 180000 = 178200 > 167000, so the default stays
		{"percent override cannot raise it", ClaudeCode{EnvPct: "99"}, "claude-haiku-4-5", 200_000, 167_000},
		{"percent out of range is ignored", ClaudeCode{EnvPct: "150"}, "claude-haiku-4-5", 200_000, 0},
		{"percent with no window is not applied", ClaudeCode{EnvPct: "50"}, "unknown-model", 0, 0},
		{"window and percent", ClaudeCode{EnvWindow: "400k", EnvPct: "50"}, "claude-opus-5", 1_000_000, 190_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := tc.cfg.Resolve(tc.model, tc.window)
			if (tc.want > 0) != ok {
				t.Fatalf("ok = %v, want %v (%+v)", ok, tc.want > 0, p)
			}
			if !ok {
				return
			}
			if p.Source != Configured {
				t.Errorf("source = %q", p.Source)
			}
			if got := p.Tokens(tc.window); got != tc.want {
				t.Errorf("C = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClaudeCodeAutoCompactOffIsTheWindow(t *testing.T) {
	for _, cfg := range []ClaudeCode{
		{EnvDisableAuto: "1"}, {EnvDisable: "true"}, {SettingsEnabled: "false"},
		{EnvDisableAuto: "1", EnvWindow: "200k"}, // off beats a window
	} {
		p, ok := cfg.Resolve("claude-opus-5", 1_000_000)
		if !ok || p.Source != Configured || p.Tokens(1_000_000) != 1_000_000 {
			t.Errorf("%+v -> %+v ok=%v, want C = the window", cfg, p, ok)
		}
	}
	// A value that is not a yes does not turn it off.
	if _, ok := (ClaudeCode{EnvDisableAuto: "0", SettingsEnabled: "true"}).Resolve("claude-opus-5", 1_000_000); ok {
		t.Error("DISABLE_AUTO_COMPACT=0 must not change C")
	}
}

func TestForIsUnchangedByTheNewFields(t *testing.T) {
	if p := For("claude-haiku-4-5"); p.Abs != 0 || p.Tokens(200_000) != 199_200 {
		t.Errorf("table point = %+v", p)
	}
}

func TestRecorded(t *testing.T) {
	if p := Recorded("claude-opus-5", 167_000, "configured"); p.Source != Configured || p.Tokens(1_000_000) != 167_000 {
		t.Errorf("configured row = %+v", p)
	}
	for _, src := range []string{"", "measured", "assumed"} {
		if p := Recorded("claude-haiku-4-5", 5, src); p.Source != Measured || p.Abs != 0 {
			t.Errorf("source %q gave %+v, want the table", src, p)
		}
	}
	if p := Recorded("claude-opus-5", 0, "configured"); p.Source == Configured {
		t.Error("a configured row with no number must fall back to the table")
	}
}
