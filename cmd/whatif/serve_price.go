package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The capacity planner's API (whatifapi) prices on the CLI's own code paths:
// a scenario's fleet is the fleet slo-to-rate and replicas-to-slo would read
// with the flags its knobs stand for, and each figure is the one those
// commands print. This file is that wiring; it adds no arithmetic.

// defaultWindow is a Prometheus source's window when the request names none:
// the last hour, ending now.
const defaultWindow = time.Hour

// servedSources resolves inputs from the server's sources: its Prometheus,
// read with capture's flags, and its recordings directory.
type servedSources struct {
	window     captureFlags
	recordings string
	warn       io.Writer
	// now is the clock a default window ends at; time.Now when nil.
	now func() time.Time
	// allowed is the namespaces a prometheus source may read, and
	// namespaceAllowed, with --auth=kubernetes, whether the request's user may
	// read one (whatifapi.Authenticator.NamespaceAllowed); nil with no auth.
	allowed          []string
	namespaceAllowed func(ctx context.Context, namespace string) error
}

// apiError is a CLI error as the API's: a usage or input error is the
// caller's (400), anything else the server's.
func apiError(err error) error {
	var u usageError
	if errors.As(err, &u) {
		return whatifapi.InputError{Err: err}
	}
	return err
}

// Resolve reads the source, replays it to the cycle, and reads the fleet at
// the recorded counts as the CLI would, the TTFT at the policy's statistic.
func (s *servedSources) Resolve(ctx context.Context, req whatifapi.InputsRequest) (*whatifapi.Resolved, error) {
	f := answerFlags{question: "serve", cycle: "last", model: req.Model, variant: req.DecodeVariant, prefillVariant: req.PrefillVariant,
		seed: whatif.DefaultResampling().Seed, set: map[string]bool{}, warn: s.warn, priceRouterWait: whatif.DefaultPriceRouterWait}
	src := whatifapi.SourceRequest{Kind: whatifapi.SourcePrometheus}
	if req.Source != nil {
		src = *req.Source
	}
	info := whatifapi.SourceInfo{Kind: src.Kind}
	var hide string
	switch src.Kind {
	case whatifapi.SourceRecording:
		path, err := s.recordingPath(src)
		if err != nil {
			return nil, err
		}
		f.recording, hide, info.Name = path, path, src.Name
		if src.Cycle > 0 {
			f.cycle = strconv.Itoa(src.Cycle)
		}
	case whatifapi.SourcePrometheus:
		if err := s.windowOf(ctx, &f, src); err != nil {
			return nil, err
		}
	default:
		return nil, whatifapi.InputErrorf("source.kind %q, want %s or %s", src.Kind, whatifapi.SourcePrometheus, whatifapi.SourceRecording)
	}
	name, data, err := f.recordingData(ctx)
	if err != nil {
		return nil, hidePath(apiError(err), hide, info.Name)
	}
	st, err := f.stateOf(ctx, name, data)
	if err != nil {
		return nil, hidePath(apiError(err), hide, info.Name)
	}
	if o := f.captured; o != nil {
		info.Prometheus = redactedURL(f.window.prometheus)
		info.From, info.To = &o.from, &o.to
	}
	info.Cycle, info.TotalCycles, info.FirstAt, info.LastAt = st.Cycle, st.Cycles, st.FirstAt, st.At
	return s.resolved(&f, st, data, info)
}

// recordingPath is the recording a source names: a file name in the
// recordings directory, never a path.
func (s *servedSources) recordingPath(src whatifapi.SourceRequest) (string, error) {
	switch {
	case s.recordings == "":
		return "", whatifapi.InputErrorf("source.kind recording: the server reads no recordings (--recordings)")
	case src.Namespace != "" || src.From != "" || src.To != "":
		return "", whatifapi.InputErrorf("source.namespace, from and to are a prometheus source's")
	case src.Cycle < 0:
		return "", whatifapi.InputErrorf("source.cycle %d, want a cycle from 1, or 0 for the last", src.Cycle)
	case src.Name == "" || src.Name != filepath.Base(src.Name) || strings.ContainsAny(src.Name, `/\:`) || strings.HasPrefix(src.Name, "."):
		return "", whatifapi.InputErrorf("source.name %q: want a file name in the server's recordings directory, not a path", src.Name)
	}
	path := filepath.Join(s.recordings, src.Name)
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return "", whatifapi.InputErrorf("source.name %q: no such recording in the server's recordings directory", src.Name)
	}
	return path, nil
}

