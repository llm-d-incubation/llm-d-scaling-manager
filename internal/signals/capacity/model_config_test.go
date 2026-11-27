package capacity

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/annotations"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// modelConfigDir holds config.json fixtures written with the public Hugging
// Face field names of each family; the file names say what the config is,
// never which model it is, since nothing is read from a name.
const modelConfigDir = "testdata/model-configs"

func readFixture(t *testing.T, name string) ModelConfig {
	t.Helper()
	m, err := ReadModelConfigFile(filepath.Join(modelConfigDir, name))
	require.NoError(t, err)
	require.True(t, m.Read(), "%s: %s", name, m.Error)
	return m
}

// Each fixture gives the attention kind and the experts its config states.
func TestModelConfig_ClassesFromFixtures(t *testing.T) {
	cases := []struct {
		file      string
		attention AttentionKind
		heads     int
		kvHeads   int
		window    int64
		windowed  int
		loraRank  int
		experts   int
		perToken  int
	}{
		// Full attention, grouped-query: 8 KV heads of 64. The config names
		// sliding_window null and use_sliding_window false.
		{file: "dense-full.json", attention: AttentionFull, heads: 64, kvHeads: 8},
		// A window named but switched off is full attention.
		{file: "window-unused.json", attention: AttentionFull, heads: 28, kvHeads: 4},
		// A window on every layer, no per-layer list.
		{file: "sliding-window.json", attention: AttentionSlidingWindow, heads: 32, kvHeads: 8, window: 4096},
		// layer_types alternating: 12 of 24 layers windowed; and 32 local experts.
		{file: "sliding-layer-types-moe.json", attention: AttentionSlidingWindow, heads: 64, kvHeads: 8, window: 128, windowed: 12,
			experts: 32, perToken: 4},
		// A latent KV (kv_lora_rank), and n_routed_experts.
		{file: "mla-moe.json", attention: AttentionMLA, heads: 128, kvHeads: 128, loraRank: 512, experts: 256, perToken: 8},
		// num_experts on full attention.
		{file: "moe-full.json", attention: AttentionFull, heads: 32, kvHeads: 4, experts: 128, perToken: 8},
		// Linear-attention layers hold no KV a token.
		{file: "hybrid-linear.json", attention: AttentionHybrid, heads: 16, kvHeads: 2, experts: 512, perToken: 10},
		// A multimodal config is read from its text_config.
		{file: "multimodal-text-config.json", attention: AttentionSlidingWindow, heads: 16, kvHeads: 8, window: 1024},

		// The public configs below are as published (auto_map and
		// _name_or_path dropped). Each hybrid states its non-attention
		// layers by a key of its own.
		// attn_layer_period 8, mamba_* keys, num_experts 16 (a Jamba).
		{file: "hybrid-attn-layer-period.json", attention: AttentionHybrid, heads: 32, kvHeads: 8, experts: 16, perToken: 2},
		// Attention and SSM in parallel on every layer: mamba_* and ssm_* keys (a Falcon-H1).
		{file: "hybrid-parallel-ssm.json", attention: AttentionHybrid, heads: 8, kvHeads: 2},
		// No attention at all: state_size, ssm_cfg, model_type mamba (a Mamba).
		{file: "state-space.json", attention: AttentionHybrid},
		// attn_type_list with 0 for a linear-attention layer (a MiniMax-Text-01).
		{file: "hybrid-attn-type-list.json", attention: AttentionHybrid, heads: 64, kvHeads: 8, experts: 32, perToken: 2},
		// linear_attn_config beside a kv_lora_rank: hybrid, not MLA (a Kimi-Linear);
		// num_experts_per_token is its spelling.
		{file: "hybrid-linear-latent.json", attention: AttentionHybrid, heads: 32, kvHeads: 32, loraRank: 512, experts: 256, perToken: 8},
		// layer_types naming mamba layers (a Granite 4.0-H).
		{file: "hybrid-layer-types-mamba.json", attention: AttentionHybrid, heads: 32, kvHeads: 8, experts: 72, perToken: 10},
		// hybrid_override_pattern with M (a Nemotron-H).
		{file: "hybrid-override-pattern.json", attention: AttentionHybrid, heads: 40, kvHeads: 8},
		// The language model nested in llm_config (an InternVL); the vision
		// tower's keys are not read.
		{file: "nested-llm-config.json", attention: AttentionFull, heads: 28, kvHeads: 4},
		// thinker_config holding a text_config (an omni model).
		{file: "nested-thinker-text-config.json", attention: AttentionFull, heads: 28, kvHeads: 4},
		// Mixtral: sliding_window null, num_local_experts 8.
		{file: "moe-no-window.json", attention: AttentionFull, heads: 32, kvHeads: 8, experts: 8, perToken: 2},
		// Derived from the Qwen2.5 config: use_sliding_window true and
		// max_window_layers 21: layers 21 to 27 are windowed, 7 of 28.
		{file: "window-from-layer.json", attention: AttentionSlidingWindow, heads: 28, kvHeads: 4, window: 4096, windowed: 7},
	}
	for _, tc := range cases {
		m := readFixture(t, tc.file)
		assert.Equal(t, tc.attention, m.Attention, tc.file)
		assert.Equal(t, tc.heads, m.NumAttentionHeads, tc.file)
		assert.Equal(t, tc.kvHeads, m.NumKeyValueHeads, tc.file)
		assert.Equal(t, tc.window, m.SlidingWindow, tc.file)
		assert.Equal(t, tc.windowed, m.SlidingWindowLayers, tc.file)
		assert.Equal(t, tc.loraRank, m.KVLoraRank, tc.file)
		assert.Equal(t, tc.experts, m.NumExperts, tc.file)
		assert.Equal(t, tc.perToken, m.ExpertsPerToken, tc.file)
		assert.Equal(t, tc.experts > 1, m.MoE(), tc.file)
	}
	assert.Equal(t, 64, readFixture(t, "mla-moe.json").QKRopeHeadDim)
	assert.Equal(t, 128, readFixture(t, "dense-full.json").HeadDim)
}

