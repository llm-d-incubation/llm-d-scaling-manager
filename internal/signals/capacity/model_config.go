package capacity

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/annotations"
)

// AttentionKind is how a model's layers attend over the context, as its
// config.json states it (issue llm-d-incubation/llm-d-scaling-manager#501).
// The zero value is unknown: no config was read.
type AttentionKind string

const (
	// AttentionUnknown is a model whose config was not read.
	AttentionUnknown AttentionKind = ""
	// AttentionFull is every layer attending over the whole context, with
	// num_key_value_heads heads of KV a token (grouped-query or multi-head).
	AttentionFull AttentionKind = "full"
	// AttentionSlidingWindow is some or all layers attending over a window
	// of the last tokens only (sliding_window, layer_types naming
	// sliding_attention, attention_chunk_size): the KV such a layer holds is
	// capped by the window, not the context.
	AttentionSlidingWindow AttentionKind = "sliding-window"
	// AttentionMLA is multi-head latent attention (kv_lora_rank): one
	// compressed latent of kv_lora_rank + qk_rope_head_dim a token, whatever
	// the head count.
	AttentionMLA AttentionKind = "mla"
	// AttentionHybrid is a model with layers that hold no KV a token at all
	// (layer_types naming linear attention or a state-space layer,
	// layers_block_type naming mamba, a hybrid_override_pattern with M).
	AttentionHybrid AttentionKind = "hybrid"
)

// String is the kind's name, "unknown" for the zero value.
func (k AttentionKind) String() string {
	if k == AttentionUnknown {
		return unknownClass
	}
	return string(k)
}

// MoEClass is whether a model routes its tokens through experts, and how the
// engine spreads them (issue llm-d-incubation/llm-d-scaling-manager#503).
// The zero value is unknown.
type MoEClass string

const (
	// MoEUnknown is no config read and no expert-parallel flag.
	MoEUnknown MoEClass = ""
	// MoEDense is a model the config states has no routed experts.
	MoEDense MoEClass = "dense"
	// MoETensorParallel is a MoE model served without expert parallelism:
	// each rank holds a slice of every expert.
	MoETensorParallel MoEClass = "moe-tensor-parallel"
	// MoEExpertParallel is a MoE model served with expert parallelism: each
	// rank holds whole experts, and the data-parallel ranks step together.
	MoEExpertParallel MoEClass = "moe-expert-parallel"
)

// String is the class's name, "unknown" for the zero value.
func (c MoEClass) String() string {
	if c == MoEUnknown {
		return unknownClass
	}
	return string(c)
}

// unknownClass is how a class nothing read is named.
const unknownClass = "unknown"

// Where a ModelConfig was read from (ModelConfig.Source) when the scaling
// manager reads it itself. A caller that reads the file names its own source,
// as the capacity planner names its --model-config path.
const (
	// ModelConfigFromAnnotation is the scaling-manager.llmd.ai/model-config
	// annotation on the scale target.
	ModelConfigFromAnnotation = "annotation"
)

// maxModelConfigBytes is the most of a config.json the reader takes: the
// largest public configs are a few KiB, and an annotation is capped at
// 256 KiB anyway.
const maxModelConfigBytes = 1 << 20

// ModelConfig is what a model's Hugging Face config.json says of its
// attention and its experts: the facts the attention kind and the MoE class
// are read from, kept beside the engine's arguments (EngineParams.ModelConfig).
// The zero value is no config read, and every class is then unknown.
//
// The fields are the config's own, under their public names. A multimodal
// config's text_config is read in place of its top level, where the language
// model's are. Nothing is read from a model's name.
//
// Read for the capacity planner's report and the controller's engine
// parameters; not priced yet, not hashed into the fingerprint and not
// compared for capacity: the checkpoint (ModelPath, ModelRevision) that
// decides it is.
type ModelConfig struct {
	// Source is where the config was read from: ModelConfigFromAnnotation,
	// or what the caller that read it names (a file path). Empty is no
	// config, and Error says why one that was given could not be read.
	Source string
	Error  string `learnrec:"omitzero"`

	// Attention is the kind detected; AttentionUnknown where Error is set.
	Attention AttentionKind `learnrec:"omitzero"`
	// NumLayers is num_hidden_layers.
	NumLayers int `learnrec:"omitzero"`
	// NumAttentionHeads is num_attention_heads, and NumKeyValueHeads
	// num_key_value_heads (num_attention_heads where the config leaves it
	// out, multi-head attention). HeadDim is head_dim, else hidden_size over
	// num_attention_heads.
	NumAttentionHeads int `learnrec:"omitzero"`
	NumKeyValueHeads  int `learnrec:"omitzero"`
	HeadDim           int `learnrec:"omitzero"`
	// SlidingWindow is the window in tokens of a sliding-window layer
	// (sliding_window, or attention_chunk_size for chunked local
	// attention), and SlidingWindowLayers how many layers attend over it;
	// 0 where the config does not say per layer (layer_types).
	SlidingWindow       int64 `learnrec:"omitzero"`
	SlidingWindowLayers int   `learnrec:"omitzero"`
	// KVLoraRank and QKRopeHeadDim are an MLA model's latent KV: kv_lora_rank
	// and qk_rope_head_dim.
	KVLoraRank    int `learnrec:"omitzero"`
	QKRopeHeadDim int `learnrec:"omitzero"`
	// NumExperts is the routed experts (num_experts, n_routed_experts or
	// num_local_experts), 0 for a dense model, and ExpertsPerToken
	// num_experts_per_tok.
	NumExperts      int `learnrec:"omitzero"`
	ExpertsPerToken int `learnrec:"omitzero"`
}

