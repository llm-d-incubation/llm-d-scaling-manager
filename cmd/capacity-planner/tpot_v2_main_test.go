package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// q32TPOTServer is a server over the q32 staircase started without the
// experimental flags, and the inputs ids of the recording without and with
// the request's tpotModel.
func q32TPOTServer(t *testing.T) (h http.Handler, plain, v2 whatifapi.InputsResponse) {
	t.Helper()
	f := serveFlags{recordings: q32Dir, listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true}
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	src := &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(q32Recording)}
	inputs := func(model string) whatifapi.InputsResponse {
		code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: src, TPOTModel: model})
		require.Equal(t, 200, code, "%s", body)
		var in whatifapi.InputsResponse
		require.NoError(t, json.Unmarshal(body, &in))
		return in
	}
	return h, inputs(""), inputs(whatif.TPOTRenewalLognormal)
}

// q32TPOTPlan is the plan of id at the recorded fleet with a TPOT target
// of target at statistic.
func q32TPOTPlan(t *testing.T, h http.Handler, id, statistic string, target float64) whatifapi.PlanResponse {
	t.Helper()
	code, body := loopbackPost(t, h, "/v1/plan", whatifapi.PlanRequest{InputsID: id, SLO: &whatifapi.SLORequest{TPOT: &whatifapi.Objective{
		Statistic: statistic, Target: ptrTo(target)}}})
	require.Equal(t, 200, code, "%s", body)
	var out whatifapi.PlanResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// A source resolved with the request's tpotModel, on a server started
// without the flag, reads the decode series and the decode engine's flags
// the experimental TPOT model fits on, and names its fit in the inputs. On
// the q32 staircase it prices: the plan's p90 curve is the planner's own
// mean at every rate times the model's p90 ratio, flagged, extrapolated
// past the load the window served; decode's limit at a p90 target is the
// planner's own limit at the mean target over that ratio; and the TTFT,
// which the model does not price, is the planner's.
func TestMainViewTPOTV2_ServedPricesTheQ32Curve(t *testing.T) {
	h, plain, v2 := q32TPOTServer(t)
	assert.Nil(t, plain.TPOTModel)
	require.NotNil(t, v2.TPOTModel)
	assert.NotEqual(t, plain.InputsID, v2.InputsID)
	m := v2.TPOTModel
	require.Empty(t, m.Abstained)
	require.NotNil(t, m.Fit)
	assert.GreaterOrEqual(t, m.Fit.FitSteps, whatifapi.MinTPOTDistFitSteps)
	assert.Positive(t, m.Fit.DecodeSlots, "max-num-seqs read from the recording")
	require.NotNil(t, m.HeldOut)
	assert.Positive(t, m.HeldOut.Steps)
	require.NotNil(t, m.MeasuredUpTo)
	require.NotNil(t, m.P90Ratio)
	r90 := *m.P90Ratio
	assert.Greater(t, r90, 1.0)

	const target = 0.05
	mean := q32TPOTPlan(t, h, plain.InputsID, "mean", target)
	own := q32TPOTPlan(t, h, plain.InputsID, "p90", target)
	got := q32TPOTPlan(t, h, v2.InputsID, "p90", target)
	require.Len(t, got.TPOT.Curve, len(mean.TPOT.Curve))
	top := *m.MeasuredUpTo
	priced, beyond := 0, 0
	for i, p := range got.TPOT.Curve {
		mp := mean.TPOT.Curve[i]
		require.InDelta(t, mp.Rate, p.Rate, 1e-12)
		if mp.Figure.Value == nil {
			assert.Nil(t, p.Figure.Value, "no mean, no percentile at %g r/s", p.Rate)
			continue
		}
		require.NotNil(t, p.Figure.Value, "%g r/s", p.Rate)
		assert.InEpsilon(t, r90**mp.Figure.Value, *p.Figure.Value, 1e-9, "the planner's mean times the ratio at %g r/s", p.Rate)
		assert.Contains(t, p.Figure.Flags, string(whatif.TPOTDistribution))
		assert.NotContains(t, p.Figure.Flags, string(whatif.CoarseTPOTBucket), "the planner's R_q read is not the model's")
		assert.Equal(t, p.Rate > top*(1+1e-9), p.Figure.Extrapolated, "%g r/s, measured up to %g", p.Rate, top)
		priced++
		if p.Figure.Extrapolated {
			beyond++
		}
	}
	assert.Greater(t, priced, 10)
	t.Logf("q32: %d points priced, %d beyond %g r/s; p90 ratio %.3f (the planner's own %.3f)", priced, beyond, top, r90,
		*own.TPOT.Ratio.Value)
	require.NotNil(t, got.TPOT.Model)
	assert.True(t, got.TPOT.Model.Priced)
	assert.Equal(t, "experimental TPOT model", got.TPOT.Ratio.Source)

	// Decode's limit at the p90 target is the planner's own at the mean
	// target over the ratio.
	at := q32TPOTPlan(t, h, plain.InputsID, "mean", target/r90)
	require.NotNil(t, at.Limits.Decode.Value)
	require.NotNil(t, got.Limits.Decode.Value)
	assert.InEpsilon(t, *at.Limits.Decode.Value, *got.Limits.Decode.Value, 1e-9)
	assert.Contains(t, got.Limits.Decode.Flags, string(whatif.TPOTDistribution))

	assert.Equal(t, own.TTFT, got.TTFT, "the TTFT is the planner's")
	assert.Equal(t, own.Limits.Prefill, got.Limits.Prefill)

	// The p95 is priced the same way, at the model's p95 ratio.
	require.NotNil(t, m.P95Ratio)
	r95 := *m.P95Ratio
	assert.Greater(t, r95, r90)
	got95 := q32TPOTPlan(t, h, v2.InputsID, "p95", target)
	require.NotNil(t, got95.TPOT.Model)
	assert.True(t, got95.TPOT.Model.Priced)
	for i, p := range got95.TPOT.Curve {
		if mp := mean.TPOT.Curve[i]; mp.Figure.Value != nil {
			require.NotNil(t, p.Figure.Value, "p95 at %g r/s", p.Rate)
			assert.InEpsilon(t, r95**mp.Figure.Value, *p.Figure.Value, 1e-9, "p95 at %g r/s", p.Rate)
			assert.Contains(t, p.Figure.Flags, string(whatif.TPOTDistribution))
		}
	}

	code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording,
		Name: filepath.Base(q32Recording)}, TPOTModel: "ratio"})
	assert.Equal(t, 400, code, "%s", body)
}

