package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The TTFT half of both questions (docs/proposals/slo-capacity-planner.md,
// "Recording mode, as built"): the fleet is read from the replayed state, by
// the rules the SLO analyzer's shadow pricing follows
// (docs/proposals/slo-tiers.md, "S1 decisions"), and priced by P3
// (internal/signals/whatif, TTFTAt and RateForTTFT). The CLI prices one P/D
// fleet. Aggregated TTFT and per-bucket answers are the planner's M1, and are
// refused.
//
// Every assumed input is named twice: in the flags, and in the words of the
// fleet's assumed list, which the text prints and the JSON carries.

// The words the JSON and the text name a source or a role by.
const (
	sourceLearned    = "learned"
	sideDecodeEngine = "decode engine"
	rolePrefill      = "prefill"
)

// The flags S1 raises beside P3's, under S1's names.
const (
	// assumedPoisson is Poisson arrivals: P3 has no flag for them.
	assumedPoisson whatif.Flag = "AssumedPoissonArrivals"
	// promptSpreadAbovePeak is a cv above whatif.PromptCVPeak for the percentile: the
	// prompt's own tail shrinks as cv rises from there, so a larger cv can
	// price a shorter floor and a higher rate.
	promptSpreadAbovePeak whatif.Flag = "PromptSpreadAbovePeak"
	// routingBound is one pod taking at least whatif.RoutingBoundShare of the work,
	// sustained: more pods would mostly sit idle behind the router. Priced at
	// that f.
	routingBound whatif.Flag = "RoutingBound"
	// imbalanceNotSustained is an f at whatif.RoutingBoundShare of the pods that has
	// not held over the whole window: chance alone can read that at two pods.
	// Priced at it, the side that asks for more replicas.
	imbalanceNotSustained whatif.Flag = "ImbalanceNotSustained"
)

// The flags the CLI alone raises.
const (
	// assumedArrivalCV2 is a Ca^2 --arrival-cv2 supplied.
	assumedArrivalCV2 whatif.Flag = "AssumedArrivalCV2"
)

// outputSpreadUnreadWords is whatif.OutputSpreadUnread in words, on the assumed
// row and the flags line. The limit is shape.WithOutputDispersion's, at IL = 2 OL.
const outputSpreadUnreadWords = "no output-length histogram: KVreq is ILeff + OL/2, which over-promises by 7-44% on a real spread"

// The closing note's measured halves. The figures are the frozen P2 table's
// (TestTTFTFrozen_TheNotesFigures, learnreplay).
const (
	ttftNoteOpen     = "TTFT is UNVALIDATED until a fresh frozen run passes, and is not a bound: "
	ttftNoteAssumed  = "at the assumed prompt cv %s it read 1.1x to 4.1x the router's percentile on the P2 recordings; at the sent spread (cv 0.06) up to 34%% under it."
	ttftNoteMeasured = "at the sent spread (cv 0.06) it read up to 34% under the router's percentile on the P2 recordings."
	// ttftNoteArrivals stands for both: the P2 table was read at Poisson arrivals.
	ttftNoteArrivals = "no recording has checked it at a stated Ca^2."
)

// assumedA is a --assume-a supplied: P3 has no flag for it, as S1 has no
// assumption for a.
const assumedA whatif.Flag = "AssumedA"

// ttftQuestion is the TTFT a command was asked for.
type ttftQuestion struct {
	// q is the percentile in (0, 1), or 0 for the mean.
	q float64
	// seconds is the target, question 1 only.
	seconds float64
}

// label names the figure: "TTFT p90", or "mean TTFT".
func (t ttftQuestion) label() string {
	if t.q == 0 {
		return "mean TTFT"
	}
	// To a millionth of a percent: 0.57 * 100 is 56.99999999999999.
	return "TTFT p" + strconv.FormatFloat(math.Round(1e8*t.q)/1e6, 'f', -1, 64)
}

// checkTTFT validates the TTFT flags both questions share. It changes no
// flag.
func (f *answerFlags) checkTTFT() error {
	switch {
	case f.set["input-bucket"]:
		return usagef("--input-bucket: per-bucket answers are not built (the planner's M1)")
	case f.set["assume-vp"] && !(f.assumeVP > 0 && finite(f.assumeVP)):
		return usagef("--assume-vp %g, want a finite throughput above 0, in prompt tokens per second", f.assumeVP)
	case f.set["assume-prompt-cv"] && !(f.assumePromptCV >= 0 && f.assumePromptCV <= whatif.PromptCVStop):
		return usagef("--assume-prompt-cv %g, want a cv from 0 to %g: past that the rate stops falling as the cv rises", f.assumePromptCV, whatif.PromptCVStop)
	case f.set["arrival-cv2"] && !(f.arrivalCV2 > 0 && finite(f.arrivalCV2)):
		return usagef("--arrival-cv2 %g, want a finite value above 0: P3 reads 0 as Poisson, not as evenly spaced arrivals", f.arrivalCV2)
	case f.set["assume-router-hop"] && f.assumeRouterHop < 0:
		return usagef("--assume-router-hop %s, want a duration of 0 or more", f.assumeRouterHop)
	case f.set["assume-a"] && f.assumeA <= 0:
		return usagef("--assume-a %s, want a duration above 0", f.assumeA)
	case f.prefill < 0:
		return usagef("--prefill %d, want 0 (the recorded count) or more", f.prefill)
	case f.ttftFlagged() && f.set["headroom"] && f.question == replicasQuestion && !(f.headroom > 0 && f.headroom < 1):
		return usagef("--headroom %g, want a fraction of the knee in (0, 1): at the knee no TTFT exists", f.headroom)
	case f.ttftFlagged() && f.set["headroom"] && !(f.headroom > 0 && f.headroom <= 1):
		return usagef("--headroom %g, want a fraction of the knee in (0, 1]", f.headroom)
	}
	return nil
}

// ttftFlagged reports whether any flag asks about TTFT.
func (f *answerFlags) ttftFlagged() bool {
	return slices.ContainsFunc([]string{"ttft", "ttft-percentile", "assume-vp", "assume-a", "assume-prompt-cv", "arrival-cv2", "assume-router-hop",
		"prefill", "prefill-variant"},
		func(name string) bool { return f.set[name] })
}

// replicasTTFT reads question 2's TTFT flags, before any replay, so that a
// bad flag is refused at once: nil when none asks about TTFT, and q 0 with no
// --ttft-percentile, for policyPercentile to fill.
func (f *answerFlags) replicasTTFT() (*ttftQuestion, error) {
	if !f.ttftFlagged() {
		return nil, nil
	}
	if f.ttft != "" {
		return nil, usagef("--ttft is a target, for slo-to-rate; replicas-to-slo takes --ttft-percentile")
	}
	if f.ttftPercentile == "" {
		return &ttftQuestion{}, nil
	}
	q, err := parseTTFTPercent("--ttft-percentile", f.ttftPercentile)
	if err != nil {
		return nil, err
	}
	return &ttftQuestion{q: q}, nil
}

// policyPercentile sets a percentile not asked for to the policy's, else 90.
func (t *ttftQuestion) policyPercentile(st *learnreplay.State) {
	if t == nil || t.q != 0 {
		return
	}
	pct := config.DefaultTTFTPercentile
	if st.Policy != nil && st.Policy.SLO != nil && st.Policy.SLO.TTFT != nil {
		pct = st.Policy.SLO.TTFTPercentile()
	}
	t.q = float64(pct) / 100
}

// rateTTFT reads question 1's TTFT flags: --ttft, a mean or with @p and a
// percentile, or --ttft-percentile beside a plain target. Nil when none asks
// about TTFT.
func (f *answerFlags) rateTTFT() (*ttftQuestion, error) {
	if f.ttft == "" {
		if f.ttftFlagged() {
			return nil, usagef("--ttft-percentile, --assume-vp, --assume-a, --assume-prompt-cv, --arrival-cv2, --assume-router-hop, --prefill and --prefill-variant need a --ttft target on slo-to-rate")
		}
		return nil, nil
	}
	value, pct, hasPct := strings.Cut(f.ttft, "@")
	d, err := time.ParseDuration(value)
	if err != nil {
		return nil, usagef("--ttft %q: %v", f.ttft, err)
	}
	if d <= 0 {
		return nil, usagef("--ttft %q: want a target above 0", f.ttft)
	}
	t := &ttftQuestion{seconds: d.Seconds()}
	switch {
	case hasPct && f.ttftPercentile != "":
		return nil, usagef("--ttft %q and --ttft-percentile both name a percentile; give one", f.ttft)
	case hasPct && !strings.HasPrefix(pct, "p"):
		return nil, usagef("--ttft %q: a percentile is written @p90", f.ttft)
	case hasPct:
		t.q, err = parseTTFTPercent("--ttft", pct)
	case f.ttftPercentile != "":
		t.q, err = parseTTFTPercent("--ttft-percentile", f.ttftPercentile)
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// parseTTFTPercent reads a TTFT percentile in percent, refusing one outside
// (0, 100) here: a question asked wrongly outranks what the recording says.
func parseTTFTPercent(flagName, s string) (float64, error) {
	q, err := parsePercent(s)
	if err != nil {
		return 0, usagef("%s %q: %v", flagName, s, err)
	}
	if !(q > 0 && q < 1) {
		return 0, usagef("%s %q: want a percentile above 0 and below 100", flagName, s)
	}
	return q, nil
}

// pricedFleet is one P/D fleet as a TTFT question prices it: the prefill role
// and the decode variant it hands to.
type pricedFleet struct {
	// pv is the decode variant; nil with none in the recording.
	pv *pricedVariant
	// refused is why the fleet cannot be priced before the library is asked:
	// an input with no source.
	refused *whatif.Unanswerable
	fleet   whatif.TTFTFleet
	// variant names the prefill variant, and q is the percentile asked.
	variant string
	q       float64
	// assumedHop is the hop --assume-router-hop supplied, nil with none.
	assumedHop *time.Duration
	// assumedA is an a --assume-a supplied.
	assumedA bool
	// promptAssumed is a prompt --assume-input-tokens supplied: no reading of
	// the decode rows' prompt says what its prefix hits are.
	promptAssumed bool
	// assumedVP is a V_P --assume-vp supplied.
	assumedVP bool
	// statedCV is a cv --assume-prompt-cv supplied, in in.PromptCV.
	statedCV bool
	// flags are the fleet's own, beside P3's.
	flags []whatif.Flag
	// inputsText is where the fleet's inputs came from, for the human
	// output; in the same for the JSON. in.Assumed names each assumed input.
	inputsText string
	in         wireTTFT
}

// label names a fleet's rows, when more than one decode variant is priced.
func (pf *pricedFleet) label(role string, fleets int) string {
	if fleets > 1 && pf.pv != nil {
		return role + "/" + pf.pv.state.Variant
	}
	return role
}

// prefillState is the prefill variant the question prices: the recording's
// only one, or the one --prefill-variant names. Nil with none. Several, with
// none named, is a usage error: the CLI prices one fleet, and the whole rate
// on one of several would read wrong.
func (f *answerFlags) prefillState(st *learnreplay.State) (*learnreplay.PrefillState, error) {
	if f.prefillVariant != "" {
		for i := range st.Prefill {
			if st.Prefill[i].Variant == f.prefillVariant {
				return &st.Prefill[i], nil
			}
		}
		return nil, usagef("--prefill-variant %q: no prefill variant of that name at this cycle", f.prefillVariant)
	}
	switch len(st.Prefill) {
	case 0:
		return nil, nil
	case 1:
		return &st.Prefill[0], nil
	}
	names := make([]string, len(st.Prefill))
	for i := range st.Prefill {
		names[i] = st.Prefill[i].Variant
	}
	return nil, usagef("the model has %d prefill variants (%s); name one with --prefill-variant", len(names), strings.Join(names, ", "))
}

// ttftFleets is the fleet of each decode variant, in order; with no decode
// variant, one fleet with no decode side.
func (f *answerFlags) ttftFleets(st *learnreplay.State, q ttftQuestion, variants []pricedVariant) ([]*pricedFleet, error) {
	ps, err := f.prefillState(st)
	if err != nil {
		return nil, err
	}
	if len(variants) == 0 {
		return []*pricedFleet{f.fleetFor(st, ps, q, nil)}, nil
	}
	decodes := 0
	for _, vs := range st.Variants {
		if domain.CanonicalRole(vs.Role) != domain.RolePrefill {
			decodes++
		}
	}
	fleets := make([]*pricedFleet, len(variants))
	for i := range variants {
		pf := f.fleetFor(st, ps, q, &variants[i])
		if pf.refused == nil && decodes > 1 {
			// As S1 prices decode: the model's whole rate.
			pf.assume("the whole rate on decode variant %s (the model has %d)", variants[i].state.Variant, decodes)
		}
		fleets[i] = pf
	}
	return fleets, nil
}

// noPrefillRole is the refusal for a fleet with no prefill variant: aggregated
// TTFT is M1, and a role the recording does not hold has no a to price.
func noPrefillRole(st *learnreplay.State) *whatif.Unanswerable {
	for _, vs := range st.Variants {
		if domain.CanonicalRole(vs.Role) == domain.RoleBoth {
			return aggregatedRefusal(vs.Variant)
		}
	}
	return &whatif.Unanswerable{Reason: whatif.NoPrefillReplicas,
		Detail: fmt.Sprintf("no prefill variant at cycle %d of %s/%s", st.Cycle, st.Namespace, st.ModelID)}
}

// aggregatedRefusal refuses TTFT on an aggregated variant.
func aggregatedRefusal(variant string) *whatif.Unanswerable {
	return &whatif.Unanswerable{Reason: whatif.NoPrefillReplicas,
		Detail: fmt.Sprintf("variant %s is aggregated: aggregated TTFT is not built (the planner's M1); the model prices P/D fleets only", variant)}
}

// fleetFor reads one fleet, and every input's source, from the state.
func (f *answerFlags) fleetFor(st *learnreplay.State, ps *learnreplay.PrefillState, q ttftQuestion, pv *pricedVariant) *pricedFleet {
	pf := f.readFleet(st, ps, q, pv)
	if pf.refused != nil {
		f.nameStatedInputs(pf)
	}
	return pf
}

// nameStatedInputs carries what --assume-prompt-cv and --arrival-cv2 asked onto
// a refused fleet: nothing was priced at them, but the output must not read as
// though they were dropped, nor quote a figure at the cv the flag replaced.
func (f *answerFlags) nameStatedInputs(pf *pricedFleet) {
	r := *pf.refused
	r.Flags = slices.Clone(r.Flags)
	if f.set["assume-prompt-cv"] {
		pf.statedCV = true
		pf.in.PromptCV, pf.in.PromptCVSource = f.assumePromptCV, shapeAssumed
		pf.assume("prompt cv %s (--assume-prompt-cv)", num(f.assumePromptCV))
		r.Flags = append(r.Flags, whatif.AssumedPromptSpread)
	}
	if f.set["arrival-cv2"] {
		pf.in.ArrivalCV2, pf.in.ArrivalsSource = f.arrivalCV2, shapeAssumed
		pf.assume("arrivals Ca^2 %s (--arrival-cv2)", num(f.arrivalCV2))
		r.Flags = append(r.Flags, assumedArrivalCV2)
	}
	pf.refused = &r
}

// processorSharing is how the CLI prices the prefill side: as
// slo.SwitchTTFTProcessorSharing on. The CLI has no switch; a test that holds
// the first-come arithmetic turns it off (firstCome).
var processorSharing = true

// readFleet is fleetFor before a refusal names the stated inputs.
func (f *answerFlags) readFleet(st *learnreplay.State, ps *learnreplay.PrefillState, q ttftQuestion, pv *pricedVariant) *pricedFleet {
	pf := &pricedFleet{pv: pv, q: q.q, flags: []whatif.Flag{assumedPoisson},
		in: wireTTFT{Percentile: q.q, TargetSeconds: q.seconds, Assumed: []string{}}}
	if pv != nil {
		pf.in.DecodeVariant = pv.state.Variant
		if domain.CanonicalRole(pv.state.Role) == domain.RoleBoth {
			pf.refused = aggregatedRefusal(pv.state.Variant)
			return pf
		}
	}
	if ps == nil {
		pf.refused = noPrefillRole(st)
		return pf
	}
	pf.variant = ps.Variant
	p, refused := f.prefillInputs(pf, ps, st.Cycle)
	if refused != nil {
		pf.refused = refused
		return pf
	}
	pf.fleet.Prefill = p
	pf.fleet.ProcessorSharing = processorSharing
	f.prefillShape(pf, ps)
	if f.set["assume-router-hop"] {
		pf.assumedHop = &f.assumeRouterHop
	}
	pf.promptAssumed = f.set["assume-input-tokens"]
	pf.decodeSide()
	if pf.fleet.Decode != nil && pf.pv.state.Shape.OutputDispersion == 0 {
		pf.assume("%s", outputSpreadUnreadWords)
	}
	pf.flowControl(st)
	f.arrivals(pf)
	if f.prefillVariant != "" && len(st.Prefill) > 1 {
		pf.assume("the whole rate on prefill variant %s (--prefill-variant; the model has %d)", ps.Variant, len(st.Prefill))
	}
	pf.raiseFlags(ps)
	pf.describe()
	return pf
}

// arrivals is the arrival process. --arrival-cv2 reaches the prefill queue
// alone: decode's admission is a birth-death chain, which has no Ca^2, so on a
// decode line Poisson stands there and stays flagged.
func (f *answerFlags) arrivals(pf *pricedFleet) {
	pf.in.ArrivalsSource = shapeAssumed
	if !f.set["arrival-cv2"] {
		pf.assume("Poisson arrivals")
		return
	}
	pf.fleet.Prefill.ArrivalCV2, pf.in.ArrivalCV2 = f.arrivalCV2, f.arrivalCV2
	pf.flags = append(pf.flags, assumedArrivalCV2)
	pf.assume("arrivals Ca^2 %s (--arrival-cv2)", num(f.arrivalCV2))
	if pf.fleet.Decode != nil {
		pf.assume("Poisson arrivals at decode admission (--arrival-cv2 does not reach it)")
		return
	}
	pf.flags = slices.DeleteFunc(pf.flags, func(fl whatif.Flag) bool { return fl == assumedPoisson })
}

// raiseFlags adds S1's flags on the spread and the imbalance. The routing
// flags read f at the recorded count, where the window was read.
func (pf *pricedFleet) raiseFlags(ps *learnreplay.PrefillState) {
	p := pf.fleet.Prefill
	if pf.q > 0 && p.PromptCV > whatif.PromptCVPeak(pf.q) {
		pf.flags = append(pf.flags, promptSpreadAbovePeak)
	}
	if p.Replicas != ps.Pods {
		return
	}
	pods := float64(p.Replicas)
	switch {
	case p.Replicas < 2 || p.Imbalance/pods < whatif.RoutingBoundShare:
	case !p.ImbalanceAssumed, ps.RawShareFull && ps.RawShareLeast/pods >= whatif.RoutingBoundShare:
		pf.flags = append(pf.flags, routingBound)
	default:
		pf.flags = append(pf.flags, imbalanceNotSustained)
	}
}

// assume names one assumed input.
func (pf *pricedFleet) assume(format string, args ...any) {
	pf.in.Assumed = append(pf.in.Assumed, fmt.Sprintf(format, args...))
}

// prefillInputs is V_P, a, f and N_p. V_P is --assume-vp, as every --assume-*
// flag overrides the recording, with P1's figure printed beside it; else P1's,
// read through the replay; else NoPrefillCapacity: it is never defaulted. a
// is --assume-a in the same way, else P1's, else NoFixedLatency: it has no
// default either, and a fleet of one prompt length never gives P1 a fit.
func (f *answerFlags) prefillInputs(pf *pricedFleet, ps *learnreplay.PrefillState, cycle int) (whatif.Prefill, *whatif.Unanswerable) {
	est := ps.Estimate
	learned := ps.EstimateRead && est.VP > 0 && est.VPRefused == ""
	switch {
	case f.set["assume-vp"]:
		if learned {
			vp := est.VP
			pf.in.VPLearned = &vp
		}
		est.VP, est.VPRefused, est.VPLowerBound, est.VPAboveSlopeBound = f.assumeVP, "", false, false
		est.VPPromptTokens = 0
		pf.assumedVP = true
		pf.in.VPSource = shapeAssumed
		pf.assume("V_P %s tok/s (--assume-vp)", num(f.assumeVP))
	case learned:
		pf.in.VPSource, pf.in.VPOrigin = sourceLearned, string(est.VPSource)
	default:
		why := fmt.Sprintf("no P1 estimate of %s at cycle %d", ps.Variant, cycle)
		if ps.EstimateRead {
			why = fmt.Sprintf("P1 refused V_P of %s: %s", ps.Variant, est.VPRefused)
		}
		return whatif.Prefill{}, &whatif.Unanswerable{Reason: whatif.NoPrefillCapacity,
			Detail: why + "; pass --assume-vp for a flagged answer"}
	}
	if f.set["assume-a"] {
		if ps.EstimateRead && est.A > 0 && est.ARefused == "" {
			a := est.A
			pf.in.FixedLatencyLearned = &a
		}
		est.A, est.ARefused = f.assumeA.Seconds(), ""
		pf.assumedA = true
		pf.flags = append(pf.flags, assumedA)
		pf.assume("a %s (--assume-a)", f.assumeA)
	} else if !ps.EstimateRead {
		return whatif.Prefill{}, &whatif.Unanswerable{Reason: whatif.NoFixedLatency,
			Detail: fmt.Sprintf("no P1 estimate of %s at cycle %d; pass --assume-a for a flagged answer", ps.Variant, cycle)}
	}
	p, refused := whatif.PrefillFromEstimate(est)
	if refused != nil {
		if refused.Reason == whatif.NoFixedLatency {
			refused.Detail += "; pass --assume-a for a flagged answer"
		}
		return whatif.Prefill{}, refused
	}
	if pf.assumedVP {
		// The caller's figure, for the prompt asked: neither rescaled nor
		// margined (Prefill.shapedVP).
		p.VPAssumed, p.VPMargin, p.CurveSlope, p.CurveCurvature = true, 0, 0, 0
	}
	pf.in.VP, pf.in.VPPromptTokens = p.VP, p.VPPromptTokens
	pf.in.FixedLatencySeconds, pf.in.FixedLatencySource = p.FixedLatency, sourceLearned
	if pf.assumedA {
		pf.in.FixedLatencySource = shapeAssumed
	}

	p.Replicas = ps.Pods
	pf.in.Replicas, pf.in.ReplicasFrom = ps.Pods, "recording"
	if f.prefill > 0 {
		p.Replicas, pf.in.ReplicasFrom = f.prefill, "--prefill"
		pf.in.Replicas = f.prefill
	}
	p.Imbalance, p.ImbalanceAssumed, pf.in.ImbalanceSource = imbalanceAt(ps, p.Replicas)
	pf.in.Imbalance = max(p.Imbalance, 1)
	if p.ImbalanceAssumed {
		pf.assume("f %s on %d pods (none learned at that count)", num(p.Imbalance), p.Replicas)
	}
	return p, nil
}

// imbalanceAt is f at pods, by S1's decision 1: P1's own at that count, else
// nothing on one pod, which P3 prices as exactly 1, else an assumed
// min(pods, max(1.5, f)), f the learned figure at the recorded count or the
// raw share. A learned f is never carried unchanged to another count, and one
// pod's exact 1 never reaches two.
func imbalanceAt(ps *learnreplay.PrefillState, pods int) (f float64, assumed bool, source string) {
	if v, ok := ps.Estimate.FByPods[pods]; ok && v >= 1 {
		return v, false, sourceLearned
	}
	if pods == 1 {
		return 0, false, "one pod"
	}
	base := ps.RawShare
	if v, ok := ps.Estimate.FByPods[ps.Pods]; ok && v >= 1 {
		base = v
	}
	return min(float64(pods), max(whatif.ImbalanceFloor, base)), true, shapeAssumed
}

// prefillShape is the prompt: --assume-input-tokens, else what the prefill
// pods computed, else the decode variant's effective prompt; and its spread
// and the KV transfer per token. The transfer is read at the measured prompt,
// whatever the prompt asked: it is a figure of the traffic that moved it.
func (f *answerFlags) prefillShape(pf *pricedFleet, ps *learnreplay.PrefillState) {
	p := &pf.fleet.Prefill
	pv := pf.pv
	switch {
	case f.set["assume-input-tokens"]:
		p.PromptTokens = pf.effectivePrompt(f.inputTokens)
		pf.in.PromptSource = shapeAssumed
		pf.assume("prompt %s tokens (--assume-input-tokens)", num(round(p.PromptTokens)))
	case ps.PromptTokens > 0:
		p.PromptTokens, pf.in.PromptSource = ps.PromptTokens, shapeMeasured
		pf.in.PromptFromCycle, pf.in.PromptToCycle = ps.PromptFromCycle, ps.PromptToCycle
	case pv != nil && pv.shape.ILeff > 0:
		p.PromptTokens, pf.in.PromptSource = pv.shape.ILeff, "decode shape"
		pf.assume("prompt %s tokens (the decode variant's; no computed-token rate on the prefill rows)", num(round(p.PromptTokens)))
	}
	pf.in.PromptTokens = p.PromptTokens

	// The window's, whatever the prompt asked: a cv is a figure of the traffic
	// the histogram counted. --assume-prompt-cv overrides it, as every
	// --assume-* flag overrides the recording, with the read figure beside it.
	spread := ps.PromptWindow.Spread()
	p.PromptCV, p.PromptCVAssumed = spread.CV, spread.Assumed
	pf.in.PromptCV, pf.in.PromptCVSource = spread.CV, shapeMeasured
	switch {
	case f.set["assume-prompt-cv"]:
		p.PromptCV, p.PromptCVAssumed, pf.statedCV = f.assumePromptCV, true, true
		pf.in.PromptCV, pf.in.PromptCVSource = f.assumePromptCV, shapeAssumed
		if spread.Assumed {
			pf.assume("prompt cv %s (--assume-prompt-cv)", num(f.assumePromptCV))
			break
		}
		read := spread.CV
		pf.in.PromptCVRead = &read
		pf.assume("prompt cv %s (--assume-prompt-cv, read %s)", num(f.assumePromptCV), num(read))
	case spread.Assumed:
		pf.in.PromptCVSource = shapeAssumed
		pf.assume("prompt cv %s (no computed-prompt histogram)", num(spread.CV))
	case spread.Clamped:
		pf.in.PromptCVSource = shapeClamped
		pf.assume("prompt cv %s (the histogram reads past its last bound, or above it)", num(spread.CV))
	}

	if pv != nil && pv.state.DecodeTTFT.TransferSeconds > 0 && ps.PromptTokens > 0 {
		p.TransferSecondsPerToken = pv.state.DecodeTTFT.TransferSeconds / ps.PromptTokens
	}
}

// effectivePrompt is an assumed prompt less the prefix hits of the decode
// variant's shape; with none, the prompt as given.
func (pf *pricedFleet) effectivePrompt(il float64) float64 {
	if pf.pv == nil {
		return il
	}
	return pf.pv.shape.ILeff
}

// sideOmitted is an input left out of the price.
const sideOmitted = "omitted"

// decodeSide is decode's part of the TTFT: the decode variant's line when
// it prices, else the decode engine's measured TTFT, which holds the KV pull
// (TTFTFleet.DecodeTTFT), else nothing, flagged by P3.
func (pf *pricedFleet) decodeSide() {
	pf.in.TransferSecondsPerToken = pf.fleet.Prefill.TransferSecondsPerToken
	pf.in.TransferSource = shapeMeasured
	if pf.in.TransferSecondsPerToken == 0 {
		pf.in.TransferSource = sideOmitted
	}
	if pf.pv != nil {
		if _, ok := whatif.KneeRate(pf.pv.decode).(whatif.Answer); ok {
			d := pf.pv.decode
			pf.fleet.Decode = &d
			pf.in.DecodeSide = "line"
			// Only a line prices the transfer: with the decode engine's TTFT it is
			// held in that figure.
			if pf.pv.state.DecodeTTFT.TransferHeld {
				pf.flags = append(pf.flags, whatif.TransferHeld)
			}
			pf.decodeTerm()
			return
		}
		pf.fleet.DecodeTTFT = pf.pv.state.DecodeTTFT.EngineTTFT
	}
	if pf.fleet.DecodeTTFT.Seconds > 0 {
		pf.in.TransferSecondsPerToken, pf.in.TransferSource = 0, sideDecodeEngine
		pf.in.DecodeSide = sideDecodeEngine
		pf.in.DecodeTTFTSeconds = pf.fleet.DecodeTTFT.Seconds
		pf.in.DecodeTTFTPromptTokens = pf.fleet.DecodeTTFT.PromptTokens
		return
	}
	pf.in.DecodeSide = sideOmitted
}

// decodeTerm is the term on the decode line: what the recording's cycles
// taught the variant, each input with no fit named as assumed.
func (pf *pricedFleet) decodeTerm() {
	t := pf.pv.state.DecodeTTFT.Term
	if pf.promptAssumed {
		t.WholePromptTokens = 0
	}
	pf.fleet.DecodeTerm = &t
	pf.in.DecodeTermSource = shapeMeasured
	pf.in.DecodeTermRefused, pf.in.RouterHopRefused = string(t.EngineRefused), string(t.HopRefused)
	switch {
	case t.EngineOmitted:
		pf.in.DecodeTermSource = sideOmitted
		pf.assume("decode term omitted (no fit, %s, and no quiet decode row): T_xfer + ITL stands for it", t.EngineRefused)
	case t.StepAssumed:
		pf.in.DecodeTermSource = shapeAssumed
		pf.in.TransferSource = termHoldsTransfer
		pf.assume("decode TTFT %.1f ms held from a quiet row, slope %s on ITL above %.2f ms (no fit: %s)",
			1000*t.Engine.Base, num(t.PerStep), 1000*t.FromITL, t.EngineRefused)
	default:
		pf.in.TransferSource = termHoldsTransfer
	}
	pf.in.DecodeBaseSeconds, pf.in.DecodePerTokenSeconds = t.Engine.Base, t.Engine.PerToken
	pf.in.DecodeHeldPromptTokens, pf.in.DecodeFromITLSeconds = t.Engine.HeldPromptTokens, t.FromITL
	pf.in.DecodePerStep = t.PerStep
	pf.in.RouterHopSource = shapeMeasured
	switch {
	case pf.assumedHop != nil:
		if !t.HopAssumed {
			learned := t.Hop.Base
			pf.in.RouterHopLearnedBaseSeconds = &learned
		}
		t.Hop, t.HopAssumed = whatif.PromptLine{Base: pf.assumedHop.Seconds()}, true
		pf.fleet.DecodeTerm = &t
		pf.in.RouterHopSource = shapeAssumed
		pf.assume("router hop %s (--assume-router-hop)", pf.assumedHop)
	case t.HopAssumed:
		pf.in.RouterHopSource = shapeAssumed
		pf.assume("router hop %.0f ms (no fit: %s); --assume-router-hop supplies one", 1000*t.Hop.Base, t.HopRefused)
	}
	pf.in.RouterHopBaseSeconds, pf.in.RouterHopPerTokenSeconds = t.Hop.Base, t.Hop.PerToken
	pf.in.RouterHopHeldPromptTokens = t.Hop.HeldPromptTokens
}

// termHoldsTransfer is T_xfer's source with a D priced: the term holds the pull.
const termHoldsTransfer = "held by the decode term, not priced"

// assumedMove is the move, either way, past which an assumed term is said to
// decide an answer. Stated: past it the operator should read the term first.
const assumedMove = 1.25

// noteAssumedTerm says on the assumed row when the decode term's assumed parts
// alone decide r: a refusal the fleet would answer, or a figure they move by
// assumedMove or more. price is the question asked of the fleet.
func (pf *pricedFleet) noteAssumedTerm(r whatif.Result, price func(whatif.TTFTFleet) whatif.Result) {
	if pf.refused != nil {
		return
	}
	plain, ok := whatif.WithoutAssumedTerms(pf.fleet)
	if !ok {
		return
	}
	figure := func(r whatif.Result) float64 {
		switch v := r.(type) {
		case whatif.Answer:
			return v.Value
		case whatif.TTFT:
			return v.Seconds
		}
		return 0
	}
	with, without := figure(r), figure(price(plain))
	switch {
	case !(without > 0):
	case !(with > 0):
		pf.assume("an assumed decode term alone decides this refusal: without it the answer is %.4g", without)
	case with >= assumedMove*without || without >= assumedMove*with:
		pf.assume("an assumed decode term moves this answer from %.4g to %.4g", without, with)
	}
}

// flowControl is flow control inferred from its series: S1's rule with
// slo.flowControl unset. The CLI does not read that field, so a tier that
// declares the state is still inferred here. On carries the upstream
// thresholds.
func (pf *pricedFleet) flowControl(st *learnreplay.State) {
	if st.FlowControlSeries {
		pf.fleet.FlowControl = whatif.FlowControl{State: whatif.FlowControlOn,
			QueueThreshold: whatif.FlowControlQueueThreshold, KVThreshold: whatif.FlowControlKVThreshold}
		pf.in.FlowControl = "on"
		pf.assume("flow-control thresholds %s and %s (upstream defaults)", num(whatif.FlowControlQueueThreshold), num(whatif.FlowControlKVThreshold))
		return
	}
	pf.fleet.FlowControl = whatif.FlowControl{State: whatif.FlowControlNoSeries}
	pf.in.FlowControl = "off, inferred"
	pf.assume("flow control off (no queue series)")
}

// describe is the fleet's inputs, in a line.
func (pf *pricedFleet) describe() {
	p := pf.fleet.Prefill
	var b strings.Builder
	fmt.Fprintf(&b, "V_P %s ", num(round(p.VP)))
	switch {
	case pf.assumedVP && pf.in.VPLearned != nil:
		fmt.Fprintf(&b, "ASSUMED, learned %s", num(round(*pf.in.VPLearned)))
	case pf.assumedVP:
		b.WriteString("ASSUMED")
	case p.VPPromptTokens > 0:
		fmt.Fprintf(&b, "learned (%s, at %s tokens)", pf.in.VPOrigin, num(round(p.VPPromptTokens)))
	default:
		fmt.Fprintf(&b, "learned (%s)", pf.in.VPOrigin)
	}
	fmt.Fprintf(&b, "; a %s; f %.4g (%s); N_p %d (%s)", pf.aText(), pf.in.Imbalance, pf.in.ImbalanceSource,
		p.Replicas, pf.in.ReplicasFrom)
	fmt.Fprintf(&b, "; prompt %s tokens (%s", num(round(p.PromptTokens)), pf.in.PromptSource)
	if pf.in.PromptFromCycle > 0 {
		fmt.Fprintf(&b, ", cycles %d-%d", pf.in.PromptFromCycle, pf.in.PromptToCycle)
	}
	b.WriteString(")")
	switch pf.in.DecodeSide {
	case "line":
		fmt.Fprintf(&b, "; decode by its line, term %s, hop %s", pf.in.DecodeTermSource, pf.in.RouterHopSource)
	case sideDecodeEngine:
		fmt.Fprintf(&b, "; decode engine TTFT %.1f ms at %s tokens, measured", 1000*pf.fleet.DecodeTTFT.Seconds, num(round(pf.fleet.DecodeTTFT.PromptTokens)))
	default:
		b.WriteString("; decode side omitted")
	}
	pf.inputsText = b.String()
}

// aText is a as the inputs row prints it: the learned figure beside an
// assumed one, as for V_P.
func (pf *pricedFleet) aText() string {
	a := fmt.Sprintf("%.2f ms", 1000*pf.fleet.Prefill.FixedLatency)
	switch {
	case pf.assumedA && pf.in.FixedLatencyLearned != nil:
		return fmt.Sprintf("%s ASSUMED, learned %.2f ms", a, 1000**pf.in.FixedLatencyLearned)
	case pf.assumedA:
		return a + " ASSUMED"
	}
	return a + " learned"
}

// verdict is the words every TTFT figure prints beside it.
func (pf *pricedFleet) verdict() string {
	v := "UNVALIDATED"
	if pf.assumedVP {
		v += "; V_P ASSUMED"
	}
	if pf.assumedA {
		v += "; a ASSUMED"
	}
	return v
}

// footerNote is the fleets' closing note. The direction turns on the spread:
// the frozen table's assumed row was read at whatif.AssumedPromptCV, so a
// measured cv (the collector's Q1), or one --assume-prompt-cv states at any
// other figure, has only the sent spread's row to go by. Both rows were read at
// Poisson arrivals, so a stated Ca^2 has neither. A refused fleet read no spread
// of its own, but what its flags stated still rules out a row.
func footerNote(fleets []*pricedFleet) string {
	note := ttftNoteOpen + fmt.Sprintf(ttftNoteAssumed, num(whatif.AssumedPromptCV))
	switch {
	case slices.ContainsFunc(fleets, func(pf *pricedFleet) bool { return pf.in.ArrivalCV2 > 0 }):
		note = ttftNoteOpen + ttftNoteArrivals
	case slices.ContainsFunc(fleets, func(pf *pricedFleet) bool {
		if pf.statedCV {
			return pf.in.PromptCV != whatif.AssumedPromptCV
		}
		return pf.refused == nil && !pf.fleet.Prefill.PromptCVAssumed
	}):
		note = ttftNoteOpen + ttftNoteMeasured
	}
	if slices.ContainsFunc(fleets, func(pf *pricedFleet) bool { return pf.assumedVP }) {
		note += " V_P ASSUMED."
	}
	if slices.ContainsFunc(fleets, func(pf *pricedFleet) bool { return pf.assumedA }) {
		note += " a ASSUMED."
	}
	return note
}

// priced is the fleet's Result from price, or the refusal of an input with no
// source.
func (pf *pricedFleet) priced(price func(whatif.TTFTFleet) whatif.Result) whatif.Result {
	if pf.refused != nil {
		return *pf.refused
	}
	return pf.floorFlags(pf.qualify(price(pf.fleet)))
}

// floorFlags adds to a floor refusal what S1's side says of the floor: above the
// peak, a larger cv prices a shorter one.
func (pf *pricedFleet) floorFlags(r whatif.Result) whatif.Result {
	u, ok := r.(whatif.Unanswerable)
	if !ok || u.Reason != whatif.TargetBelowFloor || !slices.Contains(pf.flags, promptSpreadAbovePeak) {
		return r
	}
	u.Flags = append(slices.Clone(u.Flags), promptSpreadAbovePeak)
	return u
}

// qualify adds what the fleet says of an answer: the decode variant's flags
// where its line priced it, an assumed shape in any case, and the fleet's own.
func (pf *pricedFleet) qualify(r whatif.Result) whatif.Result {
	add := slices.Clone(pf.flags)
	if pf.pv != nil {
		if pf.fleet.Decode != nil {
			r = pf.pv.qualify(r)
			add = append(add, pf.pv.flags...)
		}
		if pf.pv.assumed() {
			add = append(add, whatif.AssumedShape)
		}
	}
	return withFlags(r, add)
}

// withFlags is r with each of add it does not carry; a refusal keeps only what
// its term rests on (Unanswerable.Flags).
func withFlags(r whatif.Result, add []whatif.Flag) whatif.Result {
	merged := func(flags []whatif.Flag) []whatif.Flag {
		flags = slices.Clone(flags)
		for _, fl := range add {
			if !slices.Contains(flags, fl) {
				flags = append(flags, fl)
			}
		}
		return flags
	}
	switch v := r.(type) {
	case whatif.Answer:
		v.Flags = merged(v.Flags)
		return v
	case whatif.TTFT:
		v.Flags = merged(v.Flags)
		return v
	}
	return r
}

// knee is the prefill knee, or the refusal of its inputs.
func (pf *pricedFleet) knee() whatif.Result {
	if pf.refused != nil {
		return *pf.refused
	}
	price := func(fl whatif.TTFTFleet) whatif.Result { return whatif.PrefillKnee(fl.Prefill) }
	return withSpan(price(pf.fleet), pf.corners(price))
}

// corners is the question asked of the fleet at each corner of the V_P and f
// sweep (whatif.SensitivityFleets); none on a fleet refused before pricing.
func (pf *pricedFleet) corners(price func(whatif.TTFTFleet) whatif.Result) []whatif.Result {
	if pf.refused != nil {
		return nil
	}
	fleets := whatif.SensitivityFleets(pf.fleet)
	out := make([]whatif.Result, len(fleets))
	for i, fl := range fleets {
		out[i] = price(fl)
	}
	return out
}

// withSpan is r with its span over the corners. A refusal has no figure to
// span, and stays as it is.
func withSpan(r whatif.Result, corners []whatif.Result) whatif.Result {
	switch v := r.(type) {
	case whatif.Answer:
		v.Sensitivity = whatif.SensitivitySpan(v.Value, corners)
		return v
	case whatif.TTFT:
		v.Sensitivity = whatif.SensitivitySpan(v.Seconds, corners)
		return v
	}
	return r
}

// sweptText names what the fleet's bands swept: one pod has one f.
func (pf *pricedFleet) sweptText() string {
	p := pf.fleet.Prefill
	if p.Replicas < 2 || p.Imbalance < 1 {
		return fmt.Sprintf("V_P +-%.0f%%", 100*whatif.SensitivityShare)
	}
	return fmt.Sprintf("V_P and f +-%.0f%%", 100*whatif.SensitivityShare)
}

// spanText prints a span in format, a refused corner in place of its worse
// side: the higher TTFT, or the lower rate.
func spanText(s *whatif.Sensitivity, format string, scale float64, worseIsHigh bool) string {
	if s == nil {
		return ""
	}
	lo, hi := fmt.Sprintf(format, scale*s.Lo), fmt.Sprintf(format, scale*s.Hi)
	switch {
	case s.Refused == "":
	case worseIsHigh:
		hi = string(s.Refused)
	default:
		lo = string(s.Refused)
	}
	return fmt.Sprintf("[%s .. %s]", lo, hi)
}

// operatingTTFT is question 2's TTFT on one fleet.
type operatingTTFT struct {
	fleet *pricedFleet
	q     ttftQuestion
	// knee is the prefill knee, and ttft the answer at rate.
	knee, ttft whatif.Result
	// rateSpan is the span of a rate the prefill knee set, nil on any other:
	// the knee's, at the headroom.
	rateSpan *whatif.Sensitivity
	// prefillBinds says the prefill knee set the operating rate: it is the
	// lower of the two, and no rate was given.
	prefillBinds bool
	rate         float64
}

// atOperatingPoint prices the fleet at --rate, or at the headroom times the
// lower of the roles' knees. It leaves query as it is unless the prefill knee
// binds, and then it asks for that rate: the fleet's operating point is one
// rate, and the TPOT is read at it.
func (pf *pricedFleet) atOperatingPoint(q ttftQuestion, query whatif.ReplicasQuery, given *float64, headroom float64, decodeKnee whatif.Result) (operatingTTFT, whatif.ReplicasQuery) {
	o := operatingTTFT{fleet: pf, q: q, knee: pf.knee()}
	pk, ok := o.knee.(whatif.Answer)
	if !ok && given == nil {
		o.ttft = o.knee
		return o, query
	}
	if given != nil {
		o.rate = *given
		pf.in.RateSource = "--rate"
	} else {
		lowest := pk.Value
		dk, decoded := decodeKnee.(whatif.Answer)
		o.prefillBinds = !decoded || pk.Value < dk.Value
		pf.in.RateSource = sourcePrefillKnee
		if !o.prefillBinds {
			lowest = dk.Value
			pf.in.RateSource = "headroom x decode knee"
		}
		o.rate = headroom * lowest
		if o.prefillBinds {
			query.Rate, query.Headroom = o.rate, 0
			o.rateSpan = &whatif.Sensitivity{Lo: headroom * pk.Sensitivity.Lo, Hi: headroom * pk.Sensitivity.Hi}
			if decoded {
				// The rate is the headroom on the lower knee: past the decode
				// knee a better V_P raises nothing.
				o.rateSpan.Hi = min(o.rateSpan.Hi, headroom*dk.Value)
			}
		}
	}
	pf.in.Rate = &o.rate
	price := func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, o.rate, q.q) }
	o.ttft = withSpan(pf.priced(price), pf.corners(price))
	pf.noteAssumedTerm(o.ttft, price)
	return o, query
}

// results is the Results the exit code reads.
func (o operatingTTFT) results() []whatif.Result { return []whatif.Result{o.knee, o.ttft} }

// onDecode is a decode figure read at the operating rate. A rate the prefill
// knee set rests on V_P, so a figure read at it carries the flags the knee
// does: it is no more validated than that rate.
func (o operatingTTFT) onDecode(r whatif.Result) whatif.Result {
	if k, ok := o.knee.(whatif.Answer); ok && o.prefillBinds {
		return withFlags(r, k.Flags)
	}
	return r
}

// onRate is the operating rate with its span, where the prefill knee set it.
func (o operatingTTFT) onRate(r whatif.Result) whatif.Result {
	if a, ok := r.(whatif.Answer); ok && o.rateSpan != nil {
		a.Sensitivity = o.rateSpan
		return a
	}
	return r
}

// decodeNote is what a decode row read at the operating rate adds to its
// basis: nothing unless the prefill knee set that rate.
func (o *operatingTTFT) decodeNote() string {
	if o == nil || !o.prefillBinds {
		return ""
	}
	return "; at the prefill knee's rate; " + o.fleet.verdict()
}

// fleetRate is question 1's fleet row: the smaller of the roles' rates and
// the operating point of the lower knee. decode is nil when no TPOT was
// asked. A role's refusal is the fleet's.
func fleetRate(q ttftQuestion, headroom float64, decode, ttft whatif.Result) whatif.Result {
	if u, ok := decode.(whatif.Unanswerable); ok {
		return u
	}
	t, ok := ttft.(whatif.Answer)
	if !ok {
		return ttft
	}
	type bound struct {
		rate float64
		by   whatif.Bound
	}
	bounds := []bound{{headroom * t.Basis.TTFT.KneeRate, whatif.BoundOperatingPoint},
		{t.Value, whatif.Bound(q.label() + ", end to end")}}
	flags := slices.Clone(t.Flags)
	if d, ok := decode.(whatif.Answer); ok {
		bounds = append(bounds, bound{d.Value, whatif.Bound("decode " + boundText(d.Basis.BoundBy))})
		for _, fl := range d.Flags {
			if !slices.Contains(flags, fl) {
				flags = append(flags, fl)
			}
		}
	}
	lowest := bounds[0].rate
	for _, b := range bounds[1:] {
		lowest = min(lowest, b.rate)
	}
	a := whatif.Answer{Value: lowest, Flags: flags}
	for _, b := range bounds {
		if b.rate == lowest {
			a.Basis.BoundBy = append(a.Basis.BoundBy, b.by)
		}
	}
	return a
}

// msText is a duration in seconds, in milliseconds.
func msText(s float64) string { return fmt.Sprintf("%.1f ms", 1000*s) }

// ttftBoundText names what set a TTFT rate, the target by its percentile.
func ttftBoundText(q ttftQuestion, by []whatif.Bound) string {
	parts := make([]string, len(by))
	for i, b := range by {
		parts[i] = string(b)
		if b == whatif.BoundTTFT {
			parts[i] = q.label()
		}
	}
	return strings.Join(parts, ", ")
}

// writeInputs prints the fleet's inputs and every assumed one, under an
// indent.
func (pf *pricedFleet) writeInputs(b *strings.Builder, inputsLabel, assumedLabel string) {
	if pf.inputsText != "" {
		fmt.Fprintf(b, "%s%s\n", inputsLabel, pf.inputsText)
	}
	if len(pf.in.Assumed) > 0 {
		fmt.Fprintf(b, "%s%s\n", assumedLabel, strings.Join(pf.in.Assumed, "; "))
	}
}

// kneeBasis spells the prefill knee: N_p x V_P x (1 - margin) / (f x prompt),
// V_P at the prompt asked, read back from the knee, so it is the V_P priced
// (rescaled along P1's curve, where it was) and not the one learned.
func (pf *pricedFleet) kneeBasis(knee float64) string {
	p := pf.fleet.Prefill
	vp := knee * pf.in.Imbalance * p.PromptTokens / float64(p.Replicas) / (1 - p.VPMargin)
	margin := ""
	if p.VPMargin > 0 {
		margin = fmt.Sprintf(" x (1 - %s margin)", num(p.VPMargin))
	}
	return fmt.Sprintf("N_p %d x V_P %s%s / (f %.4g x %s tokens); %s", p.Replicas, num(round(vp)), margin,
		pf.in.Imbalance, num(round(p.PromptTokens)), pf.verdict())
}

// writeRows prints question 2's TTFT rows.
func (o operatingTTFT) writeRows(b *strings.Builder) {
	pf := o.fleet
	if k, ok := o.knee.(whatif.Answer); ok {
		row(b, "prefill knee", rateText(k.Value), spanText(k.Sensitivity, "%.2f", 1, false), pf.kneeBasis(k.Value))
	}
	switch v := o.ttft.(type) {
	case whatif.TTFT:
		row(b, o.q.label(), msText(v.Seconds), spanText(v.Sensitivity, "%.1f", 1000, true), "end to end; "+pf.verdict())
		fmt.Fprintf(b, "%-16s(bands: %s, a sweep, not a confidence interval)\n", "", pf.sweptText())
		if msText(v.Independent) != msText(v.Seconds) {
			fmt.Fprintf(b, "%-16s(%s if prefill and decode waits were independent)\n", "", msText(v.Independent))
		}
		fmt.Fprintf(b, "%-16s(floor %s, pooled %s, worst pod utilization %.3f)\n", "", msText(v.Floor), msText(v.Pooled), v.PrefillUtilization)
	case whatif.Unanswerable:
		fmt.Fprintf(b, "%-16s%s\n", o.q.label(), refusedText(v))
	}
	pf.writeInputs(b, fmt.Sprintf("%-16s", "ttft inputs"), fmt.Sprintf("%-16s", "assumed"))
}

// writeRateRows prints question 1's prefill and fleet rows, for layout's
// columns.
func writeRateRows(b *strings.Builder, layout string, pf *pricedFleet, fleets int, q ttftQuestion, ttft, fleet whatif.Result) {
	role := pf.label(rolePrefill, fleets)
	switch r := ttft.(type) {
	case whatif.Answer:
		fmt.Fprintf(b, layout, role, rateText(r.Value), ttftBoundText(q, r.Basis.BoundBy), pf.sweptBasis(r))
	case whatif.Unanswerable:
		fmt.Fprintf(b, "%-9s%s\n", role, refusedText(r))
	}
	pf.writeInputs(b, fmt.Sprintf("%-9sinputs ", ""), fmt.Sprintf("%-9sassumed ", ""))
	role = pf.label("fleet", fleets)
	switch r := fleet.(type) {
	case whatif.Answer:
		fmt.Fprintf(b, layout, role, rateText(r.Value), boundText(r.Basis.BoundBy), pf.sweptBasis(r))
	case whatif.Unanswerable:
		fmt.Fprintf(b, "%-9s%s\n", role, refusedText(r))
	}
}

// sweptBasis is a rate's basis on question 1: the verdict and the rate's span.
func (pf *pricedFleet) sweptBasis(a whatif.Answer) string {
	return fmt.Sprintf("%s %s at %s", pf.verdict(), spanText(a.Sensitivity, "%.2f", 1, false), pf.sweptText())
}