// checkNamespace refuses a namespace a request may not read through the
// server's Prometheus: one that is no DNS-1123 label, one not among the
// allowed, or, with Kubernetes authentication, one whose Pods the user may
// not get.
func (s *servedSources) checkNamespace(ctx context.Context, ns string) error {
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return whatifapi.InputErrorf("source.namespace %q: %s", ns, strings.Join(errs, "; "))
	}
	if !slices.Contains(s.allowed, ns) {
		return whatifapi.ForbiddenError{Err: fmt.Errorf("namespace %s: this server reads %s only (--allowed-namespaces)", ns, strings.Join(s.allowed, ", "))}
	}
	if s.namespaceAllowed != nil {
		return s.namespaceAllowed(ctx, ns)
	}
	return nil
}

// windowOf sets f to read a Prometheus window: the server's Prometheus and
// role map, the request's namespace (else the server's), checked
// (checkNamespace), and window (else the last hour, ending now).
func (s *servedSources) windowOf(ctx context.Context, f *answerFlags, src whatifapi.SourceRequest) error {
	switch {
	case s.window.prometheus == "":
		return whatifapi.InputErrorf("source.kind prometheus: the server reads no Prometheus (--prometheus)")
	case src.Name != "" || src.Cycle != 0:
		return whatifapi.InputErrorf("source.name and cycle are a recording source's")
	case (src.From == "") != (src.To == ""):
		return whatifapi.InputErrorf("source.from and source.to: give both, or neither for the last hour")
	}
	f.window = s.window
	if src.Namespace != "" {
		f.window.namespace = src.Namespace
	}
	if err := s.checkNamespace(ctx, f.window.namespace); err != nil {
		return err
	}
	f.window.from, f.window.to = src.From, src.To
	if src.From == "" {
		now := time.Now
		if s.now != nil {
			now = s.now
		}
		to := now().UTC().Truncate(time.Second)
		f.window.from, f.window.to = to.Add(-defaultWindow).Format(time.RFC3339), to.Format(time.RFC3339)
	}
	f.batchCap = int64(s.window.batchCap)
	return nil
}

// hidePath replaces the server's path to a recording in err by its name: an
// error names no server path.
func hidePath(err error, path, name string) error {
	if path == "" || err == nil || !strings.Contains(err.Error(), path) {
		return err
	}
	msg := errors.New(strings.ReplaceAll(err.Error(), path, name))
	if whatifapi.IsInputError(err) {
		return whatifapi.InputError{Err: msg}
	}
	return msg
}

// resolved reads the fleet from st as the CLI would at the recorded counts,
// and names the inputs by content: the recording's bytes, the cycle, the model
// and the variants.
func (s *servedSources) resolved(f *answerFlags, st *learnreplay.State, data []byte, info whatifapi.SourceInfo) (*whatifapi.Resolved, error) {
	variants, err := f.decodeVariants(st)
	if err != nil {
		return nil, apiError(err)
	}
	if len(variants) != 1 {
		names := make([]string, len(variants))
		for i, pv := range variants {
			names[i] = pv.state.Variant
		}
		return nil, whatifapi.InputErrorf("the model has %d decode variants (%s) at cycle %d; a plan prices one P/D fleet: name one with decodeVariant",
			len(variants), strings.Join(names, ", "), st.Cycle)
	}
	ps, err := f.prefillState(st)
	if err != nil {
		return nil, apiError(err)
	}
	q := ttftQuestion{}
	q.policyPercentile(st)
	if q.q == 0 {
		q = ttftQuestion{q: 0.9}
	}
	fleets, err := f.ttftFleets(st, q, variants)
	if err != nil {
		return nil, apiError(err)
	}
	sum := sha256.Sum256(data)
	prefill := ""
	if ps != nil {
		prefill = ps.Variant
	}
	content := strings.Join([]string{hex.EncodeToString(sum[:]), st.Namespace, st.ModelID, strconv.Itoa(st.Cycle), prefill,
		variants[0].state.Variant, f.prefillVariant, f.variant}, "\x00")
	return &whatifapi.Resolved{
		Inputs:  inputsDoc(f, st, &variants[0], ps, fleets[0], info),
		Content: content,
		Pricer:  &servedState{st: st, decodeVariant: f.variant, prefillVariant: f.prefillVariant},
	}, nil
}

