// Package models resolves a model id to its context window and its token prices.
//
// WHY THIS EXISTS. A host that only needs a model's window or price used to import proxyapp,
// because the resolver lived in proxyapp's facade. proxyapp is also the proxy's composition root,
// and Go links whole packages. So that host linked proxy, tenant, dash and every component just to
// read one window. Cortex's context-guru plugin did exactly that (issue #1251).
//
// This package is the resolver half of that facade, on its own. Like proxyapp, it is a FACADE:
// aliases and thin wrappers over internal/modelinfo, adding no behaviour and moving no code.
// proxyapp re-exports these names, so a host built on proxyapp still compiles.
//
// The LiteLLM gateway client is in models/litellm, because it makes a network call and this package
// does not (see internal/boundary).
//
// It must never import proxy or proxyapp. TestModelsLinksNoProxy fails if it does.
package models

import (
	"context"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Model-window and price resolution. A Resolver answers "how big is this model's context window",
// and a Pricer answers "what does a token cost on it" — both needed to report savings in money
// rather than only in tokens.
type (
	// Resolver resolves a model id to its context window.
	Resolver = modelinfo.Resolver
	// Chain tries several Resolvers in order, first answer wins.
	Chain = modelinfo.Chain
	// Static is the compiled-in table of known models.
	Static = modelinfo.Static
	// Table is a table loaded from a file.
	Table = modelinfo.Table
	// Price is one model's token prices.
	Price = modelinfo.Price
	// Pricer answers what a model's tokens cost.
	Pricer = modelinfo.Pricer
)

// DefaultStatic is the compiled-in model table.
func DefaultStatic() Static { return modelinfo.DefaultStatic() }

// LoadTable reads a model table from a file, so an operator can correct or extend the compiled-in
// one without a rebuild.
func LoadTable(path string) (*Table, error) { return modelinfo.LoadTable(path) }

// ExactWindow reports a model's window and whether the answer is exact rather than inferred. The
// distinction matters: a fraction-of-window trigger computed from a guessed window is a guess.
func ExactWindow(r Resolver, ctx context.Context, model string) (tokens int, exact, ok bool) {
	return modelinfo.Exact(r, ctx, model)
}
