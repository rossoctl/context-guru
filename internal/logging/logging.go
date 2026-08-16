// Package logging is context-guru's logging plumbing: the level/format/sink
// decision, the credential scrubber that sits in front of every handler, and the
// per-request logger that carries tenant and session onto the lines of one
// request's lifecycle — including the ones written after the response, off the
// request path (see With, and the boundary named there).
//
// It is deliberately small, and deliberately built on log/slog with no logging
// dependency added. Three shapes have to work:
//
//   - A LOCAL proxy, run by hand: useful text on stderr, no configuration, no
//     container, no Loki. This is the zero-value behaviour.
//   - The HOSTED deployment: the same records as JSON in a file, which promtail
//     tails into Loki. Shipping is a file path and nothing more — no HTTP client
//     here, no batching, no retry queue, because promtail already is one, and a log
//     shipper inside the process being debugged is a way to lose logs.
//   - Somebody who wants NONE of ours: CG_LOG_PLAIN=1 swaps in the stdlib's own
//     slog text handler and disables the file sink.
//
// Environment (all optional):
//
//	CG_LOG_LEVEL=debug|info|warn|error   default info
//	CG_LOG_FORMAT=text|json              default text, or json when CG_LOG_FILE is set
//	CG_LOG_FILE=/path/to/proxy.jsonl     ALSO write there, in JSON, for promtail → Loki
//	CG_LOG_PLAIN=1                       plain stdlib slog, no scrubbing, no file
//	CONTEXT_GURU_DEBUG=1                 legacy alias for CG_LOG_LEVEL=debug
//
// Level vocabulary, so the levels mean something across the codebase:
//
//	ERROR  we failed, or we did something that is a bug in this program.
//	WARN   we degraded but kept serving: fell open, reverted, refused, evicted.
//	INFO   the request lifecycle — ONE line per request — and startup facts.
//	DEBUG  per-component decisions: which gate declined, with what numbers.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/rossoctl/context-guru/internal/redact"
)

// Setup installs the process-wide slog default from the environment and returns a
// one-line description of what it chose, so the caller can log it (the choice of
// sink is the first thing you want to know when the logs are not where you looked).
//
// It never fails the process: a log file that cannot be opened degrades to
// stderr-only with a WARN, because refusing to boot over an observability sink
// would make logging less safe than not having it.
func Setup() string {
	level := parseLevel(os.Getenv("CG_LOG_LEVEL"))
	// The pre-existing diagnostic switch. It used to turn on two hard-coded Info dumps
	// of its own; it now means what its name says, so nothing that documented it breaks.
	if os.Getenv("CONTEXT_GURU_DEBUG") != "" && os.Getenv("CG_LOG_LEVEL") == "" {
		level = slog.LevelDebug
	}
	if plain := os.Getenv("CG_LOG_PLAIN"); plain != "" && plain != "0" && plain != "false" {
		// The opt-out. Exactly the handler a program with no logging opinions would get:
		// no scrubbing, no file, no Loki. Named in the message because it disables the
		// credential scrubber, and that is not something to discover later.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		return "plain stdlib slog text handler on stderr (CG_LOG_PLAIN), level " +
			level.String() + "; credential scrubbing and the Loki file sink are OFF"
	}

	var w io.Writer = os.Stderr
	desc := "stderr"
	asJSON := strings.EqualFold(os.Getenv("CG_LOG_FORMAT"), "json")
	if path := os.Getenv("CG_LOG_FILE"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			// Install the stderr handler FIRST so this warning has somewhere to go.
			slog.SetDefault(slog.New(New(w, level, asJSON)))
			slog.Warn("logging: could not open CG_LOG_FILE; logging to stderr only "+
				"(nothing will reach Loki)", "path", path, "err", err)
			return "stderr only, level " + level.String() + " — CG_LOG_FILE failed to open"
		}
		// Both sinks, one handler: an operator reading `journalctl` and promtail reading
		// the file must see the SAME records, and two handlers over one record is how they
		// drift. The file is not closed — the process holds it for its lifetime, and
		// os.File writes are unbuffered syscalls, so there is nothing to flush.
		//
		// ponytail: no rotation here. logrotate with `copytruncate` handles it, and
		// promtail follows a truncated file; see deploy/grafana/context-guru.logrotate.
		w = io.MultiWriter(os.Stderr, f)
		asJSON = !strings.EqualFold(os.Getenv("CG_LOG_FORMAT"), "text")
		desc = "stderr + " + path
	}
	slog.SetDefault(slog.New(New(w, level, asJSON)))
	format := "text"
	if asJSON {
		format = "json"
	}
	return desc + ", level " + level.String() + ", format " + format
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New builds a scrubbing handler over w. Exported for tests and for a host that
// wants the scrubber without the environment.
func New(w io.Writer, level slog.Level, asJSON bool) slog.Handler {
	o := &slog.HandlerOptions{Level: level, ReplaceAttr: scrubAttr}
	if asJSON {
		return scrubber{slog.NewJSONHandler(w, o)}
	}
	return scrubber{slog.NewTextHandler(w, o)}
}

