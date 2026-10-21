package routing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/dispatch"
)

const bobYAML = `
routes:
  - feature: classifier
    model_name_env: T_MODEL
    key_env: T_KEY
    base_url_env: T_URL
    dialect: bob
    price_input: 0.5
    price_output: 2
`

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type pinned struct{ eps []Endpoint }

func (p *pinned) pin(e Endpoint) dispatch.Ref {
	p.eps = append(p.eps, e)
	return dispatch.Ref("r1")
}

func TestLoadIsStrict(t *testing.T) {
	if _, err := Parse([]byte("routes:\n  - feature: x\n    key: oops\n")); err == nil {
		t.Fatal("an unknown field must fail the load")
	}
	rows, err := Parse([]byte(bobYAML))
	if err != nil || len(rows) != 1 || rows[0].PriceOutput != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestActiveRowAndLookup(t *testing.T) {
	rows, _ := Parse([]byte(bobYAML))
	p := &pinned{}
	tb := Build(rows, env(map[string]string{"T_MODEL": "bob-fast", "T_KEY": "sekret",
		"T_URL": "https://api.example.com/"}), p.pin, nil)
	r, ok := tb.Lookup("classifier")
	if !ok || r.Model != "bob-fast" || r.Ref != "r1" || r.Path != "/inference/v1/chat/completions" {
		t.Fatalf("route=%+v ok=%v", r, ok)
	}
	if r.Price.Input != 0.5/1e6 || r.Price.CacheRead != 0 {
		t.Fatalf("price=%+v", r.Price)
	}
	if len(p.eps) != 1 || p.eps[0].Key != "sekret" || p.eps[0].BaseURL != "https://api.example.com" {
		t.Fatalf("pinned=%+v", p.eps)
	}
	// The key must not reach stats.
	b, _ := json.Marshal(tb.Stats())
	if strings.Contains(string(b), "sekret") || !strings.Contains(string(b), `"base_url_host":"api.example.com"`) {
		t.Fatalf("stats=%s", b)
	}
	if _, ok := tb.Lookup("other"); ok {
		t.Fatal("no row must be none")
	}
}

func TestInactiveRows(t *testing.T) {
	rows, _ := Parse([]byte(bobYAML))
	full := map[string]string{"T_MODEL": "m", "T_KEY": "k", "T_URL": "https://h.example.com"}
	cases := map[string]map[string]string{
		"missing model": {"T_KEY": "k", "T_URL": "https://h.example.com"},
		"missing key":   {"T_MODEL": "m", "T_URL": "https://h.example.com"},
		"missing url":   {"T_MODEL": "m", "T_KEY": "k"},
		"http remote":   {"T_MODEL": "m", "T_KEY": "k", "T_URL": "http://h.example.com"},
		"relative":      {"T_MODEL": "m", "T_KEY": "k", "T_URL": "h.example.com/x"},
		"userinfo":      {"T_MODEL": "m", "T_KEY": "k", "T_URL": "https://u:p@h.example.com"},
	}
	for name, e := range cases {
		p := &pinned{}
		tb := Build(rows, env(e), p.pin, nil)
		if _, ok := tb.Lookup("classifier"); ok || len(p.eps) != 0 {
			t.Errorf("%s: row must be inactive and pin nothing", name)
		}
		st := tb.Stats()
		if len(st) != 1 || st[0].Active || st[0].Reason == "" {
			t.Errorf("%s: stats=%+v", name, st)
		}
	}
	// localhost over http is allowed.
	full["T_URL"] = "http://localhost:8080"
	if _, ok := Build(rows, env(full), (&pinned{}).pin, nil).Lookup("classifier"); !ok {
		t.Error("http://localhost must be active")
	}
	// A reason names the env var, never the value.
	full["T_URL"] = "http://h.example.com/secret-path"
	st := Build(rows, env(full), (&pinned{}).pin, nil).Stats()
	if strings.Contains(st[0].Reason, "secret-path") {
		t.Errorf("reason leaks the value: %q", st[0].Reason)
	}
}

func TestBadDialectAndNegativePriceAndDuplicate(t *testing.T) {
	e := env(map[string]string{"A": "m", "B": "k", "C": "https://h.example.com"})
	rows := []Row{
		{Feature: "a", ModelNameEnv: "A", KeyEnv: "B", BaseURLEnv: "C", Dialect: "grpc"},
		{Feature: "b", ModelNameEnv: "A", KeyEnv: "B", BaseURLEnv: "C", Dialect: "openai", PriceInput: -1},
		{Feature: "c", ModelNameEnv: "A", KeyEnv: "B", BaseURLEnv: "C", Dialect: "openai"},
		{Feature: "c", ModelNameEnv: "A", KeyEnv: "B", BaseURLEnv: "C", Dialect: "bob"},
	}
	tb := Build(rows, e, (&pinned{}).pin, nil)
	if _, ok := tb.Lookup("a"); ok {
		t.Error("bad dialect must be inactive")
	}
	if _, ok := tb.Lookup("b"); ok {
		t.Error("negative price must be inactive")
	}
	r, ok := tb.Lookup("c")
	if !ok || r.Dialect != "openai" || len(tb.Stats()) != 3 {
		t.Errorf("first row for a feature must win: %+v %v", r, tb.Stats())
	}
}

func TestCountersAndNilTable(t *testing.T) {
	rows, _ := Parse([]byte(bobYAML))
	tb := Build(rows, env(map[string]string{"T_MODEL": "m", "T_KEY": "k", "T_URL": "https://h.example.com"}),
		(&pinned{}).pin, nil)
	tb.Record("classifier", Routed, 0.25)
	tb.Record("classifier", Routed, 0.5)
	tb.Record("classifier", FellBack, 0)
	tb.Record("classifier", NotDetected, 0)
	s := tb.Stats()[0]
	if s.Calls != 3 || s.Routed != 2 || s.FellBack != 1 || s.NotDetected != 1 || s.SavedUSD != 0.75 {
		t.Fatalf("stats=%+v", s)
	}
	var nilT *Table
	nilT.Record("x", Routed, 1)
	if _, ok := nilT.Lookup("x"); ok || nilT.Stats() != nil {
		t.Fatal("a nil table is an empty table")
	}
}
