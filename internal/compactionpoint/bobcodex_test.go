package compactionpoint

import "testing"

func TestBobDefaultIsHalfTheWindow(t *testing.T) {
	p, ok := (Bob{Read: true}).Resolve(200_000)
	if !ok || p.Source != ClientDefault || p.Tokens(200_000) != 100_000 || !p.FromClient() {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
	if p, _ := (Bob{Read: true}).Resolve(1_000_000); p.Tokens(1_000_000) != 500_000 {
		t.Errorf("1M window: %+v", p)
	}
}

func TestBobUserThresholdBeatsDefault(t *testing.T) {
	p, ok := (Bob{Read: true, Threshold: "0.8"}).Resolve(200_000)
	if !ok || p.Source != Configured || p.Tokens(200_000) != 160_000 {
		t.Fatalf("got %+v", p)
	}
	if p, _ := (Bob{Read: true, Threshold: "7"}).Resolve(200_000); p.Tokens(200_000) != 200_000 {
		t.Errorf("above 1 must cap at the window: %+v", p)
	}
}

func TestBobFailsOpen(t *testing.T) {
	if _, ok := (Bob{}).Resolve(200_000); ok {
		t.Error("not read: table")
	}
	if _, ok := (Bob{Read: true}).Resolve(0); ok {
		t.Error("unknown window: table")
	}
	for _, raw := range []string{"abc", "-1", "0", "NaN", "Inf"} {
		p, ok := (Bob{Read: true, Threshold: raw}).Resolve(200_000)
		if !ok || p.Source != ClientDefault { // a bad value gives the client default, not a guess
			t.Errorf("%q gave %+v", raw, p)
		}
	}
}

func TestCodexUserLimit(t *testing.T) {
	p, ok := (Codex{Limit: "200000"}).Resolve(872_000)
	if !ok || p.Source != Configured || p.Tokens(872_000) != 200_000 {
		t.Fatalf("got %+v", p)
	}
	if p, _ := (Codex{Limit: "9999999"}).Resolve(272_000); p.Tokens(272_000) != 244_800 {
		t.Errorf("clamp to 0.9 x window: %+v", p)
	}
}

func TestCodexNoDefaultAndFailsOpen(t *testing.T) {
	for _, c := range []Codex{
		{}, {Limit: "abc"}, {Limit: "-5"}, {Limit: "0"},
		{Limit: "200000", Scope: "body_after_prefix"},
		{Limit: "200000", PostTurn: "80"},
	} {
		if p, ok := c.Resolve(272_000); ok {
			t.Errorf("%+v gave %+v, want the table", c, p)
		}
	}
	if _, ok := (Codex{Limit: "200000", Scope: "total", PostTurn: "0"}).Resolve(272_000); !ok {
		t.Error("scope total and post-turn 0 are the defaults")
	}
}

func TestRecordedClientDefault(t *testing.T) {
	if p := Recorded("gpt-5.5", 100_000, "client_default"); p.Source != ClientDefault || p.Tokens(200_000) != 100_000 {
		t.Errorf("got %+v", p)
	}
	if p := Recorded("claude-opus-5", 0, "client_default"); p.Source == ClientDefault {
		t.Error("no number: table")
	}
}
