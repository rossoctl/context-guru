package config

import (
	"reflect"
	"testing"
)

// #857: absent is nil (shipped list), [] is a non-nil empty list (rule off), a list is kept.
func TestKeepAliveTerminalToolsYAML(t *testing.T) {
	cases := []struct {
		name, doc string
		want      []string
		wantNil   bool
	}{
		{"absent", "preset: off\ncache:\n  keepalive: true\n", nil, true},
		{"empty", "preset: off\ncache:\n  keepalive: true\n  keepalive_terminal_tools: []\n", []string{}, false},
		{"list", "preset: off\ncache:\n  keepalive_terminal_tools: [Finish]\n", []string{"Finish"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := LoadBytes([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Cache.KeepAliveTerminalTools
			if (got == nil) != c.wantNil || (!c.wantNil && !reflect.DeepEqual(got, c.want)) {
				t.Fatalf("got %#v (nil=%v), want %#v (nil=%v)", got, got == nil, c.want, c.wantNil)
			}
		})
	}
}
