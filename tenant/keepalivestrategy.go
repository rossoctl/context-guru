package tenant

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	// The zoneinfo database, compiled in. Window.contains resolves an IANA name
	// (Asia/Jerusalem by default) with time.LoadLocation, and the deployed image
	// (debian:bookworm-slim, see the Dockerfile) does not install the tzdata package —
	// so without this, every window would fail to load its zone and a manager's
	// schedule would silently never fire. One blank stdlib import buys the database
	// rather than a system package this repo does not otherwise depend on.
	_ "time/tzdata"
)

// Manager-controlled keep-alive strategies: a durable, manager-authored rule that
// overrides a tenant's own account-wide keep-alive switch when it matches a live
// request. See docs/superpowers/specs/2026-08-25-keepalive-strategies-design.md for the
// full design and proxy/keepalivestrategy.go for how a Strategy is resolved against a
// request and validated before it is written here.
//
// Persisted in THIS registry, not in dash's request-metrics database: a strategy is a
// standing rule a manager expects to survive a deploy, the same durability class as a
// tenant's own configuration document — unlike a per-session override
// (proxy/keepaliveoverride.go), which is deliberately memory-only.

// Window is one recurring span a strategy is active in, evaluated in its own timezone.
// Days empty means every day. Start/End are "HH:MM" (24-hour), and End must be strictly
// after Start: an overnight window is out of scope (see the design doc's "Out of
// scope").
type Window struct {
	Days  []time.Weekday `json:"days,omitempty"`
	Start string         `json:"start"`
	End   string         `json:"end"`
	// TZ is an IANA zone name. Empty resolves to DefaultStrategyTZ.
	TZ string `json:"tz,omitempty"`
}

// DefaultStrategyTZ is what an unset Window.TZ resolves to.
const DefaultStrategyTZ = "Asia/Jerusalem"

// Target selects which tenants a strategy applies to.
type Target struct {
	Mode      string   `json:"mode"` // TargetAll | TargetList
	TenantIDs []string `json:"tenant_ids,omitempty"`
}

const (
	TargetAll  = "all"
	TargetList = "list"
)

// Strategy is a manager-authored keep-alive rule — see the design doc's "Model".
type Strategy struct {
	ID              string
	Name            string
	IdleSeconds     int
	MaxPings        int
	MinPrefixTokens int
	MaxUSDPerPing   float64
	Windows         []Window
	Target          Target
	Active          bool
	// PredictorID optionally names a registered kvcache.Predictor that must also clear
	// PredictorThreshold before a ping fires, on top of the window/target match above.
	// "" (the default) means no predictor gate at all — a strategy with an empty
	// PredictorID behaves exactly as every strategy did before this field existed.
	// Resolving the id to an actual Predictor implementation, and refusing an unknown
	// one, is proxy's job (this package only stores the reference); see
	// proxy/keepalivestrategy.go.
	PredictorID string
	// PredictorThreshold is the minimum probability PredictorID must return for a ping
	// to fire. Ignored while PredictorID is "".
	PredictorThreshold float64
	// HeadTTL1h asks for the one-hour tier on the head breakpoints, the same knob a
	// tenant's own account config exposes (config.CacheConfig.HeadTTL1h) — see
	// apply/headttl.go. False (the default) leaves the account's own setting in force;
	// a strategy can only turn this ON, never off, matching how it only ever turns
	// KeepAlive on (proxy/keepalivestrategy.go's applyStrategy).
	HeadTTL1h bool
	// HeadTTLMinTokens gates HeadTTL1h on request size. Ignored while HeadTTL1h is false.
	HeadTTLMinTokens int
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedBy        string
	UpdatedAt        time.Time
}

// StrategyPatch is a sparse update, matching Patch's own pointer convention: a nil
// field is left alone.
type StrategyPatch struct {
	Name               *string
	IdleSeconds        *int
	MaxPings           *int
	MinPrefixTokens    *int
	MaxUSDPerPing      *float64
	Windows            *[]Window
	Target             *Target
	Active             *bool
	PredictorID        *string
	PredictorThreshold *float64
	HeadTTL1h          *bool
	HeadTTLMinTokens   *int
}

// ErrNoStrategy names no keep-alive strategy.
var ErrNoStrategy = errors.New("tenant: no such keep-alive strategy")

// tzOrDefault is w.TZ, or DefaultStrategyTZ when it is empty.
func (w Window) tzOrDefault() string {
	if w.TZ == "" {
		return DefaultStrategyTZ
	}
	return w.TZ
}

