package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/context-guru/dash"
)

// Automatic campaign creation: POST /api/cache/campaigns/build fits a predictive keep-alive policy on ONE tenant's own history, replays it on a
// held-out last 30% of that history against every baseline, and stores the result as a campaign (spec cg.cache.campaign/2) that can then be run in
// shadow mode (decide on live traffic, change nothing) or active mode.
//
// # Why the build is a Python subprocess
//
// The build is training plus a replay. Training is three seeds of a 6K-parameter network on at most a few hundred thousand rows, and the replay is
// vectorised arithmetic; a Go port needs a gradient implementation, a bootstrap and the same feature code a second time, none of which runs on the
// request path. Inference (predictive/) IS Go and is tested for parity against the Python reference. The price is a runtime dependency (python3 with numpy,
// pandas and torch) on the machine that builds, not the machine that serves; set CG_CAMPAIGN_PYTHON to a venv's python. One build runs at a time,
// niced, with two threads, and a build holds no lock the proxy needs.

// campaignRoutes are appended to the control table (control.go); all manager-only.
func (h *Handler) cacheCampaignCtlRoutes() []ctlRoute {
	return []ctlRoute{
		{"POST /api/cache/campaigns/build", ctlManager, h.ctlBuildCacheCampaign},
		{"GET /api/cache/campaigns", ctlManager, h.ctlListCacheCampaigns},
		{"GET /api/cache/campaigns/{id}", ctlManager, h.ctlGetCacheCampaign},
		{"POST /api/cache/campaigns/{id}/mode", ctlManager, h.ctlSetCacheCampaignMode},
	}
}

var (
	campaignIDRe    = regexp.MustCompile(`^cmp-[0-9a-f]{12}$`)
	campaignScopeRe = regexp.MustCompile(`^[A-Za-z0-9._:@+\-]{0,128}$`)

	buildMu     sync.Mutex
	buildActive = map[string]bool{}
)

const buildTimeout = 45 * time.Minute

type cacheBuildIn struct {
	Scope     string          `json:"scope"`
	ID        string          `json:"id"`
	Window    json.RawMessage `json:"window"`
	Objective string          `json:"objective"`
	// Builder is "model" (the default: the Python pipeline trains a predictor) or "auto" (predictive.AutoTune picks a plan in-process, see campaignauto.go).
	Builder    string `json:"builder"`
	SessionEnd bool   `json:"session_end"` // auto only: score as if the client's SessionEnd signal is delivered
	// SegmentBy (auto only) tunes one plan per value of this feature (a key of segFeatures) instead of one per tenant.
	SegmentBy string `json:"segment_by,omitempty"`
	Via       string `json:"-"` // set in-process: "adapt" for a build an adapt apply started
}

