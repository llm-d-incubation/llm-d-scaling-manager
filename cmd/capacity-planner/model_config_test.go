package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// modelConfigs are the reader's config.json fixtures.
const modelConfigs = "../../internal/signals/capacity/testdata/model-configs"

func fixtureConfig(t *testing.T, name string) capacity.ModelConfig {
	t.Helper()
	m, err := capacity.ReadModelConfigFile(filepath.Join(modelConfigs, name))
	require.NoError(t, err)
	require.True(t, m.Read(), m.Error)
	return m
}

// A config read names the attention kind in the engine notes in place of the
// model's name: a full-attention config clears what the name said, and an
// MLA or a sliding-window config flags a model whose name says nothing. With
// no config the name is read, as before (TestEngineNotes_EachUnmodeledConfiguration).
func TestEngineNotes_TheModelConfigWinsOverTheName(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		config string
		want   []whatif.Flag
		detail string
	}{
		{"a full-attention config under an MLA family's name", "deepseek-ai/DeepSeek-V3", "dense-full.json", nil, ""},
		// A config never clears a hybrid the name says: the note stays, and
		// says the config reads otherwise.
		{"a full-attention config under a hybrid family's name", "openai/gpt-oss-120b", "dense-full.json",
			[]whatif.Flag{hybridAttention}, "gpt-oss by its name, though the model config reads full attention " +
				"(8 KV heads of 64): KV per request and ITL are not those of full attention"},
		// A hybrid the reader now knows, under a plain name.
		{"a Jamba-style config under a plain name", "org/model", "hybrid-attn-layer-period.json", []whatif.Flag{hybridAttention},
			"hybrid attention: KV per request and ITL are not those of full attention"},
		{"an MLA config under a plain name", "org/model", "mla-moe.json", []whatif.Flag{mlaAttention},
			"mla attention (kv_lora_rank 512): the KV binds"},
		{"a windowed config under a plain name", "org/model", "sliding-layer-types-moe.json", []whatif.Flag{hybridAttention},
			"sliding-window attention (128 tokens on 12 of 24 layers): KV per request and ITL are not those of full attention"},
		{"a linear-attention config", "org/model", "hybrid-linear.json", []whatif.Flag{hybridAttention},
			"hybrid attention: KV per request and ITL are not those of full attention"},
	}
	for _, c := range cases {
		pv := denseVariant()
		p := *pv.state.Params
		p.ModelPath, pv.model = c.model, c.model
		p.ModelConfig = fixtureConfig(t, c.config)
		pv.state.Params = &p
		got := flagsOf(pv)
		if c.want == nil {
			assert.Empty(t, got, c.name)
			continue
		}
		require.Equal(t, c.want, got, c.name)
		pv.notes = pv.engineNotes()
		assert.Equal(t, c.detail, pv.engineNoteText(c.want[0]), c.name)
	}
}

// The flags a config raises are what the envelope judges, and no row allows
// either: a figure inside a row reads outside once it carries one. That is
// how --model-config moves a verdict and never a value.
func TestModelConfig_TheFlagsItRaisesMoveTheVerdict(t *testing.T) {
	in := whatif.Figure{Kind: whatif.FigureTTFT, Percentile: 0.9, Class: q32ClassB(), KneeShare: 0.8}
	require.True(t, whatif.DefaultEnvelope().Judge(in).Validated(), "precondition: bucket B's class is inside")
	for _, fl := range []whatif.Flag{mlaAttention, hybridAttention} {
		f := in
		f.Flags = []whatif.Flag{fl}
		v := whatif.DefaultEnvelope().Judge(f)
		assert.False(t, v.Validated(), fl)
		assert.Contains(t, v.String(), string(fl), fl)
	}
}

// The engine row names both classes where a config was read, and nothing new
// where none was, so a recording without one prints the row it did.
func TestEngine_TheModelClassesAreNamed(t *testing.T) {
	p := denseVariant().state.Params
	e := engineInfo(p)
	assert.Equal(t, "unknown", e.Attention)
	assert.Equal(t, "unknown", e.MoE)
	assert.Empty(t, e.Model)
	before := engineInfoText(e)
	assert.NotContains(t, before, "model ")

	q := *p
	q.ModelConfig = fixtureConfig(t, "moe-full.json")
	q.TensorParallelSize = 4
	e = engineInfo(&q)
	assert.Equal(t, "full", e.Attention)
	assert.Equal(t, "moe-tensor-parallel", e.MoE)
	assert.Equal(t, filepath.Join(modelConfigs, "moe-full.json"), e.ModelConfigSource)
	assert.Equal(t, before+"; model full attention (4 KV heads of 32), moe-tensor-parallel (model config "+
		filepath.Join(modelConfigs, "moe-full.json")+"), for reference, not priced yet", engineInfoText(e))

	q.ExpertParallel, q.DataParallelSize = true, 8
	assert.Equal(t, "moe-expert-parallel", engineInfo(&q).MoE)

	// No config, but the expert-parallel flag: MoE, as the flag said before.
	r := *p
	r.ExpertParallel, r.DataParallelSize = true, 4
	e = engineInfo(&r)
	assert.Equal(t, "unknown", e.Attention)
	assert.Equal(t, "moe-expert-parallel", e.MoE)
	assert.Contains(t, engineInfoText(e), "model unknown attention, moe-expert-parallel, DP 4 (from --enable-expert-parallel; no model config)")
}

