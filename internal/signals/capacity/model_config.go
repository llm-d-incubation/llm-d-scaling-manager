package capacity

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
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
	// A multimodal model's language model is described in text_config; the
	// keys there win over the top level's.
	c := top
	if text, ok := top["text_config"].(map[string]any); ok {
		c = make(map[string]any, len(top)+len(text))
		for k, v := range top {
			c[k] = v
		}
		for k, v := range text {
			c[k] = v
		}
	}
	layers, hasLayers := intKey(c, "num_hidden_layers")
	heads, hasHeads := intKey(c, "num_attention_heads")
	if !hasLayers && !hasHeads {
		m.Error = "neither num_hidden_layers nor num_attention_heads"
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
	if k, ok := intKey(c, "num_experts_per_tok"); ok {
		m.ExpertsPerToken = int(k)
	}
	m.Attention = attentionOf(c, &m)
	return m
}

// attentionOf is the attention kind c states, filling the window and the
// latent rank in m. MLA is read first: a latent KV is what the cache holds
// whatever else a layer does. A layer that holds no KV comes next, then a
// window, and a config that states none of them is full attention.
func attentionOf(c map[string]any, m *ModelConfig) AttentionKind {
	if r, ok := intKey(c, "kv_lora_rank"); ok && r > 0 {
		m.KVLoraRank = int(r)
		if d, ok := intKey(c, "qk_rope_head_dim"); ok {
			m.QKRopeHeadDim = int(d)
		}
		return AttentionMLA
	}
	types := stringList(c["layer_types"])
	sliding := 0
	for _, t := range types {
		switch t {
		case "full_attention", "attention":
		case "sliding_attention", "chunked_attention":
			sliding++
		default:
			// linear_attention, mamba and the like: no KV a token.
			return AttentionHybrid
		}
	}
	for _, t := range stringList(c["layers_block_type"]) {
		if t != "attention" {
			return AttentionHybrid
		}
	}
	if p, ok := c["hybrid_override_pattern"].(string); ok && strings.Contains(p, "M") {
		return AttentionHybrid
	}
	window, hasWindow := intKey(c, "sliding_window")
	if use, ok := c["use_sliding_window"].(bool); ok && !use {
		// Qwen2-style configs name a window they do not use.
		hasWindow = false
	}
	if !hasWindow || window <= 0 {
		window, hasWindow = intKey(c, "attention_chunk_size")
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
