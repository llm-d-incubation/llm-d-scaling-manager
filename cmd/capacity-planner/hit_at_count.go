package main

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The prefix hit at another prefill count (saturation.SwitchPrefillHitAtCount,
// issue llm-d-incubation/llm-d-scaling-manager#384): with the switch on, a
// fleet priced at a prefill count other than the recorded one prices the
// computed tokens a request, E[I_eff], and the prefix-cache hit rate beside
// it at that count (prefill.HitAtCountOf), not the recorded count's. Every
// command reads the fleet here, so plan, size, at-replicas, the day plan, the
// report and backtest's count cells all price the same rule; the prefill
// knee, N_p V_P / (f E[I_eff]), moves with E[I_eff]. The rule needs no new
// series: the hit is the recording's total one, 1 - computed / whole prompt
// over every tier. Where the recording also holds vLLM's KV residency
// histograms (--kv-cache-metrics; domain.ReplicaMetrics.KVReuseGapBuckets),
// the reuse-gap rule prices the hit at fewer pods and at more from a law
// fitted on them (whatif.PromptWindow.HitModel, prefill.FitHitModel); the
// inputs carry the law (whatifapi.InputsResponse.PrefillHitModel), so the
// main view's TTFT model and a backtest's count cells price the same one.
// Without them the floor prices every count below the one the hit was read
// on, and the hit is held above it.
//
// The floor is a floor only where the hit is concave in each pod's cache
// over its working set, which a cache-aware router gives by keeping each
// prefix on one pod at both counts. Where load breaks that affinity at the
// smaller count, or a prefix is duplicated there more than at the read one,
// the hit can fall under it; the flag says the hit is extrapolated.

// wireHitAtCount is the prefix hit as the switch priced it (pricedFleet
// .hitAtCount): the hit read on FitCount prefill pods and the one priced on
// Count, the rule, the range the true hit lies in, and the computed tokens a
// request at each count.
type wireHitAtCount struct {
	FitHit          float64 `json:"fitHit"`
	Hit             float64 `json:"hit"`
	FitCount        int     `json:"fitCount"`
	Count           int     `json:"count"`
	Rule            string  `json:"rule"`
	Lo              float64 `json:"lo"`
	Hi              float64 `json:"hi"`
	FitPromptTokens float64 `json:"fitPromptTokens"`
	PromptTokens    float64 `json:"promptTokens"`
}

// priceHitAtCount moves the prefill prompt to the prefill count priced, with
// the switch on: E[I_eff] scaled by (1 - h')/(1 - h), and the prefix-cache
// hit rate by h'/h. h is the fit's total hit: the prefill variant's
// computed-token hit where the prompt is the measured one (the same window
// as E[I_eff]), else 1 - E[I_eff] / the whole prompt asked. A hit the
// question states governs and is not moved, and with no hit read nothing
// moves. The hit is read over the prompt window, so it was read on the most
// pods any of its cycles served (hitFitCount): a window that spans a
// scale-down reads the larger count's hit, and the floor taken from the
// smaller count would price over the truth at fewer pods. A backtest step's
// computed prompt (stepComputedPrompt) is not moved either: its prefill pods
// computed it at the step's own count, so it is the prompt at that count as
// measured, and moving it again would net the fit's hit into it twice.
func (pf *pricedFleet) priceHitAtCount(ps *learnreplay.PrefillState) {
	p := &pf.fleet.Prefill
	fitCount := pf.hitFitCount(ps)
	if !pf.hitAtCountOn {
		return
	}
	pf.hitModel = pf.promptWindow.HitModel(fitCount)
	if pf.stepComputed || p.Replicas == fitCount || !(p.PromptTokens > 0) {
		return
	}
	if pf.pv != nil && pf.pv.assumedHit && !pf.pv.hitRead {
		return
	}
	h, ok := pf.fitTotalHit()
	if !ok {
		return
	}
	r, ok := prefill.HitAtCountOf(h, p.PromptTokens, fitCount, p.Replicas, pf.hitModel)
	if !ok {
		return
	}
	pf.hitAtCount = &r
	pf.in.HitAtCount = &wireHitAtCount{FitHit: r.FitHit, Hit: r.Hit, FitCount: r.FitCount, Count: r.Count, Rule: string(r.Rule),
		Lo: r.Lo, Hi: r.Hi, FitPromptTokens: p.PromptTokens, PromptTokens: r.Computed}
	flag := whatif.HitAtCountFlag(r)
	if flag != "" {
		pf.flags = append(pf.flags, flag)
	}
	p.PromptHitRate = gpuHitAt(p.PromptHitRate, r)
	switch {
	case r.Moved():
		pf.assume("prefix hit %.3f on %d prefill pods from %.3f read on %d (%s; between %.3f and %.3f): %s computed tokens a request, %s on %d (--%s)",
			r.Hit, r.Count, r.FitHit, r.FitCount, hitRuleWords(r.Rule), r.Lo, r.Hi,
			num(round(r.Computed)), num(round(p.PromptTokens)), r.FitCount, hitAtCountFlag)
		p.PromptTokens = r.Computed
	case flag == whatif.PrefixHitHeld:
		pf.assume("prefix hit %.3f read on %d prefill pods held on %d: a cache-aware router spreads the prefixes over more caches, "+
			"so the hit is likely higher, up to %.3f, and capacity reads low (--%s)",
			r.FitHit, r.FitCount, r.Count, r.Hi, hitAtCountFlag)
	case r.Rule == prefill.HitReuseGap && (r.GPUScale != 1 || r.TierScale != 1):
		pf.assume("prefix hit %.3f read on %d prefill pods held on %d by a tier past the GPU (%s): the GPU's share of it x%.3g, "+
			"the tier's x%.3g (--%s)", r.FitHit, r.FitCount, r.Count, hitRuleWords(r.Rule), r.GPUScale, r.TierScale, hitAtCountFlag)
	}
}

// gpuHitAt is the GPU prefix-cache hit rate gpu, read at the fit, at the
// count r priced. Under the reuse-gap rule it moves by the law's GPU ratio
// whether or not the total moved (an unsized tier past the GPU holds the
// total and takes up the GPU's loss, tierLoad), and never above the total
// hit priced: the GPU's hits are among it, and the bracket can hold the
// total under the law's (the ceiling assumed at more pods). Under any
// other rule it moves by the total's own ratio.
func gpuHitAt(gpu float64, r prefill.HitAtCount) float64 {
	switch {
	case r.Rule == prefill.HitReuseGap:
		return min(gpu*r.GPUScale, r.Hit)
	case r.Moved() && r.FitHit > 0:
		return gpu * r.Hit / r.FitHit
	}
	return gpu
}

// hitFitCount is the prefill count the fit's hit was read on: the cycle's
// serving pods, or the most any cycle of the prompt window counted where
// that is more (whatif.PromptWindow.MaxPods). The larger count is the
// conservative one: the floor h x N'/N falls with N, and N' at or above the
// cycle's count but under it is priced at the floor, not held.
func (pf *pricedFleet) hitFitCount(ps *learnreplay.PrefillState) int {
	return max(ps.Pods, pf.promptWindow.MaxPods())
}

// hitRuleWords is a rule in the answer's words.
func hitRuleWords(r prefill.HitRule) string {
	switch r {
	case prefill.HitFloor:
		return "the floor hit x N'/N"
	case prefill.HitReuseGap:
		return "the reuse-gap rule over the KV residency histograms"
	}
	return string(r)
}

// fitTotalHit is the fit's total prefix hit, 1 - computed / whole prompt:
// the prefill variant's computed-token hit at the measured prompt, else the
// share of the whole prompt asked that E[I_eff] leaves out. false with
// neither.
func (pf *pricedFleet) fitTotalHit() (float64, bool) {
	p := pf.fleet.Prefill
	if pf.in.PromptSource == shapeMeasured {
		if h, ok := pf.promptWindow.ComputedHit(); ok {
			return h, true
		}
	}
	if p.WholePromptTokens > 0 && p.PromptTokens <= p.WholePromptTokens {
		return 1 - p.PromptTokens/p.WholePromptTokens, true
	}
	return 0, false
}

// pricedComputedHit is the prefill variant's computed-token hit as priced: the
// one the switch priced at the fleet's count where it moved, else the
// window's (whatif.PromptWindow.ComputedHit).
func (pf *pricedFleet) pricedComputedHit() (float64, bool) {
	if r := pf.hitAtCount; r != nil && r.Moved() {
		return r.Hit, true
	}
	return pf.promptWindow.ComputedHit()
}

// hitAtCountFlag is the flag that turns the switch on, on every command
// that replays a recording (answerFlags.replayOptions), and hitAtCountUsage
// its help.
const hitAtCountFlag = "prefill-hit-at-count"

// hitAtCountUsage is the flag's help, on every command that takes it.
const hitAtCountUsage = "opt-in: price the prefix hit at a prefill count other than the recorded one. Where the recording holds vLLM's " +
	"KV residency histograms (--kv-cache-metrics), the reuse-gap rule prices it at fewer and at more pods; without them, at fewer pods " +
	"the floor hit x N'/N, so the computed prompt tokens a request rise (flagged PrefixHitAtCount), and at more pods the recorded hit, " +
	"held (flagged PrefixHitHeld). A cache-aware router splits the shared prefixes over the pods, so the hit moves with the count. Off by default"
