package saturation

import "github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"

// The ITL line of an aggregated variant (issue #432).
//
// A variant with no role label serves prefill and decode on the same pods
// (domain.RoleBoth, which domain.CanonicalRole reads "" as). Its replicas
// generate tokens exactly as a decode replica's do, so the gap between two of
// them is as readable, and the line ITL(k) = A*k + B is fitted from it in the
// same band. Fitting decode alone left the ordinary, undisaggregated
// deployment with no line: no mu derived from it and no service-rate signal,
// so such a variant was sized from its saturated throughput and KV occupancy
// only. The slo analyzer has always fitted every role but prefill.
//
// Prefill stays out: a prefill replica emits about one token per request and
// hands its KV to decode, so it has no ITL to fit and tokenSec/avgOutput
// does not describe it.
//
// An aggregated pod's ITL also carries the prefill it interleaves with its
// decode steps (chunked prefill), so its line is a property of the workload's
// shape as well as of the engine (issue #275). It is not a decode pod's
// line, and the two are never shared: a line fitted on an aggregated variant
// is published and borrowed under a key of its own (itlLineKeyForRole), so a
// decode variant of the same model and engine configuration neither lends to
// it nor borrows from it. The line is priced by shape and sizes only once
// it has been checked (itl_aggregated_shape.go, issue #275); until then it is
// reported (itlLineReportOnly).

// SwitchITLFitsAggregated is the replay switch for the line of an aggregated
// variant (issue #432). On, a variant whose role is RoleBoth (or unlabeled)
// fits, publishes and borrows an ITL line, and its derived mu is reported
// (the derived-mu log line, the service-rate signal, the GPS check, the
// learner report) but not priced (itlLineReportOnly). Off, as before, only
// decode fits a line.
const SwitchITLFitsAggregated = "itl-line-fits-aggregated"

// itlLineRole reports whether a variant of this role fits, and is priced
// from, an ITL line: decode always, an aggregated variant with
// SwitchITLFitsAggregated on, and prefill never.
func (a *SaturationAnalyzer) itlLineRole(role string) bool {
	switch domain.CanonicalRole(role) {
	case domain.RoleDecode:
		return true
	case domain.RoleBoth:
		return a.itlFitsAggregated
	default:
		return false
	}
}

// itlLineKeyForRole is the shared-line key (itlPhysicsKey) a variant of this
// role publishes and borrows under. A decode variant's is the physics key as
// it is, so its lines and the learned-state snapshots that hold them keep
// their keys. An aggregated variant's carries its role: its ITL includes the
// prefill it interleaves (issue #275), so the same model on the same engine
// configuration fits a different line on aggregated pods than on decode pods,
// and the two must not be lent to each other.
//
// Only a key built on a fingerprint is shared, so only that one carries the
// role. With no fingerprint (reuse off, or a configuration not fully read)
// the physics key is the variant's own window key, which no other variant
// reads, and it is left as it is.
func itlLineKeyForRole(physicsKey, role, fingerprint string) string {
	if physicsKey == "" || fingerprint == "" || domain.CanonicalRole(role) != domain.RoleBoth {
		return physicsKey
	}
	return physicsKey + "|" + domain.RoleBoth
}

// itlLineReportOnly reports whether the mu derived from a line of this role
// is reported and not priced: an aggregated variant's is, until its lift
// (noteAggregatedLift, issue #275), which priceReplicas checks beside it.
//
// An aggregated pod's ITL carries the chunked prefill it runs between decode
// steps, about n*IL/OL prompt tokens a step, so its line is a property of
// the request shape as well as of the engine. A line fitted under one shape
// mis-prices another, and nothing bounds how long it does: a pinned fit
// takes several cycles to move, a held slope (issue #284), a restored line
// and a borrowed one carry an old shape for as long as they are kept. The
// derived mu also over-states the rate by about TTFT/(OL*ITL): a sequence
// being prefilled holds KV and emits no token. And the line is fitted at
// the fleet's shape, so a variant serving a shape of its own gets an
// average. So the figure is reported -- the derived-mu log line, the
// learner report, the service-rate signal (derivedWhyReportOnly) -- and the
// replica is priced as if it had no line: no order and no settled shape
// change rests on it. A decode line is priced as before. The shape term,
// the prefill correction and the lift (itl_aggregated_shape.go) answer the
// first two; the third is not handled.
func itlLineReportOnly(role string) bool {
	return domain.CanonicalRole(role) == domain.RoleBoth
}

// derivedWhyReportOnly is the whatif.DerivedMu.Why a replica is priced with when its
// line's figure is reported only (itlLineReportOnly).
const derivedWhyReportOnly = "report-only"

// itlReportRole is the role an ITL learner report and its learner-write line
// name for a variant: the variant's own with SwitchITLFitsAggregated on,
// decode (the one role that fitted a line) with it off.
func (a *SaturationAnalyzer) itlReportRole(role string) string {
	if !a.itlFitsAggregated {
		return domain.RoleDecode
	}
	return domain.CanonicalRole(role)
}
