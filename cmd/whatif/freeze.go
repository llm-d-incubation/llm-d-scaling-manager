package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// freeze writes the prediction a TTFT staircase is scored against
// (docs/plans/analyzers/ttft-staircase-repeat.md): a warm-up recording and a
// spec in, one JSON file out, the same bytes for the same inputs. It reads the
// fleet as slo-to-rate does (ttft.go), with the spec's shape and replica counts
// in place of the recorded ones, so a staircase scores what the CLI answers. It
// departs from the CLI in three places, each recorded in the file: the prompt's
// spread is the spec's, the arrivals and the flow-control state are the spec's,
// and the prefix hit rate is 0, since the driver's prompts share no prefix.
// The file holds no clock, path, namespace or pod name.

// frozenVersion is the frozen file's format.
const frozenVersion = 1

// freezeSpec is the part of a staircase spec the prediction reads. The rest of
// the file is the driver's (hack/benchmark/staircase_freeze.py).
type freezeSpec struct {
	// OperatingFraction is the operating rate, as a share of the bucket's lower
	// knee.
	OperatingFraction float64 `json:"operatingFraction"`
	// Percentiles are the target percentiles, 90 for p90.
	Percentiles []float64 `json:"percentiles"`
	// TargetFloorMargin is the least a target may sit above the model's floor,
	// as a multiple of it: a target on the floor measures nothing.
	TargetFloorMargin float64 `json:"targetFloorMargin"`
	// PromptCV is the spread the driver sends.
	PromptCV float64 `json:"promptCV"`
	// Steps are the default ladder, as shares of the lower knee.
	Steps   []float64      `json:"steps"`
	Buckets []freezeBucket `json:"buckets"`
}

// freezeBucket is one bucket of a spec.
type freezeBucket struct {
	Name string `json:"name"`
	// Prompt and Output are the sent mean lengths, in tokens.
	Prompt float64 `json:"prompt"`
	Output float64 `json:"output"`
	// Ca2 is the arrivals' squared coefficient of variation; 1 is Poisson.
	Ca2 float64 `json:"ca2"`
	// Prefill and Decode are N_p and N_d.
	Prefill int `json:"prefill"`
	Decode  int `json:"decode"`
	// Steps overrides the spec's ladder. Rates, when set, is an absolute ladder
	// in requests per second.
	Steps []float64 `json:"steps,omitempty"`
	Rates []float64 `json:"rates,omitempty"`
}

// frozenFile is what freeze writes.
type frozenFile struct {
	Version int `json:"version"`
	// RecordingSHA256 and SpecSHA256 name the inputs by content.
	RecordingSHA256 string `json:"recordingSha256"`
	SpecSHA256      string `json:"specSha256"`
	// Cycle and Cycles are the cycle priced at and how many the recording holds.
	Cycle  int `json:"cycle"`
	Cycles int `json:"cycles"`
	// Gate is what S2's gate reads, at that cycle.
	Gate frozenGate `json:"gate"`
	// TPOTRatio is R_q by percentile, with its flags; absent when refused.
	TPOTRatio []frozenRatio  `json:"tpotRatio"`
	Buckets   []frozenBucket `json:"buckets"`
}

// frozenGate is S2's gate at the frozen cycle.
type frozenGate struct {
	VP         bool `json:"vp"`
	A          bool `json:"a"`
	DecodeLine bool `json:"decodeLine"`
	SLOListed  bool `json:"sloListed"`
}

// frozenRatio is R_q at one percentile.
type frozenRatio struct {
	Percentile float64       `json:"percentile"`
	Value      float64       `json:"value,omitempty"`
	Flags      []whatif.Flag `json:"flags,omitempty"`
	Refused    string        `json:"refused,omitempty"`
}

// frozenBucket is one bucket's prediction.
type frozenBucket struct {
	freezeBucket
	PrefillKnee   float64 `json:"prefillKnee"`
	DecodeKnee    float64 `json:"decodeKnee"`
	Knee          float64 `json:"knee"`
	OperatingRate float64 `json:"operatingRate"`
	// MeanTPOT is decode's mean TPOT at the operating rate.
	MeanTPOT float64 `json:"meanTPOT"`
	// Onsets are the flow-control onsets, in requests per second.
	Onsets whatif.Onsets `json:"onsets"`
	// VP, A and Imbalance are the inputs priced; Flags and Assumed the fleet's own.
	VP        float64 `json:"vp"`
	A         float64 `json:"a"`
	Imbalance float64 `json:"imbalance"`
	// TransferSecondsPerToken is the transfer priced, as slo-to-rate reads it.
	// AllRowsTransferSeconds is the same read from every row, queued or not, kept
	// beside it: the figure the rule moved away from.
	TransferSecondsPerToken float64 `json:"transferSecondsPerToken"`
	AllRowsTransferSeconds  float64 `json:"allRowsTransferSeconds"`
	// RecordedPrefixHitRate is the warm-up's hit rate, which is not priced: see
	// runFreeze.
	RecordedPrefixHitRate float64        `json:"recordedPrefixHitRate"`
	Flags                 []whatif.Flag  `json:"flags"`
	Assumed               []string       `json:"assumed"`
	Targets               []frozenTarget `json:"targets"`
	Curve                 []frozenPoint  `json:"curve"`
	// Refused is why the bucket has no prediction; "" with one.
	Refused string `json:"refused,omitempty"`
}

