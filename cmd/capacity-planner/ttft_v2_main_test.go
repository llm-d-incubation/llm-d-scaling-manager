package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// A source resolved with the request's ttftModel reads the per-rank series
// the experimental model fits on, on a server started without the flag, and
// names the model's fit in its inputs. On the q32 staircase, a fit window
// under 12 h, the model abstains: the plan's TTFT line is the planner's,
// point for point, and the plan says the model was not used and why.
func TestMainViewV2_RequestAsksAndTheDefaultStandsWhereItAbstains(t *testing.T) {
	f := serveFlags{recordings: q32Dir, listen: "127.0.0.1:0", auth: whatifapi.AuthNone, noSelfCheck: true}
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	src := &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: filepath.Base(q32Recording)}
	inputs := func(model string) whatifapi.InputsResponse {
		code, body := loopbackPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Source: src, TTFTModel: model})
		require.Equal(t, 200, code, "%s", body)
		var in whatifapi.InputsResponse
		require.NoError(t, json.Unmarshal(body, &in))
		return in
	}
	plain, v2 := inputs(""), inputs(string(whatif.QueueRankMixture))
	assert.Nil(t, plain.TTFTModel)
	require.NotNil(t, v2.TTFTModel)
	assert.NotEqual(t, plain.InputsID, v2.InputsID)
	assert.NotContains(t, v2.TTFTModel.Abstained, "did not give", "the request reads the series the model needs")
	assert.Contains(t, v2.TTFTModel.Abstained, "spread not calibrated", "the 12 h fit-window rule holds")

	plan := func(id string) whatifapi.PlanResponse {
		code, body := loopbackPost(t, h, "/v1/plan", whatifapi.PlanRequest{InputsID: id, SLO: &whatifapi.SLORequest{TTFT: &whatifapi.Objective{
			Statistic: "p90", Target: ptrTo(2.0)}}})
		require.Equal(t, 200, code, "%s", body)
		var out whatifapi.PlanResponse
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}
	a, b := plan(plain.InputsID), plan(v2.InputsID)
	assert.Equal(t, a.TTFT.Line, b.TTFT.Line, "where the model abstains the planner's line stands")
	require.NotNil(t, b.TTFT.Model)
	assert.False(t, b.TTFT.Model.Priced)
	assert.Contains(t, b.TTFT.Model.NotPriced, "abstained")
	assert.Nil(t, a.TTFT.Model)
}

// at-replicas --experimental-ttft=v2 fits the model on the recording and
// says so in its answer; where it abstains, every other figure is the
// answer without the flag. The other answer commands refuse =v2.
func TestAtReplicasV2(t *testing.T) {
	args := []string{"at-replicas", "--recording", q32Recording, "--ttft", "2s@p90", "-o", "json"}
	off, offCode := runCmd(t, args...)
	on, onCode := runCmd(t, append(args, "--"+experimentalTTFTFlag+"="+experimentalTTFTV2)...)
	assert.Equal(t, offCode, onCode)
	var a, b whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(off), &a), off)
	require.NoError(t, json.Unmarshal([]byte(on), &b), on)
	assert.Nil(t, a.TTFTModel)
	require.NotNil(t, b.TTFTModel)
	assert.NotEmpty(t, b.TTFTModel.Abstained)
	b.TTFTModel = nil
	assert.Equal(t, a, b)

	out, code := runCmd(t, "slo-to-rate", "--recording", q32Recording, "--"+experimentalTTFTFlag+"="+experimentalTTFTV2)
	assert.Equal(t, 2, code, out)
}