// parseHHMM parses "HH:MM" into minutes since midnight, rejecting anything else —
// including a value Sscanf would happily half-parse.
func parseHHMM(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("tenant: %q is not an HH:MM time", s)
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("tenant: %q is not a valid HH:MM time", s)
	}
	// "24:00" IS ACCEPTED, as END OF DAY, and only in that exact spelling.
	//
	// It exists because contains() treats End as exclusive — which is what lets hour tiles abut
	// without double-covering the shared minute — and that left the final minute of the day
	// unreachable: "23:59" as an exclusive end stops at 23:58:59.999. So an all-day window could
	// not be written at all, and the one people did write silently excluded the minute they meant.
	//
	// 1440 is out of the 0..1439 range a wall-clock minute occupies, so it cannot collide with a
	// real time, and Validate's "End strictly after Start" keeps working unchanged. Only 24:00 is
	// allowed past 23:59 — 24:01 and 25:00 stay errors, because they mean nothing.
	if h == 24 && m == 0 {
		return 24 * 60, nil
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("tenant: %q is not a valid HH:MM time", s)
	}
	return h*60 + m, nil
}

// Validate checks this window's own shape: parseable HH:MM bounds, End strictly after
// Start (an overnight window is out of scope), a loadable IANA timezone, and weekdays
// in range. It does not know the keep-alive's own spend-authorization bounds
// (Idle/MaxPings/…) — that check lives in proxy, alongside validOverride's bounds.
func (w Window) Validate() error {
	start, err := parseHHMM(w.Start)
	if err != nil {
		return err
	}
	end, err := parseHHMM(w.End)
	if err != nil {
		return err
	}
	if end <= start {
		return fmt.Errorf("tenant: a window's end (%s) must be after its start (%s); "+
			"an overnight window is not supported", w.End, w.Start)
	}
	if _, err := time.LoadLocation(w.tzOrDefault()); err != nil {
		return fmt.Errorf("tenant: %q is not a known timezone: %w", w.tzOrDefault(), err)
	}
	for _, d := range w.Days {
		if d < time.Sunday || d > time.Saturday {
			return fmt.Errorf("tenant: %d is not a day of the week", d)
		}
	}
	return nil
}

// contains reports whether `now`, converted to this window's timezone, falls inside it.
func (w Window) contains(now time.Time) (bool, error) {
	loc, err := time.LoadLocation(w.tzOrDefault())
	if err != nil {
		return false, err
	}
	local := now.In(loc)
	if len(w.Days) > 0 {
		match := false
		for _, d := range w.Days {
			if local.Weekday() == d {
				match = true
				break
			}
		}
		if !match {
			return false, nil
		}
	}
	start, err := parseHHMM(w.Start)
	if err != nil {
		return false, err
	}
	end, err := parseHHMM(w.End)
	if err != nil {
		return false, err
	}
	mins := local.Hour()*60 + local.Minute()
	// THE END IS EXCLUSIVE, so adjacent windows tile without overlapping: 09:00-11:00 and
	// 11:00-12:00 cover 11:00 exactly once, which is what proxy/campaign.go's hour tiler
	// depends on.
	//
	// "24:00" IS THE END OF THE DAY, and it exists because exclusivity alone leaves the last
	// minute of the day unreachable. There is no larger HH:MM to write, so an all-day window had
	// to be spelled "00:00".."23:59" and then silently excluded 23:59 itself — the minute it most
	// obviously meant to cover. That made
	// TestKeepAliveStrategyControlRoutesResolveLiveWithNoRestart fail for ONE MINUTE IN 1440: CI
	// hit 20:59:11Z, which is 23:59 in DefaultStrategyTZ, so mins was 1439 and end was 1439.
	// campaign.go's tileHours already documented the same hole as "a known, documented gap in
	// tenant.Window itself".
	//
	// Writing 1440 for "24:00" closes it without touching exclusivity: every existing window
	// keeps its exact meaning, and a caller who means all day can now say so.
	// BOTH ENDS INCLUSIVE, and the end is the reason. A window is written in wall-clock
	// minutes, so "00:00".."23:59" is how a person says "all day" — there is no "24:00" to
	// write instead. With an exclusive end that window excluded 23:59 itself, i.e. the one
	// minute it most obviously meant to cover, and so did every window ending on the minute
	// somebody chose as its last.
	//
	// This is not a hypothetical: it made TestKeepAliveStrategyControlRoutesResolveLiveWithNoRestart
	// fail on main for exactly one minute a day. CI hit it at 20:59:11Z, which is 23:59 in
	// Asia/Jerusalem (DefaultStrategyTZ), so mins was 1439 and end was 1439 — and 1439 < 1439
	// is false. One minute in 1440 is precisely how a bug like this survives review.
	//
	// The cost of inclusivity is that a window ending 17:00 now covers 17:00:00-17:00:59. That
	// is the reading a person expects from a minute-granularity field, and two adjacent windows
	// (..17:00 and 17:00..) overlapping for that minute is harmless here: InWindow is an OR over
	// windows, so an instant covered twice is covered once.
	return mins >= start && mins < end, nil
}

