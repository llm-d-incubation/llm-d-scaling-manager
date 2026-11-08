package capacity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
)

func vllmContainer(command []string, env ...corev1.EnvVar) corev1.Container {
	return corev1.Container{Name: "vllm", Image: "vllm/vllm-openai:v0.11.0", Command: command, Env: env}
}

// A wide-EP P/D decode engine: every flag that decides what one replica holds
// and that the fields before version 3 did not describe.
func TestVLLMParsesTheWideEPAndPDFlags(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"vllm", "serve", "deepseek-ai/DeepSeek-V3",
		"--revision", "r1",
		"--data-parallel-size", "8",
		"--pipeline-parallel-size=2",
		"--enable-expert-parallel",
		"--speculative-config", `{"num_speculative_tokens": 1, "method": "mtp"}`,
		"--kv-transfer-config", `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`,
		"--kv-offloading-size", "64",
		"--kv-offloading-backend", testOffloadBackend,
		"--no-enable-prefix-caching",
	})))

	require.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
	assert.False(t, p.ModelUnresolved)
	assert.Equal(t, 8, p.DataParallelSize)
	assert.Equal(t, 2, p.PipelineParallelSize)
	assert.True(t, p.ExpertParallel)
	assert.Equal(t, `{"method":"mtp","num_speculative_tokens":1}`, p.SpeculativeConfig,
		"re-encoded with sorted keys and no spacing")
	assert.Equal(t, testKVConnector, p.KVConnector)
	assert.Equal(t, 64.0, p.KVOffloadingSize)
	assert.Equal(t, testOffloadBackend, p.KVOffloadingBackend)
	assert.True(t, p.PrefixCachingDisabled)
	assert.Equal(t, "deepseek-ai/DeepSeek-V3", p.ModelPath)
	assert.Equal(t, "r1", p.ModelRevision)
}

// The defaults are the engine's: unset parallel sizes are 1, prefix caching
// is on, and no feature is named.
func TestVLLMDefaultsForTheNewFields(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "m"})))
	assert.Equal(t, 1, p.DataParallelSize)
	assert.Equal(t, 1, p.PipelineParallelSize)
	assert.False(t, p.ExpertParallel)
	assert.False(t, p.PrefixCachingDisabled)
	assert.Empty(t, p.SpeculativeConfig)
	assert.Empty(t, p.KVConnector)
	assert.Equal(t, "m", p.ModelPath)
}

// Key order and spacing in a JSON flag are formatting, not configuration.
func TestSpeculativeConfigFormattingDoesNotSplitTheKey(t *testing.T) {
	parse := func(spec string) EngineParams {
		return ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
			[]string{"vllm", "serve", "m", "--speculative-config", spec})))
	}
	a := parse(`{"method":"mtp","num_speculative_tokens":1}`)
	b := parse(`{ "num_speculative_tokens": 1,  "method": "mtp" }`)
	c := parse(`{"method":"mtp","num_speculative_tokens":2}`)
	assert.Equal(t, a.Fingerprint(), b.Fingerprint())
	assert.NotEqual(t, a.Fingerprint(), c.Fingerprint())
}

// A config that is not JSON still tells two configurations apart, and does
// not make the read incomplete.
func TestKVTransferConfigThatIsNotJSONIsKeptWhole(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "m", "--kv-transfer-config", "{kv_connector: Nixl}"})))
	assert.True(t, p.Complete())
	assert.Equal(t, "{kv_connector:Nixl}", p.KVConnector)
}

