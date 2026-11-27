package accrec

import (
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// The request ratio of a row (#1533). The accountant builds it for each priced request (Result.Ratio): the
// provider's billed tokens of the forwarded text, over its bytes. The host stores its three counts and rebuilds
// it at read time. No host writes the byte-to-token conversion: Ratio.Tokens is the only one.

// RatioCounts are the three stored counts of a request's ratio. Known is false when the request has none: it
// was not priced, or it was written before the columns existed.
type RatioCounts struct {
	// BilledTokens and TextBytes are Ratio.Text: the tokens and the bytes of the forwarded text.
	BilledTokens, TextBytes int64
	// Capped is Ratio.Capped.
	Capped bool
	Known  bool
}

// RatioCountsOf reads the counts off a priced Result. Known is false for a Result with no ratio. It never panics.
func RatioCountsOf(res accountant.Result) RatioCounts {
	if res.Ratio == nil {
		return RatioCounts{}
	}
	tok, by := res.Ratio.Text()
	if tok <= 0 || by <= 0 {
		return RatioCounts{} // a ratio that converts nothing is no ratio: the caller keeps its fallback
	}
	return RatioCounts{BilledTokens: int64(tok), TextBytes: int64(by), Capped: res.Ratio.Capped(), Known: true}
}

// Ratio rebuilds the ratio from the stored counts. nil when the row has none, so a caller keeps its fallback.
func (c RatioCounts) Ratio() *accountant.Ratio {
	if !c.Known || c.BilledTokens <= 0 || c.TextBytes <= 0 {
		return nil
	}
	r := accountant.RatioFromText(int(c.BilledTokens), int(c.TextBytes), c.Capped)
	return &r
}

// ProviderTokens converts a removal into the provider's tokens (#1533). With the row's own ratio and the removed
// bytes, it is Ratio.Tokens of the bytes: live is true, and the host's own count is not read. Without them, it is
// the host's own token count scaled by the legacy table (tokens.LegacyFactor), the documented fallback for a row
// written before the ratio existed: live is false.
func ProviderTokens(model string, hostTokens, bytes int64, c RatioCounts) (provider float64, live bool) {
	if r := c.Ratio(); r != nil && bytes > 0 {
		return float64(r.Tokens(int(bytes))), true
	}
	f, _ := tokens.LegacyFactor(model)
	return float64(hostTokens) * f, false
}

// ApplyTo sets the row's own ratio and the removed bytes on a removal that the accountant prices (RemovalSaving),
// when the row has both (#1533). It returns the provider's tokens for each of the host's tokens of this removal,
// for a caller that holds a host token weight per item: 1 when it set nothing, so the input keeps today's answer
// (factor 1). hostTokens is the host's own count of the same removal.
func (c RatioCounts) ApplyTo(in *accountant.RemovalInput, hostTokens, bytes int64) (providerPerHost float64) {
	r := c.Ratio()
	if r == nil || bytes <= 0 {
		return 1
	}
	in.Ratio, in.RemovedBytes = r, int(bytes)
	if hostTokens <= 0 {
		return 1
	}
	return float64(r.Tokens(int(bytes))) / float64(hostTokens)
}

// RequestFactor is the provider's tokens for each of the host's tokens of what one request removed, from the request's
// own ratio and the bytes the removal took out (#1533). hostRemoved is the host's count of the same removal
// (tokens_before - tokens_after). ok is false when the request has no ratio or no bytes, or the removal is empty or
// the answer is outside 1..tokens.MaxBilledRatio (the bound the warm gate trusts: the bytes and the host tokens are
// paired only by design, and a bad pair must not scale a figure): the caller then keeps the legacy table (tokens.LegacyFactor), the fallback for a
// row written before the bytes were stored.
func RequestFactor(hostRemoved, bytes int64, c RatioCounts) (factor float64, ok bool) {
	r := c.Ratio()
	if r == nil || bytes <= 0 || hostRemoved <= 0 {
		return 0, false
	}
	f := float64(r.Tokens(int(bytes))) / float64(hostRemoved)
	if f < 1 || f > tokens.MaxBilledRatio {
		return 0, false
	}
	return f, true
}
