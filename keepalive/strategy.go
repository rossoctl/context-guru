package keepalive

import (
	"fmt"
	"time"

	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/tenant"
)

// Manager-controlled keep-alive strategies: pick the one that applies to an account, and write it
// onto a Policy. A strategy is a durable, manager-authored rule that sits above the account's own
// config and below a per-session override (see Resolve). The host keeps the strategy list, its
// lock, its storage and the control routes. The decisions live here, so every host that calls
// Resolve gets the same pings.

// BestStrategy returns the highest-priority ACTIVE strategy of the list that matches tenantID at
// now, or nil. With ignoreWindows the time windows are ignored (#858): the strategy that targets
// the tenant applies as if its window were open. BetterStrategy picks among several.
func BestStrategy(strategies []tenant.Strategy, tenantID string, now time.Time, ignoreWindows bool) *tenant.Strategy {
	var best *tenant.Strategy
	for i := range strategies {
		s := &strategies[i]
		if ignoreWindows {
			if !s.Active || !s.Target.Covers(tenantID) {
				continue
			}
		} else if !s.Matches(now, tenantID) {
			continue
		}
		if best == nil || BetterStrategy(s, best) {
			best = s
		}
	}
	return best
}

// StrategyTZ is the timezone an account's keep-alive windows are written in: the first window of
// the highest-priority active strategy that targets it (at any time of day), else
// tenant.DefaultStrategyTZ. Reports that show time of day use it, so a cell matches a window the
// user sets.
func StrategyTZ(strategies []tenant.Strategy, tenantID string) string {
	var best *tenant.Strategy
	for i := range strategies {
		s := &strategies[i]
		if s.Active && s.Target.Covers(tenantID) && len(s.Windows) > 0 && (best == nil || BetterStrategy(s, best)) {
			best = s
		}
	}
	if best == nil {
		return tenant.DefaultStrategyTZ
	}
	return best.Windows[0].Zone()
}

// ApplyStrategy writes a strategy onto a policy and returns the strategy's id. A nil strategy
// leaves the policy as it was and returns "".
//
// A strategy forces KeepAlive on and replaces Idle, MaxPings, MinPrefixTokens, MaxUSDPerPing and
// the predictor. It can only turn the 1-hour-on-wait ping ON, never off (#1243), and the same for
// 1-hour-always (#1261): like KeepAlive and HeadTTL1h there is no "deny" mode. The ping needs the
// wait plan, which the account policy carries (WaitPlan) and a strategy does not touch.
func ApplyStrategy(pol Policy, best *tenant.Strategy) (Policy, string) {
	if best == nil {
		return pol, ""
	}
	pol.KeepAlive = true
	pol.Idle = time.Duration(best.IdleSeconds) * time.Second
	pol.MaxPings = best.MaxPings
	pol.MinPrefixTokens = best.MinPrefixTokens
	pol.MaxUSDPerPing = best.MaxUSDPerPing
	pol.PredictorID = best.PredictorID
	pol.PredictorThreshold = best.PredictorThreshold
	pol.OneHourOnWait = pol.OneHourOnWait || best.OneHourOnWait
	pol.OneHourAlways = pol.OneHourAlways || best.OneHourAlways
	return pol, best.ID
}

// ResolveHeadTTL is the write-tier half of the strategy: it resolves the strategy that matches at
// now (windows respected) and returns the account's own head-TTL fallback unchanged when none
// matches or the matching one does not ask for the 1-hour head. A strategy can only turn HeadTTL1h
// ON, never off. The ping half (Resolve) and this half resolve the same strategy, except that Resolve
// ignores the windows for a wait that is not for a person (#858) and this half never does: the
// head-TTL write tier is a cost decision taken at write time, before the response.
func ResolveHeadTTL(strategies []tenant.Strategy, tenantID string, now time.Time,
	accountHeadTTL1h bool, accountHeadTTLMinBytes int) (bool, int) {
	best := BestStrategy(strategies, tenantID, now, false)
	if best == nil || !best.HeadTTL1h {
		return accountHeadTTL1h, accountHeadTTLMinBytes
	}
	// A stored strategy keeps the old token-named field: the database column and the manager API
	// carry it. It is read as bytes at 4 per token, the rule the old gate used.
	return true, best.HeadTTLMinTokens * config.HeadTTLBytesPerToken
}

// BetterStrategy reports whether candidate beats current under the fixed precedence: a list-target
// strategy beats an all-target one (more specific wins); among equally specific matches, the most
// recently updated wins. Not configurable: one less knob a manager can get wrong.
func BetterStrategy(candidate, current *tenant.Strategy) bool {
	cSpecific := candidate.Target.Mode == tenant.TargetList
	curSpecific := current.Target.Mode == tenant.TargetList
	if cSpecific != curSpecific {
		return cSpecific
	}
	return candidate.UpdatedAt.After(current.UpdatedAt)
}

// The 1-hour tier's idle and ping band, parallel to the 5-minute band (MinOverrideIdle ..
// MaxOverrideIdle, MinOverridePings .. MaxOverridePings) but scaled to the hour-long lifetime a
// HeadTTL1h strategy holds.
//
// MaxOverrideIdle1h keeps the same 10-second margin MaxOverrideIdle leaves against the 5-minute
// lifetime, applied to the 1-hour one. Past it the first ping arrives after the lifetime has
// lapsed. MaxOverridePings1h is far tighter than MaxOverridePings: a caller-pays credential is
// held in memory for up to (MaxPings+1) x Idle. At the 1-hour tier the 5-minute count would hold
// it for up to half a day. 3 keeps the worst case at (3+1) x 3590 s, about 4 hours, and the 1-hour
// tier needs far fewer refreshes to hold the same span.
const (
	MaxOverrideIdle1h  = modelinfo.AnthropicCacheLifetime1h - 10*time.Second
	MaxOverridePings1h = 3
)

