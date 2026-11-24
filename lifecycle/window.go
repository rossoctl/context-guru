package lifecycle

import (
	"context"

	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// ResolveWindow is a model's context window and whether the figure is published for that model
// rather than guessed from its family. exact is false for both "unknown" and "a guess": a caller
// that must not act on a guess must not act on an unknown either. A nil resolver gives (0, false).
// Every path of every host reads the window here, so they cannot disagree.
func ResolveWindow(ctx context.Context, r modelinfo.Resolver, model string) (window int, exact bool) {
	if r == nil {
		return 0, false
	}
	w, ex, ok := modelinfo.Exact(r, ctx, model)
	if !ok {
		return 0, false
	}
	return w, ex
}

// ResolveCompactionPoint is C for a model: the compaction point the operator states for it, else the
// built-in table of internal/compactionpoint. window is ResolveWindow's figure.
func ResolveCompactionPoint(ctx context.Context, r modelinfo.Resolver, model string, window int) compactionpoint.Point {
	stated := 0
	if n, found := modelinfo.OperatorCompactionPoint(r, ctx, model); found {
		stated = n
	}
	return compactionpoint.Resolve(model, stated, window)
}
