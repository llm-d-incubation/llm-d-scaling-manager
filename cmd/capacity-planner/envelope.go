package main

import (
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// Every figure the planner prints says how far off it read where it was
// measured (whatif.Envelope; docs/guides/capacity-planner/accuracy.md):
// "validated in-sample: error ..." inside a measured region, "OUTSIDE VALIDATED
// REGION (...)" anywhere else. This file builds what the envelope judges a
// figure by: its kind, its class, its rate over its knee, and its flags.

// judge is the shipped envelope's verdict on one figure.
func judge(kind whatif.FigureKind, q float64, class whatif.FigureClass, share float64, r whatif.Result) whatif.Validation {
	return whatif.DefaultEnvelope().Judge(whatif.Figure{Kind: kind, Percentile: q, Class: class, KneeShare: share, Flags: resultFlags(r)})
}

// resultFlags is an answer's flags, or a refusal's.
func resultFlags(r whatif.Result) []whatif.Flag {
	switch v := r.(type) {
	case whatif.Answer:
		return v.Flags
	case whatif.TTFT:
		return v.Flags
	case whatif.Unanswerable:
		return v.Flags
	}
	return nil
}

// The flags of replicas-to-slo and slo-to-rate, as the envelope reads them.
// A flag in flagsInside picks the source, the question or the output, or
// states an input the run priced at (the lengths, the spread, the arrivals,
// the flow-control state, the pod counts), which the class then holds to the
// run's. A flag in flagsOutside states an input no run priced at: setting
// it puts every figure outside, except capture's flags with --prometheus,
// which say how the window is captured (statedOutside). A flag in neither fails
// TestEnvelope_EveryFlagIsClassified, so a new flag is decided, not missed.
var (
	flagsInside = []string{"recording", "cycle", "model", "namespace", "variant", "decode", "prefill", "prefill-variant",
		"assume-input-tokens", "assume-output-tokens", "assume-prompt-cv", "arrival-cv2", "flow-control", "headroom", "rate",
		"seed", "o", "output", "ttft", "ttft-percentile", "tpot", "tpot-percentile",
		"prometheus", "bearer-token-file", "ca-file", "insecure-skip-verify", "timeout", "kube-state-metrics", "role-of",
		"from", "to", "step", "force", "no-self-check",
		// It moves no figure's value, and it can move a figure's verdict. The
		// HybridAttention and MLAAttention engine flags follow the config
		// (attentionFamily), and no envelope row allows either
		// (DefaultEnvelope's AllowedFlags), so a figure carrying one reads
		// outside. A config that reads MLA, a window or hybrid layers on a
		// model whose name raised neither puts its figures outside; one that
		// reads full attention on an MLA family's name clears MLAAttention,
		// and a figure outside by that flag alone reads inside. It never
		// clears HybridAttention a name raised. In flagsInside because it
		// states what the model is, not an input the run priced: the flags it
		// raises are what the envelope judges.
		modelConfigFlag,
		// Set, it states nothing until it applies: assumeIntercept then adds
		// --assume-itl-b to the variant's stated inputs, which puts its figures outside.
		"assume-itl-b",
		// On by default (whatif.DefaultRouterWaitInPrefill), it states
		// nothing either way: where it takes, the figure carries
		// FlowControlWaitPriced, which is outside on its own, and off is how
		// the run was priced (the SLO analyzer's switch for it,
		// slo-ttft-router-wait-in-prefill, is off by default).
		"router-wait-in-prefill",
		// Set, it states nothing until the replay reads V_P from the plateau:
		// each figure priced from it is then flagged VPFromPlateau, which
		// no envelope row allows, so it, and it alone, reads outside.
		vpPlateauFlag,
		// Set, it moves nothing at the prefill count the hit was read on;
		// at another count a figure whose hit it moves by more than
		// MaterialPrefixHitMove/2, or holds, is flagged PrefixHitAtCount or
		// PrefixHitHeld, which no envelope row allows, and a figure whose
		// hit it moves at all carries the priced hit in its class.
		hitAtCountFlag,
		// The same: every prefill and TTFT figure it prices is flagged
		// ExperimentalTTFT, which no envelope row allows.
		experimentalTTFTFlag,
		// Read only with --experimental-ttft=v2, whose every TTFT is
		// flagged RankMixtureTTFT, which no envelope row allows; without
		// v2 it states nothing.
		rankMixtureStratifiedFlag,
		// The same: read only with --experimental-ttft=v2 or =v2nl-line
		// (refused without), whose every TTFT is flagged RankMixtureTTFT; a
		// window under 12h adds RankMixtureShortFitWindow.
		"rank-mixture-kv-hold", "rank-mixture-remainder-spread", rankMixtureRemainderServerFlag, rankMixtureMinFitWindowFlag,
		// Read only with --experimental-tpot=v2 (refused without), whose
		// every percentile is flagged TPOTDistribution; under 24 steps it
		// adds TPOTDistributionShortFit.
		tpotDistMinFitStepsFlag,
		// The same: every TPOT percentile it prices is flagged
		// TPOTDistribution, which no envelope row allows.
		experimentalTPOTFlag}
	flagsOutside = []string{"batch-cap", "assume-tpot-ratio", "assume-vp", "assume-a", "assume-router-hop", "price-router-wait",
		"ps-onsets", "prefill-running-cap", "charge-decode-gate", "input-bucket", "engine", "engine-args", "prefill-engine-args", "gpus-per-replica",
		"accelerator", "policy", "engine-image", "vllm-version", "imbalance-across-counts", "assume-transfer-base",
		"load-aware-router",
		// V_P from the prefill service line failed the public cross-validation
		// (vp_service_line.go), and its figures carry no flag of their own.
		vpServiceLineFlag,
		// f against the load passed one offline test on the lab's own
		// recordings (f_load_curve.go), and its figures carry no flag of
		// their own.
		fLoadCurveFlag}
)

// statedOutside is the flags in flagsOutside that were set, as --name;
// --imbalance-across-counts and --load-aware-router only away from the
// planner's defaults, and --prefill-vp-service-line and
// --prefill-f-load-curve by their fields, whether the flag or the server's
// request set them. With --prometheus, capture's
// flags state nothing: the window's flags (windowFlags) and --batch-cap, the
// engine's cap the window is captured with (pricedBatchCap), say how the
// window is captured, as capture's own do for a recording, and the answer
// from that recording states none of them. Stating them here put the
// --prometheus answer's figures outside by "--batch-cap stated" where the
// recorded answer's read a flag (issue #359).
func (f *answerFlags) statedOutside() []string {
	var out []string
	var capture []string
	if f.prometheus() {
		capture = append(windowFlags(), "batch-cap")
	}
	for _, name := range flagsOutside {
		if slices.Contains(capture, name) {
			continue
		}
		// Either flag at the planner's default states nothing.
		if name == "imbalance-across-counts" && f.imbalanceAcrossCounts == whatif.DefaultPlannerImbalanceAcrossCounts {
			continue
		}
		if name == "load-aware-router" && f.loadAwareRouter == whatif.DefaultPlannerLoadAwareRouter {
			continue
		}
		// Read from the field, which the server sets from its request too.
		if name == vpServiceLineFlag {
			if f.vpServiceLine {
				out = append(out, "--"+name)
			}
			continue
		}
		if name == fLoadCurveFlag {
			if f.fLoadCurve {
				out = append(out, "--"+name)
			}
			continue
		}
		if f.set[name] {
			out = append(out, "--"+name)
		}
	}
	return out
}

// class is the class of a decode variant's own figures: its model,
// accelerator, tensor parallelism, replicas and shape. The prefill pods,
// the arrivals and flow control are not read: a figure a row bounds by them
// is outside unless the fleet's class is asked (TTFT asked).
func (pv pricedVariant) class() whatif.FigureClass {
	c := whatif.FigureClass{Topology: whatif.TopologyPD, Accelerator: pv.state.Accelerator, DecodeReplicas: pv.decode.Replicas,
		PromptTokens: pv.shape.AvgInputTokens, OutputTokens: pv.shape.AvgOutputTokens, Model: pv.model,
		TensorParallel: pv.state.TensorParallel, PrefixHitRate: pv.shape.PrefixHitRate, ArrivalsUnknown: true,
		Stated: pv.stated}
	if domain.CanonicalRole(pv.state.Role) == domain.RoleBoth {
		c.Topology = whatif.TopologyAggregated
	}
	return c
}

// class is the class of a fleet's figures: a spread or arrivals stated by a
// flag are known, as a measured one is; the defaults are not.
func (pf *pricedFleet) class() whatif.FigureClass {
	c := whatif.FleetClass(pf.fleet)
	c.PromptCVUnknown = pf.fleet.Prefill.PromptCVAssumed && !pf.statedCV
	c.ArrivalsUnknown = slices.Contains(pf.flags, assumedPoisson) && !slices.Contains(pf.flags, assumedArrivalCV2)
	c.Model, c.PrefillTensorParallel, c.Stated = pf.model, pf.prefillTP, pf.stated
	if pf.pv != nil {
		c.Accelerator, c.TensorParallel, c.PrefixHitRate = pf.pv.state.Accelerator, pf.pv.state.TensorParallel, pf.pv.shape.PrefixHitRate
		if pf.fleet.Decode == nil {
			c.DecodeReplicas, c.OutputTokens = pf.pv.decode.Replicas, pf.pv.shape.AvgOutputTokens
		}
		if domain.CanonicalRole(pf.pv.state.Role) == domain.RoleBoth {
			c.Topology = whatif.TopologyAggregated
		}
	}
	if r := pf.hitAtCount; r != nil && r.Moved() {
		// The hit priced at this count, not the one read: a priced hit
		// outside the measured span reads outside by it.
		c.PrefixHitRate = r.Hit
	}
	return c
}

// share is rate over knee; 0 where either is not a figure.
func share(rate, knee float64) float64 {
	if !(rate > 0 && knee > 0) {
		return 0
	}
	return rate / knee
}

// lowerKnee is the lower of a TTFT's two knees that is a figure.
func lowerKnee(t whatif.TTFT) float64 {
	k := t.PrefillKnee
	if t.DecodeKnee > 0 && (k <= 0 || t.DecodeKnee < k) {
		k = t.DecodeKnee
	}
	return k
}

// ttftValidation judges a TTFT priced at rate.
func (pf *pricedFleet) ttftValidation(q float64, rate float64, r whatif.Result) whatif.Validation {
	s := 0.0
	if t, ok := r.(whatif.TTFT); ok {
		s = share(rate, lowerKnee(t))
	}
	return judge(whatif.FigureTTFT, q, pf.class(), s, r)
}

// kneeValidation judges the prefill knee.
func (pf *pricedFleet) kneeValidation(r whatif.Result) whatif.Validation {
	return judge(whatif.FigurePrefillKnee, 0, pf.class(), 0, r)
}

// rateValidation judges a rate asked at a TTFT target, the prefill role's or
// the fleet's, as whatif.RateFigure reads it: by what bound it, at its share
// of the lower knee, which the prefill role's answer carries (role). The API
// judges its limits the same way.
func (pf *pricedFleet) rateValidation(q float64, r, role whatif.Result) whatif.Validation {
	a, ok := r.(whatif.Answer)
	if !ok {
		return judge(whatif.FigureRateAtTTFT, q, pf.class(), 0, r)
	}
	knee := 0.0
	if p, ok := role.(whatif.Answer); ok && p.Basis.TTFT != nil {
		knee = p.Basis.TTFT.KneeRate
	}
	by := make([]string, len(a.Basis.BoundBy))
	for i, b := range a.Basis.BoundBy {
		by[i] = string(b)
	}
	return whatif.DefaultEnvelope().Judge(whatif.RateFigure(a.Value, by, q, pf.class(), knee, a.Flags))
}

// decodeKneeValidation judges decode's knee in class c: the fleet's where a
// TTFT was asked, else the variant's own.
func decodeKneeValidation(c whatif.FigureClass, r whatif.Result) whatif.Validation {
	return judge(whatif.FigureDecodeKnee, 0, c, 0, r)
}

// tpotValidation judges a TPOT at rate in class c: the mean (q 0) or a
// percentile.
func tpotValidation(c whatif.FigureClass, q float64, rate float64, knee, r whatif.Result) whatif.Validation {
	kind := whatif.FigureMeanTPOT
	if q != 0 {
		kind = whatif.FigurePercentileTPOT
	}
	s := 0.0
	if k, ok := knee.(whatif.Answer); ok {
		s = share(rate, k.Value)
	}
	return judge(kind, q, c, s, r)
}

// rateValidation judges decode's rate at a TPOT target.
func (pv pricedVariant) rateValidation(r whatif.Result) whatif.Validation {
	return judge(whatif.FigureRateAtTPOT, 0, pv.class(), 0, r)
}

// validated is a result on the wire with its validation; a refusal has no
// figure to judge.
func validated(w *wireResult, v whatif.Validation) *wireResult {
	if w != nil && w.Answer != nil {
		w.Answer.Validation = whatifapi.ValidationOf(v)
	}
	return w
}

// validatedTTFT is validated for a TTFT on the wire.
func validatedTTFT(w *wireTTFTResult, v whatif.Validation) *wireTTFTResult {
	if w != nil && w.Answer != nil {
		w.Answer.Validation = whatifapi.ValidationOf(v)
	}
	return w
}

// settled is r less UnvalidatedTTFT where the envelope validates it: a
// figure the measured run covered is not printed as unvalidated beside its
// own "validated" label. The API's figures drop it the same way.
func settled(r whatif.Result, v whatif.Validation) whatif.Result {
	if !v.Validated() {
		return r
	}
	drop := func(flags []whatif.Flag) []whatif.Flag {
		return slices.DeleteFunc(slices.Clone(flags), func(f whatif.Flag) bool { return f == whatif.UnvalidatedTTFT })
	}
	switch a := r.(type) {
	case whatif.Answer:
		a.Flags = drop(a.Flags)
		return a
	case whatif.TTFT:
		a.Flags = drop(a.Flags)
		return a
	}
	return r
}