// A config whose language model sits where this reader does not look names
// what it read through, or the nested configs it left unread, in its error.
func TestModelConfig_NestedConfigsAreNamedInTheError(t *testing.T) {
	m, err := ReadModelConfigFile(filepath.Join(modelConfigDir, "nested-language-config-partial.json"))
	require.NoError(t, err)
	assert.False(t, m.Read())
	assert.Contains(t, m.Error, "read through language_config")

	m = ParseModelConfig([]byte(`{"model_type": "x", "speech_config": {"num_hidden_layers": 4}, "auto_map": {}}`), "f")
	assert.Equal(t, "neither num_hidden_layers nor num_attention_heads; nested configs not read: speech_config", m.Error)

	// A window on every layer from max_window_layers 0, and none where it
	// is the layer count.
	all := ParseModelConfig([]byte(`{"num_hidden_layers": 4, "num_attention_heads": 4, "sliding_window": 64,
		"use_sliding_window": true, "max_window_layers": 0}`), "f")
	assert.Equal(t, AttentionSlidingWindow, all.Attention)
	assert.Equal(t, 4, all.SlidingWindowLayers)
	none := ParseModelConfig([]byte(`{"num_hidden_layers": 4, "num_attention_heads": 4, "sliding_window": 64,
		"use_sliding_window": true, "max_window_layers": 4}`), "f")
	assert.Equal(t, AttentionFull, none.Attention)
}

// The controller logs what the annotation read as once per change, not every
// cycle: the store keeps the last reading per variant.
func TestModelConfig_TheControllerLogsEachChangeOnce(t *testing.T) {
	s := NewStore()
	seen := func() (string, bool) {
		v, ok := s.modelConfigSeen.Load(storeKey("ns", "m", "v"))
		if !ok {
			return "", false
		}
		return v.(string), true
	}
	s.logModelConfig("ns", "m", "v", ModelConfig{})
	_, ok := seen()
	assert.False(t, ok, "a variant that never carried the annotation is not tracked")

	bad := ParseModelConfig([]byte("{"), ModelConfigFromAnnotation)
	s.logModelConfig("ns", "m", "v", bad)
	got, ok := seen()
	require.True(t, ok)
	assert.Equal(t, "unreadable: not a JSON object", got)

	good := readFixture(t, "mla-moe.json")
	s.logModelConfig("ns", "m", "v", good)
	got, _ = seen()
	assert.Equal(t, good.String(), got)

	s.logModelConfig("ns", "m", "v", ModelConfig{})
	got, _ = seen()
	assert.Equal(t, "unknown", got, "a removed annotation is a change too")
}