// ValidStrategyBounds checks the numeric fields of a strategy against the same bounds an override
// uses (ValidIdleBand, ValidPingBand): a strategy is a broader-blast-radius version of the same
// spend authorization, so it gets at least as tight a check. With headTTL1h the band widens to
// MaxOverrideIdle1h / MaxOverridePings1h, since a 1-hour hold cannot fit in the 5-minute band.
//
// MaxUSDPerPing is the one field an override does not expose; a strategy may set it because this
// is an audited manager action. It must be non-negative, and 0 means the package default
// (Policy.Ceiling).
//
// headTTLMinTokens is the stored, token-named field, read as tokens x 4 bytes of request body
// (ResolveHeadTTL). Once headTTL1h is set, 0 is refused: the apply gate treats 0 as "never
// upgrade", so such a strategy would pay the 1-hour ping schedule for a benefit that never comes.
func ValidStrategyBounds(idle time.Duration, pings, minPrefix int, maxUSDPerPing float64,
	headTTL1h bool, headTTLMinTokens int) error {
	maxIdle, maxPings := MaxOverrideIdle, MaxOverridePings
	lifetime := int(modelinfo.AnthropicCacheLifetime5m / time.Second)
	if headTTL1h {
		maxIdle, maxPings = MaxOverrideIdle1h, MaxOverridePings1h
		lifetime = int(modelinfo.AnthropicCacheLifetime1h / time.Second)
	}
	if err := validIdleBand(idle, maxIdle, lifetime); err != nil {
		return err
	}
	if pings < MinOverridePings || pings > maxPings {
		return fmt.Errorf("the ping count must be between %d and %d", MinOverridePings, maxPings)
	}
	if minPrefix < 0 {
		return fmt.Errorf("the prefix floor cannot be negative")
	}
	if maxUSDPerPing < 0 {
		return fmt.Errorf("the per-ping cost ceiling cannot be negative")
	}
	if headTTLMinTokens < 0 {
		return fmt.Errorf("the head-TTL token floor cannot be negative")
	}
	if headTTLMinTokens > config.MaxHeadTTLMinBytes/config.HeadTTLBytesPerToken {
		return fmt.Errorf("the head-TTL token floor is too large (at most %d)",
			config.MaxHeadTTLMinBytes/config.HeadTTLBytesPerToken)
	}
	if headTTL1h && headTTLMinTokens <= 0 {
		return fmt.Errorf("a strategy asking for the 1h head-TTL tier needs a positive token " +
			"floor; 0 means the upgrade never fires, which pays the 1h ping schedule for no benefit")
	}
	return nil
}

// validIdleBand is the one idle-interval check an override and a strategy share. Below
// MinOverrideIdle a ping lands inside the previous one's own refresh. Above maxIdle the first ping
// arrives after the provider's lifetime has lapsed and pays a cache WRITE instead of a read.
func validIdleBand(idle, maxIdle time.Duration, lifetimeSeconds int) error {
	if idle < MinOverrideIdle || idle > maxIdle {
		return fmt.Errorf(
			"the idle interval must be between %d and %d seconds; past %d the first ping "+
				"arrives after the provider's %d-second lifetime has already lapsed and pays "+
				"a cache WRITE instead of a read",
			int(MinOverrideIdle.Seconds()), int(maxIdle.Seconds()), int(maxIdle.Seconds()), lifetimeSeconds)
	}
	return nil
}

// KnownPredictorIDs is the set of predictor names a strategy may reference. It is kept in lockstep
// with the predictor switch by a test, so accepting a strategy at creation can never promise a
// gate the keeper cannot evaluate.
//
// "stop-reason-gated" is the only entry: write the five-minute tier on every request, ping while
// idle only when the request just served's stop_reason clusters as kvcache.ClusterActuallyDone.
// Measured (docs/results/kv-ttl-predictor-arms.md) as +1.54% vs fixed-5m pooled (CI95
// [0.60%, 2.79%]), not distinguishable from a trained logistic regression on the same window,
// which is why it is a rule rather than a model.
var KnownPredictorIDs = map[string]bool{"stop-reason-gated": true}

// ValidPredictorRef checks predictorID against KnownPredictorIDs (empty is always valid: no
// predictor gate) and the threshold's own shape.
//
// A named predictor with a threshold <= 0 is refused: the probability functions only return values
// in [0,1], so `p >= threshold` is true for every prediction at 0, and the gate would still ping
// on the arm's own schedule without gating anything.
func ValidPredictorRef(predictorID string, threshold float64) error {
	s := tenant.Strategy{PredictorID: predictorID, PredictorThreshold: threshold}
	if err := s.ValidatePredictor(); err != nil {
		return err
	}
	if predictorID != "" && !KnownPredictorIDs[predictorID] {
		return fmt.Errorf("%q is not a registered predictor; no predictor-gated strategies "+
			"can be created yet", predictorID)
	}
	if predictorID != "" && threshold <= 0 {
		return fmt.Errorf("a strategy naming a predictor needs a positive threshold; 0 makes " +
			"the gate a no-op, since every possible prediction satisfies \">= 0\"")
	}
	return nil
}

// ValidWindows checks that a strategy has at least one window (one with no schedule can never
// fire) and that each one is valid.
func ValidWindows(windows []tenant.Window) error {
	if len(windows) == 0 {
		return fmt.Errorf("a strategy needs at least one window; one with no schedule can never fire")
	}
	for _, w := range windows {
		if err := w.Validate(); err != nil {
			return err
		}
	}
	return nil
}
