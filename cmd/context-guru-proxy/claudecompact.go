package main

import (
	"os"

	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/proxy"
)

// claudeCompactAtStart reads the user's Claude Code auto-compact setting once, in local mode. A
// hosted proxy (local false) reads nothing: it serves other people's sessions, not the user's own.
func claudeCompactAtStart(local bool) compactionpoint.ClaudeCode {
	if !local {
		return compactionpoint.ClaudeCode{}
	}
	home, _ := os.UserHomeDir()
	return proxy.ReadClaudeCompact(os.Getenv, home)
}
