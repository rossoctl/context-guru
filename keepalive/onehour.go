package keepalive

import (
	"log/slog"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// The 1-hour-on-wait ping (#1243).
//
// WHY. A ping that asks 1 hour on a prompt cached at 5 minutes can move the entry to 1 hour. On the
// gateway measured on 2026-11-19 (litellm 1.108.4, Bedrock) that switch is a READ, not a rewrite: 15 of
// 15 switches read the whole prefix and wrote only the new tokens at 1 hour. So one 1-hour ping at the
// start of a long wait keeps the prompt for 1 hour at the price of one read, and no request pays the 2x
// write premium. The last request that touches an entry sets its lifetime, so the client's own 5-minute
// request on return reads the entry and moves it back to 5 minutes.
//
// WHEN. Only on a LONG wait (LongWait), at the usual first-ping time, and it is the only ping of the
// wait. A short wait (a tool, a subagent) is plain auto.
//
// THE BODY DIFFERS FROM EVERY EARLIER BODY. Relabelling the markers changes the bytes, and the ping
// already varies its output budget (pingBodyN). litellm replays a byte-identical request from its own
// response cache (same message id, an `x-litellm-cache-key` header, about 170 ms, the OLD usage): such a
// "ping" would not reach Bedrock and would refresh nothing. A 1-hour ping body is never byte-identical
// to the real request it copies, because every marker changed.
//
// THE RUN-TIME CHECK. The result may differ on another provider route (Anthropic first-party may rewrite
// the whole prefix on the switch, per a public report that was not reproduced here). So the keeper reads
// the first 1-hour ping's usage (judgeOneHour). A read of about the whole prefix and a small write means
// the switch is free: the entry is a 1-hour entry until the next real request. A read near 0 and a large
// write means it cost a rewrite: the route is switched off for the life of the process, with one WARN,
// and its threads fall back to auto.

// oneHourRoute names the route a 1-hour ping went to, for the run-time check.
func oneHourRoute(provider bschemas.ModelProvider, endpoint, route string) string {
	return string(provider) + "|" + endpoint + "|" + route
}

// oneHourBlocked reports whether the run-time check switched the 1-hour ping off for this route.
func (k *Keeper) oneHourBlocked(provider bschemas.ModelProvider, endpoint, route string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.oneHourOff[oneHourRoute(provider, endpoint, route)]
}

// Thresholds of the run-time check, as fractions of the expected prefix (the billed prefix of the real
// request): the read must cover at least 90% for the switch to count as free, with a write of at most
// 10%. A read under 10% with a write of at least half is a rewrite.
const (
	oneHourFreeRead   = 0.9
	oneHourFreeWrite  = 0.1
	oneHourCostlyRead = 0.1
	oneHourCostlyWrit = 0.5
)

// judgeOneHour reads the usage of a ping that really went out relabelled. A free switch makes the entry
// a 1-hour entry; a costly one switches the route off. An answer that is neither (a partial read, a
// failed ping) decides nothing, and the next 1-hour ping judges again.
func (k *Keeper) judgeOneHour(j pingJob, status int, u Usage) {
	if status < 200 || status >= 300 {
		return
	}
	k.mu.Lock()
	prefix, endpoint := float64(j.e.prefix), j.e.endpoint
	k.mu.Unlock()
	if prefix <= 0 {
		return
	}
	read, write := float64(u.CacheRead), float64(max(u.CacheWrite, u.CacheWrite1h))
	switch {
	case read >= oneHourFreeRead*prefix && write <= oneHourFreeWrite*prefix:
		// From now until the next real request the session is a 1-hour session: the keeper and the
		// dashboard read e.ttl1h (cacheTTL, Entry.TTL1h).
		k.mu.Lock()
		j.e.ttl1h = true
		k.mu.Unlock()
	case read < oneHourCostlyRead*prefix && write >= oneHourCostlyWrit*prefix:
		key := oneHourRoute(j.provider, endpoint, j.route)
		k.mu.Lock()
		first := !k.oneHourOff[key]
		k.oneHourOff[key] = true
		k.mu.Unlock()
		if first {
			slog.Warn("context-guru: a 1-hour keep-alive ping rewrote the prefix instead of reading it; "+
				"no more 1-hour pings on this route, the threads fall back to the plain schedule",
				"provider", string(j.provider), "route", j.route, "endpoint", endpoint,
				"cache_read", u.CacheRead, "cache_write", u.CacheWrite, "cache_write_1h", u.CacheWrite1h,
				"expected_prefix", int64(prefix))
		}
	}
}