// Read reports whether a config was read: given and understood.
func (m ModelConfig) Read() bool {
	return m.Source != "" && m.Error == ""
}

// MoE reports whether the config states routed experts.
func (m ModelConfig) MoE() bool {
	return m.Read() && m.NumExperts > 1
}

// String is the classes in words: "mla attention, 256 experts (8 a token)".
func (m ModelConfig) String() string {
	if !m.Read() {
		if m.Error != "" {
			return "unreadable: " + m.Error
		}
		return unknownClass
	}
	var b strings.Builder
	b.WriteString(m.AttentionText())
	if m.NumExperts > 1 {
		fmt.Fprintf(&b, ", %d experts", m.NumExperts)
		if m.ExpertsPerToken > 0 {
			fmt.Fprintf(&b, " (%d a token)", m.ExpertsPerToken)
		}
	} else {
		b.WriteString(", dense")
	}
	return b.String()
}

// AttentionText is the attention kind with what it rests on:
// "sliding-window attention (4096 tokens on 24 of 48 layers)".
func (m ModelConfig) AttentionText() string {
	kind := m.Attention.String() + " attention"
	switch m.Attention {
	case AttentionMLA:
		return fmt.Sprintf("%s (kv_lora_rank %d)", kind, m.KVLoraRank)
	case AttentionSlidingWindow:
		if m.SlidingWindowLayers > 0 && m.NumLayers > 0 {
			return fmt.Sprintf("%s (%d tokens on %d of %d layers)", kind, m.SlidingWindow, m.SlidingWindowLayers, m.NumLayers)
		}
		return fmt.Sprintf("%s (%d tokens)", kind, m.SlidingWindow)
	case AttentionFull:
		if m.NumKeyValueHeads > 0 {
			return fmt.Sprintf("%s (%d KV heads of %d)", kind, m.NumKeyValueHeads, m.NumAttentionHeads)
		}
	}
	return kind
}

// AttentionKind is the attention kind of the model p serves, from its config;
// AttentionUnknown where none was read.
func (p *EngineParams) AttentionKind() AttentionKind {
	if p == nil || !p.ModelConfig.Read() {
		return AttentionUnknown
	}
	return p.ModelConfig.Attention
}

// MoEClass is the MoE class of the engine p describes: dense, or MoE with or
// without expert parallelism, from the model's config and the engine's
// expert-parallel flag. Without a config, the flag alone still says
// MoEExpertParallel, as it did before the config was read, and anything else
// is MoEUnknown: a MoE model served with tensor parallelism alone is not
// called dense from the absence of a flag.
func (p *EngineParams) MoEClass() MoEClass {
	switch {
	case p == nil:
		return MoEUnknown
	case p.ModelConfig.Read() && !p.ModelConfig.MoE():
		return MoEDense
	case p.ModelConfig.Read() && !p.ExpertParallel:
		return MoETensorParallel
	case p.ExpertParallel:
		return MoEExpertParallel
	}
	return MoEUnknown
}

// MoEText is MoEClass in words, with the data-parallel size the expert
// parallelism spans: "moe-expert-parallel, DP 8".
func (p *EngineParams) MoEText() string {
	c := p.MoEClass()
	if c == MoEExpertParallel {
		return fmt.Sprintf("%s, DP %d", c, parallelSize(p.DataParallelSize))
	}
	return c.String()
}

// ReadModelConfigFile reads a model's config.json at path, or the
// config.json inside path where it is a directory (a downloaded checkpoint),
// naming path as the source. An unreadable file is an error; a file that is
// read but is no model config gives a ModelConfig with Error set.
func ReadModelConfigFile(path string) (ModelConfig, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "config.json")
	}
	f, err := os.Open(path)
	if err != nil {
		return ModelConfig{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxModelConfigBytes+1))
	if err != nil {
		return ModelConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxModelConfigBytes {
		return ModelConfig{}, fmt.Errorf("%s: larger than %d bytes, not a model config", path, maxModelConfigBytes)
	}
	return ParseModelConfig(data, path), nil
}

