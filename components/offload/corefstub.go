package offload

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// The residue a cut leaves behind, and why it is more than a cosmetic choice.
//
// Reversibility is a CAPABILITY, not a guarantee. The stash guarantees the bytes can be
// recovered; only the model can decide to recover them, by calling the expand tool. So a
// wrong cut has three outcomes, not one:
//
//  1. the model notices and expands the right marker — one round-trip plus a cache-write;
//  2. it notices something is missing but cannot tell WHICH marker holds it — several
//     expands, or it gives up;
//  3. it never notices, and answers from less information than it had.
//
// Only (1) is the cost the design originally claimed. (3) is silent, and no counter this
// component keeps can see it — expand-rate measures noticed errors only, which is why
// reward is the sole instrument that detects it.
//
// What the residue can actually influence is the gap between (1) and (2): whether the
// model can tell, without expanding, that THIS marker is where the thing it wants lives.
// A head peek — the first ~96 characters — does that well for a file read or a traceback,
// where the head identifies the whole. It does it badly for a record set, where the head
// is one arbitrary row: an agent hunting for someone's address cannot tell from
// `[{"name":"david","id":123,...` whether addresses are in here at all, let alone whose.
//
// So for structured content the residue describes the SHAPE instead: how many records, and
// what fields they carry. That is addressable — "records with keys name/id/address, 200 of
// them" tells the model where to look — where a peek is merely evocative.

// stubCap bounds the descriptor so the marker can never dominate the message it replaces
// (tryMark's never-worse check would drop the rewrite anyway, but a cut that fails to
// shrink is a wasted candidate rather than a bug).
const stubCap = 200

// maxStubKeys bounds how many field names the descriptor lists. Enough to identify what
// the records hold; not a schema dump.
const maxStubKeys = 12

// corefStub describes what was cut, in the terms most likely to let the model decide
// whether it needs it back. Returns "" when it can say nothing useful, in which case the
// caller falls back to a head peek.
//
// Deliberately structural and never evaluative: it says what the content IS, never what it
// was worth. An earlier version of this component wrote "no later turn referred back to
// it" into the marker, which is precisely the claim that is FALSE whenever the reference
// was transformed or semantic (tiers 2 and 3) — so it read as reassurance and discouraged
// the expand call that would have repaired the mistake. A marker must not talk the model
// out of recovering content.
func corefStub(content string) string {
	t := strings.TrimSpace(content)
	if len(t) == 0 {
		return ""
	}
	switch t[0] {
	case '[':
		return stubArray(t)
	case '{':
		return stubObject(t)
	}
	return ""
}

// stubArray describes a JSON array: its length, and the union of keys across the records
// it holds (sampled — a 10k-element array does not need a full scan to be described).
func stubArray(t string) string {
	var items []json.RawMessage
	if json.Unmarshal([]byte(t), &items) != nil {
		return ""
	}
	if len(items) == 0 {
		return ""
	}
	keys := map[string]struct{}{}
	sampled := 0
	for _, it := range items {
		if sampled >= 32 {
			break
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(it, &obj) != nil {
			continue // scalar or nested array: no field names to report
		}
		sampled++
		for k := range obj {
			keys[k] = struct{}{}
		}
	}
	out := strconv.Itoa(len(items)) + " records"
	if ks := sortedKeys(keys); len(ks) > 0 {
		out += ", fields: " + joinKeys(ks)
	}
	return clipRunes(out, stubCap)
}

// stubObject describes a JSON object by its top-level keys, and — the common shape for a
// tool that wraps its payload — the length of the one array it contains.
func stubObject(t string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(t), &obj) != nil {
		return ""
	}
	if len(obj) == 0 {
		return ""
	}
	keys := map[string]struct{}{}
	for k := range obj {
		keys[k] = struct{}{}
	}
	out := "object, fields: " + joinKeys(sortedKeys(keys))
	// A single wrapped collection is worth counting: "rows: 400" is the fact that decides
	// whether this is the output holding what the model is looking for.
	for _, k := range sortedKeys(keys) {
		var arr []json.RawMessage
		if json.Unmarshal(obj[k], &arr) == nil && len(arr) > 0 {
			out += " (" + k + ": " + strconv.Itoa(len(arr)) + " items)"
			break
		}
	}
	return clipRunes(out, stubCap)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out) // stable order: the marker text must be byte-identical on replay
	return out
}

// joinKeys lists field names, truncating past maxStubKeys so a wide record does not turn
// the marker into a schema dump.
func joinKeys(ks []string) string {
	if len(ks) > maxStubKeys {
		return strings.Join(ks[:maxStubKeys], ", ") + ", …+" + strconv.Itoa(len(ks)-maxStubKeys)
	}
	return strings.Join(ks, ", ")
}