// ValidatePredictor checks only this strategy's own shape: a threshold in [0,1], and only
// when a predictor is actually named — an unset PredictorID with a nonzero, unused
// threshold left over from a previous edit is not an error, since it is ignored either
// way. Whether PredictorID actually NAMES a registered kvcache.Predictor is not knowable
// here (this package has no registry of them) — that check belongs to the caller that
// does, exactly as an unknown strategy name is refused by kvcache.NewStrategy rather than
// silently defaulted. See proxy/keepalivestrategy.go.
func (s Strategy) ValidatePredictor() error {
	if s.PredictorID == "" {
		return nil
	}
	if s.PredictorThreshold < 0 || s.PredictorThreshold > 1 {
		return fmt.Errorf("tenant: a predictor threshold must be between 0 and 1, got %v",
			s.PredictorThreshold)
	}
	return nil
}

// Validate checks the target's own shape: a known mode, and at least one tenant id for
// a list-target strategy (an empty list matches nothing, which is never what was meant).
func (t Target) Validate() error {
	switch t.Mode {
	case TargetAll:
		return nil
	case TargetList:
		if len(t.TenantIDs) == 0 {
			return fmt.Errorf("tenant: a list-target strategy needs at least one tenant id")
		}
		return nil
	default:
		return fmt.Errorf("tenant: target mode must be %q or %q", TargetAll, TargetList)
	}
}

func (t Target) matches(tenantID string) bool {
	switch t.Mode {
	case TargetAll:
		return true
	case TargetList:
		for _, id := range t.TenantIDs {
			if id == tenantID {
				return true
			}
		}
	}
	return false
}

// InWindow reports whether this strategy's schedule covers `now`, ignoring Target and
// Active — the "would this fire right now, for whoever it targets" flag the strategies
// list shows at a glance.
func (s Strategy) InWindow(now time.Time) bool {
	for _, w := range s.Windows {
		if ok, err := w.contains(now); err == nil && ok {
			return true
		}
	}
	return false
}

// Matches reports whether this strategy applies to tenantID right now: Active, the
// target covers tenantID, and now falls inside one of its windows — see the design
// doc's "Model" for the precise rule.
func (s Strategy) Matches(now time.Time, tenantID string) bool {
	if !s.Active || !s.Target.matches(tenantID) {
		return false
	}
	return s.InWindow(now)
}

