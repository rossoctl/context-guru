package counterfactual

import "time"

// Fingerprint is the most recent request of one thread (§5.1). The library builds it inside
// Compute, because Cached comes from the usage, which arrives after the response.
type Fingerprint struct {
	Thread                                  string
	Model                                   string    // the model the request was sent for; "" when the host did not say
	At                                      time.Time // the start of the request; the request gap is measured from it (§5.2)
	Client                                  Body      // client body, before any plugin
	Forwarded                               Body      // body as sent upstream
	ClientBreakpoints, ForwardedBreakpoints []Breakpoint
	Cached                                  Position // extent cached after its response, in forwarded space

	// splices are the message splices that produced Forwarded from Client, in pipeline order. The
	// library needs them to map Cached back into client space (§5.1). Not part of the API.
	splices []MessageSplice
}

// Body lists every position in hash order: tools, then system, then messages. The slice index is
// the position. Start gives the first index of each section.
type Body struct {
	Start [3]int    `json:"start"`
	Pos   []PosHash `json:"pos"`
}

// PosHash holds hashes and sizes only (R2).
type PosHash struct {
	Hash  uint64 `json:"hash"` // cumulative: equal hashes mean an equal prefix up to this position
	Bytes int32  `json:"bytes"`
}

// Breakpoint is a cache breakpoint in a body.
type Breakpoint struct {
	Pos Position `json:"pos"`
	TTL string   `json:"ttl"` // "5m" | "1h"
}

// NewBody hashes one body in hash order, so every host builds the same Body for the same bytes.
// The host calls it for the client body (before BeginRequest) and for the forwarded body (before
// EndRequest).
func NewBody(tools, system, messages [][]byte) Body {
	b := Body{Start: [3]int{0, len(tools), len(tools) + len(system)}}
	var h uint64 = 14695981039346656037
	for _, section := range [][][]byte{tools, system, messages} {
		for _, p := range section {
			for _, c := range p { // FNV-1a, chained so the hash of a position covers its whole prefix
				h = (h ^ uint64(c)) * 1099511628211
			}
			h = (h ^ 0xff) * 1099511628211 // position separator
			b.Pos = append(b.Pos, PosHash{Hash: h, Bytes: int32(len(p))})
		}
	}
	return b
}