// A directory is read as a checkpoint: its config.json.
func TestModelConfig_ReadsADirectorysConfig(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join(modelConfigDir, "mla-moe.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600))
	m, err := ReadModelConfigFile(dir)
	require.NoError(t, err)
	assert.Equal(t, AttentionMLA, m.Attention)
	assert.Equal(t, filepath.Join(dir, "config.json"), m.Source)

	_, err = ReadModelConfigFile(filepath.Join(dir, "absent.json"))
	assert.Error(t, err, "a file that is not there is an error, not an unknown class")
}

// A config the reader cannot use says why, and every class is unknown.
func TestModelConfig_UnreadableIsUnknown(t *testing.T) {
	for _, data := range []string{"not json", "[1,2]", "{}", `{"model_type":"x"}`} {
		m := ParseModelConfig([]byte(data), "f")
		assert.False(t, m.Read(), data)
		assert.NotEmpty(t, m.Error, data)
		p := EngineParams{ModelConfig: m}
		assert.Equal(t, AttentionUnknown, p.AttentionKind(), data)
		assert.Equal(t, MoEUnknown, p.MoEClass(), data)
		assert.Equal(t, "unknown", p.AttentionKind().String(), data)
	}
}

// The MoE class reads the config and the expert-parallel flag together
// (issue #503): a MoE model served with tensor parallelism alone is no
// longer dense, and without a config the flag alone says what it said
// before.
func TestModelConfig_MoEClass(t *testing.T) {
	moe, mla, dense := readFixture(t, "moe-full.json"), readFixture(t, "mla-moe.json"), readFixture(t, "dense-full.json")
	cases := []struct {
		name string
		p    EngineParams
		want MoEClass
		text string
	}{
		{"num_experts, no EP", EngineParams{ModelConfig: moe, TensorParallelSize: 4}, MoETensorParallel, "moe-tensor-parallel"},
		{"num_experts, EP", EngineParams{ModelConfig: moe, ExpertParallel: true, DataParallelSize: 4}, MoEExpertParallel,
			"moe-expert-parallel, DP 4"},
		{"n_routed_experts, no EP", EngineParams{ModelConfig: mla, TensorParallelSize: 8}, MoETensorParallel, "moe-tensor-parallel"},
		{"n_routed_experts, EP", EngineParams{ModelConfig: mla, ExpertParallel: true, DataParallelSize: 8}, MoEExpertParallel,
			"moe-expert-parallel, DP 8"},
		{"dense", EngineParams{ModelConfig: dense}, MoEDense, "dense"},
		// The flag on a dense model moves no expert: dense.
		{"dense, EP flag", EngineParams{ModelConfig: dense, ExpertParallel: true}, MoEDense, "dense"},
		// No config: the flag alone, as before; nothing else is called dense.
		{"no config, EP", EngineParams{ExpertParallel: true}, MoEExpertParallel, "moe-expert-parallel, DP 1"},
		{"no config", EngineParams{}, MoEUnknown, "unknown"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.p.MoEClass(), tc.name)
		assert.Equal(t, tc.text, tc.p.MoEText(), tc.name)
	}
	var nilParams *EngineParams
	assert.Equal(t, MoEUnknown, nilParams.MoEClass())
	assert.Equal(t, AttentionUnknown, nilParams.AttentionKind())
}

// The controller reads the config from the scale target's annotation, along
// the path every engine parameter is parsed on; no annotation is unknown, as
// before.
func TestModelConfig_FromTheScaleTargetAnnotation(t *testing.T) {
	cfg, err := os.ReadFile(filepath.Join(modelConfigDir, "mla-moe.json"))
	require.NoError(t, err)
	st := func(annots map[string]string) scaletarget.ScaleTargetAccessor {
		return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Annotations: annots},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "vllm", Image: "vllm/vllm-openai:v0.11.0",
				Command: []string{"vllm", "serve", "m", "--tensor-parallel-size", "8"},
			}}}}},
		})
	}
	for _, engine := range []inferenceengine.Engine{inferenceengine.EngineVLLM, inferenceengine.EngineSGLang} {
		p := ParseEngineArgsOn(engine, st(map[string]string{annotations.ModelConfig: string(cfg)}), "")
		assert.Equal(t, ModelConfigFromAnnotation, p.ModelConfig.Source, engine)
		assert.Equal(t, AttentionMLA, p.AttentionKind(), engine)
		assert.Equal(t, MoETensorParallel, p.MoEClass(), engine)

		bare := ParseEngineArgsOn(engine, st(nil), "")
		assert.Equal(t, ModelConfig{}, bare.ModelConfig, engine)
		assert.Equal(t, AttentionUnknown, bare.AttentionKind(), engine)
		assert.Equal(t, MoEUnknown, bare.MoEClass(), engine)

		bad := ParseEngineArgsOn(engine, st(map[string]string{annotations.ModelConfig: "{"}), "")
		assert.Equal(t, ModelConfigFromAnnotation, bad.ModelConfig.Source, engine)
		assert.NotEmpty(t, bad.ModelConfig.Error, engine)
		assert.Equal(t, AttentionUnknown, bad.AttentionKind(), engine)
	}
	// The config moves no digest: the checkpoint decides it.
	with := ParseEngineArgs(inferenceengine.EngineVLLM, st(map[string]string{annotations.ModelConfig: string(cfg)}))
	without := ParseEngineArgs(inferenceengine.EngineVLLM, st(nil))
	assert.Equal(t, without.Fingerprint(), with.Fingerprint())
	assert.True(t, with.IsCapacityCompatible(&without))
}