// CreateStrategy inserts a new strategy, stamping its id and both timestamps. The
// caller (proxy's control route) validates the numeric bounds — the keep-alive's own
// spend-authorization limits, which this package does not know about — and every
// window and the target before calling this.
func (r *Registry) CreateStrategy(actorID string, s Strategy) (Strategy, error) {
	if strings.TrimSpace(s.Name) == "" {
		return Strategy{}, fmt.Errorf("tenant: a strategy needs a name")
	}
	windowsJSON, err := json.Marshal(s.Windows)
	if err != nil {
		return Strategy{}, err
	}
	targetJSON, err := json.Marshal(s.Target)
	if err != nil {
		return Strategy{}, err
	}
	now := time.Now()
	s.ID = newID()
	s.CreatedBy, s.UpdatedBy = actorID, actorID
	s.CreatedAt, s.UpdatedAt = now, now
	if _, err := r.db.Exec(`INSERT INTO keepalive_strategies
	  (id,name,idle_seconds,max_pings,min_prefix_tokens,max_usd_per_ping,windows_json,
	   target_json,active,predictor_id,predictor_threshold,head_ttl_1h,head_ttl_min_tokens,
	   created_by,created_at,updated_by,updated_at)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Name, s.IdleSeconds, s.MaxPings, s.MinPrefixTokens, s.MaxUSDPerPing,
		string(windowsJSON), string(targetJSON), boolInt(s.Active), s.PredictorID,
		s.PredictorThreshold, boolInt(s.HeadTTL1h), s.HeadTTLMinTokens, s.CreatedBy,
		s.CreatedAt.UnixMilli(), s.UpdatedBy, s.UpdatedAt.UnixMilli()); err != nil {
		return Strategy{}, err
	}
	return s, nil
}

// UpdateStrategy applies a sparse patch and returns the resolved strategy.
func (r *Registry) UpdateStrategy(actorID, id string, p StrategyPatch) (Strategy, error) {
	s, err := r.StrategyByID(id)
	if err != nil {
		return Strategy{}, err
	}
	if p.Name != nil {
		if strings.TrimSpace(*p.Name) == "" {
			return Strategy{}, fmt.Errorf("tenant: a strategy needs a name")
		}
		s.Name = *p.Name
	}
	if p.IdleSeconds != nil {
		s.IdleSeconds = *p.IdleSeconds
	}
	if p.MaxPings != nil {
		s.MaxPings = *p.MaxPings
	}
	if p.MinPrefixTokens != nil {
		s.MinPrefixTokens = *p.MinPrefixTokens
	}
	if p.MaxUSDPerPing != nil {
		s.MaxUSDPerPing = *p.MaxUSDPerPing
	}
	if p.Windows != nil {
		s.Windows = *p.Windows
	}
	if p.Target != nil {
		s.Target = *p.Target
	}
	if p.Active != nil {
		s.Active = *p.Active
	}
	if p.PredictorID != nil {
		s.PredictorID = *p.PredictorID
	}
	if p.PredictorThreshold != nil {
		s.PredictorThreshold = *p.PredictorThreshold
	}
	if p.HeadTTL1h != nil {
		s.HeadTTL1h = *p.HeadTTL1h
	}
	if p.HeadTTLMinTokens != nil {
		s.HeadTTLMinTokens = *p.HeadTTLMinTokens
	}
	s.UpdatedBy, s.UpdatedAt = actorID, time.Now()
	windowsJSON, err := json.Marshal(s.Windows)
	if err != nil {
		return Strategy{}, err
	}
	targetJSON, err := json.Marshal(s.Target)
	if err != nil {
		return Strategy{}, err
	}
	if _, err := r.db.Exec(`UPDATE keepalive_strategies SET
	  name=?, idle_seconds=?, max_pings=?, min_prefix_tokens=?, max_usd_per_ping=?,
	  windows_json=?, target_json=?, active=?, predictor_id=?, predictor_threshold=?,
	  head_ttl_1h=?, head_ttl_min_tokens=?, updated_by=?, updated_at=? WHERE id=?`,
		s.Name, s.IdleSeconds, s.MaxPings, s.MinPrefixTokens, s.MaxUSDPerPing,
		string(windowsJSON), string(targetJSON), boolInt(s.Active), s.PredictorID,
		s.PredictorThreshold, boolInt(s.HeadTTL1h), s.HeadTTLMinTokens, s.UpdatedBy,
		s.UpdatedAt.UnixMilli(), s.ID); err != nil {
		return Strategy{}, err
	}
	return s, nil
}

// DeleteStrategy removes a strategy. It does not un-ping anything already sent — there
// is nothing to undo — it only stops the strategy matching new requests.
func (r *Registry) DeleteStrategy(id string) error {
	res, err := r.db.Exec(`DELETE FROM keepalive_strategies WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoStrategy
	}
	return nil
}

// StrategyByID reads one strategy.
func (r *Registry) StrategyByID(id string) (Strategy, error) {
	s, err := scanStrategy(r.db.QueryRow(
		`SELECT `+strategyCols+` FROM keepalive_strategies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Strategy{}, ErrNoStrategy
	}
	return s, err
}

// ListStrategies returns every strategy, newest first. Loaded into the keeper's memory
// at process start and refreshed on every write — see proxy/keepalivestrategy.go.
func (r *Registry) ListStrategies() ([]Strategy, error) {
	rows, err := r.db.Query(`SELECT ` + strategyCols + ` FROM keepalive_strategies ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Strategy{}
	for rows.Next() {
		s, err := scanStrategy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const strategyCols = `id,name,idle_seconds,max_pings,min_prefix_tokens,max_usd_per_ping,
	windows_json,target_json,active,predictor_id,predictor_threshold,head_ttl_1h,
	head_ttl_min_tokens,created_by,created_at,updated_by,updated_at`

func scanStrategy(sc scanner) (Strategy, error) {
	var out Strategy
	var windowsJSON, targetJSON string
	var active, headTTL1h int
	var createdAt, updatedAt int64
	if err := sc.Scan(&out.ID, &out.Name, &out.IdleSeconds, &out.MaxPings, &out.MinPrefixTokens,
		&out.MaxUSDPerPing, &windowsJSON, &targetJSON, &active, &out.PredictorID,
		&out.PredictorThreshold, &headTTL1h, &out.HeadTTLMinTokens, &out.CreatedBy, &createdAt,
		&out.UpdatedBy, &updatedAt); err != nil {
		return Strategy{}, err
	}
	out.HeadTTL1h = headTTL1h != 0
	if windowsJSON != "" {
		if err := json.Unmarshal([]byte(windowsJSON), &out.Windows); err != nil {
			return Strategy{}, err
		}
	}
	if targetJSON != "" {
		if err := json.Unmarshal([]byte(targetJSON), &out.Target); err != nil {
			return Strategy{}, err
		}
	}
	out.Active = active != 0
	out.CreatedAt, out.UpdatedAt = msTime(createdAt), msTime(updatedAt)
	return out, nil
}
