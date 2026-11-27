package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// --experimental-ttft=v2 prices a backtest's TTFT with the experimental
// rank-mixture model, where --experimental-ttft (or =v1) turns on the TTFT
// model's switches (experimental_ttft.go). It is backtest's only: the
// rank-mixture model is fitted on a backtest's fit window. The model (whatif.QueueRankMixture; whatifapi.BacktestRequest.TTFTModel):
// processor sharing per prefill data-parallel rank, mixed over the ranks'
// measured traffic shares, a fitted KV-deferral wait and a per-prompt-token
// non-queue part, fitted on the backtest's fit window. Every TTFT it prices
// is flagged whatif.RankMixtureTTFT and reads OUTSIDE VALIDATED REGION.

// experimentalTTFTV2 is the flag's value that asks for the rank mixture.
const experimentalTTFTV2 = "v2"

// experimentalTTFTV2NLLine is the flag's value that asks for the rank
// mixture's variant with Z measured part by part on v2's service line
// (whatif.QueueRankMixtureNLLine), wherever =v2 is taken: backtest, serve
// and at-replicas, each fitting and pricing it as backtest does
// (whatifapi's fitRankMixtureSteps).
const experimentalTTFTV2NLLine = "v2nl-line"

// experimentalTTFTNLLineUsage is the help for =v2nl-line, on every command
// that takes it.
const experimentalTTFTNLLineUsage = "=v2nl-line prices with its variant that measures each part of the non-queue time on its own, " +
	"calibrates nothing to the router and abstains unless its fit window prices itself (flagged the same)"

// experimentalTTFTV2Usage is backtest's and predict's help for the flag: the
// switches' (experimentalTTFTUsage), and =v2.
const experimentalTTFTV2Usage = experimentalTTFTUsage + ". On backtest, =v2 instead prices TTFT with an experimental model: limited " +
	"processor sharing per prefill rank, mixed over the ranks' measured traffic shares, with a fitted KV-deferral wait and a cost per " +
	"prompt token, fitted on the fit window; every TTFT it prices is flagged RankMixtureTTFT and reads OUTSIDE VALIDATED REGION"

// rankMixtureStratifiedFlag is the switch that prices the rank mixture with
// stratified service moments (whatif.DefaultRankMixtureStratified), on by
// default wherever =v2 prices.
const rankMixtureStratifiedFlag = "rank-mixture-stratified"

// rankMixtureStratifiedUsage is its help, the same on every command.
const rankMixtureStratifiedUsage = "with --experimental-ttft=v2, read each prefill rank's mean service and its spread from the " +
	"computed-token histogram itself, not from the rank's own random draws, so a figure moves far less with the draws; " +
	"on by default, =false reads them from the draws"

// optionalBool is a boolean flag that is nil until it is given, so that a
// flags struct's zero value is the default (def), whatever it is: the
// caller reads nil as def.
type optionalBool struct {
	p   **bool
	def bool
}

// String is the value, def where none was given.
func (v optionalBool) String() string {
	if v.p == nil || *v.p == nil {
		return strconv.FormatBool(v.def)
	}
	return strconv.FormatBool(**v.p)
}

// Set reads one value.
func (v optionalBool) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*v.p = &b
	return nil
}

// IsBoolFlag lets the flag stand without a value: alone it is true.
func (v optionalBool) IsBoolFlag() bool { return true }

// stratifiedOr is a --rank-mixture-stratified given, or the default.
func stratifiedOr(given *bool) bool {
	if given == nil {
		return whatif.DefaultRankMixtureStratified
	}
	return *given
}

// experimentalTTFTValue is --experimental-ttft where it also takes =v2: alone,
// true or v1 it sets *switches (the TTFT model's switches), v2 sets *v2,
// false neither.
type experimentalTTFTValue struct {
	switches, v2 *bool
	// nlLine, where set, takes =v2nl-line: v2 and nlLine both true.
	nlLine *bool
}

// String is the value.
func (v experimentalTTFTValue) String() string {
	switch {
	case v.v2 != nil && *v.v2 && v.nlLine != nil && *v.nlLine:
		return experimentalTTFTV2NLLine
	case v.v2 != nil && *v.v2:
		return experimentalTTFTV2
	case v.switches != nil && *v.switches:
		return "true"
	}
	return "false"
}

// Set reads one value.
func (v experimentalTTFTValue) Set(s string) error {
	switch s {
	case "true", "v1":
		*v.switches, *v.v2 = true, false
	case "false":
		*v.switches, *v.v2 = false, false
	case experimentalTTFTV2:
		*v.switches, *v.v2 = false, true
	case experimentalTTFTV2NLLine:
		if v.nlLine == nil {
			return fmt.Errorf("--%s=%s is backtest's, serve's, at-replicas' and day-plan's", experimentalTTFTFlag, s)
		}
		*v.switches, *v.v2 = false, true
	default:
		return fmt.Errorf("want --%s, --%s=false or --%s=%s", experimentalTTFTFlag, experimentalTTFTFlag, experimentalTTFTFlag, experimentalTTFTV2)
	}
	if v.nlLine != nil {
		*v.nlLine = s == experimentalTTFTV2NLLine
	}
	return nil
}

