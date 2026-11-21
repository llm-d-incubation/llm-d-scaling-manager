package main

import (
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// The vLLM release a fleet runs decides the defaults of the flags its engines
// are not given (docs/reference/vllm-versions.md). The planner reports the
// release and the step budget, memory share and block size it resolves to,
// the controller's figures, for reference: no answer reads them yet. A
// capture whose stand-in names no release reads every fleet as unknown.

// engineOfAnswer is the decode variant's engine, as replicas-to-slo's
// inputs name it, for the sweep's window read with extra flags.
func engineOfAnswer(t *testing.T, srv *promtest.Server, extra ...string) wireEngine {
	t.Helper()
	out, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, sweepWindow(srv.URL), extra, []string{"-o", "json"})...)
	w := decodeOutput(t, out)
	require.Equalf(t, 0, code, "replicas-to-slo: exit %d\n%s", code, out)
	require.NotEmpty(t, w.Roles, out)
	require.NotNil(t, w.Roles[0].Inputs, out)
	require.NotNil(t, w.Roles[0].Inputs.Engine, "no engine in the priced inputs:\n%s", out)
	return *w.Roles[0].Inputs.Engine
}

func TestEngine_TheReleaseSetsTheReportedDefaults(t *testing.T) {
	srv := sweepServer(t)
	cases := []struct {
		name  string
		flags []string
		want  wireEngine
	}{
		{"v0.34 on an H200", []string{"--accelerator", "NVIDIA-H200", "--vllm-version", "0.34"},
			wireEngine{VLLMVersion: "0.34", VLLMVersionFrom: whatifsource.VLLMVersionFromFlag,
				StepBudget: 8192, GPUMemoryUtilization: 1.0, BlockSize: 64}},
		{"v0.27.1 on an H200, from the image", []string{"--accelerator", "NVIDIA-H200",
			"--engine-image", "registry.example.com/vllm/vllm-openai:v0.27.1"},
			wireEngine{VLLMVersion: "0.27", VLLMVersionFrom: whatifsource.VLLMVersionFromEngineImage,
				StepBudget: 8192, GPUMemoryUtilization: 0.92, BlockSize: 16}},
		{"unknown on an H200", []string{"--accelerator", "NVIDIA-H200"},
			wireEngine{VLLMVersionFrom: capacity.VLLMVersionUnread, StepBudget: 8192, GPUMemoryUtilization: 0.9, BlockSize: 16}},
		{"an image whose tag is not vLLM's", []string{"--accelerator", "NVIDIA-H200",
			"--engine-image", "ghcr.io/llm-d/llm-d-cuda:v0.4.0"},
			wireEngine{VLLMVersionFrom: capacity.VLLMVersionUnread, StepBudget: 8192, GPUMemoryUtilization: 0.9, BlockSize: 16}},
		// The budget is where the release moves the reported figures: from v0.28 an
		// accelerator of 160 GiB or more steps 16384 tokens, and an unknown
		// release gets the smaller of the two.
		{"v0.34 on a B200", []string{"--accelerator", "NVIDIA-B200", "--vllm-version", "0.34"},
			wireEngine{VLLMVersion: "0.34", VLLMVersionFrom: whatifsource.VLLMVersionFromFlag,
				StepBudget: 16384, GPUMemoryUtilization: 1.0, BlockSize: 64}},
		{"unknown on a B200", []string{"--accelerator", "NVIDIA-B200"},
			wireEngine{VLLMVersionFrom: capacity.VLLMVersionUnread, StepBudget: 8192, GPUMemoryUtilization: 0.9, BlockSize: 16}},
		// A flag the engine is given wins over every release's default.
		{"v0.34 with the flags given", []string{"--accelerator", "NVIDIA-B200", "--vllm-version", "0.34",
			"--engine-args", "--max-num-batched-tokens=4096 --gpu-memory-utilization=0.85 --block-size=32"},
			wireEngine{VLLMVersion: "0.34", VLLMVersionFrom: whatifsource.VLLMVersionFromFlag,
				StepBudget: 4096, StepBudgetSource: engineSetByFlag, GPUMemoryUtilization: 0.85,
				GPUMemoryUtilizationSource: engineSetByFlag, BlockSize: 32, BlockSizeSource: engineSetByFlag}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := c.want
			want.Engine = "vllm"
			for _, s := range []*string{&want.StepBudgetSource, &want.GPUMemoryUtilizationSource, &want.BlockSizeSource} {
				if *s == "" {
					*s = engineDefault
				}
			}
			assert.Equal(t, want, engineOfAnswer(t, srv, c.flags...))
		})
	}
}