// frozenTarget is one percentile's target and the rate predicted at it.
type frozenTarget struct {
	Percentile float64 `json:"percentile"`
	Seconds    float64 `json:"seconds"`
	Floor      float64 `json:"floor"`
	// Source is "operating rate", "floor margin" when the margin raised it, or
	// "frozen file" when it came from --targets.
	Source       string  `json:"source"`
	PredictedOff float64 `json:"predictedOff"`
	PredictedOn  float64 `json:"predictedOn"`
	// Refused is why a predicted rate is missing; "" with both.
	Refused string `json:"refused,omitempty"`
}

// frozenPoint is the model at one ladder rate, flow control off.
type frozenPoint struct {
	Rate float64 `json:"rate"`
	// Mean, P90 and P95 are in seconds; absent where Refused says why.
	Mean    float64          `json:"mean,omitempty"`
	P90     float64          `json:"p90,omitempty"`
	P95     float64          `json:"p95,omitempty"`
	Parts   whatif.TTFTParts `json:"parts"`
	Refused string           `json:"refused,omitempty"`
}

// freezeFlags is the freeze command's flags.
type freezeFlags struct {
	recording, spec, out, targets string
	ca2                           ca2Overrides
	assumeVP                      float64
}

// ca2Overrides is --ca2, bucket to measured Ca^2.
type ca2Overrides map[string]float64

