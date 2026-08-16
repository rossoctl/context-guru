package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Upstream is one entry in the server's allow-list of places traffic may be
// forwarded to. A hosted proxy lets tenants CHOOSE an upstream by name; it must
// never let them supply a URL, because that turns the service into an
// unauthenticated request-forwarder for anything its network can reach. Names in,
// URLs owned by the operator.
type Upstream struct {
	Name    string `yaml:"name"`
	Dialect string `yaml:"dialect"` // anthropic | openai | bob
	BaseURL string `yaml:"base_url"`
	// KeyEnv optionally names an environment variable holding a SERVER-HELD
	// credential for this upstream. Leave it empty — the default — and the caller's
	// own provider key is forwarded instead, so each user's traffic is billed to their
	// own account. Set it only for a gateway deployment where the agent holds a
	// placeholder key (eval containers, local single-tenant).
	//
	// The key itself is never in this file, never in the database, and never in a
	// tenant's configuration — it is read from the environment at request time, so a
	// leaked config or a database dump contains no usable secret.
	KeyEnv string `yaml:"key_env"`
	// Header is the request header the key is injected into. Empty defaults per
	// dialect: x-api-key for anthropic, Authorization: Bearer for openai and bob.
	Header string `yaml:"header"`
}

// Dialects an upstream may declare.
const (
	DialectAnthropic = "anthropic"
	DialectOpenAI    = "openai"
	DialectBob       = "bob"
)

type upstreamsFile struct {
	Upstreams []Upstream `yaml:"upstreams"`
}

// LoadUpstreams reads and validates the server's upstream allow-list.
//
// Validation is strict and happens at BOOT, not at first request. key_env is
// OPTIONAL: an upstream without one forwards the caller's own provider credential,
// which is the hosted default. When one IS named it must be present in the
// environment, because a gateway deployment that silently lost its key would forward
// a placeholder and fail every request in a way nobody can read.
func LoadUpstreams(path string) ([]Upstream, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f upstreamsFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("upstreams: %w", err)
	}
	if len(f.Upstreams) == 0 {
		return nil, fmt.Errorf("upstreams: %s declares none", path)
	}
	seen := map[string]bool{}
	for i := range f.Upstreams {
		u := &f.Upstreams[i]
		u.Name = strings.TrimSpace(u.Name)
		u.Dialect = strings.ToLower(strings.TrimSpace(u.Dialect))
		u.BaseURL = strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
		u.KeyEnv = strings.TrimSpace(u.KeyEnv)
		u.Header = strings.TrimSpace(u.Header)
		if u.Name == "" {
			return nil, fmt.Errorf("upstreams: entry %d has no name", i+1)
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("upstreams: duplicate name %q", u.Name)
		}
		seen[u.Name] = true
		switch u.Dialect {
		case DialectAnthropic, DialectOpenAI, DialectBob:
		default:
			return nil, fmt.Errorf("upstreams: %q has unknown dialect %q (want %s, %s or %s)",
				u.Name, u.Dialect, DialectAnthropic, DialectOpenAI, DialectBob)
		}
		if err := checkBaseURL(u.Name, u.BaseURL); err != nil {
			return nil, err
		}
		if u.KeyEnv != "" && os.Getenv(u.KeyEnv) == "" {
			return nil, fmt.Errorf("upstreams: %q names key_env %s, which is unset — "+
				"either export it or remove key_env to forward the caller's own credential",
				u.Name, u.KeyEnv)
		}
	}
	return f.Upstreams, nil
}

// checkBaseURL requires an absolute http(s) URL with no path, query or credentials.
// A path here would silently prefix every forwarded route; userinfo in the URL is a
// credential in a config file.
func checkBaseURL(name, raw string) error {
	if raw == "" {
		return fmt.Errorf("upstreams: %q has no base_url", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstreams: %q base_url: %w", name, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("upstreams: %q base_url must be http(s), got %q", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("upstreams: %q base_url has no host", name)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("upstreams: %q base_url must be scheme://host only, got %q", name, raw)
	}
	if u.User != nil {
		return fmt.Errorf("upstreams: %q base_url carries userinfo; put the credential in key_env", name)
	}
	return nil
}