// modelConfigOf is the config the scaling-manager.llmd.ai/model-config
// annotation carries; the zero value with no annotation.
func modelConfigOf(annots map[string]string) ModelConfig {
	v, ok := annots[annotations.ModelConfig]
	if !ok {
		return ModelConfig{}
	}
	if len(v) > maxModelConfigBytes {
		return ModelConfig{Source: ModelConfigFromAnnotation, Error: "larger than a model config"}
	}
	return ParseModelConfig([]byte(v), ModelConfigFromAnnotation)
}

// ParseModelConfig reads the attention kind and the experts from a model's
// config.json, naming source as where it came from. It never fails: a config
// it cannot read gives Error, and every class unknown.
func ParseModelConfig(data []byte, source string) ModelConfig {
	m := ModelConfig{Source: source}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		m.Error = "not a JSON object"
		return m
	}
	c, through := languageModelConfig(top)
	layers, hasLayers := intKey(c, "num_hidden_layers")
	heads, hasHeads := intKey(c, "num_attention_heads")
	if !hasLayers && !hasHeads {
		m.Error = "neither num_hidden_layers nor num_attention_heads"
		if len(through) > 0 {
			m.Error += " (read through " + strings.Join(through, ", ") + ")"
		} else if objs := nestedObjects(top); len(objs) > 0 {
			m.Error += "; nested configs not read: " + strings.Join(objs, ", ")
		}
		return m
	}
	m.NumLayers, m.NumAttentionHeads = int(layers), int(heads)
	m.NumKeyValueHeads = m.NumAttentionHeads
	if kv, ok := intKey(c, "num_key_value_heads"); ok {
		m.NumKeyValueHeads = int(kv)
	}
	if d, ok := intKey(c, "head_dim"); ok {
		m.HeadDim = int(d)
	} else if h, ok := intKey(c, "hidden_size"); ok && heads > 0 {
		m.HeadDim = int(h / heads)
	}
	for _, key := range []string{"num_experts", "n_routed_experts", "num_local_experts"} {
		if n, ok := intKey(c, key); ok && n > 0 {
			m.NumExperts = int(n)
			break
		}
	}
	// num_experts_per_token is Kimi-Linear's spelling.
	for _, key := range []string{"num_experts_per_tok", "num_experts_per_token"} {
		if k, ok := intKey(c, key); ok {
			m.ExpertsPerToken = int(k)
			break
		}
	}
	m.Attention = attentionOf(c, &m)
	return m
}

// languageModelKeys are where a multimodal config nests its language model:
// text_config (most), llm_config (InternVL), language_config (DeepSeek-VL2),
// and thinker_config (an omni model's thinker, itself holding a
// text_config). The first found is read, and then its own nesting.
var languageModelKeys = []string{"text_config", "llm_config", "language_config", "thinker_config"}

// languageModelConfig is the config the language model is described by: top
// with each nested language-model config's keys over it, innermost winning,
// and the keys it was read through, outermost first.
func languageModelConfig(top map[string]any) (map[string]any, []string) {
	c := top
	var through []string
	for range languageModelKeys {
		var next map[string]any
		key := ""
		for _, k := range languageModelKeys {
			if obj, ok := c[k].(map[string]any); ok {
				next, key = obj, k
				break
			}
		}
		if next == nil {
			break
		}
		merged := make(map[string]any, len(c)+len(next))
		for k, v := range c {
			if !slices.Contains(languageModelKeys, k) {
				merged[k] = v
			}
		}
		for k, v := range next {
			merged[k] = v
		}
		c, through = merged, append(through, key)
	}
	return c, through
}