// A new flag whose value cannot be read is a gap like any other: recorded,
// so the configuration is not used as an equality.
// A parallel size computed at start-up -- llm-d's wide-EP LeaderWorkerSet
// passes `--data-parallel-size $((LWS_GROUP_SIZE * DP_SIZE_LOCAL))` -- is
// recorded as written and withholds only sharing. In Unresolved it made every
// such variant's read incomplete, so its own learned state was neither
// exported nor restored after a restart.
func TestAnUnreadableParallelSizeWithholdsOnlySharing(t *testing.T) {
	for name, tc := range map[string]struct {
		container corev1.Container
		want      string
	}{
		"shell arithmetic, split by the shell splitter": {
			container: vllmContainer([]string{"/bin/sh", "-c",
				"vllm serve org/m --data-parallel-size $((LWS_GROUP_SIZE * DP_SIZE_LOCAL)) --enable-expert-parallel"}),
			want: "data_parallel_size=$((LWS_GROUP_SIZE * DP_SIZE_LOCAL))",
		},
		"kubelet reference to a pod-only variable": {
			container: vllmContainer([]string{"vllm", "serve", "org/m", "--data-parallel-size", "$(LWS_GROUP_SIZE)"}),
			want:      "data_parallel_size=$(LWS_GROUP_SIZE)",
		},
		"a variable nothing in the template sets": {
			container: vllmContainer([]string{"vllm", "serve", "org/m", "--pipeline-parallel-size=$PP"}),
			want:      "pipeline_parallel_size=$PP",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(tc.container))
			assert.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
			assert.Equal(t, []string{tc.want}, p.SharingUnresolved)
			assert.Equal(t, 1, p.DataParallelSize, "the default stands in")
			assert.Equal(t, "org/m", p.ModelPath)
		})
	}

	// The text is the identity: a changed expression is a changed
	// configuration, for the variant's own restore and for the KV carry.
	a := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"/bin/sh", "-c", "vllm serve org/m --data-parallel-size $((LWS_GROUP_SIZE * 8))"})))
	b := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"/bin/sh", "-c", "vllm serve org/m --data-parallel-size $((LWS_GROUP_SIZE * 4))"})))
	assert.NotEqual(t, a.ArgsFingerprint(), b.ArgsFingerprint())
	assert.False(t, a.IsCapacityCompatible(&b))
	assert.False(t, a.IsCapacityCompatible(&a),
		"an expression nothing evaluated is not known to be the same engine, even as itself: "+
			"the same text is a 2x8 group in one LWS and a 4x8 group in another")
	assert.True(t, a.IsCapacityClampCompatible(&a), "and is still a ceiling")

	// The KV total measured under one expression is not carried to another.
	assert.True(t, kvTotalCarries(a, a))
	assert.False(t, kvTotalCarries(a, b))

	// A later occurrence that reads takes over, as the engine takes the last.
	c := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "org/m", "--data-parallel-size", "$(LWS_GROUP_SIZE)", "--data-parallel-size", "4"})))
	assert.Empty(t, c.SharingUnresolved)
	assert.Equal(t, 4, c.DataParallelSize)
}

// vLLM's single-dash aliases and SGLang's --ep, which read as absent with the
// configuration complete.
func TestShortAndAliasParallelFlagsAreRead(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "org/m", "-tp", "2", "-dp=4", "-pp", "2", "--enforce-eager", "-tp", "8"})))
	assert.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
	assert.Equal(t, 8, p.TensorParallelSize, "the last occurrence wins")
	assert.Equal(t, 4, p.DataParallelSize)
	assert.Equal(t, 2, p.PipelineParallelSize)
	assert.True(t, p.EnforceEager, "a boolean before a short flag does not take it as its value")

	neg := ParseEngineArgs(inferenceengine.EngineSGLang, deploymentWithContainers(corev1.Container{
		Name: "sglang", Image: "lmsysorg/sglang:v0.5", Command: []string{
			"python", "-m", "sglang.launch_server", "--model-path", "org/m", "--chunked-prefill-size", "-1", "--ep", "8",
		}}))
	assert.True(t, neg.Complete(), "unresolved: %v", neg.Unresolved)
	assert.False(t, neg.ChunkedPrefillEnabled, "a negative number is a value, not a flag")
	assert.True(t, neg.ExpertParallel)
}

// A model after the flags, where a boolean before it takes it as its value,
// is an unread checkpoint, not an absent one that compares equal to every
// other absent one.
func TestAModelAfterTheFlagsIsMarkedUnread(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "--enable-expert-parallel", "org/m"})))
	assert.True(t, p.ModelUnresolved)
	assert.True(t, p.Complete(), "unresolved: %v", p.Unresolved)

	named := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "--enable-expert-parallel", "--model", "org/m"})))
	assert.False(t, named.ModelUnresolved)
	assert.Equal(t, "org/m", named.ModelPath)
}

// vLLM's dotted spelling of its JSON flags, one key per flag.
func TestDottedJSONFlagsAreRead(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "org/m",
			"--speculative-config.num_speculative_tokens", "3", "--speculative-config.method", "mtp",
			"--kv-transfer-config.kv_connector", "NixlConnector", "--kv-transfer-config.kv_role", "kv_both"})))
	assert.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
	assert.Equal(t, "config.method=mtp;config.num_speculative_tokens=3", p.SpeculativeConfig)
	assert.Equal(t, "NixlConnector", p.KVConnector)

	none := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "org/m"})))
	assert.NotEqual(t, none.Fingerprint(), p.Fingerprint())

	unread := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "org/m", "--speculative-config.method", "$METHOD"})))
	assert.True(t, unread.Complete(), "unresolved: %v", unread.Unresolved)
	assert.Equal(t, []string{"speculative_config.method=$METHOD"}, unread.SharingUnresolved)
}

