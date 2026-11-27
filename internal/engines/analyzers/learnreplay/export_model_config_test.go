package learnreplay

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// modelConfigRecording is a recording of one Analyze of a variant filed with
// m as its model config, under names a cluster gives.
func modelConfigRecording(t *testing.T, m capacity.ModelConfig) []byte {
	t.Helper()
	const ns, model, pod = "payroll", "acme-llm", "payroll-7f9c4-x7k2p"
	in := ttftInput()
	in.Namespace, in.ModelID = ns, model
	row := &in.ReplicaMetrics[0]
	row.Namespace, row.ModelID, row.PodName = ns, model, pod

	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	f.store.FileScaleTarget(ns, model, "v", "H100", 1, capacity.EngineParams{Engine: inferenceengine.EngineVLLM,
		MaxNumSeqs: 64, ModelPath: model, Image: "img:1", ModelConfig: m}, false, replayAt)
	_, err := f.sat.Analyze(context.Background(), in)
	require.NoError(t, err)
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes())
}

// A model config's source can be a local path and its error can quote the
// config's own keys: both are redacted by the export, never passed through.
// The attention kind is the code's word and is kept. The export replays byte
// for byte.
func TestExport_RedactsTheModelConfigsSourceAndError(t *testing.T) {
	read := capacity.ParseModelConfig([]byte(`{"num_hidden_layers": 61, "num_attention_heads": 128, "kv_lora_rank": 512,
		"n_routed_experts": 256, "num_experts_per_tok": 8}`), "/home/payroll/models/acme-llm/config.json")
	require.True(t, read.Read())
	unread := capacity.ParseModelConfig([]byte(`{"payroll_config": {"acme": 1}}`), capacity.ModelConfigFromAnnotation)
	require.Contains(t, unread.Error, "payroll_config", "precondition: the error quotes the config's keys")

	for _, m := range []capacity.ModelConfig{read, unread} {
		raw := modelConfigRecording(t, m)
		require.Contains(t, string(raw), `"ModelConfig":`, "precondition: the recording holds it")

		out, _, err := Export(raw, ExportOptions{Key: []byte("k")})
		require.NoError(t, err)
		for _, secret := range []string{"payroll", "acme", "/home", "config.json"} {
			assert.NotContains(t, string(out), secret, "the export kept a path or a key")
		}
		assert.Contains(t, string(out), `"Source":"redacted"`)
		if m.Read() {
			assert.Contains(t, string(out), `"Attention":"mla"`, "the export changed the attention kind")
		} else {
			assert.Contains(t, string(out), `"Error":"redacted"`)
		}

		r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
		require.NoError(t, err)
		assert.Empty(t, r.Failed())
		assert.Empty(t, r.Mismatches)
		assert.Equal(t, string(out), string(r.Recording), "the export replays byte for byte")
	}
}
