//go:build !cg_record

package proxy

import "log/slog"

// sessionRecorder is the stand-in for the recorder in a build without -tags cg_record.
// It is never non-nil, so every `h.recorder != nil` check on the request path is false and
// recording costs nothing. The real recorder is in sessionrec.go.
type sessionRecorder struct {
	teeCap int
}

func (r *sessionRecorder) Record(*recEvent) {}

func (r *sessionRecorder) Close() error { return nil }

// Decline does nothing: this build records nothing, so there is nothing to stop or delete.
func (r *sessionRecorder) Decline(string, string) (stopped, deleted bool, err error) {
	return false, false, nil
}

// HasSession is always false: this build writes no files.
func (r *sessionRecorder) HasSession(string, string) bool { return false }

// Dir returns "": this build records nothing.
func (r *sessionRecorder) Dir() string { return "" }

// RecorderBuiltIn reports whether this binary has the session recorder.
const RecorderBuiltIn = false

// NewSessionRecorder always returns nil in this build. main checks RecorderBuiltIn and
// warns once when a record flag is set.
func NewSessionRecorder(dir, endpoint string, bodyCap, teeCap, queue int, log *slog.Logger) (*sessionRecorder, error) {
	return nil, nil
}
