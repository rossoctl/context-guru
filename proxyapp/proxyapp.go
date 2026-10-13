// Package proxyapp re-exports the handful of internal symbols a host needs to build its own
// context-guru proxy binary.
//
// WHY THIS EXISTS. cmd/context-guru-proxy is this module's own composition root, and it imports
// three internal/ packages: buildinfo, logging and modelinfo. (cheapmodel used to be a fourth —
// see below.) Go forbids importing an internal/ package from another module, so a deployment that
// needs the standard proxy PLUS its own routes — an enterprise build with SSO, a bespoke activation
// surface, a private landing page — cannot reuse that root. It has to copy it, and then it owns a
// 1300-line fork of somebody else's file for ever.
//
// That is what this package removes. It is a FACADE, deliberately: aliases and thin wrappers over
// the existing internal packages, adding no behaviour and moving no code. Nothing internal is
// relocated, so all ~70 files that import those packages today are untouched.
//
// ONE EXCEPTION, from the same dispatch.Dispatcher split noted below: AnthropicCheapModel and
// OpenAICheapModel lost their BaseURL/APIKey/AuthScheme/Client fields in favor of a
// dispatch.Dispatcher + dispatch.Ref, and proxy.Options.CheapModel changed type from
// components.Model to *proxy.CheapModelConfig. A host built on this facade that constructed
// either cheap-model type directly, or passed one to Options.CheapModel, stops compiling. There
// is no way to re-export the OLD shape here without also re-exporting the credential-holding
// code this project is removing from the core, so this one break is accepted rather than papered
// over.
//
// WHY A FACADE RATHER THAN PROMOTING THE PACKAGES. Moving internal/modelinfo (38 importers) out of
// internal/ would churn ~70 files across the tree and publish its entire surface as API this
// project then has to keep stable. The composition root uses THIRTEEN of its symbols. Exporting
// those thirteen is a promise worth making; exporting the whole package by accident is not.
//
// cheapmodel IS promoted, by exception: the dispatch.Dispatcher split (see dispatch/dispatch.go)
// requires it. Every host now builds its own cheapmodel.Anthropic/OpenAI directly over its own
// Dispatcher, and a host outside this module (Cortex, say) cannot do that with an internal/
// package. Its surface is small and host-agnostic on purpose — wire format only, no transport, no
// credential — so promoting it is a promise this project can keep.
//
// WHAT THIS IS NOT. It is not a plugin system and it does not run the proxy for you. A host still
// writes its own main(), and proxy.Options remains the contract. This only unblocks the parts a
// root cannot reach today.
package proxyapp

import (
	"context"
	"net/http"
	"time"

	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/internal/buildinfo"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/modelinfo/litellm"
)

// Build identity, as set by the Makefile's -ldflags. A host that wants its own stamp sets its own
// variables and ignores these; these are what this module's own build records.
//
// Read as functions rather than re-exported as variables, because a var alias would let a host
// overwrite the value another part of the process already reported.
func Version() string { return buildinfo.Version }

// Commit is the revision this module was built from.
func Commit() string { return buildinfo.Commit }

// SetupLogging configures the process logger from the environment (CG_LOG_LEVEL, CG_LOG_FILE) and
// returns a one-line description of what it did, for the startup log.
//
// It never fails the process: a log file that cannot be opened degrades to stderr with a warning,
// because refusing to boot over an observability sink would make logging less safe than not
// having it.
func SetupLogging() string { return logging.Setup() }

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
	// LiteLLM resolves against the LiteLLM community price/window map (internal/modelinfo/litellm:
	// the one network call in this family, kept out of modelinfo itself so that package stays
	// I/O-free).
	LiteLLM = litellm.LiteLLM
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

// NewLiteLLM resolves model windows against a LiteLLM gateway, cached for ttl.
func NewLiteLLM(url string, client *http.Client, ttl time.Duration) *LiteLLM {
	return litellm.NewLiteLLM(url, client, ttl)
}

// ExactWindow reports a model's window and whether the answer is exact rather than inferred. The
// distinction matters: a fraction-of-window trigger computed from a guessed window is a guess.
func ExactWindow(r Resolver, ctx context.Context, model string) (tokens int, exact, ok bool) {
	return modelinfo.Exact(r, ctx, model)
}

// The cheap-model clients, for the components that call a small model to do their work (extraction
// and summarisation). A host wires whichever dialect its gateway speaks.
type (
	// AnthropicCheapModel calls an Anthropic-dialect endpoint.
	AnthropicCheapModel = cheapmodel.Anthropic
	// OpenAICheapModel calls an OpenAI-dialect endpoint.
	OpenAICheapModel = cheapmodel.OpenAI
)
