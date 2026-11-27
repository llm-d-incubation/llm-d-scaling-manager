package whatif

import (
	"fmt"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// PrefillFromEstimate is the Prefill P1 supports: V_P, a and f from what
// internal/signals/prefill learned for one variant, with the replicas, the
// prompt and its spread left for the caller (the question's fleet and the
// traffic shape, which P1 does not learn).
//
// A V_P P1 refused is NoPrefillCapacity and an a it refused is
// NoFixedLatency, each with P1's reason as the detail. An f it refused (one
// pod, too few minutes) is left 0: one pod prices as 1, and more than one is
// refused when priced (NoImbalance) unless the caller assumes one.
//
// The prompt V_P was read at (VPPromptTokens) and, where a was taken from
// the curve (AModel quadratic), its per-token cost b + c*I are carried, so
// V_P is rescaled to the question's prompt (Prefill.shapedVP). The margin on
// V_P near the knee is DefaultVPMargin.
//
// P1's f is de-biased for Poisson arrivals, and the de-bias errs both ways
// (docs/proposals/slo-capacity-planner-ttft.md, "Where it is most wrong"):
// low on a real imbalance at few requests a pod (over-promises TTFT), high on
// equal pods with variable prompts (under-promises). It is carried as
// learned; P2 is what will show which way a real fleet leans.
func PrefillFromEstimate(est prefill.Estimate) (Prefill, *Unanswerable) {
	if !(est.VP > 0) || est.VPRefused != "" {
		u := refuse(NoPrefillCapacity, fmt.Sprintf("P1 refused V_P: %s", est.VPRefused))
		return Prefill{}, &u
	}
	if !(est.A > 0) || est.ARefused != "" {
		u := refuse(NoFixedLatency, fmt.Sprintf("P1 refused a: %s", est.ARefused))
		return Prefill{}, &u
	}
	p := Prefill{
		VP: est.VP, VPLowerBound: est.VPLowerBound, VPAboveSlopeBound: est.VPAboveSlopeBound,
		VPServedOnly: est.VPSource == prefill.SourceServedPeak, VPFromPlateau: est.VPSource == prefill.SourceOccupancyPlateau,
		FixedLatency: est.A, VPPromptTokens: est.VPPromptTokens, VPMargin: DefaultVPMargin,
		VPWholePromptTokens: est.VPWholePromptTokens, VPPromptHitRate: est.VPPromptHitRate,
		VPPromptLow: est.VPPromptLow, VPPromptHigh: est.VPPromptHigh,
		VPRequestSeconds: est.VPRequestSeconds,
	}
	if est.AModel == prefill.AModelQuadratic {
		p.CurveSlope, p.CurveCurvature = est.ASlope, est.ACurve
	}
	if est.F >= 1 && est.FRefused == "" {
		p.Imbalance = est.F
	}
	return p, nil
}

// ImbalanceUtilizationAt is the mean prefill pod utilization f was read at,
// for f at pods (Prefill.ImbalanceUtilization): P1's load at that count over
// vp, the learned V_P (Prefill.VP; prefillPlan.rhoM rescales it to the
// question's prompt), where it learned one there, else the load of the
// minutes its F is the median of, else 0 (none read). Held to
// MaxImbalanceUtilization.
func ImbalanceUtilizationAt(est prefill.Estimate, pods int, vp float64) float64 {
	if !(vp > 0) || !Finite(vp) {
		return 0
	}
	load, ok := est.FLoadByPods[pods]
	if _, learned := est.FByPods[pods]; !ok || !learned {
		load = est.FLoad
	}
	if !(load > 0) || !Finite(load) {
		return 0
	}
	return min(load/vp, MaxImbalanceUtilization)
}

// KneeAtServedWork is a prefill knee priced from a lower bound of V_P and
// raised to the computed-token work the fleet's prefill pods sustained at the
// pod count priced (ServedKneeFloor): at least the rate that work serves at
// the prompt priced.
const KneeAtServedWork Flag = "KneeAtServedWork"

// ServedWorkUnsteady is a prefill knee priced from a lower bound of V_P that
// no served work floors: at the pod count priced the prefill pods served a
// stretch of prefill.ServedWorkWindow, and the backlog grew in every one
// (prefill.ServedWork.PeaksUnsteady). A burst carried with a queue building
// is not a rate the pods hold (#477), so the knee stays at its lower bound.
const ServedWorkUnsteady Flag = "ServedWorkUnsteady"

// ServedKneeFloor is r, a prefill knee of p, raised to the rate the
// computed-token work the fleet's prefill pods sustained serves at the
// prompt priced, where r rests on a lower bound of V_P (VPLowerBound) and
// that is higher: served[p.Replicas] / p.PromptTokens, flagged
// KneeAtServedWork. served is the work by pod count
// (prefill.ServedWork.Peaks) and unsteady the counts whose work was not
// sustained (prefill.ServedWork.PeaksUnsteady). A pod computes no faster
// than its capacity, so the work the pods kept up over
// prefill.ServedWorkWindow with no backlog growing is work they can do, and
// where they served only with a backlog growing the knee is left as priced,
// flagged ServedWorkUnsteady. The knee is at least the rate it serves; both
// sides are read off the engines' computed-token counter, so no model of the
// prompt stands between them. A knee priced from a V_P that is no lower
// bound is left as priced: a window that served past it contradicts the
// model (#297), which a floor would hide. Only the knee is raised: a TTFT is
// priced from V_P as before, and a rate between the two knees is past the
// priced one (PastLowerBoundKnee).
//
// The capacity planner floors the knees it prices with it (#494), from the
// work its replay keeps behind saturation.SwitchPrefillVPServedUnqueued;
// with no work kept (nil maps) r is returned as it is. No analyzer calls it
// yet.
func ServedKneeFloor(r Result, p Prefill, served map[int]float64, unsteady map[int]bool) Result {
	a, ok := r.(Answer)
	if !ok || !a.HasFlag(VPLowerBound) || !(p.PromptTokens > 0) {
		return r
	}
	work := served[p.Replicas]
	if !(work > 0) && unsteady[p.Replicas] {
		a.Flags = append(slices.Clone(a.Flags), ServedWorkUnsteady)
		return a
	}
	floor := work / p.PromptTokens
	if !Finite(floor) || !(floor > a.Value) {
		return r
	}
	// The span over V_P and f: no lower than the floor, which is measured
	// work, not a V_P swept; its high side as priced where that is higher.
	span := &Sensitivity{Lo: floor, Hi: floor}
	if a.Sensitivity != nil && a.Sensitivity.Refused == "" {
		span.Hi = max(floor, a.Sensitivity.Hi)
	}
	a.Value = floor
	a.Flags = append(slices.Clone(a.Flags), KneeAtServedWork)
	a.Sensitivity = span
	if a.Basis.TTFT != nil {
		basis := *a.Basis.TTFT
		basis.KneeRate = floor
		a.Basis.TTFT = &basis
	}
	return a
}
