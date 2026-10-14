package proxy

// Who may call /api/adapt/*, and how an applied option becomes the caller's configuration.
// Shared by every adapt kind (compaction, tools, cache), so the token scope, the rate limit,
// the audit trail and the undo are one piece of code.
//
// Identity. Hosted: the dashboard cookie, OR the account's own proxy token — the second only
// here, because the plugin asks its question and applies the answer from the terminal, where
// there is no browser. The token reaches no other control-plane route, acts only on its own
// account (a manager's ?tenant= needs the cookie), and may only apply an option a report just
// offered that account (dash.API.AdaptOffer, AdaptOfferTTL). Single-tenant: loopback callers,
// the same boundary as the dashboard's own reads on a local proxy.
//
// Writing. Hosted: the registry's Update — the same validation and tenant_config_audit row as
// a settings save, and a rebuild of the account's pipeline on its next request. Single-tenant:
// an override document beside the dashboard database, and the pipeline is rebuilt in-process
// and swapped atomically, keeping the state store so offloaded originals still expand.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/tenant"
)

// adaptWho is an authenticated /api/adapt/* caller.
type adaptWho struct {
	// TenantID is whose configuration is read and written; "" on a single-tenant proxy.
	TenantID string
	actor    *tenant.Tenant // hosted: who acts (the account itself, or a manager)
	Local    bool
	ViaToken bool
}

var errAdaptLoopback = statusError{http.StatusForbidden, "this proxy has no accounts, so /api/adapt is answered only on loopback"}

// adaptPrincipal resolves the caller of an /api/adapt/* route.
func (h *Handler) adaptPrincipal(r *http.Request) (adaptWho, error) {
	reg := h.registry()
	if reg == nil {
		// A reverse proxy on the same box makes every remote caller loopback; one that relayed
		// a request is refused, the rule /stats uses.
		if !directLoopback(r) {
			return adaptWho{}, errAdaptLoopback
		}
		return adaptWho{Local: true}, nil
	}
	t, err := h.webPrincipal(r)
	if err == nil {
		who := adaptWho{TenantID: t.ID, actor: t}
		if q := r.URL.Query().Get("tenant"); q != "" && q != "me" && t.IsManager() {
			who.TenantID = q
		}
		return who, nil
	}
	tok := TokenFromRequest(r)
	if tok == "" {
		return adaptWho{}, err
	}
	t, err = reg.Resolve(tok)
	switch {
	case errors.Is(err, tenant.ErrDisabled):
		return adaptWho{}, tenantOff(err)
	case err != nil:
		return adaptWho{}, errBadToken
	}
	return adaptWho{TenantID: t.ID, actor: t, ViaToken: true}, nil
}

// adaptConfig is the caller's EFFECTIVE configuration document.
func (h *Handler) adaptConfig(who adaptWho) (string, error) {
	if who.Local {
		h.adaptMu.Lock()
		defer h.adaptMu.Unlock()
		if ov := h.loadAdaptOverride(); ov != nil {
			return ov.Doc, nil
		}
		return h.opts.BaseConfig, nil
	}
	t, err := h.registry().Get(who.TenantID)
	if err != nil {
		return "", statusError{http.StatusNotFound, "no such account"}
	}
	return h.registry().Config(t), nil
}

// Applies per account per window: enough to try options and revert, not enough to hammer the
// rebuild path.
const (
	adaptApplyBurst  = 6
	adaptApplyWindow = 10 * time.Minute
)

func (h *Handler) adaptAllow(key string) bool {
	now := time.Now()
	h.adaptMu.Lock()
	defer h.adaptMu.Unlock()
	if h.adaptHits == nil {
		h.adaptHits = map[string][]time.Time{}
	}
	keep := h.adaptHits[key][:0]
	for _, t := range h.adaptHits[key] {
		if now.Sub(t) < adaptApplyWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) >= adaptApplyBurst {
		h.adaptHits[key] = keep
		return false
	}
	h.adaptHits[key] = append(keep, now)
	return true
}

var errAdaptRate = statusError{http.StatusTooManyRequests, "too many changes in a few minutes; wait and try again"}

