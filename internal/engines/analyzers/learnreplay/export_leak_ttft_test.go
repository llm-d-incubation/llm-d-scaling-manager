package learnreplay

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The fields R1 records for the TTFT model (analyzer-learner.md, 2.9) hold
// numbers only: histograms, means, presence bits, and per-pool figures with
// the pool's name dropped. So the export needs no rule of its own for them.
// This holds it to that: a recording whose every Analyze input carries all of
// them, under names a cluster gives, exports with every name renamed and
// every R1 figure kept, and the export replays byte for byte.
func TestExportLeak_TTFTInputsAreNumbersOnly(t *testing.T) {
	const ns, model, pod = "payroll", "acme-llm", "payroll-7f9c4-x7k2p"
	in := ttftInput()
	in.Namespace, in.ModelID = ns, model
	in.ReplicaMetrics[0].Namespace, in.ReplicaMetrics[0].ModelID, in.ReplicaMetrics[0].PodName = ns, model, pod

	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	f.store.FileScaleTarget(ns, model, "v", "H100", 1,
		capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, ModelPath: model, Image: "img:1"}, false, replayAt)
	_, err := f.sat.Analyze(context.Background(), in)
	require.NoError(t, err)
	require.NoError(t, rec.Err())
	raw := bytes.Clone(f.buf.Bytes())
	for _, name := range ttftFieldNames {
		require.Contains(t, string(raw), `"`+name+`":`, "precondition: the recording holds %s", name)
	}

	out, _, err := Export(raw, ExportOptions{Key: []byte("k")})
	require.NoError(t, err, "the export refused a recording of numbers it should keep")
	for _, secret := range []string{ns, model} {
		assert.NotContains(t, string(out), secret, "the export kept a name")
	}
	for _, name := range ttftFieldNames {
		assert.Contains(t, string(out), `"`+name+`":`, "the export dropped %s", name)
	}
	for _, kept := range []string{
		`"PromptTokensBuckets":[{"UpperBound":1024,"Rate":1.25},{"UpperBound":"+Inf","Rate":2.5}]`,
		`"TPOTBuckets":[{"UpperBound":0.05,"Rate":2.25},{"UpperBound":"+Inf","Rate":2.5}]`,
		`"AvgKVTransferTime":0.0075`,
		`"DecodeSaturation":0.875`,
	} {
		assert.True(t, strings.Contains(string(out), kept), "the export changed a figure: %s", kept)
	}

	r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
	require.NoError(t, err)
	assert.Empty(t, r.Failed())
	assert.Empty(t, r.Mismatches)
	assert.Equal(t, string(out), string(r.Recording), "the export replays byte for byte")
}