// String prints the overrides in a stable order.
func (c ca2Overrides) String() string {
	parts := make([]string, 0, len(c))
	for k, v := range c {
		parts = append(parts, k+"="+num(v))
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// Set reads one NAME=VALUE.
func (c ca2Overrides) Set(s string) error {
	name, value, ok := strings.Cut(s, "=")
	v, err := strconv.ParseFloat(value, 64)
	if !ok || name == "" || err != nil || !finite(v) || v < 0 {
		return fmt.Errorf("want BUCKET=CA2, got %q", s)
	}
	c[name] = v
	return nil
}

// runFreeze writes the frozen file, and exits 3 when any bucket refuses, so that
// a run cannot start on a partial prediction.
func runFreeze(ctx context.Context, args []string, out, warn io.Writer) error {
	fs := newFlags("freeze", out)
	f := freezeFlags{ca2: ca2Overrides{}}
	fs.StringVar(&f.recording, "recording", "", "the warm-up's learner recording (.jsonl or .jsonl.gz) (required)")
	fs.StringVar(&f.spec, "spec", "", "the staircase spec, YAML (required)")
	fs.StringVar(&f.out, "o", "", "write here, refusing a file that exists; empty writes to stdout")
	fs.StringVar(&f.targets, "targets", "", "take the targets from this frozen file: a rerun at a measured Ca2 holds them")
	fs.Var(f.ca2, "ca2", "price BUCKET at this measured Ca2, BUCKET=CA2 (repeatable)")
	fs.Float64Var(&f.assumeVP, "assume-vp", 0, "price this V_P, flagged, in place of the recording's")
	if ok, err := parse(fs, args); !ok {
		return err
	}
	if f.recording == "" || f.spec == "" {
		return usagef("--recording and --spec are required")
	}
	spec, specSum, err := readFreezeSpec(f.spec)
	if err != nil {
		return err
	}
	held, err := f.heldTargets()
	if err != nil {
		return err
	}
	a := answerFlags{question: "freeze", recording: f.recording, cycle: "last", warn: warn}
	st, err := a.readState(ctx)
	if err != nil {
		return err
	}
	_, raw, err := a.recordingData(ctx)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	// The driver's prompts share no prefix: a warm-up's hit rate is not theirs.
	// The CLI would price it; the file says what was dropped.
	hitRates := map[string]float64{}
	for i := range st.Variants {
		hitRates[st.Variants[i].Variant] = st.Variants[i].Shape.PrefixHitRate
		st.Variants[i].Shape.PrefixHitRate = 0
	}
	file := frozenFile{Version: frozenVersion, RecordingSHA256: hex.EncodeToString(sum[:]), SpecSHA256: specSum,
		Cycle: st.Cycle, Cycles: st.Cycles, TPOTRatio: []frozenRatio{}}
	for _, p := range spec.Percentiles {
		file.TPOTRatio = append(file.TPOTRatio, frozenRatioOf(st, p/100))
	}
	refused := false
	for _, b := range spec.Buckets {
		fb, err := f.priceBucket(st, spec, b, held)
		if err != nil {
			return err
		}
		for _, rate := range hitRates {
			fb.RecordedPrefixHitRate = max(fb.RecordedPrefixHitRate, rate)
		}
		refused = refused || fb.Refused != ""
		file.Buckets = append(file.Buckets, fb)
	}
	file.Gate = gateOf(st)
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("frozen file: %w", err)
	}
	if err := writeFrozen(f.out, append(body, '\n'), out); err != nil {
		return err
	}
	if refused {
		return exitStatus(whatif.ExitRefused)
	}
	return nil
}

// readFreezeSpec reads and checks a spec, and hashes its bytes.
func readFreezeSpec(path string) (*freezeSpec, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", usagef("--spec: %v", err)
	}
	var s freezeSpec
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, "", usagef("--spec %s: %v", path, err)
	}
	if err := s.check(); err != nil {
		return nil, "", usagef("--spec %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return &s, hex.EncodeToString(sum[:]), nil
}

// check refuses a spec that could only price nonsense. Each bound is written so
// that a NaN fails it.
func (s *freezeSpec) check() error {
	switch {
	case !(s.OperatingFraction > 0 && s.OperatingFraction <= 1):
		return fmt.Errorf("operatingFraction %g, want a share in (0, 1]", s.OperatingFraction)
	case !(s.TargetFloorMargin >= 1 && finite(s.TargetFloorMargin)):
		return fmt.Errorf("targetFloorMargin %g, want at least 1", s.TargetFloorMargin)
	case !(s.PromptCV >= 0 && s.PromptCV <= 10):
		return fmt.Errorf("promptCV %g, want 0 to 10", s.PromptCV)
	case len(s.Percentiles) == 0:
		return errors.New("no percentiles")
	case len(s.Buckets) == 0:
		return errors.New("no buckets")
	}
	for _, p := range s.Percentiles {
		if !(p > 0 && p < 100) {
			return fmt.Errorf("percentile %g, want (0, 100)", p)
		}
	}
	seen := map[string]bool{}
	for _, b := range s.Buckets {
		switch {
		case b.Name == "" || seen[b.Name]:
			return fmt.Errorf("bucket name %q is empty or repeated", b.Name)
		case !(b.Prompt > 0 && finite(b.Prompt)) || !(b.Output > 0 && finite(b.Output)):
			return fmt.Errorf("bucket %s: prompt %g, output %g, want both above 0", b.Name, b.Prompt, b.Output)
		case !(b.Ca2 >= 0 && finite(b.Ca2)):
			return fmt.Errorf("bucket %s: ca2 %g, want 0 or more", b.Name, b.Ca2)
		case b.Prefill < 1 || b.Decode < 1:
			return fmt.Errorf("bucket %s: prefill %d, decode %d, want at least 1 of each", b.Name, b.Prefill, b.Decode)
		}
		seen[b.Name] = true
	}
	return nil
}

// heldTargets is --targets, by bucket and percentile; nil without it.
func (f *freezeFlags) heldTargets() (map[string]map[float64]float64, error) {
	if f.targets == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(f.targets)
	if err != nil {
		return nil, usagef("--targets: %v", err)
	}
	var file frozenFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, usagef("--targets %s: %v", f.targets, err)
	}
	held := map[string]map[float64]float64{}
	for _, b := range file.Buckets {
		held[b.Name] = map[float64]float64{}
		for _, t := range b.Targets {
			held[b.Name][t.Percentile] = t.Seconds
		}
	}
	return held, nil
}

// frozenRatioOf is R_q as the warm-up learned it.
func frozenRatioOf(st *learnreplay.State, q float64) frozenRatio {
	r, flags, u := whatif.TPOTRatioFromWindows(st.TPOTWindows, q)
	if u != nil {
		return frozenRatio{Percentile: 100 * q, Refused: refusalText(*u)}
	}
	return frozenRatio{Percentile: 100 * q, Value: r.Value, Flags: flags}
}

