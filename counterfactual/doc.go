// Package counterfactual prices context operations in tokens for each billing tier.
//
// Context operators report what they changed as facts (RequestOps, Operation). Compute turns
// those facts and the provider's cache state into tokens. The host turns tokens into money.
//
// The package is a standalone module. It imports only the standard library, so context-guru,
// Cortex and any other AI framework can use it (docs/counterfactual/design.md, R3, D7).
// boundary_test.go enforces that rule.
//
// # Implementation decisions
//
// The design leaves some rules open. This implementation decides them as follows. Each one is
// the simplest rule that the design allows.
//
//  1. Tokens from bytes. Size is bytes only (§4.2 rule 7). The ratio is the billed prompt tokens
//     (input + cache read + cache write) divided by the bytes of the forwarded body, because the
//     billed tokens bill the forwarded body (§5.3). Every priced rewrite and every effect that uses
//     the ratio is Estimated.
//  2. No usage. A response with no usage prices nothing (§8): every operation goes to Unpriced
//     with ReasonNoUsage. The fixed fallback ratio of §4.2 rule 7 is not used.
//  3. Extents from usage. The provider reports token counts, not positions. The read extent is the
//     last candidate position whose prefix, converted with the ratio, fits in Usage.CacheRead, with
//     a tolerance of 128 tokens plus 2%. The candidates are the breakpoints of this and the previous
//     request and the previous extent; with none, every position. The extent this request leaves
//     cached (Fingerprint.Cached) is found the same way from CacheRead + CacheWrite.
//  4. The end of a change (§11 question 12). An active rewrite ends when a later splice removes
//     its position, when the client body first differs from the previous one at or before its
//     position (a client edit or compaction, §4.3), or when a valid rewrite or setting with the same
//     ChangeID is reported again (that one replaces it: observe to enforce, §4.6). A spend, or a
//     malformed report, with that ChangeID does not replace it. An active setting has no position
//     and stays until it is replaced. A restore does not bring an ended change back.
//  5. Pipeline order of an active and a reported operation. The library cannot see it. It reads it
//     from the sizes: when a splicing operation's SizeBefore equals the client bytes of the range it
//     removed, it saw the original content, so it ran first and the active changes inside have
//     ended. Otherwise they ran before it and keep their credit (§4.5 table). The guess is made once,
//     on the request that reports the splice; while both stay active the result holds. A wrong guess
//     fails the sum check. A DryRun or malformed splice changed nothing upstream, so it moves no
//     position.
//  6. Active operations are stored in client space, and are applied before the reported ones in
//     pipeline order. Result.Ops lists the reported ones first, then the active ones (§6.3).
//  7. B* (§5.1) is the last candidate at or before x = min(prevCachedExtent, clientFirstDiff - 1),
//     where x is first snapped down to this request's last client breakpoint at or before it, unless
//     a breakpoint of this request is at or after x (the provider reads only from a breakpoint). The
//     candidates are the previous extent and the client breakpoints of this and the previous
//     request. The row-4 prediction snaps the same way, with the forwarded breakpoints. The minimum cacheable length and the 128-token rounding of older OpenAI models are
//     not modelled.
//  8. A break moves the forwarded tokens after the read extent, up to B* as forwarded, from read to
//     write, at the TTL of the first forwarded breakpoint at or after B* (5m when there is none).
//     A setting break (§5.2 row 3) starts at the last candidate before the earliest
//     SettingChange.At; with no At it starts at the read and adds penalty_unknown. Row 3 and row 4
//     need the read to be below the prediction by more than the tolerance of decision 3.
//  9. Phase. With no previous fingerprint, no TTL, no cached extent inside the TTL, or a gap past a minimum or unknown
//     lifetime with no ping, the phase is unknown and penalty_unknown is added (§6.4); a read in the usage then
//     makes it hot up to the read extent, with no rescue. The cold expiry adds an empty break
//     (§5.2 row 5).
//  10. A rescue (§4.4) needs a ping after the previous request, a request gap past an exact or minimum TTL, an
//     entry gap inside it, and a read larger than the write. Its tokens are the smaller of the ping's
//     read (converted with the ratio) and Usage.CacheRead, at the TTL of EntryFacts.TTL.
//  11. A spend is never priced in tokens: its Call is passed through (D4). A setting is not priced
//     (R50); it only gives its ChangeID to a break. ComputeStandalone marks any operation that is not
//     a spend as ReasonNotSpend.
//  12. The end of an in-place change is inferred (§11 question 12): the forwarded item at its position
//     has the client item's size again (cumulative hashes cannot be compared when another change sits
//     earlier), or a splice or a client edit removed the position. cachesplit and headttl are priced as
//     the split, ttlPremium and ttlRescue effects. Conservation is checked against the bill with a
//     tolerance of the rounding plus one token.
//
// # Not implemented
//
// These parts of the design are not implemented. Compute adds no figure for them. Compute does not
// recover from a panic: it leaves the ThreadState unchanged, and the host wraps the call to fail open.
//
//   - Result.Interaction on a rescued request (§6.2). It is always zero.
//   - The order check (§6.5 check 2). The attribution check (5) holds by
//     construction: a break only takes the ChangeID of an operation of the same request.
//   - The minimum cacheable length and the 20-block lookback (§5.1), and the long-context tier.
//   - MessageSplice moves message positions only. A rewrite that removes or adds whole tools or
//     system blocks moves no later position in its section (§4.3 gives no splice for them).
package counterfactual