// parseBuildWindow accepts {"days":N}, {"since":ms,"until":ms}, or "30d"; the default is the last 30 days.
func parseBuildWindow(raw json.RawMessage, now time.Time) (since, until int64, err error) {
	until = now.UnixMilli()
	if len(raw) == 0 || string(raw) == "null" {
		return until - 30*24*3600*1000, until, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		var n int
		if _, e := fmt.Sscanf(s, "%dd", &n); e != nil || n < 1 || n > 365 || !strings.HasSuffix(s, "d") {
			return 0, 0, fmt.Errorf(`window string must look like "30d" (1-365 days)`)
		}
		return until - int64(n)*24*3600*1000, until, nil
	}
	var w struct {
		Days  int   `json:"days"`
		Since int64 `json:"since"`
		Until int64 `json:"until"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return 0, 0, fmt.Errorf("window must be a string like \"30d\" or an object with days or since/until")
	}
	switch {
	case w.Days > 0:
		if w.Days > 365 {
			return 0, 0, fmt.Errorf("window.days is at most 365")
		}
		return until - int64(w.Days)*24*3600*1000, until, nil
	case w.Since > 0:
		if w.Until == 0 {
			w.Until = until
		}
		if w.Until <= w.Since {
			return 0, 0, fmt.Errorf("window.until must be after window.since")
		}
		return w.Since, w.Until, nil
	}
	return 0, 0, fmt.Errorf("window needs days or since")
}

func (h *Handler) managerOnly(w http.ResponseWriter, r *http.Request) (actorID string, ok bool) {
	actor, err := h.webPrincipal(r)
	if err != nil {
		code, msg := statusOf(err)
		ctlErr(w, code, msg)
		return "", false
	}
	if !actor.IsManager() {
		ctlErr(w, http.StatusForbidden, "manager only")
		return "", false
	}
	return actor.ID, true
}

func newCampaignID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "cmp-" + hex.EncodeToString(b[:])
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ctlBuildCacheCampaign starts a build and returns 202 with the campaign id to poll.
func (h *Handler) ctlBuildCacheCampaign(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.managerOnly(w, r)
	if !ok {
		return
	}
	var in cacheBuildIn
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	if in.Scope != "user" && in.Scope != "tenant" {
		ctlErr(w, http.StatusBadRequest, `scope must be "user" or "tenant" (in this service they are the same principal)`)
		return
	}
	if !campaignScopeRe.MatchString(in.ID) {
		ctlErr(w, http.StatusBadRequest, "id contains characters a tenant id never has")
		return
	}
	if in.Objective == "" {
		in.Objective = "net_usd"
	}
	if in.Objective != "net_usd" {
		ctlErr(w, http.StatusBadRequest, `objective must be "net_usd"`)
		return
	}
	if in.Builder == "" {
		in.Builder = "model"
	}
	if in.Builder != "model" && in.Builder != "auto" {
		ctlErr(w, http.StatusBadRequest, `builder must be "model" or "auto"`)
		return
	}
	if _, ok := segFeatures[in.SegmentBy]; in.SegmentBy != "" && (!ok || in.Builder != "auto") {
		ctlErr(w, http.StatusBadRequest, `segment_by needs builder "auto" and one of dow, hour, prefix, model, client, stop_reason`)
		return
	}
	since, until, err := parseBuildWindow(in.Window, time.Now())
	if err != nil {
		ctlErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, code, msg := h.startCacheBuild(r.Context(), actorID, in, since, until, nil)
	if code != http.StatusAccepted {
		ctlErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// startCacheBuild starts one build for in.ID and returns the 202 body, or an HTTP status and message. after, if set, runs when an auto build ends.
func (h *Handler) startCacheBuild(ctx context.Context, actorID string, in cacheBuildIn, since, until int64, after func(id string)) (map[string]any, int, string) {
	dir := h.campaignDir()
	if h.opts.Dashboard == nil || h.opts.Dashboard.DB() == nil || dir == "" {
		return nil, http.StatusServiceUnavailable, "campaign builds need the dashboard database, which is not enabled on this deployment"
	}
	models, err := h.opts.Dashboard.DB().WithContext(ctx).KVCacheModels(dash.Filter{Tenant: in.ID, Since: since, Until: until})
	if err != nil {
		slog.Error("context-guru: could not list models for a campaign build", "err", err)
		return nil, http.StatusInternalServerError, "could not read this tenant's history"
	}
	if len(models) == 0 {
		return nil, http.StatusNotFound, "no traffic for this scope in that window"
	}
	prices := map[string]map[string]float64{}
	for _, m := range models {
		if h.opts.Prices == nil {
			break
		}
		if p, ok := h.opts.Prices.Price(ctx, m); ok && !p.Zero() {
			prices[m] = map[string]float64{"in": p.Input, "out": p.Output, "cache_read": p.CacheRead, "cache_write_5m": p.CacheWrite}
		}
	}
	if len(prices) == 0 {
		return nil, http.StatusUnprocessableEntity, "none of this tenant's models has a price, so dollars cannot be computed"
	}
	buildMu.Lock()
	if len(buildActive) > 0 {
		buildMu.Unlock()
		return nil, http.StatusTooManyRequests, "another campaign build is running; builds run one at a time"
	}
	id := newCampaignID()
	buildActive[id] = true
	buildMu.Unlock()
	out := filepath.Join(dir, id)
	if err := os.MkdirAll(out, 0o700); err != nil {
		buildMu.Lock()
		delete(buildActive, id)
		buildMu.Unlock()
		return nil, http.StatusInternalServerError, "could not create the campaign directory"
	}
	meta := campaignMeta{ID: id, Tenant: in.ID, ScopeType: in.Scope, Mode: campaignModeOff, CreatedAt: time.Now().UnixMilli(), Actor: actorID, ModeBy: actorID, Via: in.Via}
	_ = writeJSONFile(filepath.Join(out, "meta.json"), meta)
	_ = writeJSONFile(filepath.Join(out, "request.json"), map[string]any{"scope": in.Scope, "id": in.ID, "since": since, "until": until, "objective": in.Objective, "builder": in.Builder, "session_end": in.SessionEnd, "segment_by": in.SegmentBy})
	_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "running", "stage": "starting"})
	pricesPath := filepath.Join(out, "prices.json")
	if err := writeJSONFile(pricesPath, prices); err != nil {
		buildMu.Lock()
		delete(buildActive, id)
		buildMu.Unlock()
		return nil, http.StatusInternalServerError, "could not write the price table"
	}
	if reg := h.registry(); reg != nil {
		if err := reg.AuditWrite(actorID, in.ID, "cache_campaign:"+id, "", "build requested"); err != nil {
			slog.Warn("context-guru: could not audit a campaign build", "err", err)
		}
	}
	if in.Builder == "auto" {
		go func() {
			h.runAutoBuild(id, out, in, since, until)
			if after != nil {
				after(id)
			}
		}()
	} else {
		go h.runCampaignBuild(id, out, in, since, until, pricesPath)
	}
	return map[string]any{"campaign_id": id, "state": "running", "poll": "/api/cache/campaigns/" + id,
		"scope": map[string]string{"type": in.Scope, "id": in.ID}, "window": map[string]int64{"since": since, "until": until}, "objective": in.Objective, "builder": in.Builder}, http.StatusAccepted, ""
}

// buildCommand is the program that builds. Overridable for tests and for deployments with the pipeline elsewhere.
func buildCommand() (name string, pre []string) {
	py := os.Getenv("CG_CAMPAIGN_PYTHON")
	if py == "" {
		py = "python3"
	}
	script := os.Getenv("CG_CAMPAIGN_SCRIPT")
	if script == "" {
		script = "scripts/campaign/build_campaign.py"
		if exe, err := os.Executable(); err == nil {
			if p := filepath.Join(filepath.Dir(exe), "scripts", "campaign", "build_campaign.py"); fileExists(p) {
				script = p
			}
		}
	}
	if n, err := exec.LookPath("nice"); err == nil {
		return n, []string{"-n", "19", py, script}
	}
	return py, []string{script}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func (h *Handler) runCampaignBuild(id, out string, in cacheBuildIn, since, until int64, pricesPath string) {
	defer func() { buildMu.Lock(); delete(buildActive, id); buildMu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	name, pre := buildCommand()
	// --name=value, so a value that starts with "-" cannot be taken for an option.
	args := append(pre, "--out="+out, "--scope="+in.Scope, "--id="+in.ID, "--since="+fmt.Sprint(since), "--until="+fmt.Sprint(until),
		"--requests-db="+h.opts.Dashboard.DB().Path(), "--prices="+pricesPath, "--objective="+in.Objective)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = buildEnv(os.Environ())
	if lf, err := os.Create(filepath.Join(out, "build.log")); err == nil {
		defer lf.Close()
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	err := cmd.Run()
	if err != nil {
		// The script writes its own failure status; this covers it never starting or being killed.
		st := readStatus(out)
		if st["state"] != "failed" {
			msg := "build did not finish: " + err.Error()
			if ctx.Err() != nil {
				msg = "build timed out"
			}
			_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "failed", "error": msg})
		}
		slog.Warn("context-guru: campaign build failed", "campaign", id, "err", err)
		return
	}
	// Re-validate what the script wrote through the same parser the runtime uses, so a malformed spec fails here and not at enable time.
	if _, _, _, lerr := readCampaignFiles(filepath.Dir(out), id); lerr != nil {
		_ = writeJSONFile(filepath.Join(out, "status.json"), map[string]any{"state": "failed", "error": "the built spec did not validate: " + lerr.Error()})
	}
}

func readStatus(dir string) map[string]any {
	st := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(dir, "status.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// campaignView is the wire shape of GET /api/cache/campaigns/{id}.
func (h *Handler) campaignView(id string, withSpec bool) (map[string]any, error) {
	dir := h.campaignDir()
	if !campaignIDRe.MatchString(id) || dir == "" {
		return nil, os.ErrNotExist
	}
	base := filepath.Join(dir, id)
	mb, err := os.ReadFile(filepath.Join(base, "meta.json"))
	if err != nil {
		return nil, os.ErrNotExist
	}
	var meta campaignMeta
	_ = json.Unmarshal(mb, &meta)
	st := readStatus(base)
	buildMu.Lock()
	running := buildActive[id]
	buildMu.Unlock()
	if st["state"] == "running" && !running {
		st = map[string]any{"state": "failed", "error": "the build was interrupted (the service restarted while it ran)"}
	}
	var req map[string]any
	if b, err := os.ReadFile(filepath.Join(base, "request.json")); err == nil {
		_ = json.Unmarshal(b, &req)
	}
	v := map[string]any{"id": id, "state": st["state"], "stage": st["stage"], "error": st["error"], "created_at": meta.CreatedAt,
		"scope": map[string]any{"type": meta.ScopeType, "id": meta.Tenant}, "window": map[string]any{"since": req["since"], "until": req["until"]}, "objective": req["objective"],
		"mode": meta.Mode, "recommendation": st["recommendation"], "seconds": st["seconds"]}
	if pc := h.predRT().forTenant(meta.Tenant); pc != nil && pc.meta.ID == id {
		s, mode := pc.snapshot()
		v["live"], v["mode"], v["shadow"] = true, mode, s
	} else {
		v["live"] = false
	}
	if st["state"] == "done" {
		if sb, err := os.ReadFile(filepath.Join(base, "spec.json")); err == nil {
			var spec map[string]any
			if json.Unmarshal(sb, &spec) == nil {
				v["summary"] = campaignSummary(spec)
				if withSpec {
					v["spec"] = spec
				}
			}
		}
	}
	return v, nil
}

// campaignSummary lifts the headline out of the evidence block: the model against the best simple baseline on the held-out slice.
func campaignSummary(spec map[string]any) map[string]any {
	ev, _ := spec["evidence"].(map[string]any)
	if ev == nil {
		return nil
	}
	out := map[string]any{"recommendation": ev["recommendation"], "oracle_net_usd": ev["oracle_net_usd"], "split": ev["split"], "class": ev["klass"], "reason": ev["reason"]}
	subject, _ := ev["subject"].(string) // the auto builder names its row; the model builder's is fixed
	if subject == "" {
		subject = "predictive(model)"
	}
	table, _ := ev["table"].([]any)
	for _, r := range table {
		row, _ := r.(map[string]any)
		if row["strategy"] == subject {
			out["model"] = row
		}
		if row["strategy"] == ev["best_simple_baseline"] {
			out["best_baseline"] = row
		}
	}
	paired, _ := ev["paired_vs_baselines"].([]any)
	for _, p := range paired {
		row, _ := p.(map[string]any)
		if row["vs"] == ev["best_simple_baseline"] {
			out["vs_best_baseline"] = row
		}
	}
	return out
}

func (h *Handler) ctlGetCacheCampaign(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.managerOnly(w, r); !ok {
		return
	}
	v, err := h.campaignView(r.PathValue("id"), true)
	if err != nil {
		ctlErr(w, http.StatusNotFound, "no such campaign")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) ctlListCacheCampaigns(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.managerOnly(w, r); !ok {
		return
	}
	out := []map[string]any{}
	for _, id := range listCampaignIDs(h.campaignDir()) {
		if v, err := h.campaignView(id, false); err == nil {
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": out})
}

type campaignModeIn struct {
	Mode  string `json:"mode"`
	Force bool   `json:"force"`
}

// ctlSetCacheCampaignMode switches a finished campaign between off, shadow and active. Active needs the build's own recommendation to be "ship" unless force is set.
func (h *Handler) ctlSetCacheCampaignMode(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.managerOnly(w, r)
	if !ok {
		return
	}
	var in campaignModeIn
	if err := readJSON(w, r, &in); err != nil {
		readErr(w, err)
		return
	}
	if in.Mode != campaignModeOff && in.Mode != campaignModeShadow && in.Mode != campaignModeActive {
		ctlErr(w, http.StatusBadRequest, `mode must be "off", "shadow" or "active"`)
		return
	}
	v, code, msg := h.setCampaignMode(actorID, r.PathValue("id"), in.Mode, in.Force)
	if code != http.StatusOK {
		ctlErr(w, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// campaignModeMu serialises campaign mode changes, so a check made just before one (setCampaignModeIf) still holds when it is written.
var campaignModeMu sync.Mutex

// setCampaignMode switches a finished campaign's mode (audited) and returns its view, or an HTTP status and message.
func (h *Handler) setCampaignMode(actorID, id, mode string, force bool) (map[string]any, int, string) {
	return h.setCampaignModeIf(actorID, id, mode, force, nil)
}

// setCampaignModeIf is setCampaignMode refused with 409 when refuse, asked under the same lock as the write, says so.
func (h *Handler) setCampaignModeIf(actorID, id, mode string, force bool, refuse func() (bool, string)) (map[string]any, int, string) {
	campaignModeMu.Lock()
	defer campaignModeMu.Unlock()
	if refuse != nil {
		if no, why := refuse(); no {
			return nil, http.StatusConflict, why
		}
	}
	v, err := h.campaignView(id, false)
	if err != nil {
		return nil, http.StatusNotFound, "no such campaign"
	}
	if v["state"] != "done" {
		return nil, http.StatusConflict, "the campaign build has not finished"
	}
	if mode == campaignModeActive && v["recommendation"] != "ship" && !force {
		return nil, http.StatusConflict, fmt.Sprintf("the build recommends %q, not ship; run it in shadow first, or pass force to override", v["recommendation"])
	}
	if mode != campaignModeOff && v["recommendation"] == "insufficient_data" {
		return nil, http.StatusConflict, "this scope has too little history to run a campaign"
	}
	dir := h.campaignDir()
	mb, _ := os.ReadFile(filepath.Join(dir, id, "meta.json"))
	var meta campaignMeta
	_ = json.Unmarshal(mb, &meta)
	before := meta.Mode
	meta.Mode, meta.ModeBy = mode, actorID
	if reg := h.registry(); reg != nil {
		if err := reg.AuditWrite(actorID, meta.Tenant, "cache_campaign:"+id, before, mode); err != nil {
			return nil, http.StatusInternalServerError, "could not record this in the audit log, so it was not changed"
		}
	} else {
		slog.Info("context-guru: cache campaign mode changed", "campaign", id, "from", before, "to", mode)
	}
	if err := writeJSONFile(filepath.Join(dir, id, "meta.json"), meta); err != nil {
		return nil, http.StatusInternalServerError, "could not save the mode"
	}
	rt := h.predRT()
	if rt == nil {
		return nil, http.StatusServiceUnavailable, "the keep-alive is not built into this deployment"
	}
	if err := rt.load(id); err != nil {
		return nil, http.StatusInternalServerError, "could not load the campaign: " + err.Error()
	}
	v, _ = h.campaignView(id, false)
	return v, http.StatusOK, ""
}

func (h *Handler) predRT() *predRuntime {
	if h.keeper == nil {
		return nil
	}
	return h.keeper.pred
}

// buildEnv is the environment of the build subprocess: the program search path, locale and temp dir, this feature's own settings and the thread limits. Nothing else,
// in particular none of the proxy's upstream or gateway credentials: the process runs numpy / pandas / lightgbm and an operator-set script.
func buildEnv(parent []string) []string {
	env := []string{"OMP_NUM_THREADS=2", "MKL_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2"}
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "LANG" || k == "LC_ALL" || k == "TMPDIR" || strings.HasPrefix(k, "CG_CAMPAIGN_") {
			env = append(env, kv)
		}
	}
	return env
}
