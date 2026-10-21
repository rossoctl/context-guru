package proxy

import (
	"context"
	"log/slog"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/routing"
)

// buildRouting turns Options.Routing into the handler's table. The Pinner is where a row's key
// and base URL leave the routing package: they are pinned on the Dispatcher, which masks the key,
// and the table keeps only the Ref. With no rows the table is nil and every call below is a no-op.
func (h *Handler) buildRouting() {
	if len(h.opts.Routing) == 0 {
		return
	}
	d := h.dispatcher()
	h.routes = routing.Build(h.opts.Routing, h.opts.RoutingEnv, func(e routing.Endpoint) dispatch.Ref {
		prov := bschemas.OpenAI // openai and bob: Authorization: Bearer
		if e.Dialect == config.DialectAnthropic {
			prov = bschemas.Anthropic // x-api-key
		}
		return d.Pin(e.BaseURL, AuthHeader(prov, e.Key, ""))
	}, slog.Default())
}

// RoutedUsage is the token counts of one routed call, as the routed endpoint billed them.
type RoutedUsage struct {
	Fresh, CacheRead, CacheWrite, Output int64
}

// routedSaving is what the call would have cost at the original model's price, minus what it cost
// at the row's price. Both use the routed call's own token counts: the original model never ran,
// so its usage is not known, and the same counts are the fair comparison.
//
// An original model the price table does not know saves 0, never a guess. A row with no prices
// costs 0, so its saving is the whole original cost. Negative when the row is dearer than the
// original; that is a true number and is kept.
func (h *Handler) routedSaving(ctx context.Context, origModel string, u RoutedUsage, routed dispatch.Price) float64 {
	if h.opts.Prices == nil || strings.TrimSpace(origModel) == "" {
		return 0
	}
	p, ok := h.opts.Prices.Price(ctx, origModel)
	if !ok || p.Zero() {
		return 0
	}
	return p.Cost(u.Fresh, u.CacheRead, u.CacheWrite, u.Output) -
		routed.Cost(u.Fresh, u.CacheRead, u.CacheWrite, u.Output)
}
