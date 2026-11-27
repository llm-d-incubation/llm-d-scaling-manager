package whatif

import (
	"fmt"

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
