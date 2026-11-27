package whatifapi

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// stubResolver resolves every request to one synthetic model: prefill knee
// 10 r/s per prefill replica, decode knee 12 r/s per decode replica, a TTFT
// and a TPOT rising with the rate. With refusePrefill, V_P is refused, as a
// recording that learned none: every TTFT figure and the fleet's limit are
// refusals.
type stubResolver struct {
	refusePrefill bool
	// fleets counts the fleets priced.
	fleets atomic.Int64
	// gate, when set, holds every Fleet until it is closed; onPlainLimits,
	// when set, is called with the count of PlainLimits calls so far.
	gate          chan struct{}
	onPlainLimits func(n int64)
	plainLimits   atomic.Int64
	// dip, when set, cuts the prefill knee of the fleets it holds for: a
	// limit that does not rise with the replicas.
	dip func(prefill, decode int) bool
	// onFleet, when set, is called with the knobs of every fleet priced.
	onFleet func(k Knobs)
	// loRefused, when set, refuses the low end of every fleet limit's span,
	// as a corner of V_P and f under a TTFT floor does.
	loRefused whatif.Reason
	// ttftDelay, when set, is how long every TTFT point takes: a figure as
	// slow as a plan near a knee.
	ttftDelay time.Duration
	// fleetErr, when set, is the error of every fleet it returns one for.
	fleetErr func(k Knobs) error
	// refuseFrom2, when set, refuses the prefill limit, and the fleet's,
	// with this reason from 2 prefill replicas on: a refusal that holds at
	// more replicas than one, as NoImbalance would with f unlearned.
	refuseFrom2 whatif.Reason
	// pinnedLine, when set, is a decode line pinned on the default
	// intercept (PinnedDefaultB): decode's knee, TPOT and TPOT limit are
	// refused, and with no TPOT asked decode's limit and the fleet's are the
	// knee's refusal, as Fleet.Limits says.
	pinnedLine bool
	// noPrefillRole, when set, is a source with no prefill variant: the
	// inputs carry no prefill role, and every prefill figure is refused
	// NoPrefillReplicas, as the CLI refuses a role the recording does not
	// hold.
	noPrefillRole bool
	// knees counts the DecodeKnee calls.
	knees atomic.Int64
	// gpus, when set, is each role's GPUs per replica the inputs state.
	gpus *FleetCounts
	// noPolicyTTFT, when set, is a policy that sets no TTFT target.
	noPolicyTTFT bool
}

var pinnedB = whatif.Unanswerable{Reason: whatif.PinnedDefaultB, Detail: "stub: a pinned-b line on the default intercept"}

var noPrefillVariant = whatif.Unanswerable{Reason: whatif.NoPrefillReplicas, Detail: "stub: no prefill variant"}

// prefillRefusal is the refusal of every prefill figure of a refused prefill
// role: no V_P, or no prefill role at all.
func (f *stubFleet) prefillRefusal() whatif.Unanswerable {
	if f.r.noPrefillRole {
		return noPrefillVariant
	}
	return noVP
}

func (r *stubResolver) Resolve(_ context.Context, req InputsRequest) (*Resolved, error) {
	switch req.Model {
	case "bad":
		return nil, InputErrorf("no model bad")
	case "boom":
		return nil, errors.New("the source broke")
	}
	in := InputsResponse{Model: "model-01", Namespace: "ns-01", Source: SourceInfo{Kind: SourceRecording, Name: "run.jsonl.gz", Cycle: 10, TotalCycles: 10},
		Fleet: FleetInfo{Prefill: &RoleInfo{Variant: "p", Replicas: 1}, Decode: RoleInfo{Variant: "d", Replicas: 1}},
		Observed: Observed{Rate: Figure{Refused: &Refusal{Reason: "NotRecorded", Detail: "stub"}}, Buckets: Buckets(),
			Mix: ObservedMix{Refused: &Refusal{Reason: "NoPerRequestLengths", Detail: "stub"}}},
		Policy: PolicyInfo{TTFT: &PolicyObjective{Statistic: "p90", Seconds: 0.2}, TPOT: &PolicyObjective{Statistic: "mean", Seconds: 0.012},
			Headroom: 0.85, FlowControl: "off, inferred"},
		Terms: []Term{}, Assumed: []string{"Poisson arrivals"}}
	if r.gpus != nil {
		in.Fleet.Prefill.GPUsPerReplica, in.Fleet.Decode.GPUsPerReplica = r.gpus.Prefill, r.gpus.Decode
	}
	if req.Model == "decode-only" || r.noPrefillRole {
		in.Fleet.Prefill = nil
	}
	if r.noPolicyTTFT {
		in.Policy.TTFT = nil
	}
	// ctx-16k is a source whose engines' arguments set a 16k context window.
	// ctx-refused and ctx-zero carry the term with no window: refused, and 0.
	switch req.Model {
	case "ctx-16k":
		in.Terms = []Term{{Name: TermMaxModelLen, Source: "engine arguments", Figure: Figure{Value: ptr(16384.0), Unit: "tokens"}}}
	case "ctx-refused":
		in.Terms = []Term{{Name: TermMaxModelLen, Figure: Figure{Refused: &Refusal{Reason: "NotRecorded", Detail: "stub"}}}}
	case "ctx-zero":
		in.Terms = []Term{{Name: TermMaxModelLen, Figure: Figure{Value: ptr(0.0), Unit: "tokens"}}}
	}
	return &Resolved{Inputs: in, Content: fmt.Sprintf("stub %v %s", r.refusePrefill, req.Model), Pricer: r}, nil
}