// servedState prices fleets from one replayed state. The state is read only,
// so fleets may be priced from it at once.
type servedState struct {
	st                            *learnreplay.State
	decodeVariant, prefillVariant string
}

// seconds is x seconds as a duration, to the nanosecond.
func seconds(x float64) time.Duration { return time.Duration(math.Round(x * float64(time.Second))) }

// Fleet reads the fleet at k: the flags k stands for, checked as the CLI
// checks them, and the fleet slo-to-rate would read with them.
func (s *servedState) Fleet(k whatifapi.Knobs) (whatifapi.Fleet, error) {
	f := answerFlags{question: rateQuestion, cycle: "last", variant: s.decodeVariant, prefillVariant: s.prefillVariant,
		seed: whatif.DefaultResampling().Seed, set: map[string]bool{"ttft": true, "headroom": true}, warn: io.Discard,
		decode: k.Decode, prefill: k.Prefill, headroom: k.Headroom, priceRouterWait: whatif.DefaultPriceRouterWait}
	set := func(name string) { f.set[name] = true }
	if k.PromptTokens > 0 {
		f.inputTokens = k.PromptTokens
		set("assume-input-tokens")
	}
	if k.OutputTokens > 0 {
		f.outputTokens = k.OutputTokens
		set("assume-output-tokens")
	}
	a := k.Assume
	if a.VP != nil {
		f.assumeVP = *a.VP
		set("assume-vp")
	}
	if a.A != nil {
		f.assumeA = seconds(*a.A)
		set("assume-a")
	}
	if a.PromptCV != nil {
		f.assumePromptCV = *a.PromptCV
		set("assume-prompt-cv")
	}
	if a.RouterHop != nil {
		f.assumeRouterHop = seconds(*a.RouterHop)
		set("assume-router-hop")
	}
	if a.TPOTRatio != nil {
		f.tpotRatio = *a.TPOTRatio
		set("assume-tpot-ratio")
	}
	if k.ArrivalCV2 != nil {
		f.arrivalCV2 = *k.ArrivalCV2
		set("arrival-cv2")
	}
	if err := f.checkTTFT(); err != nil {
		return nil, apiError(err)
	}
	variants, err := f.decodeVariants(s.st)
	if err != nil {
		return nil, apiError(err)
	}
	if len(variants) != 1 {
		return nil, fmt.Errorf("%d decode variants priced, want the one the inputs read", len(variants))
	}
	q := ttftQuestion{q: k.TTFTPercentile}
	fleets, err := f.ttftFleets(s.st, q, variants)
	if err != nil {
		return nil, apiError(err)
	}
	sf := &servedFleet{f: &f, st: s.st, pv: variants[0], pf: fleets[0], q: q, tpotQ: k.TPOTPercentile,
		assumed: slices.Clone(fleets[0].in.Assumed)}
	if k.TPOTPercentile != 0 {
		sf.ratio = f.ratioFor(s.st, k.TPOTPercentile)
	}
	return sf, nil
}

// servedFleet is one fleet, priced figure by figure as the CLI prices it. It
// is one request's, never shared.
type servedFleet struct {
	f     *answerFlags
	st    *learnreplay.State
	pv    pricedVariant
	pf    *pricedFleet
	q     ttftQuestion
	tpotQ float64
	ratio resolvedRatio
	// assumed is the fleet's assumed row as read, before any figure added to
	// it.
	assumed []string
}

// PrefillKnee is replicas-to-slo's prefill knee.
func (sf *servedFleet) PrefillKnee() whatif.Result { return sf.pf.knee() }

// DecodeKnee is replicas-to-slo's decode knee.
func (sf *servedFleet) DecodeKnee() whatif.Result {
	return sf.pv.qualify(sf.pv.band(whatif.KneeRate(sf.pv.decode), whatif.KneeRate))
}

// Onsets is the fleet's flow-control onsets; nil with flow control off, or a
// fleet refused before pricing.
func (sf *servedFleet) Onsets() whatif.Result {
	if sf.pf.refused != nil || sf.pf.fleet.FlowControl.State != whatif.FlowControlOn {
		return nil
	}
	return whatif.FlowControlOnsets(sf.pf.fleet)
}

// TTFTAt is the TTFT at rate with its span, as replicas-to-slo --rate prices
// it, less the assumed words it adds.
func (sf *servedFleet) TTFTAt(rate float64) whatif.Result {
	price := func(fl whatif.TTFTFleet) whatif.Result { return whatif.TTFTAt(fl, rate, sf.q.q) }
	return withSpan(sf.pf.priced(price), sf.pf.corners(price))
}

