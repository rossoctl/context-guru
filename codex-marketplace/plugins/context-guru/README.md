# context-guru for Codex

This is the Codex plugin bundle for context-guru. It installs a dedicated Codex profile, starts a
loopback proxy, exposes health and statistics skills, and can remove only the state it owns.

For local development, register and install the containing marketplace:

```sh
codex plugin marketplace add ./codex-marketplace
codex plugin add context-guru@context-guru
```

The setup skill deliberately does not edit `~/.codex/config.toml`. After setup, start a routed
session with:

```sh
codex -p context-guru
```

Codex uses OpenAI's Responses API. The proxy currently forwards that envelope byte-for-byte; the
existing context-reduction pipeline still applies only to Chat Completions and Anthropic Messages
requests. This compatibility mode makes routing safe and observable without pretending that a
Responses request was optimized. Responses-native rewriting is tracked as the next integration
step.

The plugin requires `context-guru-proxy` on `PATH`, or a binary built at
`bin/context-guru-proxy` in this checkout.

Setup copies a standalone recovery command to
`~/.local/state/context-guru-codex/context-guru-reset`. Use it from an ordinary, unrouted shell if
the proxy is down and the routed profile cannot start Codex. It backs up and removes only the
marked profile, and signals only the recorded process whose command still names the recorded
binary.