// gateOf is S2's gate at the state's cycle: V_P and a from P1, a decode line that
// prices, and slo listed.
func gateOf(st *learnreplay.State) frozenGate {
	g := frozenGate{SLOListed: st.Policy != nil && st.Policy.SLOAnalyzerListed()}
	for _, ps := range st.Prefill {
		g.VP = g.VP || ps.EstimateRead && ps.Estimate.VP > 0 && ps.Estimate.VPRefused == ""
		g.A = g.A || ps.EstimateRead && ps.Estimate.ARefused == ""
	}
	f := answerFlags{question: "freeze", cycle: "last"}
	if variants, err := f.decodeVariants(st); err == nil {
		g.DecodeLine = slices.ContainsFunc(variants, func(pv pricedVariant) bool {
			_, ok := whatif.KneeRate(pv.decode).(whatif.Answer)
			return ok && domain.CanonicalRole(pv.state.Role) == domain.RoleDecode
		})
	}
	return g
}

// priceBucket prices one bucket. A refusal is the bucket's, in the file; an error
// is a question this command cannot ask.
func (f *freezeFlags) priceBucket(st *learnreplay.State, spec *freezeSpec, b freezeBucket,
	held map[string]map[float64]float64) (frozenBucket, error) {
	out := frozenBucket{freezeBucket: b, Flags: []whatif.Flag{}, Assumed: []string{}, Targets: []frozenTarget{}, Curve: []frozenPoint{}}
	if v, ok := f.ca2[b.Name]; ok {
		out.Ca2 = v
	}
	a := answerFlags{question: "freeze", cycle: "last", decode: b.Decode, prefill: b.Prefill,
		inputTokens: b.Prompt, outputTokens: b.Output, assumeVP: f.assumeVP, seed: 1,
		set: map[string]bool{"assume-input-tokens": true, "assume-output-tokens": true}}
	if f.assumeVP > 0 {
		a.set["assume-vp"] = true
	}
	variants, err := a.decodeVariants(st)
	if err != nil {
		return out, err
	}
	if len(variants) != 1 {
		return out, usagef("the recording holds %d decode variants, want 1: a staircase prices one fleet", len(variants))
	}
	ps, err := a.prefillState(st)
	if err != nil {
		return out, err
	}
	pf := a.fleetFor(st, ps, ttftQuestion{q: 0.9}, &variants[0])
	out.Flags, out.Assumed = settledFlags(pf.flags, out.Ca2), settledAssumed(pf.in.Assumed)
	if pf.refused != nil {
		out.Refused = refusalText(*pf.refused)
		return out, nil
	}
	off := pf.fleet
	off.Prefill.PromptCV, off.Prefill.PromptCVAssumed = spec.PromptCV, false
	out.TransferSecondsPerToken = off.Prefill.TransferSecondsPerToken
	out.AllRowsTransferSeconds = variants[0].state.DecodeTTFT.AllRowsTransferSeconds
	// ArrivalCV2 reads 0 as Poisson, and a Poisson bucket flags no burst.
	off.Prefill.ArrivalCV2 = 0
	if out.Ca2 != 1 {
		off.Prefill.ArrivalCV2 = out.Ca2
	}
	off.FlowControl = whatif.FlowControl{State: whatif.FlowControlOff}
	on := off
	on.FlowControl = whatif.FlowControl{State: whatif.FlowControlOn,
		QueueThreshold: whatif.FlowControlQueueThreshold, KVThreshold: whatif.FlowControlKVThreshold}
	out.VP, out.A, out.Imbalance = off.Prefill.VP, off.Prefill.FixedLatency, off.Prefill.Imbalance

	base, ok := whatif.TTFTAt(off, 1, 0.9).(whatif.TTFT)
	if !ok {
		out.Refused = refusalOf(whatif.TTFTAt(off, 1, 0.9))
		return out, nil
	}
	out.PrefillKnee, out.DecodeKnee = base.PrefillKnee, base.DecodeKnee
	out.Knee = base.PrefillKnee
	if base.DecodeKnee > 0 {
		out.Knee = min(base.PrefillKnee, base.DecodeKnee)
	}
	out.OperatingRate = spec.OperatingFraction * out.Knee
	if on, ok := whatif.FlowControlOnsets(on).(whatif.Onsets); ok {
		out.Onsets = on
	}
	if off.Decode != nil {
		if m, ok := whatif.SLOAtReplicas(*off.Decode, whatif.ReplicasQuery{Rate: out.OperatingRate}).MeanTPOT.(whatif.Answer); ok {
			out.MeanTPOT = m.Value
		}
	}
	for _, p := range spec.Percentiles {
		t, why := pricedTarget(off, on, out.OperatingRate, p, spec.TargetFloorMargin, held[b.Name])
		if why != "" {
			out.Refused = why
			return out, nil
		}
		out.Targets = append(out.Targets, t)
		if t.Refused != "" {
			out.Refused = t.Refused
		}
	}
	for _, rate := range ladderRates(spec, b, out.Knee) {
		out.Curve = append(out.Curve, modelPoint(off, rate))
	}
	return out, nil
}