// A wide-EP engine with a batched all-to-all and more than one DP rank steps
// 256 tokens in every release, as vLLM's own default: the capture's window
// line, which the generated run files every scale target from, holds it.
func TestEngine_BatchedDPStepsTwoHundredFiftySixTokens(t *testing.T) {
	srv := sweepServer(t)
	for _, version := range []string{"0.34", "0.27"} {
		out := t.TempDir() + "/c.gz"
		text, err := runCLI(slices.Concat([]string{"capture"}, sweepWindow(srv.URL), []string{"--accelerator", "NVIDIA-H200",
			"--vllm-version", version, "--engine-args",
			"--enable-expert-parallel --data-parallel-size=2 --all2all-backend=deepep_low_latency", "-o", out})...)
		require.NoError(t, err)
		params := capturedParams(t, out)
		assert.Equalf(t, int64(256), params.EffectiveMaxBatchedTokens, "v%s: step budget", version)
		assert.Contains(t, text, fmt.Sprintf("vLLM %s (from --vllm-version)", version))
	}
}

// What kube-state-metrics shows the workload running wins over the flags:
// the engine container's vllm-openai image, and over it the Deployment's
// scaling-manager.llmd.ai/vllm-version annotation, as the controller reads
// them. A sidecar's image names no release and is passed over.
func TestEngine_TheLiveWorkloadWinsOverTheFlags(t *testing.T) {
	k := whatifsource.KSMQueries(sweepNS)
	withKSM := func(annotation string) *promtest.Server {
		srv := sweepServer(t)
		for j := range sweepPods {
			pod := fmt.Sprintf("m-dec-7c9d-%d", j)
			sweepKSM(srv, pod, false)
			engine := "registry.example.com/vllm/vllm-openai:v0.34.0"
			if annotation == "rollout" && j == sweepPods-1 {
				engine = "registry.example.com/vllm/vllm-openai:v0.32.1"
			}
			for container, image := range map[string]string{"vllm": engine,
				"routing-proxy": "ghcr.io/llm-d/llm-d-routing-sidecar:v0.4.0"} {
				srv.Add(k.PodContainers, promtest.Const(map[string]string{"namespace": sweepNS, "pod": pod,
					"container": container, "image_spec": image, "image": image}, 1))
			}
		}
		if annotation != "" && annotation != "rollout" {
			srv.Add(k.DeploymentAnnotations, promtest.Const(map[string]string{"namespace": sweepNS, "deployment": "m-dec",
				"annotation_scaling_manager_llmd_ai_vllm_version": annotation}, 1))
		}
		return srv
	}
	got := engineOfAnswer(t, withKSM(""), "--kube-state-metrics", "--vllm-version", "0.27")
	assert.Equal(t, "0.34", got.VLLMVersion)
	assert.Equal(t, "image tag", got.VLLMVersionFrom)
	assert.Equal(t, int64(64), got.BlockSize)

	got = engineOfAnswer(t, withKSM("0.27"), "--kube-state-metrics")
	assert.Equal(t, "0.27", got.VLLMVersion)
	assert.Equal(t, "annotation", got.VLLMVersionFrom)
	assert.Equal(t, int64(16), got.BlockSize)

	text, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, sweepWindow(withKSM("").URL), []string{"--kube-state-metrics"})...)
	require.Equal(t, 0, code, text)
	assert.Contains(t, text, "vLLM 0.34 (from image tag); step budget 8192 (default), priced as a cap on the batch per rank; "+
		"for reference, not priced yet: GPU memory 1 (default), block size 64 (default)")

	// A flag that names another release is not used, and the capture says
	// so, naming both.
	out := t.TempDir() + "/c.gz"
	text, err := runCLI(slices.Concat([]string{"capture"}, sweepWindow(withKSM("").URL),
		[]string{"--kube-state-metrics", "--vllm-version", "0.27", "-o", out})...)
	require.NoError(t, err)
	assert.Contains(t, text, "flag: variant m-dec runs vLLM 0.34 (from image tag), not 0.27 as --vllm-version states: "+
		"the workload's is used")
	text, err = runCLI(slices.Concat([]string{"capture"}, sweepWindow(withKSM("").URL),
		[]string{"--kube-state-metrics", "--vllm-version", "0.34", "-o", out})...)
	require.NoError(t, err)
	assert.NotContains(t, text, "states: the workload's is used", "a flag that agrees is no caveat")

	// A rollout in the window is named. Both caveats survive generate and
	// the export, which places every word of a caveat.
	text, err = runCLI(slices.Concat([]string{"capture"}, sweepWindow(withKSM("rollout").URL),
		[]string{"--kube-state-metrics", "--vllm-version", "0.27", "-o", out})...)
	require.NoError(t, err)
	assert.Contains(t, text, "flag: variant m-dec ran more than one vLLM release in the window: mostly 0.34, also 0.32")
	assert.Contains(t, text, "not 0.27 as --vllm-version states")
	recording := t.TempDir() + "/run.jsonl"
	_, err = runCLI("generate", "-in", out, "-o", recording)
	require.NoError(t, err)
	b, err := os.ReadFile(recording)
	require.NoError(t, err)
	_, _, err = learnreplay.Export(b, learnreplay.ExportOptions{})
	require.NoError(t, err, "the export refuses a caveat's words")
}

