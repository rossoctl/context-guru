package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/classifier"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/internal/logging"
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

// routeClassifier answers an auto-mode classifier request with the routed model, and reports
// whether it did. false means "carry on with the normal path", and the request is untouched: with
// no active `classifier` row it returns before reading anything, so a proxy without a routing file
// forwards byte for byte what it did before this feature.
//
// It runs before the pipeline on purpose. A classifier request is tiny and the pipeline has
// nothing to offer it, but the dashboard row is still written (cp), so the request does not
// vanish from the cost view.
//
// FAIL OPEN: a panic, a conversion error or an inaccessible route all return false, and the
// original request goes upstream unchanged. Nothing is written to w until the answer is in hand.
func (h *Handler) routeClassifier(w http.ResponseWriter, r *http.Request, provider bschemas.ModelProvider,
	api string, tn *Tenancy, body []byte) (served bool) {
	if provider != bschemas.Anthropic || api != "" {
		return false
	}
	route, ok := h.routes.Lookup(classifier.Feature)
	if !ok {
		return false
	}
	defer func() {
		if rec := recover(); rec != nil {
			logging.From(r.Context()).Error("cg.classifier: recovered from panic; sending original", "panic", rec)
			h.routes.Record(classifier.Feature, routing.FellBack, 0)
			served = false
		}
	}()
	if !classifier.Detect(r.Header, body) {
		h.routes.Record(classifier.Feature, routing.NotDetected, 0)
		return false
	}
	lg := logging.From(r.Context())
	start := time.Now()
	res, err := classifier.Redirect(r.Context(), h.dispatcher(), route, body, classifier.Options{
		Budget: h.opts.ClassifierBudget})
	if err != nil {
		// A line without the content, the key or the URL: the error names a status or a transport
		// cause only.
		lg.Warn("cg.classifier: routed model unusable; sending original", "model", route.Model,
			"ms", msSince(start, time.Time{}), "err", err)
		h.routes.Record(classifier.Feature, routing.FellBack, 0)
		return false
	}
	orig := gjson.GetBytes(body, "model").String()
	saved := h.routedSaving(r.Context(), orig, res.Usage, route.Price)
	h.routes.Record(classifier.Feature, routing.Routed, saved)
	lg.Info("cg.classifier", "routed_model", route.Model, "original_model", orig,
		"attempts", res.Attempts, "ms", msSince(start, time.Time{}), "saved_usd", saved)

	w.Header().Set("Content-Type", "application/json")
	w.Write(res.Body)

	// The dashboard row: the routed model and its tokens. Its cost is whatever the price table
	// knows for that model; the saving is on /stats only (see routedSaving).
	cp := h.newCapture(r, string(provider), "/v1/messages", tn)
	cp.noteModel(route.Model)
	cp.noteMeta(metaFromBody(body))
	cp.noteUpstream(float64(time.Since(start).Microseconds())/1000.0, http.StatusOK)
	cp.finish(Usage{FreshInput: res.Usage.Fresh, CacheRead: res.Usage.CacheRead,
		CacheWrite: res.Usage.CacheWrite, Output: res.Usage.Output,
		StopReason: gjson.GetBytes(res.Body, "stop_reason").String()},
		true, false, h.contentCap(), h.contentMax())
	return true
}

// routedSaving is what the call would have cost at the original model's price, minus what it cost
// at the row's price. Both use the routed call's own token counts: the original model never ran,
// so its usage is not known, and the same counts are the fair comparison.
//
// An original model the price table does not know saves 0, never a guess. A row with no prices
// costs 0, so its saving is the whole original cost. Negative when the row is dearer than the
// original; that is a true number and is kept.
func (h *Handler) routedSaving(ctx context.Context, origModel string, u classifier.Usage, routed dispatch.Price) float64 {
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