func (r *stubResolver) Fleet(k Knobs) (Fleet, error) {
	if k.Assume.VP != nil && *k.Assume.VP > 1e9 {
		return nil, InputErrorf("--assume-vp %g: too large", *k.Assume.VP)
	}
	if r.gate != nil {
		<-r.gate
	}
	if r.onFleet != nil {
		r.onFleet(k)
	}
	if r.fleetErr != nil {
		if err := r.fleetErr(k); err != nil {
			return nil, err
		}
	}
	r.fleets.Add(1)
	return &stubFleet{k: k, refusePrefill: r.refusePrefill || r.noPrefillRole, r: r}, nil
}

// stubFleet is one synthetic fleet.
type stubFleet struct {
	k             Knobs
	refusePrefill bool
	r             *stubResolver
	// model is the R_q an experimental model gave (WithTPOTRatio): the
	// percentile is the mean times it, and decode's limit at a percentile
	// target the mean's at the target over it.
	model *ModelRatio
}

// WithTPOTRatio is the fleet with its TPOT percentile priced at r.
func (f *stubFleet) WithTPOTRatio(r ModelRatio) Fleet {
	c := *f
	c.model = &r
	return &c
}

var noVP = whatif.Unanswerable{Reason: whatif.NoPrefillCapacity, Detail: "no P1 estimate"}

// prefillKnee is 10 r/s a replica at prompts up to 1000 tokens, less above,
// and a hundredth of it where the resolver's dip holds.
func (f *stubFleet) prefillKnee() float64 {
	k := 10 * float64(f.k.Prefill) * 1000 / max(f.k.PromptTokens, 1000)
	if f.r.dip != nil && f.r.dip(f.k.Prefill, f.k.Decode) {
		k /= 100
	}
	return k
}

// decodeKnee is 12 r/s a replica.
func (f *stubFleet) decodeKnee() float64 { return 12 * float64(f.k.Decode) }

func (f *stubFleet) PrefillKnee() whatif.Result {
	if f.refusePrefill {
		return f.prefillRefusal()
	}
	return whatif.Answer{Value: f.prefillKnee(), Flags: []whatif.Flag{whatif.UnvalidatedTTFT}}
}

func (f *stubFleet) DecodeKnee() whatif.Result {
	f.r.knees.Add(1)
	if f.r.pinnedLine {
		return pinnedB
	}
	return whatif.Answer{Value: f.decodeKnee(), Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundBatchCap}}}
}

func (f *stubFleet) Onsets() whatif.Result { return nil }

func (f *stubFleet) TTFTAt(rate float64) whatif.Result {
	time.Sleep(f.r.ttftDelay)
	if f.refusePrefill {
		return f.prefillRefusal()
	}
	if rate >= min(f.prefillKnee(), f.decodeKnee()) {
		return whatif.Unanswerable{Reason: whatif.Saturated, Detail: fmt.Sprintf("%g r/s at or above the knee", rate)}
	}
	s := 0.1 / (1 - rate/min(f.prefillKnee(), f.decodeKnee()))
	return whatif.TTFT{Seconds: s, Floor: 0.1, Flags: []whatif.Flag{whatif.UnvalidatedTTFT},
		Sensitivity: &whatif.Sensitivity{Lo: 0.9 * s, Hi: 1.1 * s}}
}

func (f *stubFleet) TTFTPoint(rate float64) (whatif.Result, []string) {
	return f.TTFTAt(rate), []string{"a stub's note at the rate"}
}

func (f *stubFleet) TPOTAt(rate float64) (mean, percentile whatif.Result) {
	if rate >= f.decodeKnee() {
		u := whatif.Unanswerable{Reason: whatif.Saturated, Detail: "at the decode knee"}
		return u, nil
	}
	mean = whatif.Answer{Value: 0.005 + rate/1000, Basis: whatif.Basis{Floor: 0.005}, Band: &whatif.Interval{Lo: 0.004, Hi: 0.02}}
	if f.k.TPOTPercentile != 0 {
		percentile = whatif.Unanswerable{Reason: whatif.NoTPOTHistogram, Detail: "no TPOT histogram"}
		switch {
		case f.model != nil:
			percentile = whatif.Answer{Value: f.model.Value * (0.005 + rate/1000), Flags: []whatif.Flag{f.model.Flag}}
		case f.k.Assume.TPOTRatio != nil:
			percentile = whatif.Answer{Value: *f.k.Assume.TPOTRatio * (0.005 + rate/1000), Flags: []whatif.Flag{whatif.AssumedTPOTRatio}}
		}
	}
	return mean, percentile
}