// scrubber removes credentials on the way OUT, so a careless
// `slog.Info("...", "header", h)` written a year from now cannot leak one. That
// direction matters: the alternative — trusting every call site to redact — is a
// rule that holds only until somebody who has not read this file adds a line.
//
// It is load-bearing rather than decorative, because the proxy forwards the
// CALLER'S OWN provider key: request handling has credentials in hand on every
// request, in headers, in upstream URLs and in error strings.
//
// Two halves, both needed:
//
//   - ReplaceAttr (scrubAttr) covers attribute VALUES, including the ones baked in
//     by Logger.With — the built-in handlers run it at WithAttrs time as well as
//     per record, so a per-request logger's attrs are scrubbed once rather than
//     never.
//   - This wrapper covers the MESSAGE, which ReplaceAttr never sees. A message is
//     usually a constant, but `slog.Info("upstream "+url+" failed")` is the shape
//     that leaks userinfo, and it costs one prefiltered pass to close.
type scrubber struct{ h slog.Handler }

func (s scrubber) Enabled(ctx context.Context, l slog.Level) bool { return s.h.Enabled(ctx, l) }

func (s scrubber) Handle(ctx context.Context, r slog.Record) error {
	if m := redact.Content(r.Message, 0); m != r.Message {
		r.Message = m
	}
	return s.h.Handle(ctx, r)
}

// WithAttrs/WithGroup must re-wrap: returning the inner handler would silently drop
// the scrubber for every logger derived from this one, which is every per-request
// logger in the process.
func (s scrubber) WithAttrs(as []slog.Attr) slog.Handler { return scrubber{s.h.WithAttrs(as)} }
func (s scrubber) WithGroup(n string) slog.Handler       { return scrubber{s.h.WithGroup(n)} }

// scrubAttr redacts an attribute by KEY NAME first (an `api_key` attr is a
// credential whatever its value looks like) and then by VALUE SHAPE (a bearer
// token in a header dump under an innocent key).
//
// Numeric, boolean, duration and time values are passed through untouched: they
// cannot carry a credential, and this runs on every attribute of every record, so
// the cheap cases must stay cheap.
func scrubAttr(_ []string, a slog.Attr) slog.Attr {
	if redact.IsSecretKey(a.Key) {
		return slog.String(a.Key, redact.Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redact.Content(a.Value.String(), 0))
	case slog.KindAny:
		// Headers, errors, structs, maps. Stringify and scrub: a *url.Error stringifies
		// as `Post "<upstream URL>": ...` and an http.Header prints its values, so this
		// is the case that actually leaks. The formatting loss (a struct becomes its
		// Go-syntax string) is worth a leak that cannot happen.
		v := a.Value.Any()
		if err, ok := v.(error); ok {
			return slog.String(a.Key, redact.Content(err.Error(), 0))
		}
		return slog.String(a.Key, redact.Content(fmt.Sprint(v), 0))
	}
	return a
}

// Per-request correlation. Every line of one request's lifecycle carries the same
// tenant and session, so `{tenant="…"} | json | session="…"` in Loki is the whole
// investigation. The logger travels in the context rather than in a parameter
// because the request path already threads a context everywhere and the pipeline's
// components receive one — adding a logger parameter to every signature between the
// HTTP handler and a component is the churn this avoids.
//
// "The request's lifecycle" outlives the *http.Request: observe mode's pipeline run
// happens on a worker pool AFTER the response is written, under a context that is
// deliberately not the request's (the request's is already cancelled). Work that
// crosses that boundary must lift the logger out with From and re-attach it with
// With on the far side — a plain value that is safe to outlive the request, since
// only cancellation belongs to the context it came from. apply also stamps `mode`,
// so an observe projection is never mistaken for, or summed with, an enforced run.
//
// Three classes of line are NOT request-scoped and carry no tenant, by nature
// rather than by omission:
//
//   - anything logged before authentication resolves one — an unknown or revoked
//     token has no tenant to attribute the refusal to;
//   - startup and configuration lines, which describe the process;
//   - a resource's own lines about itself: the off-path pool's "queue full" and its
//     last-resort panic recovery name the job whose key names the tenant, not the
//     tenant as a field, because the pool holds jobs rather than requests.
type ctxKey struct{}

// With returns a context carrying l, for From to find downstream.
func With(ctx context.Context, l *slog.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, l)
}

// From returns the request's logger, or the process default when there is none —
// so a library caller who never set one up still gets working logs, and no call
// site needs a nil check.
func From(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
			return l
		}
	}
	return slog.Default()
}

// Debugging reports whether DEBUG records are being emitted, for guarding a payload
// that costs something to build. The rule in this codebase: if the arguments to a
// DEBUG call are more than field reads, guard them with this.
func Debugging(ctx context.Context) bool {
	return From(ctx).Enabled(ctx, slog.LevelDebug)
}
