// Package litellm resolves model windows and prices against a LiteLLM gateway.
//
// It is the one network call in the models family. It sits apart from package models for the
// same reason internal/modelinfo/litellm sits apart from internal/modelinfo: models stays
// I/O-free, so it passes the core boundary (internal/boundary). This package is a host package,
// like the one it wraps.
//
// It is a FACADE: an alias and a thin wrapper over internal/modelinfo/litellm, adding no
// behaviour. It must never import proxy or proxyapp (issue #1251).
package litellm

import (
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo/litellm"
)

// LiteLLM resolves against the LiteLLM community price/window map. It is a models.Resolver and a
// models.Pricer.
type LiteLLM = litellm.LiteLLM

// New resolves model windows against a LiteLLM gateway, cached for ttl.
func New(url string, client *http.Client, ttl time.Duration) *LiteLLM {
	return litellm.NewLiteLLM(url, client, ttl)
}