func (f *stubFleet) Limits(ttftSeconds, tpotSeconds, headroom float64) LimitSet {
	d, p, fl := f.PlainLimits(ttftSeconds, tpotSeconds, headroom)
	_, _, uncapped := f.PlainLimits(ttftSeconds, tpotSeconds, 1)
	// The fleet's limit spans 10% either way over V_P and f.
	if a, ok := fl.(whatif.Answer); ok {
		a.Sensitivity = &whatif.Sensitivity{Lo: 0.9 * a.Value, Hi: 1.1 * a.Value, Refused: f.r.loRefused}
		fl = a
	}
	return LimitSet{Decode: d, Prefill: p, Fleet: fl, WithoutHeadroom: uncapped, Notes: []string{"a stub's note at the target"}}
}

func (f *stubFleet) PlainLimits(ttftSeconds, tpotSeconds, headroom float64) (decode, prefill, fleet whatif.Result) {
	if n := f.r.plainLimits.Add(1); f.r.onPlainLimits != nil {
		f.r.onPlainLimits(n)
	}
	knee := min(f.prefillKnee(), f.decodeKnee())
	switch {
	case f.refusePrefill:
		prefill = f.prefillRefusal()
	case f.r.refuseFrom2 != "" && f.k.Prefill > 1:
		prefill = whatif.Unanswerable{Reason: f.r.refuseFrom2, Detail: "stub: refused from 2 prefill replicas on"}
	default:
		prefill = whatif.Answer{Value: knee * (1 - 0.1/ttftSeconds), Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundTTFT}}}
	}
	switch {
	case tpotSeconds > 0 && f.model != nil && f.k.TPOTPercentile != 0:
		// The mean's limit at the target over R_q.
		at := (tpotSeconds/f.model.Value - 0.005) * 1000
		decode = whatif.Answer{Value: min(headroom*f.decodeKnee(), at), Flags: []whatif.Flag{f.model.Flag},
			Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundTPOT}, Uncapped: at}}
	case tpotSeconds > 0:
		decode = whatif.Answer{Value: min(headroom*f.decodeKnee(), (tpotSeconds-0.005)*1000),
			Basis: whatif.Basis{BoundBy: []whatif.Bound{whatif.BoundTPOT}, Uncapped: (tpotSeconds - 0.005) * 1000}}
	}
	if f.r.pinnedLine {
		// With a TPOT target or none: the line prices neither TPOT nor the
		// knee. Decode's refusal is the fleet's first, as the CLI's is.
		decode = pinnedB
	}
	if u, ok := decode.(whatif.Unanswerable); ok {
		return u, prefill, u
	}
	if u, ok := prefill.(whatif.Unanswerable); ok {
		return decode, prefill, u
	}
	// A percentile with no ratio refuses decode's limit, and the fleet's.
	if tpotSeconds > 0 && f.k.TPOTPercentile != 0 && f.k.Assume.TPOTRatio == nil && f.model == nil {
		u := whatif.Unanswerable{Reason: whatif.NoTPOTHistogram, Detail: "no TPOT histogram"}
		return u, prefill, u
	}
	p, _ := valueOf(prefill)
	lowest, by := headroom*knee, whatif.BoundOperatingPoint
	if p < lowest {
		lowest, by = p, whatif.BoundTTFT
	}
	var flags []whatif.Flag
	if d, ok := decode.(whatif.Answer); ok && d.Value < lowest {
		lowest, by, flags = d.Value, "decode TPOT", d.Flags
	}
	return decode, prefill, whatif.Answer{Value: lowest, Flags: flags, Basis: whatif.Basis{BoundBy: []whatif.Bound{by}}}
}

func (f *stubFleet) TPOTRatio() Ratio {
	switch {
	case f.k.TPOTPercentile == 0:
		return Ratio{}
	case f.model != nil:
		v := f.model.Value
		return Ratio{Value: &v, Source: f.model.Source, Flags: []string{string(f.model.Flag)}}
	case f.k.Assume.TPOTRatio == nil:
		return Ratio{Refused: &Refusal{Reason: string(whatif.NoTPOTHistogram), Detail: "no TPOT histogram"}}
	}
	return Ratio{Value: f.k.Assume.TPOTRatio, Source: "assumed", Flags: []string{string(whatif.AssumedTPOTRatio)}}
}

func (f *stubFleet) Assumed() []string {
	if f.model != nil {
		return []string{"Poisson arrivals", f.model.Assumed}
	}
	return []string{"Poisson arrivals"}
}

// Class is a P/D fleet the envelope knows nothing else of.
func (f *stubFleet) Class() whatif.FigureClass {
	return whatif.FigureClass{Topology: whatif.TopologyPD}
}