// A capture that cannot read the release says how to give it: with the
// default kube-state-metrics allowlist the annotation series carries no
// annotation, and an llm-d image's tag is not vLLM's.
func TestEngine_AnUnreadReleaseSaysHowToGiveIt(t *testing.T) {
	k := whatifsource.KSMQueries(sweepNS)
	srv := sweepServer(t)
	for j := range sweepPods {
		pod := fmt.Sprintf("m-dec-7c9d-%d", j)
		sweepKSM(srv, pod, false)
		srv.Add(k.PodContainers, promtest.Const(map[string]string{"namespace": sweepNS, "pod": pod,
			"container": "vllm", "image_spec": "ghcr.io/llm-d/llm-d-cuda:v0.4.0"}, 1))
	}
	srv.Add(k.DeploymentAnnotations, promtest.Const(map[string]string{"namespace": sweepNS, "deployment": "m-dec"}, 1))
	out := t.TempDir() + "/c.gz"
	text, err := runCLI(slices.Concat([]string{"capture"}, sweepWindow(srv.URL), []string{"--kube-state-metrics", "-o", out})...)
	require.NoError(t, err)
	assert.Contains(t, text, "flag: vLLM release unknown for 1 of 1 variants: give --vllm-version, or let kube-state-metrics "+
		"export the vllm-version annotation of Deployments (--metric-annotations-allowlist)")
	// The engine's own image is still the one fingerprinted, as the
	// controller fingerprints it.
	assert.Equal(t, "ghcr.io/llm-d/llm-d-cuda:v0.4.0", capturedParams(t, out).Image)

	text, err = runCLI(slices.Concat([]string{"capture"}, sweepWindow(srv.URL),
		[]string{"--kube-state-metrics", "--vllm-version", "0.34", "-o", out})...)
	require.NoError(t, err)
	assert.NotContains(t, text, "vLLM release unknown")
	assert.Contains(t, text, "vLLM 0.34 (from --vllm-version)")
}

// --vllm-version takes a release, and refuses anything else rather than
// read the fleet as unknown.
func TestEngine_TheVersionFlagIsARelease(t *testing.T) {
	srv := sweepServer(t)
	_, err := runCLI(slices.Concat([]string{"replicas-to-slo"}, sweepWindow(srv.URL), []string{"--vllm-version", "latest"})...)
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
	assert.Contains(t, err.Error(), `--vllm-version "latest" is not a vLLM release`)
}

// capturedParams is the engine parameters of a capture's one variant, as its
// window line holds them.
func capturedParams(t *testing.T, path string) capacity.EngineParams {
	t.Helper()
	b, err := readMaybeGzipped(path)
	require.NoError(t, err)
	read, err := learnrec.Read(b)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(read.Events), 2)
	var window learnreplay.CaptureWindow
	require.NoError(t, read.Events[1].DecodeData(&window))
	require.Len(t, window.Variants, 1)
	return window.Variants[0].Params
}
