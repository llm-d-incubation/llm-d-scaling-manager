package main

import (
	"fmt"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --experimental-tpot=v2 prices a backtest's TPOT percentiles with the
// experimental per-request TPOT distribution (whatif.TPOTRenewalLognormal;
// whatifapi.BacktestRequest.TPOTModel): lognormal around the planner's mean
// TPOT, its log-variance the decode groups' spread plus a renewal term over
// the step's output lengths, fitted on the backtest's fit window by
// interval-censored likelihood over the router's TPOT buckets. On
// at-replicas it prices the tables' TPOT p90 and p95 the same way, fitted
// on the --recording (answerFlags.experimentalReport), and serve's main
// view with it (whatifapi.InputsRequest.TPOTModel). Every percentile it
// prices
// is flagged whatif.TPOTDistribution and reads OUTSIDE VALIDATED REGION.

// experimentalTPOTFlag is the flag's name.
const experimentalTPOTFlag = "experimental-tpot"

// experimentalTPOTV2 is the flag's value for the one lognormal at the
// output lengths' mean (whatif.TPOTRenewalLognormal).
const experimentalTPOTV2 = "v2"

// experimentalTPOTV2Mix is the flag's value for the per-length mixture
// (whatif.TPOTPerLengthMixture): one lognormal per output-length group, in
// place of one at their mean. Off unless given.
const experimentalTPOTV2Mix = "v2-mix"

// experimentalTPOTUsage is backtest's help for the flag.
const experimentalTPOTUsage = "=v2 prices the TPOT percentiles with an experimental per-request TPOT distribution: lognormal around the " +
	"planner's mean TPOT, its spread the decode groups' plus the renewal of speculative decode steps over the step's output lengths, " +
	"fitted on the fit window by interval-censored likelihood over the router's TPOT buckets and scored at the histogram's resolution; " +
	"=v2-mix prices it as a mixture over the output lengths, one lognormal per length; " +
	"every percentile it prices is flagged TPOTDistribution and reads OUTSIDE VALIDATED REGION"

// tpotDistribution asks req for the experimental TPOT distribution where
// --experimental-tpot is given, and reads each side's decode cycles for
// it: test from testPath, fit from fitPath.
func (f *backtestFlags) tpotDistribution(req *whatifapi.BacktestRequest, testPath string, test *whatifapi.Resolved, fitPath string, fit *whatifapi.Resolved) error {
	model, err := experimentalTPOTOf(f.experimentalTPOT)
	if err != nil || model == "" {
		return err
	}
	req.TPOTModel = model
	req.DecodeSlots, req.SpecTokens = decodeEngineOf(fit)
	req.TPOTDistMinFitSteps = f.tpotFit.steps()
	if err := withDecodeSeries(testPath, test, nil); err != nil {
		return err
	}
	return withDecodeSeries(fitPath, fit, req.FitCycles)
}

// experimentalTPOTOf reads --experimental-tpot's value, backtest's and
// serve's alike: the distribution it names, "" unset.
func experimentalTPOTOf(s string) (string, error) {
	switch s {
	case "":
		return "", nil
	case experimentalTPOTV2:
		return whatif.TPOTRenewalLognormal, nil
	case experimentalTPOTV2Mix:
		return whatif.TPOTPerLengthMixture, nil
	}
	return "", usagef("--%s=%q, want --%s=%s or --%s=%s", experimentalTPOTFlag, s, experimentalTPOTFlag, experimentalTPOTV2,
		experimentalTPOTFlag, experimentalTPOTV2Mix)
}

// withDecodeSeries is res with the recording's cycles as the TPOT
// distribution reads them (learnreplay.DecodeSeriesFrom), read from the file
// at path in a pass of their own: only the cycles keep holds, where it is
// not nil, as res was read (a load split's fit).
func withDecodeSeries(path string, res *whatifapi.Resolved, keep *whatifapi.CycleSet) error {
	src, err := openRecordingCycles(path, keep)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	return decodeSeriesFrom(src.open, res)
}

// decodeSeriesFrom is withDecodeSeries on a source already open: a
// recording file or a captured window, as serve reads it.
func decodeSeriesFrom(open learnreplay.Source, res *whatifapi.Resolved) error {
	v := learnreplay.SeriesVariants{Decode: res.Inputs.Fleet.Decode.Variant}
	if p := res.Inputs.Fleet.Prefill; p != nil {
		v.Prefill = p.Variant
	}
	cycles, err := learnreplay.DecodeSeriesFrom(open, learnreplay.StateQuery{Namespace: res.Inputs.Namespace, ModelID: res.Inputs.Model}, v)
	if err != nil {
		return err
	}
	if len(cycles) != len(res.Series) {
		return fmt.Errorf("the decode series read %d cycles of the recording and the series %d", len(cycles), len(res.Series))
	}
	res.Decode = cycles
	return nil
}

// decodeEngineOf is the decode engine's max-num-seqs (0 not read) and
// num_speculative_tokens (0 none or not read), as the recording's state
// gives its flags.
func decodeEngineOf(res *whatifapi.Resolved) (slots, spec int) {
	s, ok := res.Pricer.(*servedState)
	if !ok {
		return 0, 0
	}
	return variantParams(s.st, res.Inputs.Fleet.Decode.Variant).DecodeSlots()
}
