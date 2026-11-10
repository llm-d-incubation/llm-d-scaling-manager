package scaler

import "time"

// WithZeroRecheckForTest shortens how often StreamIsActive re-reads a held
// variant it reports inactive, on this handler only. Returns h for chaining.
func (h *Handler) WithZeroRecheckForTest(d time.Duration) *Handler {
	h.zeroRecheck = d
	return h
}

// ZeroRecheckOf is the handler's recheck interval.
func ZeroRecheckOf(h *Handler) time.Duration { return h.zeroRecheck }

// RecheckDue is recheckDue.
var RecheckDue = recheckDue

// ReachOf reports what the server's reachability tracker recorded: the
// refusals since the last served handshake or call, when that was, and the
// streams open. Zero values before Start.
func ReachOf(s *Server) (refused int, lastServed time.Time, streams int) {
	r := s.reach.Load()
	if r == nil {
		return 0, time.Time{}, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused, r.lastServed, r.streams
}
