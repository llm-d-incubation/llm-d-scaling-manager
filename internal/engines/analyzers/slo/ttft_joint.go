package slo

import (
	"math"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// J1 of the joint prefill and decode TTFT sizing
// (docs/proposals/slo-capacity-planner-ttft.md, "Proposed: joint prefill and
// decode TTFT sizing"): the joint model's inputs, learned in shadow. For a
// model with one prefill and one decode variant, each cycle's rows go to a
// prefill.JointWindow; what it supports is published on the prefill
// variant's TTFT reading (metrics.SLOTTFTJointReading), with the joint mean
// at the running fleet. Nothing acts on it: no ask, no composite, and the
// engine's exclusions of slo stand.
//
// One input reaches S1: the inverted V_P (prefill.SourceInverted) stands
// where P1 refused every source that needs a queue. A pod that queues has a
// queueing source, which wins.

// SwitchTTFTJointInputs is the replay switch for J1: on, the joint inputs
// are learned and published, and the inverted V_P stands where P1 refused
// V_P; off, as before, neither.
const SwitchTTFTJointInputs = "slo-ttft-joint-inputs"

// jointWindow is a model's joint readings, keyed by its prefill variant.
type jointWindow struct {
	readings *prefill.JointWindow
	// decodeKey is the decode variant's window key: forgetting it forgets
	// this window too.
	decodeKey string
	seen      time.Time
}

// jointCycle is what one cycle's joint pricing read and gave.
type jointCycle struct {
	est   prefill.JointEstimate
	price whatif.JointTTFT
	// reason is constants.SLOTTFTJointReasonPriced or a whatif.Joint*
	// refusal.
	reason string
	// question is what the mean was priced at.
	question whatif.JointQuestion
	// engineMeasured is the prefill and the decode pods' mean TTFT summed,
	// router excluded; routerMeasured the router's mean TTFT; NaN unread.
	engineMeasured, routerMeasured float64
}

// noteJoint folds the cycle into the model's joint window and prices the
// joint mean at the running fleet; nil with the switch off or not one
// variant per role.
func (a *Analyzer) noteJoint(input domain.AnalyzerInput, fleet fleetReading, prefills, decodes []string,
	prefillRows, decodeRows []domain.ReplicaMetrics, arrival float64, term *whatif.DecodeTerm, now time.Time) *jointCycle {
	if !a.ttftJointInputs || len(prefills) != 1 || len(decodes) != 1 {
		return nil
	}
	key := ttftWindowKey(input.Namespace, input.ModelID, prefills[0])
	var tag strings.Builder
	for _, v := range []string{prefills[0], decodes[0]} {
		args := a.engineParams(input.Namespace, input.ModelID, v).ArgsTag()
		for _, vs := range fleet.variants {
			if vs.VariantName == v {
				tag.WriteString(vs.AcceleratorName + "|" + args + "|")
			}
		}
	}
	cycle := prefill.JointCycle{
		Now: now, Prefill: prefillRows, Decode: decodeRows, SGLang: a.sglang(input, prefills),
		Settle: &prefill.Settle{View: a.settleView(), Model: utils.GetNamespacedKey(input.Namespace, input.ModelID)},
	}
	a.mu.Lock()
	w := a.joint[key]
	if w == nil {
		w = &jointWindow{readings: prefill.NewJointWindow()}
		a.joint[key] = w
	}
	w.seen, w.decodeKey = now, ttftWindowKey(input.Namespace, input.ModelID, decodes[0])
	w.readings.Under(tag.String())
	w.readings.Observe(cycle)
	est := w.readings.Estimate()
	a.mu.Unlock()

	q := whatif.JointQuestion{
		PrefillReplicas: fleet.ready[prefills[0]], DecodeReplicas: fleet.ready[decodes[0]], Rate: arrival,
	}
	q.HopSeconds, q.HopAssumed = whatif.AssumedRouterHopSeconds, true
	if term != nil && !term.HopAssumed {
		q.HopSeconds, q.HopAssumed = term.HopAt(est.PrefillWorkPrompt), false
	}
	out := &jointCycle{est: est, question: q, engineMeasured: math.NaN(), routerMeasured: math.NaN()}
	price, refused := whatif.JointTTFTMean(est, q)
	out.price, out.reason = price, refused
	if refused == "" {
		out.reason = constants.SLOTTFTJointReasonPriced
		if q.HopAssumed {
			out.price.Flags = append(out.price.Flags, whatif.RouterHopAssumed)
		}
	}
	p, pOK := decodeEngineTTFT(prefillRows)
	d, dOK := decodeEngineTTFT(decodeRows)
	if pOK && dOK {
		out.engineMeasured = p + d
	}
	if r := input.RouterLatency; r != nil && r.TTFTCountRate > 0 && r.TTFTSumRate > 0 {
		out.routerMeasured = r.TTFTSumRate / r.TTFTCountRate
	}
	return out
}

// withInvertedVP is est with the inverted V_P where P1 refused V_P and the
// joint window supports one; est unchanged otherwise.
func withInvertedVP(est prefill.Estimate, j *jointCycle) prefill.Estimate {
	if j == nil || est.VP > 0 && est.VPRefused == "" || !(j.est.VP > 0) || j.est.PrefillWorkRefused != "" {
		return est
	}
	est.VP, est.VPSource, est.VPRefused = j.est.VP, prefill.SourceInverted, ""
	est.VPLowerBound, est.VPLowerBoundReason = false, ""
	est.VPSamples, est.VPPromptTokens = j.est.PrefillWorkSamples, j.est.PrefillWorkPrompt
	return est
}

// reading is the cycle's joint reading as the snapshot holds it.
func (j *jointCycle) reading() *metrics.SLOTTFTJointReading {
	if j == nil {
		return nil
	}
	e := j.est
	inputs := map[string]float64{}
	put := func(name string, v float64, ok bool) {
		if ok && whatif.Finite(v) {
			inputs[name] = v
		}
	}
	put(constants.SLOTTFTJointPrefillWork, e.PrefillWork, e.PrefillWorkRefused == "")
	put(constants.SLOTTFTJointPromptTokens, e.PrefillWorkPrompt, e.PrefillWorkRefused == "")
	put(constants.SLOTTFTJointOverhead, e.Overhead, e.OverheadRefused == "")
	put(constants.SLOTTFTJointPrefillUtilization, e.PrefillUtilization, e.PrefillUtilizationKnown)
	if j.reason == constants.SLOTTFTJointReasonPriced {
		put(constants.SLOTTFTJointPull, j.price.PullWork, true)
	}
	put(constants.SLOTTFTJointITLAlpha, e.ITLAlpha, e.ITLRefused == "")
	put(constants.SLOTTFTJointITLBeta, e.ITLBeta, e.ITLRefused == "")
	put(constants.SLOTTFTJointDecodeSteps, e.DecodeSteps, e.DecodeRefused == "")
	put(constants.SLOTTFTJointDecodeFixed, e.DecodeFixed, e.DecodeRefused == "")
	put(constants.SLOTTFTJointOutputTokens, e.OutputTokens, e.OutputTokens > 0)
	out := &metrics.SLOTTFTJointReading{Inputs: inputs, MeanSeconds: math.NaN(), Reason: j.reason}
	if j.reason == constants.SLOTTFTJointReasonPriced {
		out.MeanSeconds = j.price.Seconds
		out.Flags = publishedFlags(j.price.Flags)
	}
	return out
}

// logJoint is the joint model's line in the cycle log: its inputs, its mean
// and parts at the running fleet, and what the engines and the router
// measured beside it.
func logJoint(logger logr.Logger, input domain.AnalyzerInput, variant string, j *jointCycle) {
	log := logger.V(logging.VERBOSE)
	if j == nil || !log.Enabled() {
		return
	}
	e, p := j.est, j.price
	nan := math.NaN()
	mean, engine := nan, nan
	if j.reason == constants.SLOTTFTJointReasonPriced {
		mean, engine = p.Seconds, p.Engine
	}
	log.Info("slo-ttft-joint",
		"modelID", input.ModelID, "namespace", input.Namespace, "variant", variant, "reason", j.reason,
		"rate", j.question.Rate, "prefillPods", j.question.PrefillReplicas, "decodePods", j.question.DecodeReplicas,
		"mean", mean, "engine", engine, "engineMeasured", j.engineMeasured, "routerMeasured", j.routerMeasured,
		"overhead", p.Overhead, "prefill", p.Prefill, "pull", p.Pull, "decode", p.Decode, "hop", p.Hop,
		"hopAssumed", j.question.HopAssumed, "rhoP", p.PrefillUtilization, "pullWork", p.PullWork,
		"rhoPull", p.PullUtilization, "batch", p.Batch, "itl", p.ITL, "flags", p.Flags,
		"prefillWork", e.PrefillWork, "prompt", e.PrefillWorkPrompt, "vp", e.VP, "workSamples", e.PrefillWorkSamples,
		"workRefused", string(e.PrefillWorkRefused), "a0", e.Overhead, "a0Samples", e.OverheadSamples,
		"pullBuckets", len(e.Pull), "pullRefused", string(e.PullRefused),
		"itlAlpha", e.ITLAlpha, "itlBeta", e.ITLBeta, "itlSamples", e.ITLSamples, "itlRefused", string(e.ITLRefused),
		"decodeSteps", e.DecodeSteps, "decodeFixed", e.DecodeFixed, "decodeSamples", e.DecodeSamples,
		"decodeRefused", string(e.DecodeRefused), "outputTokens", e.OutputTokens)
}