// adaptWrite applies edit to the caller's configuration document and makes the result the
// configuration. kind and option go to the audit trail.
func (h *Handler) adaptWrite(who adaptWho, kind, option string, edit func(doc string) (string, error)) (prev, next string, err error) {
	if !h.adaptAllow(who.TenantID) {
		return "", "", errAdaptRate
	}
	if who.Local {
		return h.adaptWriteLocal(kind, option, edit)
	}
	reg := h.registry()
	t, gerr := reg.Get(who.TenantID)
	if gerr != nil {
		return "", "", statusError{http.StatusNotFound, "no such account"}
	}
	prev = reg.Config(t)
	if next, err = edit(prev); err != nil {
		return "", "", err
	}
	if next == prev {
		return prev, next, nil
	}
	if err := reg.Update(who.actor, who.TenantID, tenant.Patch{ConfigYAML: &next}); err != nil {
		if errors.Is(err, tenant.ErrForbidden) {
			return "", "", statusError{http.StatusForbidden, "not permitted"}
		}
		return "", "", statusError{http.StatusBadRequest, err.Error()}
	}
	// The STORED documents, the pair Update's config_yaml row records, so revert can match them.
	h.adaptAudit(who, kind+" "+option, t.ConfigYAML, next)
	return prev, next, nil
}

// adaptRevert restores the configuration as it was before the latest configuration change,
// provided nothing changed it since — so it undoes an adapt apply and can never undo somebody
// else's later edit.
func (h *Handler) adaptRevert(who adaptWho, kind string) (string, error) {
	if !h.adaptAllow(who.TenantID) {
		return "", errAdaptRate
	}
	if who.Local {
		return h.adaptRevertLocal(kind)
	}
	reg := h.registry()
	t, err := reg.Get(who.TenantID)
	if err != nil {
		return "", statusError{http.StatusNotFound, "no such account"}
	}
	// Only an adapt apply of THIS kind that THIS caller made is undone, and only while it is still the latest
	// change: the newest config_yaml row must be one this actor's adapt apply wrote (its "adapt"
	// row carries the same actor, before and after), so a manager's edit — later, or made on the
	// account's behalf — can never be rolled back through this route.
	entries, err := reg.Audit(who.TenantID, 50)
	if err != nil {
		return "", err
	}
	for k, e := range entries {
		if e.Field != "config_yaml" {
			continue
		}
		ours := false
		for _, a := range entries[:k] {
			ours = ours || (a.Field == "adapt" && a.Actor == who.actor.ID && a.Before == docHash(e.Before) &&
				strings.HasPrefix(a.After, docHash(e.After)+" "+kind+" ") && !strings.Contains(a.After, " revert via "))
		}
		if !ours || e.After != t.ConfigYAML {
			return "", statusError{http.StatusConflict, "the last change to this configuration was not an adapt apply; nothing to undo"}
		}
		before := e.Before
		if err := reg.Update(who.actor, who.TenantID, tenant.Patch{ConfigYAML: &before}); err != nil {
			return "", statusError{http.StatusBadRequest, err.Error()}
		}
		h.adaptAudit(who, kind+" revert", e.After, before)
		return before, nil
	}
	return "", statusError{http.StatusConflict, "there is no change to undo"}
}

// adaptAudit records an adapt change beside the config_yaml row Update already wrote: which
// adapt kind and option, and whether a proxy token rather than a dashboard session did it.
func (h *Handler) adaptAudit(who adaptWho, what, before, after string) {
	how := "dashboard session"
	if who.ViaToken {
		how = "proxy token"
	}
	if err := h.registry().AuditWrite(who.actor.ID, who.TenantID, "adapt",
		docHash(before), docHash(after)+" "+what+" via "+how); err != nil {
		slog.Warn("context-guru: adapt audit row not written", "err", err)
	}
	slog.Info("context-guru: adapt change", "what", what, "tenant", who.TenantID, "actor", who.actor.ID,
		"via", how, "before", docHash(before), "after", docHash(after))
}

func docHash(doc string) string {
	sum := sha256.Sum256([]byte(doc))
	return fmt.Sprintf("%x", sum[:6])
}

// --- single-tenant override --------------------------------------------------------------

// adaptOverride is the file a single-tenant proxy keeps its applied configuration in.
type adaptOverride struct {
	// Base is docHash of the configuration the proxy was started with when this was written. A
	// different one at start means the user changed their preset or config file since, and
	// their newer choice wins: the override is ignored (and logged), never silently applied.
	Base     string             `json:"base"`
	Doc      string             `json:"doc"`
	Previous string             `json:"previous"`
	Log      []adaptOverrideLog `json:"log"`
}

type adaptOverrideLog struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Option string    `json:"option"`
	Before string    `json:"before"`
	After  string    `json:"after"`
}