// serve --experimental-tpot=v2 resolves every source with the model, so a
// page user need not ask: the inputs name it, as a request that asks does.
func TestMainViewTPOTV2_ServeFlagIsTheDefault(t *testing.T) {
	f := serveFlags{recordings: q32Dir, listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true, experimentalTPOT: experimentalTPOTV2}
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	_, _, asked := q32TPOTServer(t)
	code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording,
		Name: filepath.Base(q32Recording)}})
	require.Equal(t, 200, code, "%s", body)
	var in whatifapi.InputsResponse
	require.NoError(t, json.Unmarshal(body, &in))
	require.NotNil(t, in.TPOTModel)
	assert.Equal(t, asked.InputsID, in.InputsID)
	assert.Equal(t, asked.TPOTModel, in.TPOTModel)
}

// at-replicas --experimental-tpot=v2 fits the model on the recording and
// prices the measured shape's TPOT p90 at each row as the model's ratio
// times the row's own mean, flagged; every other figure, the verdict and
// the summary cells among them, is the answer without the flag. The
// measured shape is asked: the default, the load's own shape, is the
// planner's (the model prices the source's own lengths). The other
// answer commands refuse it, and so does a value other than v2.
func TestAtReplicasTPOTV2(t *testing.T) {
	args := []string{"at-replicas", "--recording", q32Recording, "--tpot", "50ms@p90", "--shape", "measured", "-o", "json"}
	off, offCode := runCmd(t, args...)
	on, onCode := runCmd(t, append(args, "--"+experimentalTPOTFlag+"="+experimentalTPOTV2)...)
	assert.Equal(t, offCode, onCode)
	var a, b whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(off), &a), off)
	require.NoError(t, json.Unmarshal([]byte(on), &b), on)
	assert.Nil(t, a.TPOTModel)
	require.NotNil(t, b.TPOTModel)
	require.Empty(t, b.TPOTModel.Abstained)
	r90 := *b.TPOTModel.P90Ratio
	priced := 0
	for i := range b.Shapes {
		sa, sb := &a.Shapes[i], &b.Shapes[i]
		for j := range sb.Table {
			ra, rb := &sa.Table[j], &sb.Table[j]
			if sb.Label == "measured" && rb.TPOTPercentile != nil && rb.TPOTMean.Value != nil && rb.Rate > 0 {
				require.NotNil(t, rb.TPOTPercentile.Value)
				assert.InEpsilon(t, r90**rb.TPOTMean.Value, *rb.TPOTPercentile.Value, 1e-9)
				assert.Contains(t, rb.TPOTPercentile.Flags, string(whatif.TPOTDistribution))
				priced++
			}
			ra.TPOTPercentile = rb.TPOTPercentile
		}
		sa.Flags, sa.Assumed = sb.Flags, sb.Assumed
	}
	assert.Positive(t, priced)
	b.TPOTModel = nil
	assert.Equal(t, a, b, "every other figure is the answer without the flag")

	for _, bad := range [][]string{
		{"slo-to-rate", "--recording", q32Recording, "--tpot", "50ms@p90", "--" + experimentalTPOTFlag + "=" + experimentalTPOTV2},
		{"at-replicas", "--recording", q32Recording, "--" + experimentalTPOTFlag + "=v3"},
	} {
		out, code := runCmd(t, bad...)
		assert.Equal(t, 2, code, out)
	}
}