// settledAssumed is the fleet's assumptions, less those the spec settles: what the
// CLI assumes of the traffic and the gate, a staircase sends and sets.
func settledAssumed(assumed []string) []string {
	return slices.DeleteFunc(slices.Clone(assumed), func(s string) bool {
		return strings.HasPrefix(s, "prompt cv") || strings.HasPrefix(s, "flow control") ||
			strings.HasPrefix(s, "flow-control") || s == "Poisson arrivals"
	})
}

// settledFlags is the fleet's flags, less Poisson on a bucket sent at another Ca^2.
func settledFlags(flags []whatif.Flag, ca2 float64) []whatif.Flag {
	if ca2 == 1 {
		return flags
	}
	return slices.DeleteFunc(slices.Clone(flags), func(f whatif.Flag) bool { return f == assumedPoisson })
}

// ladderRates is the bucket's ladder in requests per second.
func ladderRates(spec *freezeSpec, b freezeBucket, knee float64) []float64 {
	if len(b.Rates) > 0 {
		return b.Rates
	}
	steps := b.Steps
	if len(steps) == 0 {
		steps = spec.Steps
	}
	rates := make([]float64, len(steps))
	for i, s := range steps {
		rates[i] = s * knee
	}
	return rates
}

// pricedTarget is one percentile's target and the rates predicted at it. The
// target is the TTFT priced at the operating rate, raised to the margin over the
// floor when it sits under that; held, when set, replaces it.
func pricedTarget(off, on whatif.TTFTFleet, rate, pct, margin float64, held map[float64]float64) (frozenTarget, string) {
	q := pct / 100
	r := whatif.TTFTAt(off, rate, q)
	priced, ok := r.(whatif.TTFT)
	if !ok {
		return frozenTarget{}, refusalOf(r)
	}
	t := frozenTarget{Percentile: pct, Seconds: priced.Seconds, Floor: priced.Floor, Source: "operating rate"}
	if t.Seconds < margin*t.Floor {
		t.Seconds, t.Source = margin*t.Floor, "floor margin"
	}
	if v, ok := held[pct]; ok {
		t.Seconds, t.Source = v, "frozen file"
	}
	target := whatif.TTFTTarget{Seconds: t.Seconds, Percentile: q}
	rateAt := func(fleet whatif.TTFTFleet) float64 {
		res := whatif.RateForTTFT(fleet, target)
		if a, ok := res.(whatif.Answer); ok {
			return a.Value
		}
		t.Refused = refusalOf(res)
		return 0
	}
	t.PredictedOff, t.PredictedOn = rateAt(off), rateAt(on)
	return t, ""
}

// modelPoint is the model at one rate: a rate past a knee is a refusal, not a
// failed freeze, since a ladder climbs past the knee on purpose.
func modelPoint(fl whatif.TTFTFleet, rate float64) frozenPoint {
	p := frozenPoint{Rate: rate}
	mean, ok := whatif.TTFTAt(fl, rate, 0).(whatif.TTFT)
	if !ok {
		p.Refused = refusalOf(whatif.TTFTAt(fl, rate, 0))
		return p
	}
	p.Mean, p.Parts = mean.Seconds, mean.Parts
	tail := func(q float64) float64 {
		if r, ok := whatif.TTFTAt(fl, rate, q).(whatif.TTFT); ok {
			return r.Seconds
		}
		return 0
	}
	p.P90, p.P95 = tail(0.9), tail(0.95)
	return p
}

// refusalOf is a result that is not an answer, as text.
func refusalOf(r whatif.Result) string {
	if u, ok := r.(whatif.Unanswerable); ok {
		return refusalText(u)
	}
	return fmt.Sprintf("%T", r)
}

// refusalText is a refusal's reason and detail.
func refusalText(u whatif.Unanswerable) string { return fmt.Sprintf("%s: %s", u.Reason, u.Detail) }

// writeFrozen writes the file. A frozen file is never replaced: a run scores what
// was written before its first step.
func writeFrozen(path string, body []byte, out io.Writer) error {
	if path == "" {
		_, err := out.Write(body)
		return err
	}
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return usagef("-o: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}
