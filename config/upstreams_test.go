package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUpstreams(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upstreams.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadUpstreamsHappyPath(t *testing.T) {
	t.Setenv("UP_A", "secret-a")
	t.Setenv("UP_B", "secret-b")
	p := writeUpstreams(t, `
upstreams:
  - name: ibm-litellm
    dialect: anthropic
    base_url: https://gateway.example.com/
    key_env: UP_A
  - name: bob-us-east
    dialect: bob
    base_url: https://api.us-east.bob.example.com
    key_env: UP_B
    header: x-goog-api-key
`)
	got, err := LoadUpstreams(p)
	if err != nil {
		t.Fatalf("LoadUpstreams: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d upstreams", len(got))
	}
	// The trailing slash is normalised away, or every forwarded path gets a double one.
	if got[0].BaseURL != "https://gateway.example.com" {
		t.Errorf("base_url = %q", got[0].BaseURL)
	}
	if got[1].Header != "x-goog-api-key" {
		t.Errorf("header = %q", got[1].Header)
	}
	// The file must never carry the credential itself.
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "secret-a") {
		t.Error("the fixture put a real key in the config file")
	}
}

// The load-time check that matters most: no credential means we would forward the
// CLIENT's header — which in hosted mode is the token we minted — to a third party.
// Refusing to boot is the only safe answer.
func TestLoadUpstreamsRefusesMissingCredential(t *testing.T) {
	p := writeUpstreams(t, `
upstreams:
  - name: up
    dialect: openai
    base_url: https://api.example.com
    key_env: DEFINITELY_NOT_SET_ANYWHERE
`)
	_, err := LoadUpstreams(p)
	if err == nil {
		t.Fatal("loaded an upstream whose credential is not in the environment")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_ANYWHERE") {
		t.Errorf("error does not name the missing variable: %v", err)
	}
}

func TestLoadUpstreamsRejections(t *testing.T) {
	t.Setenv("UP_A", "x")
	cases := map[string]string{
		"no name":        "upstreams:\n  - dialect: openai\n    base_url: https://a.example.com\n    key_env: UP_A\n",
		"bad dialect":    "upstreams:\n  - name: u\n    dialect: gemini\n    base_url: https://a.example.com\n    key_env: UP_A\n",
		"no key_env":     "upstreams:\n  - name: u\n    dialect: openai\n    base_url: https://a.example.com\n",
		"no base_url":    "upstreams:\n  - name: u\n    dialect: openai\n    key_env: UP_A\n",
		"bad scheme":     "upstreams:\n  - name: u\n    dialect: openai\n    base_url: file:///etc/passwd\n    key_env: UP_A\n",
		"has path":       "upstreams:\n  - name: u\n    dialect: openai\n    base_url: https://a.example.com/v1\n    key_env: UP_A\n",
		"has userinfo":   "upstreams:\n  - name: u\n    dialect: openai\n    base_url: https://user:pw@a.example.com\n    key_env: UP_A\n",
		"empty file":     "upstreams: []\n",
		"unknown field":  "upstreams:\n  - name: u\n    dialect: openai\n    base_url: https://a.example.com\n    key_env: UP_A\n    kye_env: typo\n",
		"duplicate name": "upstreams:\n  - name: u\n    dialect: openai\n    base_url: https://a.example.com\n    key_env: UP_A\n  - name: u\n    dialect: bob\n    base_url: https://b.example.com\n    key_env: UP_A\n",
	}
	for name, body := range cases {
		if _, err := LoadUpstreams(writeUpstreams(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadUpstreamsMissingFile(t *testing.T) {
	if _, err := LoadUpstreams(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("loaded a nonexistent file")
	}
}

// Validate is the gate a tenant's saved configuration goes through. It must catch
// an unknown component name, which only building the pipeline reveals.
func TestValidateCatchesUnknownComponent(t *testing.T) {
	if err := Validate([]byte("pipeline: [format]\n")); err != nil {
		t.Fatalf("a valid config was rejected: %v", err)
	}
	if err := Validate([]byte("pipeline: [no_such_component]\n")); err == nil {
		t.Fatal("an unknown component name passed validation")
	}
	if err := Validate([]byte("pipelien: [format]\n")); err == nil {
		t.Fatal("a typo'd top-level key passed validation")
	}
	if err := Validate([]byte("preset: nonesuch\n")); err == nil {
		t.Fatal("an unknown preset passed validation")
	}
	if err := Validate([]byte("mode: sideways\n")); err == nil {
		t.Fatal("an invalid mode passed validation")
	}
}