// A config read is written into a recording and read back as it was; one
// with none read writes no ModelConfig key at all.
func TestModelConfig_RecordingRoundTrip(t *testing.T) {
	p := EngineParams{Engine: inferenceengine.EngineVLLM, ModelPath: "m", ModelConfig: readFixture(t, "sliding-layer-types-moe.json")}
	b, err := learnrec.Marshal(p)
	require.NoError(t, err)
	var back EngineParams
	require.NoError(t, learnrec.Unmarshal(b, &back))
	assert.Equal(t, p, back)

	b, err = learnrec.Marshal(EngineParams{Engine: inferenceengine.EngineVLLM})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "ModelConfig")
}

// goldenRecordings are the learner replay's golden recordings, every one made
// before the model config was read.
const goldenRecordings = "../../engines/analyzers/learnreplay/testdata/goldens"

// Every EngineParams a golden recording holds, all made before the model
// config was read, reads back with every recorded field written again byte
// for byte and no ModelConfig key added. Fields older than this change that
// carry no omitzero tag (VLLMVersion and the like) are written where the
// recording left them out, as they were before it; they are logged, not
// asserted, since this change does not touch them.
func TestModelConfig_OldRecordingsReadBackWithoutTheField(t *testing.T) {
	const perRecording = 40
	files, err := filepath.Glob(filepath.Join(goldenRecordings, "*", "recording.jsonl.gz"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	total := 0
	added := map[string]bool{}
	for _, f := range files {
		n := 0
		// The first ones of each recording: the rest repeat them, and a full
		// walk takes a minute.
		eachEngineParams(t, f, func(raw json.RawMessage) bool {
			if n++; n > perRecording {
				return false
			}
			total++
			require.NotContains(t, string(raw), `"ModelConfig"`, f)
			var p EngineParams
			require.NoError(t, learnrec.Unmarshal(raw, &p), f)
			require.Zero(t, p.ModelConfig, f)
			again, err := learnrec.Marshal(p)
			require.NoError(t, err, f)
			var before, after map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &before), f)
			require.NoError(t, json.Unmarshal(again, &after), f)
			for k, v := range before {
				require.Equal(t, string(v), string(after[k]), "%s: %s is not written again byte for byte", f, k)
			}
			for k := range after {
				if _, ok := before[k]; !ok {
					added[k] = true
				}
			}
			return true
		})
	}
	// Not an empty pass: the goldens hold engine parameters, and the walk read them.
	require.Greater(t, total, 500, "the walk read only %d engine parameters", total)
	assert.NotContains(t, added, "ModelConfig", "a zero ModelConfig is written into an old recording")
	t.Logf("%d engine parameters across %d recordings; fields older than this change written where left out: %v",
		total, len(files), slices.Sorted(maps.Keys(added)))
}

// eachEngineParams calls fn with the raw bytes of every "EngineParams" object
// in the gzipped recording at path, at any depth, until fn returns false.
func eachEngineParams(t *testing.T, path string, fn func(json.RawMessage) bool) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<28)
	more := true
	var walk func(raw json.RawMessage)
	walk = func(raw json.RawMessage) {
		if !more {
			return
		}
		s := strings.TrimSpace(string(raw))
		switch {
		case strings.HasPrefix(s, "{"):
			var obj map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &obj), path)
			for k, v := range obj {
				if k == "EngineParams" && strings.HasPrefix(strings.TrimSpace(string(v)), "{") {
					more = more && fn(v)
					continue
				}
				walk(v)
			}
		case strings.HasPrefix(s, "["):
			var list []json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &list), path)
			for _, v := range list {
				walk(v)
			}
		}
	}
	for more && sc.Scan() {
		walk(json.RawMessage(append([]byte{}, sc.Bytes()...)))
	}
	require.NoError(t, sc.Err(), path)
}