// The config fills the variants whose recording holds none, keeps the one a
// recording holds (and says when the two disagree), and leaves a variant
// with no engine parameters without any.
func TestModelConfig_AppliedToARecordingsVariants(t *testing.T) {
	mla := fixtureConfig(t, "mla-moe.json")
	dense := fixtureConfig(t, "dense-full.json")
	recorded := &capacity.EngineParams{ModelConfig: dense}
	unread := &capacity.EngineParams{ModelPath: "m"}
	st := &learnreplay.State{Variants: []learnreplay.VariantState{
		{Variant: "a", Params: unread}, {Variant: "b", Params: recorded}, {Variant: "c"},
	}}
	var warn bytes.Buffer
	applyModelConfig(st, mla, &warn)
	assert.Equal(t, mla, st.Variants[0].Params.ModelConfig)
	assert.Equal(t, capacity.ModelConfig{}, unread.ModelConfig, "the recorded parameters are copied, not changed")
	assert.Equal(t, dense, st.Variants[1].Params.ModelConfig)
	assert.Contains(t, warn.String(), "variant b: the recording's model config")
	assert.Nil(t, st.Variants[2].Params)

	warn.Reset()
	applyModelConfig(st, capacity.ModelConfig{}, &warn)
	assert.Empty(t, warn.String(), "no config: nothing applied, nothing said")
}

// --model-config wins; else the config.json beside the recording is read;
// one that is no model config is warned of and left out; the server, whose
// recordings share a directory, reads nothing beside them.
func TestModelConfig_WhereAnAnswerReadsIt(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "recording.jsonl.gz")
	cfg, err := os.ReadFile(filepath.Join(modelConfigs, "mla-moe.json"))
	require.NoError(t, err)

	var warn bytes.Buffer
	f := answerFlags{recording: rec, warn: &warn}
	m, err := f.answerModelConfig()
	require.NoError(t, err)
	assert.Equal(t, capacity.ModelConfig{}, m, "nothing beside the recording")

	beside := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(beside, cfg, 0o600))
	m, err = f.answerModelConfig()
	require.NoError(t, err)
	assert.Equal(t, beside, m.Source)
	assert.Equal(t, capacity.AttentionMLA, m.Attention)

	f.regularOnly = true
	m, err = f.answerModelConfig()
	require.NoError(t, err)
	assert.Equal(t, capacity.ModelConfig{}, m, "the server reads nothing beside its recordings")
	f.regularOnly = false

	f.window.modelConfig = filepath.Join(modelConfigs, "dense-full.json")
	m, err = f.answerModelConfig()
	require.NoError(t, err)
	assert.Equal(t, capacity.AttentionFull, m.Attention, "the flag wins over the file beside")

	f.window.modelConfig = filepath.Join(dir, "absent.json")
	_, err = f.answerModelConfig()
	assert.Equal(t, 2, exitCode(err), "a --model-config that cannot be read is the caller's input")
	f.window.modelConfig = rec
	require.NoError(t, os.WriteFile(rec, []byte("[]"), 0o600))
	_, err = f.answerModelConfig()
	assert.Equal(t, 2, exitCode(err), "a --model-config that is no model config is the caller's input")

	f.window.modelConfig = ""
	require.NoError(t, os.WriteFile(beside, []byte(`{"kit": "settings"}`), 0o600))
	m, err = f.answerModelConfig()
	require.NoError(t, err)
	assert.Equal(t, capacity.ModelConfig{}, m)
	assert.Contains(t, warn.String(), "is not a model config")
}

// End to end on a golden recording: the classes are named in the JSON and
// the text, and no figure moves. The recording's model has an anonymous name,
// so no engine note is raised either way and the two outputs differ in the
// engine's model fields alone.
func TestModelConfig_NamedAndNoFigureMoves(t *testing.T) {
	args := []string{"replicas-to-slo", "--recording", olsSweep, "--model", "model-02", "--tpot-percentile", "90",
		"--assume-tpot-ratio", "1.3", "-o", "json", "--assume-input-tokens", "1000", "--assume-output-tokens", "1000"}
	without, code := runCmd(t, args...)
	require.Equal(t, 0, code, without)
	cfg := filepath.Join(modelConfigs, "dense-full.json")
	with, code := runCmd(t, append(args, "--"+modelConfigFlag, cfg)...)
	require.Equal(t, 0, code, with)

	assert.Equal(t, []string{"unknown/unknown"}, engineClasses(t, without))
	assert.Equal(t, []string{"full/dense"}, engineClasses(t, with))
	assert.Equal(t, strippedModelFields(t, without), strippedModelFields(t, with))

	text, code := runCmd(t, "replicas-to-slo", "--recording", olsSweep, "--model", "model-02",
		"--"+modelConfigFlag, filepath.Join(modelConfigs, "mla-moe.json"))
	require.Equal(t, 0, code, text)
	assert.Contains(t, text, "model mla attention (kv_lora_rank 512), moe-tensor-parallel (model config")
}

// engineClasses is every engine object's attention/moe in the JSON answer
// out, each distinct pair once, in the order met.
func engineClasses(t *testing.T, out string) []string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	var got []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if _, isEngine := x["stepBudgetSource"]; isEngine {
				pair := fmt.Sprintf("%v/%v", x["attention"], x["moe"])
				if !slices.Contains(got, pair) {
					got = append(got, pair)
				}
			}
			for _, k := range slices.Sorted(maps.Keys(x)) {
				walk(x[k])
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return got
}

// strippedModelFields is the JSON answer out with every engine's model
// fields removed, re-encoded.
func strippedModelFields(t *testing.T, out string) string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(out), &v))
	var strip func(any)
	strip = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range []string{"attention", "moe", "modelConfigSource", "model"} {
				if _, isEngine := x["stepBudgetSource"]; isEngine {
					delete(x, k)
				}
			}
			for _, e := range x {
				strip(e)
			}
		case []any:
			for _, e := range x {
				strip(e)
			}
		}
	}
	strip(v)
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(b), `"moe"`), "an engine's moe field survived the strip")
	return string(b)
}
