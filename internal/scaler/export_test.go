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