// IsBoolFlag lets the flag stand without a value: alone it is true.
func (v experimentalTTFTValue) IsBoolFlag() bool { return true }

// withRankMixture is res with the recording's cycles as the rank mixture reads
// them (learnreplay.MixtureSeriesFrom), read from the file at path in a pass
// of their own: only the cycles keep holds, where it is not nil, as res
// was read (a load split's fit).
func withRankMixture(path string, res *whatifapi.Resolved, keep *whatifapi.CycleSet) error {
	src, err := openRecordingCycles(path, keep)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	return rankMixtureFrom(src.open, res)
}

// rankMixtureFrom is withRankMixture on a source already open: a recording
// file or a captured window, as serve reads it.
func rankMixtureFrom(open learnreplay.Source, res *whatifapi.Resolved) error {
	v := learnreplay.SeriesVariants{Decode: res.Inputs.Fleet.Decode.Variant}
	if p := res.Inputs.Fleet.Prefill; p != nil {
		v.Prefill = p.Variant
	}
	cycles, err := learnreplay.MixtureSeriesFrom(open, learnreplay.StateQuery{Namespace: res.Inputs.Namespace, ModelID: res.Inputs.Model}, v)
	if err != nil {
		return err
	}
	if len(cycles) != len(res.Series) {
		return fmt.Errorf("the rank mixture read %d cycles of the recording and the series %d", len(cycles), len(res.Series))
	}
	res.Mixture = cycles
	return nil
}

// prefillSlotsOf is K, the prompts a prefill rank runs at once, as the
// recording's state gives the engine's flags: --max-num-seqs with chunked
// prefill, 1 without (a rank then serves first come); 0 where
// --max-num-seqs is not read.
func prefillSlotsOf(res *whatifapi.Resolved) int {
	s, ok := res.Pricer.(*servedState)
	if !ok || res.Inputs.Fleet.Prefill == nil {
		return 0
	}
	return variantParams(s.st, res.Inputs.Fleet.Prefill.Variant).PrefillSlots()
}

// experimentalReport prices rep's tables with the experimental models
// asked: the TTFT with the rank-mixture model (--experimental-ttft=v2) or
// its variant (=v2nl-line), the
// TPOT p90 and p95 with the per-request TPOT distribution
// (--experimental-tpot=v2), each fitted on the recording read again with
// the series it reads, as serve's POST /v1/report does.
func (f *answerFlags) experimentalReport(ctx context.Context, rep *whatifapi.ReportResponse, q whatifapi.ReportQuery) error {
	cycle, err := f.cycleNumber()
	if err != nil {
		return err
	}
	req := whatifapi.InputsRequest{Model: f.model, PrefillVariant: f.prefillVariant, DecodeVariant: f.variant, PrefillVPPlateau: f.vpPlateau,
		PrefillHitAtCount: f.hitAtCount, PrefillVPServiceLine: f.vpServiceLine, PrefillFLoadCurve: f.fLoadCurve, Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(f.recording), Cycle: cycle}}
	if f.rankMixtureTTFT {
		req.TTFTModel = rankMixtureModel(f.rankMixtureNLLine)
	}
	if f.tpotDistribution {
		req.TPOTModel = f.tpotModel
	}
	src := &servedSources{warn: f.warn, recordings: filepath.Dir(f.recording)}
	res, err := src.Resolve(ctx, req)
	if err != nil {
		if whatifapi.IsInputError(err) {
			return usageError{err}
		}
		return err
	}
	if f.rankMixtureTTFT {
		rep.TTFTModel = whatifapi.ApplyTTFTModel(res, rep, q,
			f.rankMixtureFit.options(req.TTFTModel, stratifiedOr(f.rankMixtureStratified)))
	}
	if f.tpotDistribution {
		rep.TPOTModel = whatifapi.ApplyTPOTModel(res, rep, q, f.tpotFit.steps())
	}
	return nil
}

// rankMixtureModel is the experimental TTFT model --experimental-ttft=v2
// asks for (whatif.QueueRankMixture), or =v2nl-line where nlLine
// (whatif.QueueRankMixtureNLLine): the name a backtest's, a source's and a
// server's request carry (whatifapi BacktestRequest.TTFTModel,
// InputsRequest.TTFTModel, Config.TTFTModel).
func rankMixtureModel(nlLine bool) string {
	if nlLine {
		return string(whatif.QueueRankMixtureNLLine)
	}
	return string(whatif.QueueRankMixture)
}

// rankMixtureValue is the --experimental-ttft value the answer was asked
// with, v2 or v2nl-line, for its messages.
func (f *answerFlags) rankMixtureValue() string {
	if f.rankMixtureNLLine {
		return experimentalTTFTV2NLLine
	}
	return experimentalTTFTV2
}
