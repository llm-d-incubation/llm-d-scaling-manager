package whatif

import (
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// The joint prefill and decode TTFT model, J1 of
// docs/proposals/slo-capacity-planner-ttft.md ("Proposed: joint prefill and
// decode TTFT sizing"): the mean TTFT at a fleet of N_p prefill and N_d
// decode pods, from the inputs prefill.JointWindow learns,
//
//	TTFT(N_p, N_d) = a(ρ_p) + T_P + T_X + m_d·ITL(k) + d + R
//
//   - a(ρ_p) = a₀/(1 − ρ_p), the prefill pod's overhead, stretched;
//   - T_P = S/(1 − ρ_p), the prefill pod as processor sharing, ρ_p = λ_p·S
//     with λ_p = λ/N_p;
//   - T_X = X/(1 − λ_d·X), the KV pull as processor sharing on the decode
//     pod, λ_d = λ/N_d, X the pull table's unshared work at ρ_p;
//   - m_d·ITL(k) + d, the decode pod's own part, ITL(k) = α + β·k at the
//     batch the closed loop holds, k = n_out·λ_d·α/(1 − n_out·λ_d·β);
//   - R, the router hop, as built.
//
// It prices the mean only, with the arrivals split evenly over each role's
// pods. The percentile and the count search are J2. In J1 nothing acts on
// it: the slo analyzer publishes it beside S1, and the what-if CLI prints it.

// The joint model's flags.
const (
	// PullUnseen is a prefill utilization past the highest bucket of the pull
	// table: the pull is priced at that bucket's work, which reads low,
	// since the pull grows with the prefill load. J2's search treats such a
	// count as not meeting the target.
	PullUnseen Flag = "PullUnseen"
	// OverheadMeasured is a₀ from the prefill pods' histograms, not P1's a.
	// Every joint answer carries it.
	OverheadMeasured Flag = "OverheadMeasured"
)

// The joint model's refusals, in the vocabulary of llmsm_slo_ttft_reason.
const (
	JointNoPrefillCapacity = "no-prefill-capacity"
	JointNoOverhead        = "no-overhead"
	JointNoPull            = "no-pull"
	JointNoITLLine         = "no-itl-line"
	JointNoDecodeOwnLine   = "no-decode-own-line"
	JointNoOutputLength    = "no-output-length"
	JointNoReplicas        = "no-replicas"
	// JointSaturated is a prefill pod or a pull at or past its knee.
	JointSaturated = "saturated"
	// JointBatchUnheld is n_out·λ_d·β ≥ 1: the decode batch grows without
	// bound at that rate.
	JointBatchUnheld  = "batch-unheld"
	JointInvalidInput = "input-invalid"
)

// JointQuestion is the fleet and load the joint model is priced at.
type JointQuestion struct {
	// PrefillReplicas and DecodeReplicas are N_p and N_d.
	PrefillReplicas, DecodeReplicas int
	// Rate is the model's arrival rate, requests/s.
	Rate float64
	// PromptTokens is the prompt asked about; 0 is the prompt the work was
	// read at. The prefill work and the pull scale with it.
	PromptTokens float64
	// HopSeconds is R; HopAssumed says it was not measured.
	HopSeconds float64
	HopAssumed bool
}

// JointTTFT is the joint model's mean TTFT and its parts, seconds.
type JointTTFT struct {
	Seconds float64
	// Overhead is a(ρ_p), Prefill T_P, Pull T_X, Decode m_d·ITL(k) + d, and
	// Hop R. Engine is the sum less R: what the engines' own TTFT histograms
	// measure, the prefill pod's and the decode pod's.
	Overhead, Prefill, Pull, Decode, Hop, Engine float64
	// PrefillUtilization is ρ_p, PullWork X, PullUtilization λ_d·X, Batch k,
	// and ITL ITL(k).
	PrefillUtilization, PullWork, PullUtilization, Batch, ITL float64
	Flags                                                     []Flag
}

// HasFlag reports whether the mean carries f.
func (j JointTTFT) HasFlag(f Flag) bool {
	return slices.Contains(j.Flags, f)
}

// JointTTFTMean is the joint model's mean TTFT at q, or why it has none: one
// of the Joint* refusals.
func JointTTFTMean(est prefill.JointEstimate, q JointQuestion) (JointTTFT, string) {
	switch {
	case q.PrefillReplicas < 1 || q.DecodeReplicas < 1:
		return JointTTFT{}, JointNoReplicas
	case !Finite(q.Rate) || q.Rate < 0 || !Finite(q.PromptTokens) || q.PromptTokens < 0 ||
		!Finite(q.HopSeconds) || q.HopSeconds < 0:
		return JointTTFT{}, JointInvalidInput
	case est.PrefillWorkRefused != "" || !(est.PrefillWork > 0):
		return JointTTFT{}, JointNoPrefillCapacity
	case est.OverheadRefused != "" || !(est.Overhead > 0):
		return JointTTFT{}, JointNoOverhead
	case est.PullRefused != "" || len(est.Pull) == 0:
		return JointTTFT{}, JointNoPull
	case est.ITLRefused != "":
		return JointTTFT{}, JointNoITLLine
	case est.DecodeRefused != "":
		return JointTTFT{}, JointNoDecodeOwnLine
	case !(est.OutputTokens > 0):
		return JointTTFT{}, JointNoOutputLength
	}
	scale := func(atPrompt float64) float64 {
		if q.PromptTokens > 0 && atPrompt > 0 {
			return q.PromptTokens / atPrompt
		}
		return 1
	}
	out := JointTTFT{Flags: []Flag{OverheadMeasured}}
	s := est.PrefillWork * scale(est.PrefillWorkPrompt)
	out.PrefillUtilization = q.Rate / float64(q.PrefillReplicas) * s
	if out.PrefillUtilization >= 1 {
		return JointTTFT{}, JointSaturated
	}
	stretch := 1 / (1 - out.PrefillUtilization)
	out.Overhead, out.Prefill = est.Overhead*stretch, s*stretch

	bucket, unseen := pullAt(est.Pull, out.PrefillUtilization)
	if unseen {
		out.Flags = append(out.Flags, PullUnseen)
	}
	lambdaD := q.Rate / float64(q.DecodeReplicas)
	out.PullWork = bucket.Seconds * scale(bucket.PromptTokens)
	out.PullUtilization = lambdaD * out.PullWork
	if out.PullUtilization >= 1 {
		return JointTTFT{}, JointSaturated
	}
	out.Pull = out.PullWork / (1 - out.PullUtilization)

	loop := est.OutputTokens * lambdaD * est.ITLBeta
	if loop >= 1 {
		return JointTTFT{}, JointBatchUnheld
	}
	out.Batch = est.OutputTokens * lambdaD * est.ITLAlpha / (1 - loop)
	out.ITL = est.ITLAlpha + est.ITLBeta*out.Batch
	out.Decode = est.DecodeSteps*out.ITL + est.DecodeFixed

	out.Hop = q.HopSeconds
	out.Engine = out.Overhead + out.Prefill + out.Pull + out.Decode
	out.Seconds = out.Engine + out.Hop
	if !Finite(out.Seconds) || out.Seconds < 0 {
		return JointTTFT{}, JointInvalidInput
	}
	return out, ""
}

// pullAt is the pull table's bucket for prefill utilization rho, and whether
// rho is past the highest bucket seen. Below the lowest bucket seen it is the
// lowest, and in a gap between seen buckets the next one up: the pull grows
// with the prefill load, so either reads high. Past the highest it is the
// highest, which reads low, and is flagged.
func pullAt(table []prefill.PullBucket, rho float64) (prefill.PullBucket, bool) {
	b := math.Floor(rho/prefill.PullBucketWidth+1e-9) * prefill.PullBucketWidth
	for _, x := range table {
		if x.Utilization >= b-1e-9 {
			return x, false
		}
	}
	return table[len(table)-1], true
}

// HopAt is R at a prompt of i tokens, as the decode term prices it: its
// fixed part and its per-token part at i (PromptLine.split).
func (t DecodeTerm) HopAt(i float64) float64 {
	fixed, perToken := t.Hop.split(i)
	return fixed + perToken*i
}
