package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// plannerV2At is the capacity planner's experimental models fitted on the
// recording at cycle (0 the last), as at-replicas --experimental-ttft=v2
// --experimental-tpot=v2 fits them.
func plannerV2At(t *testing.T, recording string, cycle int) (*whatifapi.TTFTModelInfo, *whatifapi.TPOTModelInfo) {
	t.Helper()
	src := &servedSources{warn: io.Discard, recordings: filepath.Dir(recording)}
	res, err := src.Resolve(context.Background(), whatifapi.InputsRequest{TTFTModel: string(whatif.QueueRankMixture),
		TPOTModel: whatif.TPOTRenewalLognormal,
		Source:    &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(recording), Cycle: cycle}})
	require.NoError(t, err)
	var out whatifapi.ReportResponse
	return whatifapi.ApplyTTFTModel(res, &out, whatifapi.ReportQuery{},
			whatifapi.TTFTModelOptions{Model: string(whatif.QueueRankMixture), Stratified: whatif.DefaultRankMixtureStratified}),
		whatifapi.ApplyTPOTModel(res, &out, whatifapi.ReportQuery{}, 0)
}

// controllerV2At is the slo analyzer's v2 learners refitted at cycle on the
// same recording, replayed through learnreplay as the engine feeds a passive
// model's cycles; edit changes each input first.
func controllerV2At(t *testing.T, recording string, cycle int, edit func(*domain.AnalyzerInput)) slo.ModelFit {
	t.Helper()
	raw, err := os.ReadFile(recording)
	require.NoError(t, err)
	b, err := gunzipped(raw)
	require.NoError(t, err)
	fit, err := learnreplay.ModelV2At(context.Background(), learnreplay.BytesSource(b), learnreplay.StateQuery{Cycle: cycle}, edit)
	require.NoError(t, err)
	return fit
}

// The controller and the capacity planner give the same v2 figures on the
// same recording (issue #440): the slo analyzer's learners, fed the
// recording's cycles through learnreplay, and the planner's experimental
// models, fitted on the same cycles by at-replicas, call the same fit cores
// (whatif.FitMixtureWindow, whatif.FitTPOTWindow) on cycles built by the same
// builder (whatif.FitCycleOf), and agree to the last bit: the TPOT model's
// fit, its p90 and p95 ratios and the highest load it served; the TTFT
// model's fit, here abstaining on the q32 warm-up (2.4 h, under the 12 h its
// spread is calibrated on) for the same reason. Two cycles: the last, and
// one inside the recording, where the TPOT window is shorter.
//
// Negative control: dropping the router's TPOT histogram on the controller
// side alone moves the TPOT fit, so the comparison reads the inputs and not
// only the code.
func TestV2Parity_ControllerAndPlannerAgreeOnTheRecording(t *testing.T) {
	for _, cycle := range []int{0, 400} {
		ttft, tpot := plannerV2At(t, q32Recording, cycle)
		got := controllerV2At(t, q32Recording, cycle, nil)

		require.NotNil(t, tpot.Fit)
		require.NotNil(t, got.TPOT.Fitted)
		assert.Equal(t, *tpot.Fit, got.TPOT.Fitted.Report, "cycle %d: the TPOT fit", cycle)
		assert.Equal(t, tpot.Abstained, got.TPOT.Abstained, "cycle %d", cycle)
		if cycle != 0 {
			// 20 steps of the router's histogram by cycle 400: under the 24
			// the distribution fits on, for both.
			assert.Contains(t, got.TPOT.Abstained, "under 24")
			continue
		}
		require.Empty(t, tpot.Abstained, "the TPOT model prices on the whole warm-up")
		require.NotNil(t, tpot.P90Ratio)
		require.NotNil(t, tpot.P95Ratio)
		assert.Equal(t, *tpot.P90Ratio, got.TPOT.Ratio(0.9), "cycle %d: p90 ratio", cycle)
		assert.Equal(t, *tpot.P95Ratio, got.TPOT.Ratio(0.95), "cycle %d: p95 ratio", cycle)
		require.NotNil(t, tpot.MeasuredUpTo)
		assert.Equal(t, *tpot.MeasuredUpTo, got.TPOT.Top, "cycle %d: highest load served", cycle)

		require.NotNil(t, ttft.Fit, "the TTFT model fitted, then abstained")
		require.NotNil(t, got.TTFT.Fitted)
		assert.Equal(t, *ttft.Fit, got.TTFT.Fitted.Report, "cycle %d: the TTFT fit", cycle)
		assert.Equal(t, ttft.Abstained, got.TTFT.Abstained(), "cycle %d", cycle)
		assert.Contains(t, got.TTFT.Abstained(), "spread not calibrated")
	}

	dropped := controllerV2At(t, q32Recording, 0, func(in *domain.AnalyzerInput) {
		if in.RouterLatency != nil {
			r := *in.RouterLatency
			r.TPOTBuckets = nil
			in.RouterLatency = &r
		}
	})
	_, tpot := plannerV2At(t, q32Recording, 0)
	assert.NotEqual(t, *tpot.Fit, dropped.TPOT.Fitted.Report, "an input dropped on the controller side moves the fit")
	assert.NotEmpty(t, dropped.TPOT.Abstained)

	// The decode engines' TPOT histogram rides on the controller's decode
	// cycle (whatif.DecodeCycle.EngineTPOT) and joins the router's in the
	// fit's fold scores: dropped on the controller side alone, they move.
	joined := 0
	for _, f := range tpot.Fit.Fold {
		joined += f.EngineJoined
	}
	require.Positive(t, joined, "the recording's decode engines read a TPOT histogram the folds join")
	noEngine := controllerV2At(t, q32Recording, 0, func(in *domain.AnalyzerInput) {
		for i := range in.ReplicaMetrics {
			in.ReplicaMetrics[i].TPOTBuckets = nil
		}
	})
	assert.NotEqual(t, tpot.Fit.Fold, noEngine.TPOT.Fitted.Report.Fold, "the engines' histogram dropped on the controller side moves the folds")
}
