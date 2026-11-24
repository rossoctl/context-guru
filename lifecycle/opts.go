package lifecycle

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
)

// Request is what every call of the pipeline needs, however the host reaches it: the enforced
// request path, the off-path observation, and the /compact endpoint. One build of these fields
// keeps the three from drifting (they did once: /compact hard-coded the window as unknown).
type Request struct {
	Provider bschemas.ModelProvider
	API      string
	Body     []byte
	// Session is the host-supplied session id ("" means a content hash).
	Session string
	Tenant  string
	Models  components.ModelSpec
	// Window and WindowExact are ResolveWindow's answer. CompactionPoint and
	// CompactionPointSource are ResolveCompactionPoint's, as Point.Tokens and Point.Source.
	Window                int
	WindowExact           bool
	CompactionPoint       int
	CompactionPointSource string
	// CacheMode is "auto" | "on" | "off".
	CacheMode string
	Tracker   *modes.Tracker
}

// Opts is the part of apply.Opts all three callers share. A caller sets what is its own on the
// result: the bypass flag, the rate card, ThreadOf, the cache and keep-alive hooks, the mode.
func (q Request) Opts() apply.Opts {
	return apply.Opts{
		Provider: q.Provider, API: q.API, Body: q.Body, Session: q.Session, Tenant: q.Tenant,
		Models: q.Models, Window: q.Window, WindowExact: q.WindowExact, CacheMode: q.CacheMode,
		CompactionPoint: q.CompactionPoint, CompactionPointSource: q.CompactionPointSource,
		Tracker: q.Tracker,
	}
}
