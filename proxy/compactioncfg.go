package proxy

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"runtime"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/agent/bob"
	"github.com/rossoctl/context-guru/compactionpoint"
)

// The user's own Claude Code auto-compact settings (#948).
//
// WHEN IT IS READ: ONCE, when the proxy starts, in local mode only (cmd/context-guru-proxy calls
// ReadClaudeCompact and hands the result to Options.ClaudeCompact). It is held in memory for the
// life of the proxy. A setting changed while the proxy runs takes effect at the next proxy restart.
// No per-session or per-request report, no stored copy: nothing to purge.
//
// WHERE: the host, because only the host can read the user's files and env. The rule that turns the
// raw values into C stays in compactionpoint (ClaudeCode.Resolve), which never imports
// proxy.
//
// WHICH ENV. The plugin starts the proxy from the SessionStart hook (start-proxy.sh, `setsid` or
// `nohup`, no `env -i`), so the proxy inherits the env of the hook, which is the env of the Claude
// Code session that started it. That is the env of ONE session: a later session with other values
// does not change a running proxy. A proxy started by hand reads the env of that shell.
//
// WHICH FILES (the sources the client reads, verified on claude 2.1.319): the user settings file
// ${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json, and the legacy global config
// ${CLAUDE_CONFIG_DIR:-~}/.claude.json for autoCompactEnabled. Project settings files depend on the
// directory the client runs in, which the proxy does not know, so they are NOT read.

// ClaudeCompactOffEnv turns the start-up read off ("off", "0" or "false"). C then comes from the
// table.
const ClaudeCompactOffEnv = compactionpoint.OffEnv

// ReadClaudeCompact reads the settings from env and the user's settings files. getenv and home are
// injected for tests. It never fails: an unreadable or odd file is skipped (fail open).
func ReadClaudeCompact(getenv func(string) string, home string) compactionpoint.ClaudeCode {
	return compactionpoint.ReadClaude(getenv, home, hostOpen)
}

// hostOpen opens a settings file with os.Open, so every path means what it always meant. The
// library reads settings files through it. Only tests may write this variable.
var hostOpen compactionpoint.Open = func(path string) (fs.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// compactionPointFor is C for this request (cl is the request's client, agent.Identify, computed once): the user's own setting, read at start-up, when it
// changes C for the request's model; else the operator's compaction_point in the price table, else the
// bootstrap table. A hosted proxy (Options.Tenants)
// never has a user setting. Fail open: any odd value gives the table.
//
// WHICH REQUESTS. Each client's setting applies only to a request whose User-Agent is that client's
// own (agent.Identify, Client.Name, mapped by compactionClient; the client sets it itself). A request of another client on the same local
// proxy gets the table. Bob also gets its own verified default when the user set nothing (#963).
// Codex has no default (see compactionpoint.Codex). The limit: the header is the client's own
// claim, and the Claude Agent SDK sends the same User-Agent, so an SDK program run with the same
// env is also covered, which is right, because it reads the same settings. A request with no
// User-Agent gets the table.
//
// MODEL MATCH. A modelSettings key matches by substring on the normalized model id, longest key
// first, the same style as the table in compactionpoint.For. So a key for claude-opus-5 also applies
// to a newer id that contains it (claude-opus-5-1). That is the client's own rule too: its canonical
// model name matches dated and variant spellings.
func (h *Handler) compactionPointFor(r *http.Request, cl agent.Client, body []byte, window int) compactionpoint.Point {
	model := gjson.GetBytes(body, "model").String()
	if h.opts.Tenants == nil && r != nil {
		set := compactionpoint.Settings{Claude: h.opts.ClaudeCompact, Bob: h.opts.BobCompact, Codex: h.opts.CodexCompact}
		if p, ok := set.Pick(compactionClient(cl), model, window, h.bobWindow); ok {
			return p
		}
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	return h.resolveCompactionPoint(ctx, body, window)
}

// bobWindow is Bob's own max_input_tokens for the model, as seen in the model/info response that
// passed through this proxy (the agent's ControlPlane reply, stored by noteBobWindows). 0 = not seen. Metadata only: a number for each
// model id, in memory, at most bobWindowMax entries.
func (h *Handler) bobWindow(model string) int {
	h.bobWinMu.Lock()
	defer h.bobWinMu.Unlock()
	return h.bobWin[model]
}

const bobWindowMax = bob.MaxModelWindows

// noteBobModelInfo records bob.ModelWindows(body). A body that is not that shape records nothing.
func (h *Handler) noteBobModelInfo(body []byte) { h.noteBobWindows(bob.ModelWindows(body)) }

// noteBobWindows records the agent's window catalog (agent.ControlReply.Windows). An empty catalog,
// or one over bobWindowMax entries, records nothing. A window <= 0 is dropped.
func (h *Handler) noteBobWindows(windows map[string]int) {
	got := compactionpoint.CleanBobWindows(windows, bobWindowMax)
	if got == nil {
		return
	}
	h.bobWinMu.Lock()
	defer h.bobWinMu.Unlock()
	h.bobWin = got // the whole catalog replaces the old one: bounded, and never stale
}

// ReadBobCompact reads Bob Shell's model.compressionThreshold (#963) from the user settings file
// ~/.bob/settings.json and the system settings file, which wins in the client's merge. The project
// file is not read (the proxy does not know the directory). Read is true whenever the read ran, so
// Bob's own default (0.5) applies when nothing is set. Same switch as the others: off gives the zero
// value. It never fails.
func ReadBobCompact(getenv func(string) string, home string) compactionpoint.Bob {
	return compactionpoint.ReadBob(getenv, home, runtime.GOOS, hostOpen)
}

// ReadCodexCompact reads three top-level keys of $CODEX_HOME/config.toml (default ~/.codex) (#963):
// model_auto_compact_token_limit, model_auto_compact_token_limit_scope and
// model_post_turn_compact_threshold_percent. It is a small line scanner, not a TOML parser: it reads
// only `key = value` lines before the first [table] header, and skips anything it does not
// understand (fail open). Same switch as the others.
func ReadCodexCompact(getenv func(string) string, home string) compactionpoint.Codex {
	return compactionpoint.ReadCodex(getenv, home, hostOpen)
}

// compactionClient maps the request's Client to the client the compaction point keys on. A client with no
// setting (Pi, OpenCode, any other) gives ClientNone: the table applies.
func compactionClient(cl agent.Client) compactionpoint.Client {
	switch cl.Name {
	case agent.ClientClaude:
		return compactionpoint.ClientClaude
	case agent.ClientBob:
		return compactionpoint.ClientBob
	case agent.ClientCodex:
		return compactionpoint.ClientCodex
	}
	return compactionpoint.ClientNone
}