// loadAdaptOverride reads the override, or nil when there is none that applies. Caller holds adaptMu.
func (h *Handler) loadAdaptOverride() *adaptOverride {
	p := h.opts.AdaptOverride
	if p == "" {
		return h.adaptMem
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var ov adaptOverride
	if json.Unmarshal(b, &ov) != nil || ov.Doc == "" || ov.Base != docHash(h.opts.BaseConfig) {
		return nil
	}
	return &ov
}

// saveAdaptOverride writes ov atomically (nil removes it). Caller holds adaptMu.
func (h *Handler) saveAdaptOverride(ov *adaptOverride) error {
	p := h.opts.AdaptOverride
	if p == "" {
		h.adaptMem = ov
		return nil
	}
	if ov == nil {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// reloadLocal rebuilds the running pipeline from doc and swaps it in. The state store and the
// observe-mode shadow are kept: they hold the originals behind markers already sent.
func (h *Handler) reloadLocal(doc string) error {
	if h.opts.BuildConfig == nil {
		return statusError{http.StatusNotImplemented, "this proxy cannot rebuild its pipeline; restart it with the new configuration"}
	}
	cur := h.static.Load()
	built, err := h.opts.BuildConfig([]byte(doc), cur.Pipe.Emitter())
	if err != nil {
		return statusError{http.StatusBadRequest, err.Error()}
	}
	t := *cur
	t.Pipe, t.Preset = built.Pipe, built.Preset
	h.static.Store(&t)
	return nil
}

func (h *Handler) adaptWriteLocal(kind, option string, edit func(string) (string, error)) (prev, next string, err error) {
	h.adaptMu.Lock()
	defer h.adaptMu.Unlock()
	ov := h.loadAdaptOverride()
	prev = h.opts.BaseConfig
	if ov != nil {
		prev = ov.Doc
	} else {
		ov = &adaptOverride{Base: docHash(h.opts.BaseConfig)}
	}
	if next, err = edit(prev); err != nil {
		return "", "", err
	}
	if next == prev {
		return prev, next, nil
	}
	if err := h.reloadLocal(next); err != nil {
		return "", "", err
	}
	ov.Previous, ov.Doc = prev, next
	ov.Log = append(ov.Log, adaptOverrideLog{At: time.Now().UTC(), Kind: kind, Option: option,
		Before: docHash(prev), After: docHash(next)})
	if err := h.saveAdaptOverride(ov); err != nil {
		return prev, next, statusError{http.StatusInternalServerError,
			"applied to the running proxy, but could not be saved for the next start: " + err.Error()}
	}
	slog.Info("context-guru: adapt applied", "kind", kind, "option", option, "tenant", "local",
		"before", docHash(prev), "after", docHash(next))
	return prev, next, nil
}

func (h *Handler) adaptRevertLocal(kind string) (string, error) {
	h.adaptMu.Lock()
	defer h.adaptMu.Unlock()
	ov := h.loadAdaptOverride()
	// The same rule as hosted: only the latest change, and only one this kind made.
	if ov == nil || ov.Previous == "" || len(ov.Log) == 0 || ov.Log[len(ov.Log)-1].Kind != kind {
		return "", statusError{http.StatusConflict, "the last change was not a " + kind + " apply; nothing to undo"}
	}
	restored := ov.Previous
	if err := h.reloadLocal(restored); err != nil {
		return "", err
	}
	ov.Log = append(ov.Log, adaptOverrideLog{At: time.Now().UTC(), Kind: kind, Option: "revert",
		Before: docHash(ov.Doc), After: docHash(restored)})
	ov.Doc, ov.Previous = restored, ""
	if restored == h.opts.BaseConfig {
		ov = nil // back to the configuration the proxy was started with
	}
	if err := h.saveAdaptOverride(ov); err != nil {
		return restored, statusError{http.StatusInternalServerError, "reverted the running proxy, but could not save it: " + err.Error()}
	}
	slog.Info("context-guru: adapt reverted", "kind", kind, "tenant", "local", "after", docHash(restored))
	return restored, nil
}

// startAdaptOverride applies a saved override at start. A stale or unreadable one is logged and
// ignored: the proxy runs the configuration it was started with.
func (h *Handler) startAdaptOverride() {
	if h.opts.Tenants != nil || h.opts.AdaptOverride == "" {
		return
	}
	h.adaptMu.Lock()
	defer h.adaptMu.Unlock()
	if _, err := os.Stat(h.opts.AdaptOverride); err != nil {
		return
	}
	ov := h.loadAdaptOverride()
	if ov == nil {
		slog.Warn("context-guru: adapt override ignored: the configuration this proxy was started "+
			"with changed since it was written", "path", h.opts.AdaptOverride)
		return
	}
	if err := h.reloadLocal(ov.Doc); err != nil {
		slog.Warn("context-guru: adapt override ignored", "path", h.opts.AdaptOverride, "err", err)
		return
	}
	slog.Info("context-guru: adapt override applied", "path", h.opts.AdaptOverride, "config", docHash(ov.Doc))
}