// The zero-replica estimate's ceiling is found under the looser equality: a
// sibling differing only in a field the borrow's equality added is still a
// ceiling.
func TestTheClampEqualityIgnoresTheAddedIdentityFields(t *testing.T) {
	base := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&base)
	for name, mutate := range map[string]func(*EngineParams){
		"checkpoint spelling": func(p *EngineParams) { p.ModelPath = "/models/org-m" },
		"prefix caching":      func(p *EngineParams) { p.PrefixCachingDisabled = true },
		"KV connector":        func(p *EngineParams) { p.KVConnector = testKVConnector },
		"data parallel size":  func(p *EngineParams) { p.DataParallelSize = 8 },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			mutate(&other)
			assert.False(t, base.IsCapacityCompatible(&other), "the borrow's equality refuses it")
			assert.True(t, base.IsCapacityClampCompatible(&other), "the clamp's does not")
		})
	}
	other := base
	other.MaxNumSeqs = 64
	assert.False(t, base.IsCapacityClampCompatible(&other), "a field the clamp always compared still refuses")
}

// An unread checkpoint is NOT an incomplete read: that would withhold the
// variant's own restore. It is marked on its own field.
func TestAnUnreadModelIsMarkedButDoesNotMakeTheReadIncomplete(t *testing.T) {
	for name, cmd := range map[string][]string{
		"positional": {"vllm", "serve", "$MODEL"},
		"flag":       {"python", "-m", "vllm.entrypoints.openai.api_server", "--model", "$MODEL"},
	} {
		t.Run(name, func(t *testing.T) {
			p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(cmd)))
			assert.True(t, p.ModelUnresolved)
			assert.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
			assert.Empty(t, p.ModelPath)
		})
	}

	resolved := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "$MODEL"}, corev1.EnvVar{Name: "MODEL", Value: "org/m"})))
	assert.False(t, resolved.ModelUnresolved)
	assert.Equal(t, "org/m", resolved.ModelPath)
}

func TestSGLangParsesTheWideEPAndPDFlags(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineSGLang, deploymentWithContainers(corev1.Container{
		Name: "sglang", Image: "lmsysorg/sglang:v0.5", Command: []string{
			"python", "-m", "sglang.launch_server",
			"--model-path", "org/m", "--revision", "r2",
			"--dp-size", "4", "--pp-size", "2", "--ep-size", "8",
			"--speculative-num-steps", "3", "--speculative-algorithm", "EAGLE",
			"--disaggregation-transfer-backend", "mooncake",
			"--enable-hierarchical-cache", "--hicache-size", "32",
			"--disable-radix-cache",
		}}))

	require.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
	assert.Equal(t, 4, p.DataParallelSize)
	assert.Equal(t, 2, p.PipelineParallelSize)
	assert.True(t, p.ExpertParallel)
	assert.Equal(t, "algorithm=EAGLE;num_steps=3", p.SpeculativeConfig, "sorted, whatever the flag order")
	assert.Equal(t, "mooncake", p.KVConnector)
	assert.Equal(t, "hierarchical", p.KVOffloadingBackend)
	assert.Equal(t, 32.0, p.KVOffloadingSize)
	assert.True(t, p.PrefixCachingDisabled)
	assert.Equal(t, "org/m", p.ModelPath)
	assert.Equal(t, "r2", p.ModelRevision)
}

// A wide-EP flag of the other engine is the wrong-parser signal, as every
// mapped value key is.
func TestTheVLLMParserOverSGLangParallelFlagsIsIncomplete(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "m", "--dp-size", "4"})))
	assert.False(t, p.Complete())
	assert.Contains(t, p.Unresolved, "dp_size")
}

// A configuration built without the parallel sizes is the same configuration
// as one parsed from flags that do not set them.
func TestUnsetParallelSizesCompareAsOne(t *testing.T) {
	parsed := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "m"})))
	built := parsed
	built.DataParallelSize, built.PipelineParallelSize = 0, 0
	assert.True(t, parsed.IsCapacityCompatible(&built))
	assert.Equal(t, parsed.Fingerprint(), built.Fingerprint())
}

// The KV total measured under one layout or checkpoint does not stand as
// another's k1.
func TestTheKVTotalDoesNotCarryAcrossALayoutOrCheckpointChange(t *testing.T) {
	base := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&base)
	require.True(t, kvTotalCarries(base, base))
	for name, mutate := range map[string]func(*EngineParams){
		"data parallel":     func(p *EngineParams) { p.DataParallelSize = 8 },
		"pipeline parallel": func(p *EngineParams) { p.PipelineParallelSize = 2 },
		"expert parallel":   func(p *EngineParams) { p.ExpertParallel = true },
		"speculative":       func(p *EngineParams) { p.SpeculativeConfig = testSpeculativeConfig },
		"checkpoint":        func(p *EngineParams) { p.ModelPath = "org/m-FP8" },
		"revision":          func(p *EngineParams) { p.ModelRevision = "r2" },
	} {
		t.Run(name, func(t *testing.T) {
			cur := base
			mutate(&cur)
			assert.False(t, kvTotalCarries(base, cur))
		})
	}
}