// TTFTPoint is replicas-to-slo --rate's TTFT, and the words it adds to the
// assumed row.
func (sf *servedFleet) TTFTPoint(rate float64) (whatif.Result, []string) {
	n := len(sf.pf.in.Assumed)
	o, _ := sf.pf.atOperatingPoint(sf.q, whatif.ReplicasQuery{}, &rate, 0, nil)
	return o.ttft, slices.Clone(sf.pf.in.Assumed[n:])
}

// TPOTAt is replicas-to-slo --rate's mean TPOT and percentile.
func (sf *servedFleet) TPOTAt(rate float64) (mean, percentile whatif.Result) {
	query := whatif.ReplicasQuery{Rate: rate}
	if sf.tpotQ != 0 {
		query.PercentileTarget = percentileTarget(sf.tpotQ, sf.ratio)
	}
	a, _ := replicasAt(sf.pv, nil, nil, query, &rate, 0, sf.ratio)
	return a.slo.MeanTPOT, a.slo.PercentileTPOT
}

// decodeRate is slo-to-rate's decode rate at headroom, nil with no TPOT
// target.
func (sf *servedFleet) decodeRate(tpotSeconds, headroom float64) whatif.Result {
	if !(tpotSeconds > 0) {
		return nil
	}
	target := whatif.TPOTTarget{Seconds: tpotSeconds, Percentile: sf.tpotQ, Ratio: sf.ratio.ratio}
	return decodeRate(sf.pv, rateQuery(target, headroom, sf.ratio), sf.ratio)
}

// Limits is slo-to-rate's decode, prefill and fleet rates, and the fleet's
// at headroom 1 from the same prefill rate.
func (sf *servedFleet) Limits(ttftSeconds, tpotSeconds, headroom float64) whatifapi.LimitSet {
	n := len(sf.pf.in.Assumed)
	t := ttftQuestion{q: sf.q.q, seconds: ttftSeconds}
	a := rateAnswer{pv: sf.pv, fleet: sf.pf, result: sf.decodeRate(tpotSeconds, headroom)}
	corners := a.priceTTFTRate(t)
	return whatifapi.LimitSet{Decode: a.result, Prefill: a.ttft, Fleet: a.fleetAt(t, headroom, a.result, corners),
		WithoutHeadroom: a.fleetAt(t, 1, sf.decodeRate(tpotSeconds, 1), corners), Notes: slices.Clone(sf.pf.in.Assumed[n:])}
}

// PlainLimits is Limits' rates at headroom with no span: the prefill role's
// rate is RateForTTFT's alone, not swept over V_P and f.
func (sf *servedFleet) PlainLimits(ttftSeconds, tpotSeconds, headroom float64) (decode, prefill, fleet whatif.Result) {
	t := ttftQuestion{q: sf.q.q, seconds: ttftSeconds}
	decode = sf.decodeRate(tpotSeconds, headroom)
	prefill = sf.pf.priced(func(fl whatif.TTFTFleet) whatif.Result {
		return whatif.RateForTTFT(fl, whatif.TTFTTarget{Seconds: t.seconds, Percentile: t.q})
	})
	return decode, prefill, fleetRate(t, headroom, decode, prefill)
}

// TPOTRatio is R_q as the percentile asked resolved it.
func (sf *servedFleet) TPOTRatio() whatifapi.Ratio {
	if sf.tpotQ == 0 {
		return whatifapi.Ratio{}
	}
	if u := sf.ratio.refused; u != nil {
		return whatifapi.Ratio{Refused: &whatifapi.Refusal{Reason: string(u.Reason), Detail: u.Detail, Flags: whatifapi.FlagNames(u.Flags)}}
	}
	r := sf.ratio.ratio.Value
	out := whatifapi.Ratio{Value: &r, Source: sourceLearned, Flags: whatifapi.FlagNames(sf.ratio.flags)}
	switch {
	case sf.ratio.ratio.Assumed:
		out.Source = shapeAssumed
		out.Flags = append(out.Flags, string(whatif.AssumedTPOTRatio))
	case sf.f.engineTPOT:
		out.Source = "engine histogram"
	}
	return out
}

// Assumed is the fleet's assumed inputs as read, in the CLI's words.
func (sf *servedFleet) Assumed() []string { return sf.assumed }
