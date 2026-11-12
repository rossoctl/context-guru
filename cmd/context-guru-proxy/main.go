// Command context-guru-proxy is the LLM proxy integration and the
// eval-containers gateway. It runs the context-guru component pipeline on
// inbound chat requests, then forwards them to the configured upstream
// provider, exposing /openai + /anthropic on one port plus /healthz, /stats,
// and /expand.
//
// Config is loaded from --config (YAML); upstreams and listen address from
// flags/env. Fail open: on any pipeline trouble the original request is
// forwarded untouched.
//
// The startup itself is proxyapp.Main, so a host with its own parts reuses it through
// proxyapp.Hooks instead of copying this file. This binary passes no hooks.
package main

import "github.com/rossoctl/context-guru/proxyapp"

func main() { proxyapp.Main(proxyapp.Hooks{}) }
