package proxyapp

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
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// No home: a relative path would read the working directory's own .claude/settings.json,
		// which is a project file. Read env only.
		return proxy.ReadClaudeCompact(os.Getenv, "")
	}
	return proxy.ReadClaudeCompact(os.Getenv, home)
}

// bobCompactAtStart and codexCompactAtStart read Bob's and Codex's settings once, in local mode
// (#963). A hosted proxy reads nothing.
func bobCompactAtStart(local bool) compactionpoint.Bob {
	if !local {
		return compactionpoint.Bob{}
	}
	home, _ := os.UserHomeDir()
	return proxy.ReadBobCompact(os.Getenv, home)
}

func codexCompactAtStart(local bool) compactionpoint.Codex {
	if !local {
		return compactionpoint.Codex{}
	}
	home, _ := os.UserHomeDir()
	return proxy.ReadCodexCompact(os.Getenv, home)
}