// nestedObjects is the keys of top whose values are objects, sorted: what an
// error names when no attention fields were found at the top level.
func nestedObjects(top map[string]any) []string {
	var out []string
	for k, v := range top {
		if _, ok := v.(map[string]any); ok && k != "auto_map" && k != "quantization_config" {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// hybridModelTypes are model_type values whose layers are not all attention:
// state-space, linear-attention and parallel attention-plus-SSM families. A
// model_type is data the config states, not the model's name; it backs up the
// keys hasHybridLayers reads, for a family whose other keys it does not know.
var hybridModelTypes = []string{"mamba", "mamba2", "falcon_mamba", "falcon_h1", "jamba", "bamba", "zamba", "zamba2",
	"nemotron_h", "granitemoehybrid", "plamo2", "lfm2", "qwen3_next", "minimax", "minimax_text_01", "minimax_m1",
	"kimi_linear", "rwkv", "rwkv7", "recurrent_gemma"}

// hasHybridLayers reports whether c states layers that hold no KV a token, or
// a recurrent state beside it, by any key a public hybrid config uses:
//   - layer_types naming a layer that is not attention (linear_attention,
//     mamba: Qwen3-Next, Granite 4.0-H), or layers_block_type naming one;
//   - hybrid_override_pattern with an M (Nemotron-H);
//   - attn_layer_period above 1 (Jamba: attention every period-th layer);
//   - attn_type_list naming a 0, a linear-attention layer (MiniMax);
//   - linear_attn_config, set (Kimi-Linear);
//   - a mamba_ or ssm_ key, state_size or ssm_cfg (Mamba, Falcon-H1);
//   - a model_type of hybridModelTypes.
func hasHybridLayers(c map[string]any) bool {
	for _, t := range stringList(c["layer_types"]) {
		switch t {
		case "full_attention", "attention", "sliding_attention", "chunked_attention":
		default:
			return true
		}
	}
	for _, t := range stringList(c["layers_block_type"]) {
		if t != "attention" {
			return true
		}
	}
	if p, ok := c["hybrid_override_pattern"].(string); ok && strings.Contains(p, "M") {
		return true
	}
	if n, ok := intKey(c, "attn_layer_period"); ok && n > 1 {
		return true
	}
	if list, ok := c["attn_type_list"].([]any); ok {
		for _, e := range list {
			if f, ok := e.(float64); ok && f == 0 {
				return true
			}
		}
	}
	if v, ok := c["linear_attn_config"]; ok && v != nil {
		return true
	}
	for k, v := range c {
		if v != nil && (strings.HasPrefix(k, "mamba_") || strings.HasPrefix(k, "ssm_") || k == "state_size" || k == "ssm_cfg") {
			return true
		}
	}
	mt, _ := c["model_type"].(string)
	return slices.Contains(hybridModelTypes, mt)
}

// attentionOf is the attention kind c states, filling the window and the
// latent rank in m. Layers that hold no KV a token are read first: a hybrid
// may carry a latent KV on its attention layers too (Kimi-Linear has a
// kv_lora_rank), and what its cache holds is then not MLA's alone. MLA comes
// next, then a window, and a config that states none of them is full
// attention.
//
// A window is read as vLLM reads it. With layer_types, the layers it names
// sliding_attention are windowed. Without, a sliding_window applies to every
// layer: vLLM sets the cache's window for the whole model, so Mistral-7B v0.1
// is served with a 4096-token window on all 32 layers. Not where
// use_sliding_window is false (Qwen2, whose window is named and unused); and
// where it is true with a max_window_layers, the layers from that index up are
// windowed, as transformers' Qwen2 config builds its layer_types. vLLM's
// --disable-sliding-window is not read from the engine's flags: an engine
// started with it is still reported windowed.
func attentionOf(c map[string]any, m *ModelConfig) AttentionKind {
	if r, ok := intKey(c, "kv_lora_rank"); ok && r > 0 {
		m.KVLoraRank = int(r)
		if d, ok := intKey(c, "qk_rope_head_dim"); ok {
			m.QKRopeHeadDim = int(d)
		}
	}
	if hasHybridLayers(c) {
		return AttentionHybrid
	}
	if m.KVLoraRank > 0 {
		return AttentionMLA
	}
	types := stringList(c["layer_types"])
	sliding := 0
	for _, t := range types {
		if t == "sliding_attention" || t == "chunked_attention" {
			sliding++
		}
	}
	window, hasWindow := intKey(c, "sliding_window")
	use, useStated := c["use_sliding_window"].(bool)
	if useStated && !use {
		hasWindow = false
	}
	if !hasWindow || window <= 0 {
		window, hasWindow = intKey(c, "attention_chunk_size")
	}
	if len(types) == 0 && use && hasWindow && window > 0 && m.NumLayers > 0 {
		if from, ok := intKey(c, "max_window_layers"); ok {
			if sliding = max(m.NumLayers-int(from), 0); sliding == 0 {
				return AttentionFull
			}
		}
	}
	switch {
	case len(types) > 0 && sliding == 0:
		return AttentionFull
	case hasWindow && window > 0:
		m.SlidingWindow, m.SlidingWindowLayers = window, sliding
		return AttentionSlidingWindow
	case sliding > 0:
		// layer_types names sliding layers but no window size.
		m.SlidingWindowLayers = sliding
		return AttentionSlidingWindow
	}
	return AttentionFull
}

// intKey is c[key] as a whole number: absent, null, fractional or out of
// range is not ok.
func intKey(c map[string]any, key string) (int64, bool) {
	f, ok := c[key].(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// stringList is v as a list of strings; nil where it is not one.
func stringList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}
