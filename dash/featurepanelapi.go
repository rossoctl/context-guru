package dash

import (
	"net/http"

	"github.com/rossoctl/context-guru/kvcache"
	"github.com/rossoctl/context-guru/kvcache/predictor"
)

// fpCorrelationsPath is where deploy/harbor/kv_ttl_featcorr.py (a sibling job, not part of
// this change) writes its output. A constant rather than a query parameter: this is an
// operator-owned path on the host running the dashboard, not something a caller should be
// able to point at a different file.
const fpCorrelationsPath = "/home/vpcuser/cg-night-0928/exp4/featcorr/correlations.json"

// featurePanelRoutes is the feature/predictor inspection panel's one read route, declared
// beside its handler and appended in API.routes() for the same reason every other tab's
// routes are: that table is what TestEveryMountedRouteDeclaresItsScope and
// TestNoRouteServesContentTextFromAnUntrustedAddress both walk.
func (a *API) featurePanelRoutes() []route {
	return []route{
		{"GET /api/featurepanel", scopeTenant, a.featurePanel},
	}
}

// featurePanel serves the whole feature-browser + correlation + predictor-comparison payload
// for the caller's own tenant scope. ?session=<id> additionally fills each feature's Example
// from that one session's real, leak-free replay; without it the catalog is served with no
// examples rather than a made-up one. It returns numbers, enum labels and this file's own
// static prose only — no prompt text, no transcript, no tool schema.
func (a *API) featurePanel(w http.ResponseWriter, r *http.Request) {
	f, _, ok := a.scope(r)
	if !ok {
		a.unauthorized(w)
		return
	}
	freg := predictor.DefaultFeatures()
	preg := FeaturePanelDefaultPredictors()

	var reqs []*kvcache.Request
	if f.Session != "" {
		rows, _, err := a.db(r).KVCacheDataset(f, KVCacheOptions{})
		if err != nil {
			httpErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		reqs = rows
	}
	features, err := FeaturePanelWithExample(reqs, freg)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	cohort := f
	cohort.Session = ""
	predictors, err := a.db(r).FeaturePanelPredictors(cohort, KVCacheOptions{}, a.pricer, freg, preg)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, FeaturePanelPayload{
		Features:     features,
		Correlations: FeaturePanelCorrelations(fpCorrelationsPath),
		Predictors:   predictors,
	})
}

// FeaturePanelPayload is the whole /api/featurepanel response.
type FeaturePanelPayload struct {
	Features     []FPFeature             `json:"features"`
	Correlations FPCorrelations          `json:"correlations"`
	Predictors   []FPPredictorComparison `json:"predictors"`
}
