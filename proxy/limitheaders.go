package proxy

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/dash"
)

// The subscription limit headers (#984). Anthropic sends them on each answer to a Claude
// subscription (an OAuth sign-in): the used share of the 5-hour and the 7-day window, when each
// resets, and a status word. An API key, Bedrock and Vertex do not get them.
//
// Reading them is transport, so it lives here, in the host, and not in the library. The values
// go on the request's dashboard row beside its token counts, so a later fit can learn how the
// limit weights each token kind, and GET /api/me/limits can show the latest share.
const limitHeaderPrefix = "anthropic-ratelimit-unified-"

// readLimitHeaders reads the 5h and 7d utilization, reset and status headers. A missing or bad
// value is left out; nil when none is usable. It only reads h, so it can never change the
// response the client gets. http.Header.Get is case-insensitive, as header names are.
func readLimitHeaders(h http.Header) *dash.Limits {
	if h == nil {
		return nil
	}
	l := &dash.Limits{FiveHour: readLimitWindow(h, "5h"), SevenDay: readLimitWindow(h, "7d")}
	if l.Empty() {
		return nil
	}
	return l
}

func readLimitWindow(h http.Header, win string) dash.LimitWindow {
	p := limitHeaderPrefix + win + "-"
	var w dash.LimitWindow
	if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(p+"utilization")), 64); err == nil &&
		!math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= limitMaxUtilization {
		w.Utilization = &v
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(h.Get(p+"reset")), 10, 64); err == nil && v > 0 {
		w.ResetS = &v
	}
	if s := strings.ToLower(strings.TrimSpace(h.Get(p + "status"))); limitStatusOK(s) {
		w.Status = s
	}
	return w
}

// limitMaxUtilization bounds the share. The header gives 0..1; a share over 1 is possible when
// the account is past its limit, so a little room is kept, and a value far out is a bad header.
const limitMaxUtilization = 10

// limitStatusOK accepts a short status word ("allowed", "allowed_warning", "rejected"): lower-case
// letters and underscores, at most 32 bytes. Anything else is not stored, so a header can never
// put free text on a dashboard row.
func limitStatusOK(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

// noteLimits records the subscription limit headers of an upstream answer. On a request with
// several upstream rounds the last answer that carried them wins: it is the newest share.
func (c *capture) noteLimits(h http.Header) {
	if c == nil {
		return
	}
	if l := readLimitHeaders(h); l != nil {
		c.limits = l
	}
}

// PingLimits reads the subscription limit headers of a keep-alive ping's response (#995). A ping is
// a real request, so its answer carries the same headers, and recording them keeps the 5-hour share
// current while the user is idle. It is the same parser as the request path; nil when none is usable.
func PingLimits(r components.KeepAliveReport) *dash.Limits { return readLimitHeaders(r.Header) }

// subscriptionTokenPrefix is the fixed prefix of a Claude subscription (OAuth) access token.
const subscriptionTokenPrefix = "sk-ant-oat"

// callerIsSubscription reports whether the client authenticated with a Claude subscription token:
// `Authorization: Bearer sk-ant-oat...`, as Claude Code sends after a claude.ai sign-in. An
// x-api-key, any other credential, and one of our own tokens are not. It reads the same slot
// CallerKey and CallerAuthScheme pick. Only the fixed prefix is tested; the token is never kept,
// logged or returned.
func callerIsSubscription(r *http.Request) bool {
	return CallerAuthScheme(r) == "bearer" && strings.HasPrefix(CallerKey(r), subscriptionTokenPrefix)
}
