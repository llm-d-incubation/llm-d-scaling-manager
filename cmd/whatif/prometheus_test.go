package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// The --prometheus mode (the planner's D4) is an adapter: capture, then
// generate, then answer, in memory. These tests hold it to the files: the
// same window answered through capture -o, generate -o and --recording is
// the same answer, byte for byte.

const (
	sweepNS    = "llm"
	sweepSteps = 120
	sweepStep  = 15 * time.Second
	sweepPods  = 3
)

var sweepFrom = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// sweepNotReadyFrom is the step from which kube-state-metrics reads the
// sweep's last Pod as not Ready.
const sweepNotReadyFrom = 100

// sweepStepOf is the step an instant falls on, ok false off the grid or
// past the window.
func sweepStepOf(at time.Time) (int, bool) {
	d := at.Sub(sweepFrom)
	if d < 0 || d%sweepStep != 0 {
		return 0, false
	}
	return int(d / sweepStep), d/sweepStep < sweepSteps
}

// sweepLevels is the fill the fleet holds, one level per sweepHold steps,
// so each level settles and the ITL line has a span to fit over.
var sweepLevels = []float64{0.2, 0.35, 0.5, 0.65, 0.5, 0.35}

const sweepHold = 10

// sweepKV is pod j's KV utilization at step i: the level, each pod a little
// apart.
func sweepKV(j, i int) float64 {
	return sweepLevels[(i/sweepHold)%len(sweepLevels)] + 0.02*float64(j)
}

// sweepITL is the ITL at KV fill k: a line, 6 ms at no load and 20 ms more
// at a full cache.
func sweepITL(k float64) float64 { return 0.006 + 0.02*k }

// sweepServer is a Prometheus holding a window of one decode variant of
// model m on vLLM, three Pods sweeping their fill, with every series the
// decode pricing reads, and the kube-state-metrics series that describe the
// Pods.
func sweepServer(t *testing.T) *promtest.Server {
	t.Helper()
	return sweepServerFor(t, inferenceengine.EngineVLLM)
}

// sweepServerFor is sweepServer with the engine's own series: a query
// registered per engine (registration.EngineQuery) is served under the
// engine's name, SGLang's capacity as its token gauge.
func sweepServerFor(t *testing.T, engine inferenceengine.Engine) *promtest.Server {
	t.Helper()
	srv := promtest.New()
	t.Cleanup(srv.Close)
	q := sweepQueries(t, engine)
	for j := range sweepPods {
		labels := map[string]string{"model_name": "m", "pod": fmt.Sprintf("m-dec-7c9d-%d", j),
			"instance": fmt.Sprintf("10.0.0.%d:8000", j+1), "namespace": sweepNS}
		with := func(k, v string) map[string]string {
			l := map[string]string{}
			for a, b := range labels {
				l[a] = b
			}
			l[k] = v
			return l
		}
		at := func(f func(i int) float64) func(time.Time) (float64, bool) {
			return func(ts time.Time) (float64, bool) {
				i, ok := sweepStepOf(ts)
				if !ok {
					return 0, false
				}
				return f(i), true
			}
		}
		kv := func(i int) float64 { return sweepKV(j, i) }
		itl := func(i int) float64 { return sweepITL(kv(i)) }
		c := func(x float64) func(int) float64 { return func(int) float64 { return x } }
		add := func(name string, l map[string]string, f func(i int) float64) {
			srv.Add(q(name), promtest.Series{Labels: l, Value: at(f)})
		}
		add(registration.QueryKvCacheUsage, labels, kv)
		add(registration.QueryMaxRankKvCacheUsage, labels, kv)
		add(registration.QueryQueueLength, labels, c(0))
		add(registration.QueryMaxRankQueueLength, labels, c(0))
		add(registration.QueryCapacityQueueLength, labels, c(0))
		add(registration.QueryMaxRankCapacityQueueLength, labels, c(0))
		if engine == inferenceengine.EngineSGLang {
			add(registration.QueryCacheConfigInfo, labels, c(8192*16))
		} else {
			srv.Add(q(registration.QueryCacheConfigInfo), promtest.Series{Labels: map[string]string{
				"instance": labels["instance"], "pod": labels["pod"], "num_gpu_blocks": "8192", "block_size": "16"},
				Value: at(c(1))})
		}
		add(registration.QueryAvgOutputTokens, labels, c(1000))
		add(registration.QueryAvgOutputTokensRecent, labels, c(1000))
		add(registration.QueryAvgInputTokens, labels, c(1000))
		add(registration.QueryAvgInputTokensRecent, labels, c(1000))
		add(registration.QueryPrefixCacheHitRate, labels, c(0))
		add(registration.QueryAvgITL, labels, itl)
		running := func(i int) float64 { return math.Round(kv(i) * 64) }
		add(registration.QueryRunningRequests, labels, running)
		add(registration.QueryGenerationTokenRate, labels, func(i int) float64 { return running(i) / itl(i) })
		add(registration.QueryRequestRate, labels, func(i int) float64 { return running(i) / itl(i) / 1000 })
		add(registration.QueryAvgTTFT, labels, c(0.2))
		add(registration.QueryAvgServiceTime, labels, func(i int) float64 { return 1000 * itl(i) })
		add(registration.QueryKvUsageInstant, labels, kv)
		add(registration.QueryKvUsageMean, labels, kv)
		add(registration.QueryPreemptionRate, labels, c(0))
		add(registration.QueryTPOTBuckets, with("le", "0.02"), c(0.5))
		add(registration.QueryTPOTBuckets, with("le", "0.05"), c(1))
		add(registration.QueryTPOTBuckets, with("le", "+Inf"), c(1))
		sweepKSM(srv, labels["pod"], j == sweepPods-1)
	}
	return srv
}

// sweepQueries is each registered query's text on the engine, as a capture
// issues it.
func sweepQueries(t *testing.T, engine inferenceengine.Engine) func(string) string {
	t.Helper()
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	registration.RegisterQueueingModelQueries(reg)
	registration.RegisterSLOQueries(reg)
	registration.RegisterFlowControlPoolQueries(reg)
	registration.RegisterAlwaysOnQueries(reg)
	return func(name string) string {
		name = registration.EngineQuery(engine, name)
		params := map[string]string{source.ParamNamespace: sweepNS}
		if qt := reg.Get("prometheus").QueryList().Get(name); qt != nil && len(qt.Params) == 0 {
			params = nil
		}
		s, err := reg.Get("prometheus").QueryList().Build(name, params)
		require.NoError(t, err, name)
		return s
	}
}

// sweepKSM is kube-state-metrics' account of one of the sweep's Pods: a
// Deployment's, Ready, with a role label, two GPUs on an H200 node; the
// last Pod (late) is not Ready from sweepNotReadyFrom on, which only
// kube-state-metrics shows. A capture reads it with --kube-state-metrics,
// and ignores it without.
func sweepKSM(srv *promtest.Server, pod string, late bool) {
	k := whatifsource.KSMQueries(sweepNS)
	l := func(extra ...string) map[string]string {
		m := map[string]string{"namespace": sweepNS, "pod": pod}
		for x := 0; x+1 < len(extra); x += 2 {
			m[extra[x]] = extra[x+1]
		}
		return m
	}
	ready := float64(sweepFrom.Add(-30 * time.Minute).Unix())
	srv.Add(k.NodeLabels, promtest.Const(map[string]string{"node": "n1", "label_nvidia_com_gpu_product": "NVIDIA-H200"}, 1))
	srv.Add(k.ReplicaSetOwner, promtest.Const(map[string]string{"namespace": sweepNS, "replicaset": "m-dec-7c9d",
		"owner_kind": "Deployment", "owner_name": "m-dec"}, 1))
	srv.Add(k.DeploymentReplicas, promtest.Const(map[string]string{"namespace": sweepNS, "deployment": "m-dec"}, sweepPods))
	srv.Add(k.PodNode, promtest.Const(l("node", "n1"), 1))
	srv.Add(k.PodLabels, promtest.Const(l("label_llm_d_ai_role", "decode"), 1))
	srv.Add(k.PodOwner, promtest.Const(l("owner_kind", "ReplicaSet", "owner_name", "m-dec-7c9d"), 1))
	srv.Add(k.PodRunning, promtest.Const(l(), 1))
	srv.Add(k.PodReady, promtest.Series{Labels: l(), Value: func(at time.Time) (float64, bool) {
		i, ok := sweepStepOf(at)
		return 1, ok && (!late || i < sweepNotReadyFrom)
	}})
	srv.Add(k.PodReadyTime, promtest.Const(l(), ready))
	srv.Add(k.PodCreated, promtest.Const(l(), ready-60))
	srv.Add(k.PodStarted, promtest.Const(l(), ready-60))
	srv.Add(k.PodGPUs, promtest.Const(map[string]string{"pod": pod}, 2))
}

// sweepPolicy is a scaling-policy file whose thresholds the policy's own
// check keeps, and that move the answer: the operating point at 0.6 of the
// knee, not the default 0.85.
func sweepPolicy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("scaleUpThreshold: 0.6\nscaleDownBoundary: 0.4\n"), 0o600))
	return path
}

// sweepWindow is the --prometheus flags of the sweep's window.
func sweepWindow(prom string) []string {
	return []string{"--prometheus", prom, "--namespace", sweepNS, "--role-of", "decode=app=m-dec",
		"--from", sweepFrom.Format(time.RFC3339), "--to", sweepFrom.Add((sweepSteps - 1) * sweepStep).Format(time.RFC3339),
		"--batch-cap", "64"}
}

// filesAnswer answers question the long way: whatif capture -o, whatif
// generate -o, then the question with --recording, each a command line of
// its own. window is the --prometheus flags, which capture takes as they
// are.
func filesAnswer(t *testing.T, window, question []string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	capture, recording := filepath.Join(dir, "run.capture.jsonl.gz"), filepath.Join(dir, "run.jsonl")
	_, err := runCLI(slices.Concat([]string{"capture"}, window, []string{"-o", capture})...)
	require.NoError(t, err)
	_, err = runCLI("generate", "-in", capture, "-o", recording)
	require.NoError(t, err)
	return runCmd(t, slices.Concat(question[:1], []string{"--recording", recording, "--namespace", sweepNS}, question[1:])...)
}

// withoutSourceLine is a --prometheus mode's text answer without the line
// that names its window, which the files' answer does not have: the rest is
// the answer. It fails the test when the line is not there.
func withoutSourceLine(t *testing.T, text string) string {
	t.Helper()
	first, rest, ok := strings.Cut(text, "\n")
	require.True(t, ok, text)
	require.True(t, strings.HasPrefix(first, "answered for "+sweepNS+"/m from "), "no source line:\n%s", text)
	return rest
}

// sameAnswer reports how two JSON outputs differ, "" when they do not: every
// key byte for byte, but for where the source says it came from (the
// recording's path, or the Prometheus window), which names the source and
// not the answer.
func sameAnswer(t *testing.T, files, prom string) string {
	t.Helper()
	var a, b map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(files), &a), files)
	require.NoError(t, json.Unmarshal([]byte(prom), &b), prom)
	for _, k := range []string{"source"} {
		var sa, sb map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(a[k], &sa))
		require.NoError(t, json.Unmarshal(b[k], &sb))
		require.NotEmpty(t, sa["recording"], "the files' source names no recording")
		require.NotEmpty(t, sb["window"], "the --prometheus source names no window")
		for _, s := range []map[string]json.RawMessage{sa, sb} {
			delete(s, "recording")
			delete(s, "window")
		}
		ja, _ := json.Marshal(sa)
		jb, _ := json.Marshal(sb)
		a[k], b[k] = ja, jb
	}
	var diffs []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range a {
			if !yield(k) {
				return
			}
		}
	}) {
		if !bytes.Equal(a[k], b[k]) {
			diffs = append(diffs, fmt.Sprintf("%s:\nfiles: %s\nprometheus: %s", k, a[k], b[k]))
		}
	}
	if len(a) != len(b) {
		diffs = append(diffs, fmt.Sprintf("%d keys, %d keys", len(a), len(b)))
	}
	return strings.Join(diffs, "\n")
}

// identityCases is question flags the identity is held on: both questions,
// answers and refusals, the recorded shape and an assumed one, an earlier
// cycle and a priced replica count.
var identityCases = []struct {
	name string
	args []string
	code int
}{
	{"replicas-to-slo", []string{"replicas-to-slo"}, 0},
	{"replicas-to-slo at a rate", []string{"replicas-to-slo", "--rate", "5", "--decode", "2"}, 0},
	{"replicas-to-slo at cycle 90", []string{"replicas-to-slo", "--cycle", "90"}, 0},
	{"replicas-to-slo, an assumed ratio", []string{"replicas-to-slo", "--tpot-percentile", "90", "--assume-tpot-ratio", "1.3",
		"--assume-output-tokens", "500"}, 0},
	{"replicas-to-slo refused: the engine's bucket is too wide", []string{"replicas-to-slo", "--tpot-percentile", "90"}, 3},
	{"slo-to-rate", []string{"slo-to-rate", "--tpot", "15ms"}, 0},
	{"slo-to-rate, a headroom", []string{"slo-to-rate", "--tpot", "12ms", "--headroom", "0.6", "--seed", "7"}, 0},
	{"slo-to-rate refused: below the floor", []string{"slo-to-rate", "--tpot", "5ms"}, 3},
}

// The --prometheus answer is the answer capture, generate and --recording
// give on the same window, byte for byte, in JSON and in text: the mode is
// an adapter, and prices nothing itself.
func TestPrometheus_TheAnswerIsTheFilesAnswer(t *testing.T) {
	srv := sweepServer(t)
	window := sweepWindow(srv.URL)
	for _, c := range identityCases {
		t.Run(c.name, func(t *testing.T) {
			for _, output := range []string{outputJSON, outputText} {
				q := slices.Concat(c.args, []string{"-o", output})
				files, fileCode := filesAnswer(t, window, q)
				prom, promCode := runCmd(t, slices.Concat(c.args[:1], window, c.args[1:], []string{"-o", output})...)
				require.Equal(t, c.code, fileCode, "the files' exit code\n%s", files)
				require.Equal(t, fileCode, promCode, "exit codes differ\n%s", prom)
				if output == outputText {
					assert.Equal(t, files, withoutSourceLine(t, prom), "the text differs")
					continue
				}
				if d := sameAnswer(t, files, prom); d != "" {
					t.Errorf("the --prometheus answer is not the files' answer:\n%s", d)
				}
			}
		})
	}
	// The capture's own flags reach the answer the same way on both paths.
	// The capture's own flags reach the answer the same way on both paths.
	// Each case is a base window and the flag under test, on one server:
	// first the base alone must answer differently, or the identity would
	// say nothing about the flag. --gpus-per-replica and --accelerator are
	// not here: they key the capacity store's reuse, not the capacity a
	// decode answer prices, so no answer can show them;
	// TestPrometheus_CaptureFlagsReachTheCapture holds them instead.
	t.Run("capture flags", func(t *testing.T) {
		policy := sweepPolicy(t)
		sglang := sweepServerFor(t, inferenceengine.EngineSGLang)
		noCap := slices.Clone(window[:len(window)-2])
		require.Equal(t, "--batch-cap", window[len(window)-2])
		for _, c := range []struct {
			name       string
			base, flag []string
		}{
			{"--policy", window, []string{"--policy", policy}},
			// On SGLang's series: without the flag the capture reads them
			// as vLLM's, and finds none.
			{"--engine sglang", sweepWindow(sglang.URL), []string{"--engine", "sglang"}},
			// With no --batch-cap, which would set the same cap.
			{"--engine-args", noCap, []string{"--engine-args", "--max-num-seqs=32 --max-model-len=8192"}},
			// Beside --role-of, which maps the roles: kube-state-metrics
			// adds readiness, and one Pod leaves Ready.
			{"--kube-state-metrics", window, []string{"--kube-state-metrics"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				with := slices.Concat(c.base, c.flag)
				q := []string{"replicas-to-slo", "-o", "json"}
				got, _ := runCmd(t, slices.Concat(q[:1], with, q[1:])...)
				without, _ := runCmd(t, slices.Concat(q[:1], c.base, q[1:])...)
				require.NotEqual(t, without, got, "%s does not move the answer", c.name)
				for _, q := range [][]string{
					{"slo-to-rate", "--tpot", "15ms", "-o", "json"},
					{"replicas-to-slo", "-o", "json"},
					{"replicas-to-slo"},
				} {
					files, fileCode := filesAnswer(t, with, q)
					prom, promCode := runCmd(t, slices.Concat(q[:1], with, q[1:])...)
					require.Equal(t, fileCode, promCode, "exit codes differ\nfiles:\n%s\nprometheus:\n%s", files, prom)
					require.Equal(t, 0, promCode, "%s", prom)
					if !slices.Contains(q, outputJSON) {
						assert.Equal(t, files, withoutSourceLine(t, prom), "the text differs")
						continue
					}
					if d := sameAnswer(t, files, prom); d != "" {
						t.Errorf("the --prometheus answer is not the files' answer:\n%s", d)
					}
				}
			})
		}
	})

	// The window answered something: a line was learned and priced.
	out, code := runCmd(t, slices.Concat([]string{"slo-to-rate"}, window, []string{"--tpot", "15ms", "-o", "json"})...)
	require.Equal(t, 0, code, out)
	w := decodeOutput(t, out)
	require.Len(t, w.Roles, 1)
	require.NotNil(t, w.Roles[0].Results.Rate.Answer, "no answer to compare:\n%s", out)
	assert.Equal(t, "ols", w.Roles[0].Inputs.Line.Tier)
	assert.Equal(t, int64(64), w.Roles[0].Inputs.BatchCap, "--batch-cap is the captured engine's cap")
	assert.Equal(t, sweepSteps, w.Source.TotalCycles)
}

// The negative control: the comparison sees a difference when there is one.
// The files' capture is made without the --batch-cap the --prometheus run
// captures with, so the engine prices another cap, and the answers differ.
func TestPrometheus_TheIdentityCheckSeesADifference(t *testing.T) {
	srv := sweepServer(t)
	window := sweepWindow(srv.URL)
	other := slices.Clone(window)
	i := slices.Index(other, "--batch-cap")
	require.GreaterOrEqual(t, i, 0)
	other[i+1] = "32"
	q := []string{"slo-to-rate", "--tpot", "15ms", "-o", "json"}
	files, _ := filesAnswer(t, other, q)
	prom, _ := runCmd(t, slices.Concat(q[:1], window, q[1:])...)
	d := sameAnswer(t, files, prom)
	assert.Contains(t, d, "roles", "a capture at another batch cap answered the same")

	// And a window one step shorter: another recording, another answer.
	shorter := slices.Clone(window)
	j := slices.Index(shorter, "--to")
	shorter[j+1] = sweepFrom.Add((sweepSteps - 2) * sweepStep).Format(time.RFC3339)
	files, _ = filesAnswer(t, shorter, q)
	assert.NotEmpty(t, sameAnswer(t, files, prom), "a shorter window answered the same")
}

// The source says which window the answer was captured from, in JSON and
// in text. The URL is printed with no userinfo, query or fragment, any of
// which can carry a secret.
func TestPrometheus_TheSourceNamesTheWindow(t *testing.T) {
	srv := sweepServer(t)
	// The Prometheus served under a path holding a key, as a gateway's.
	const keyPath = "/t/k3ypath"
	prefixed := httptest.NewServer(http.StripPrefix(keyPath, srv.Config.Handler))
	t.Cleanup(prefixed.Close)
	secret := strings.Replace(prefixed.URL, "http://", "http://reader:hunter2@", 1) + keyPath + "/?token=s3cr3t&x=y#frag"
	leaks := []string{"hunter2", "reader", "s3cr3t", "token", "frag", "k3ypath"}
	out, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, sweepWindow(secret), []string{"--step", "15s", "-o", "json"})...)
	require.Equal(t, 0, code, out)
	for _, leak := range leaks {
		assert.NotContains(t, out, leak)
	}
	w := decodeOutput(t, out)
	require.NotNil(t, w.Source)
	require.NotNil(t, w.Source.Window)
	assert.Empty(t, w.Source.Recording)
	win := w.Source.Window
	assert.Equal(t, prefixed.URL, win.Prometheus)
	assert.True(t, win.From.Equal(sweepFrom), "from %s", win.From)
	assert.True(t, win.To.Equal(sweepFrom.Add((sweepSteps-1)*sweepStep)), "to %s", win.To)
	assert.Equal(t, 15.0, win.StepSeconds)
	assert.Equal(t, "role-of", win.Roles)
	assert.Equal(t, sweepNS, w.Source.Namespace)

	text, code := runCmd(t, slices.Concat([]string{"replicas-to-slo"}, sweepWindow(secret))...)
	require.Equal(t, 0, code, text)
	first, _, _ := strings.Cut(text, "\n")
	assert.Equal(t, fmt.Sprintf("answered for %s/m from %s, %s to %s every 15s (captured and generated in memory)", sweepNS, prefixed.URL,
		sweepFrom.Format(time.RFC3339), sweepFrom.Add((sweepSteps-1)*sweepStep).Format(time.RFC3339)), first)
	for _, leak := range leaks {
		assert.NotContains(t, text, leak)
	}

	// A recording's source has no window, and its text no source line.
	files, _ := filesAnswer(t, sweepWindow(srv.URL), []string{"replicas-to-slo", "-o", "json"})
	fw := decodeOutput(t, files)
	assert.Nil(t, fw.Source.Window)
	assert.NotEmpty(t, fw.Source.Recording)
	files, _ = filesAnswer(t, sweepWindow(srv.URL), []string{"replicas-to-slo"})
	assert.NotContains(t, files, "answered for")
}

// An error names no secret of the URL, whether the HTTP client quotes the
// request's URL (a query that cannot be sent) or the flags' check does (a
// token over http): capture's and the --prometheus mode's, in text and in
// -o json. It keeps its exit code and what errors.Is finds.
func TestPrometheus_ErrorsNameNoSecret(t *testing.T) {
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()
	srv := sweepServer(t)
	failing := sweepServer(t)
	failing.Fail(queryOf(t, registration.QueryKvCacheUsage), "query processing would load too many samples")
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("abc\n"), 0o600))
	const secret = "/t/k3ypath/?token=s3cr3t%2F&k=pw%20d"
	out := filepath.Join(t.TempDir(), "c.gz")
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"unreachable", slices.Concat([]string{"replicas-to-slo"}, sweepWindow(deadURL+secret)), 1},
		{"unreachable, capture", slices.Concat([]string{"capture"}, sweepWindow(deadURL+secret), []string{"-o", out}), 1},
		{"a failed query", slices.Concat([]string{"slo-to-rate"}, sweepWindow(failing.URL+secret), []string{"--tpot", "15ms"}), 1},
		{"a token over http", slices.Concat([]string{"replicas-to-slo"}, sweepWindow(srv.URL+secret), []string{"--bearer-token-file", token}), 2},
		{"a token over http, capture", slices.Concat([]string{"capture"}, sweepWindow(srv.URL+secret),
			[]string{"--bearer-token-file", token, "-o", out}), 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := runCLI(c.args...)
			require.Error(t, err)
			assert.Equal(t, c.code, exitCode(err), "%v", err)
			for _, leak := range []string{"s3cr3t", "pw d", "pw%20d", "token=", "k=", "k3ypath"} {
				assert.NotContains(t, err.Error(), leak)
			}
			if !strings.HasPrefix(c.name, "a failed query") {
				// An API error does not quote the URL; the others do.
				assert.Contains(t, err.Error(), "REDACTED", "%v", err)
			}
			if c.code == 1 {
				assert.True(t, errors.Is(err, whatifsource.ErrQueryFailed), "%v", err)
			}
			if c.args[0] == "capture" {
				return
			}
			js, code := runCmd(t, append(slices.Clone(c.args), "-o", "json")...)
			assert.Equal(t, c.code, code)
			assert.NotContains(t, js, "s3cr3t")
			assert.NotContains(t, js, "k3ypath")
			w := decodeOutput(t, js)
			require.NotNil(t, w.Error, js)
			assert.Equal(t, err.Error(), w.Error.Message)
		})
	}
}

// --bearer-token-file over https: every request carries the token, and
// nothing whatif prints, on stdout, stderr or in the error, in text or
// JSON, holds it, whether the answer is reached or a query fails. That the
// token goes to Prometheus' own host only, never on a redirect elsewhere,
// is TestClient_TheBearerTokenStaysOnItsHost's and
// TestBearer_SendsTheTokenOverHTTPSToItsHostOnly's.
func TestPrometheus_TheBearerTokenIsNeverPrinted(t *testing.T) {
	const token = "tok-9f8e7d6c5b4a"
	srv := sweepServer(t)
	failing := sweepServer(t)
	failing.Fail(queryOf(t, registration.QueryKvCacheUsage), "query processing would load too many samples")
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(token+"\n"), 0o600))
	for _, c := range []struct {
		name string
		prom *promtest.Server
		code int
	}{
		{"answered", srv, 0},
		{"a failed query", failing, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			secure := newAuthSeen(t, c.prom.Config.Handler.ServeHTTP)
			window := slices.Concat(sweepWindow(secure.URL), []string{"--bearer-token-file", tokenFile, "--ca-file", caFile(t, secure.Server)})
			for _, args := range [][]string{
				slices.Concat([]string{"replicas-to-slo"}, window),
				slices.Concat([]string{"slo-to-rate"}, window, []string{"--tpot", "15ms", "-o", "json"}),
				slices.Concat([]string{"capture"}, window, []string{"-o", filepath.Join(t.TempDir(), "c.gz")}),
			} {
				var out, warn bytes.Buffer
				err := run(context.Background(), args, &out, &warn)
				code := 0
				if err != nil {
					code = exitCode(err)
					assert.NotContains(t, err.Error(), token)
				}
				assert.Equal(t, c.code, code, "%v\n%s", err, out.String())
				assert.NotContains(t, out.String(), token)
				assert.NotContains(t, warn.String(), token)
			}
			seen := secure.headers()
			require.NotEmpty(t, seen)
			for _, h := range seen {
				assert.Equal(t, "Bearer "+token, h, "a request without the token")
			}
		})
	}
}

// The capture's caveats go to stderr in --prometheus mode, as generate
// prints them: -o json is still one object.
func TestPrometheus_CaveatsGoToStderr(t *testing.T) {
	srv := sweepServer(t)
	var out, warn bytes.Buffer
	args := slices.Concat([]string{"replicas-to-slo"}, sweepWindow(srv.URL), []string{"-o", "json"})
	require.NoError(t, run(context.Background(), args, &out, &warn))
	assert.Contains(t, warn.String(), "whatif: caveat: "+whatifsource.FlagNoReadiness)
	assert.Contains(t, warn.String(), "whatif: caveat: "+whatifsource.FlagNoRollouts)
	decodeOutput(t, out.String())
	assert.NotContains(t, out.String(), "caveat")
}

// Each exit code of the --prometheus mode is capture's or the question's:
// 2 for a usage error or the caller's input (a window too large without
// --force, a role map that attributes nothing), 1 for a failed query, 3
// for a refusal, 0 answered. With -o json each error is one JSON object.
func TestPrometheus_ExitCodes(t *testing.T) {
	srv := sweepServer(t)
	failing := sweepServer(t)
	failing.Fail(queryOf(t, registration.QueryKvCacheUsage), "query processing would load too many samples")
	from, to := sweepFrom.Format(time.RFC3339), sweepFrom.Add(time.Hour).Format(time.RFC3339)
	recording := filepath.Join(t.TempDir(), "r.jsonl")
	require.NoError(t, os.WriteFile(recording, []byte("{}\n"), 0o600))
	q2 := func(window []string, extra ...string) []string {
		return slices.Concat([]string{"replicas-to-slo"}, window, extra)
	}
	for _, c := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"answered", q2(sweepWindow(srv.URL)), 0, ""},
		{"refused", slices.Concat([]string{"slo-to-rate"}, sweepWindow(srv.URL), []string{"--tpot", "5ms"}), 3, "TargetBelowFloor"},
		{"usage: neither source", []string{"replicas-to-slo"}, 2, "--recording or --prometheus is required"},
		{"usage: both sources", q2(sweepWindow(srv.URL), "--recording", recording), 2, "give one"},
		{"usage: a window flag without --prometheus", []string{"replicas-to-slo", "--recording", recording, "--from", from}, 2,
			"--from is for --prometheus"},
		{"usage: --force without --prometheus", []string{"replicas-to-slo", "--recording", recording, "--force"}, 2,
			"--force is for --prometheus"},
		{"usage: no namespace", q2([]string{"--prometheus", srv.URL, "--role-of", "decode=app=m-dec", "--from", from, "--to", to}), 2,
			"replicas-to-slo needs --namespace"},
		{"usage: no window", q2([]string{"--prometheus", srv.URL, "--namespace", sweepNS, "--role-of", "decode=app=m-dec"}), 2,
			"needs --from and --to"},
		{"usage: no role map", q2([]string{"--prometheus", srv.URL, "--namespace", sweepNS, "--from", from, "--to", to}), 2,
			"cannot tell prefill from decode"},
		{"usage: a bad URL", q2(sweepWindow("ftp://prom")), 2, "not an http or https URL"},
		{"usage: a token over http", q2(sweepWindow(srv.URL), "--bearer-token-file", recording), 2, "https Prometheus only"},
		{"usage: a negative --batch-cap", q2(sweepWindow(srv.URL), "--batch-cap", "-1"), 2, "--batch-cap -1"},
		{"refused: TTFT with no prefill pod", q2(sweepWindow(srv.URL), "--prefill", "1"), 3, "NoPrefillReplicas"},
		{"input: nothing attributed", q2([]string{"--prometheus", srv.URL, "--namespace", sweepNS, "--role-of", "decode=app=nothing",
			"--from", from, "--to", to}), 2, "no Pod the role map attributes"},
		{"input: a window too large", q2([]string{"--prometheus", srv.URL, "--namespace", sweepNS, "--role-of", "decode=app=m-dec",
			"--from", from, "--to", sweepFrom.Add(1_000_000 * time.Second).Format(time.RFC3339), "--step", "1s"}), 2, "--force"},
		{"input: a model the window lacks", q2(sweepWindow(srv.URL), "--model", "nope"), 2, "nope"},
		{"input: a namespace the recording lacks", []string{"replicas-to-slo", "--recording", olsSweep, "--namespace", "nope"}, 2, "holds no saturation cycle"},
		{"query failure", q2(sweepWindow(failing.URL)), 1, "too many samples"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), c.args, &out, io.Discard)
			code := 0
			if err != nil {
				code = exitCode(err)
			}
			text := out.String()
			if err != nil {
				text += err.Error()
			}
			assert.Equal(t, c.code, code, "%s", text)
			assert.Contains(t, text, c.want)
			if c.code == 1 {
				assert.Equal(t, 1, strings.Count(err.Error(), "capture:"), "one prefix: %v", err)
				assert.True(t, errors.Is(err, whatifsource.ErrQueryFailed), "%v", err)
			}

			js, jsCode := runCmd(t, append(slices.Clone(c.args), "-o", "json")...)
			assert.Equal(t, c.code, jsCode)
			w := decodeOutput(t, js)
			assert.Equal(t, c.code, w.ExitCode)
			if c.code == 1 || c.code == 2 {
				require.NotNil(t, w.Error, js)
				assert.Contains(t, w.Error.Message, c.want)
				assert.Nil(t, w.Source, "an error object holds no source")
			} else {
				assert.Nil(t, w.Error, js)
			}
			if c.code == 3 {
				assert.Contains(t, js, c.want, "the refusal is in the JSON")
			}
		})
	}
	// The window too large was refused before anything was asked.
	for _, r := range srv.Requests() {
		assert.False(t, r.End.Equal(sweepFrom.Add(1_000_000*time.Second)), "the window too large was asked")
	}
}

// --force reaches the capture: the window bound is lifted, as capture's
// --force lifts it.
func TestPrometheus_ForceReachesTheCapture(t *testing.T) {
	for _, force := range []bool{false, true} {
		fs := newFlags("replicas-to-slo", io.Discard)
		f := answerFlags{question: "replicas-to-slo", warn: io.Discard}
		f.register(fs)
		args := sweepWindow("http://prom.example:9090")
		if force {
			args = append(args, "--force")
		}
		ok, err := parse(fs, args)
		require.True(t, ok)
		require.NoError(t, err)
		require.NoError(t, f.check(fs))
		opts, err := f.windowOptions()
		require.NoError(t, err)
		assert.Equal(t, force, opts.Force)
		assert.Equal(t, 64, opts.BatchCap, "--batch-cap is the capture's")
		assert.Zero(t, f.pricedBatchCap(), "--batch-cap is not also a cap to price at")
		assert.Equal(t, int64(64), f.batchCap, "check changes no flag")
		require.NoError(t, f.check(fs), "check again")
		again, err := f.windowOptions()
		require.NoError(t, err)
		assert.Equal(t, 64, again.BatchCap, "checked twice, the same capture")
		assert.Equal(t, defaultTimeout, f.window.timeout)
	}
}

// Every capture flag reaches the capture the --prometheus mode makes, as
// capture's own: the identity test cannot see a flag both paths lose, since
// they share captureFlags.options. --gpus-per-replica and --accelerator
// move no decode answer (they key the capacity store's reuse, not the live
// capacity priced), so this, and capture's window line, is where they are
// held.
func TestPrometheus_CaptureFlagsReachTheCapture(t *testing.T) {
	policy := sweepPolicy(t)
	fs := newFlags("slo-to-rate", io.Discard)
	f := answerFlags{question: "slo-to-rate", warn: io.Discard}
	f.register(fs)
	args := slices.Concat(sweepWindow("http://prom.example:9090"), []string{"--policy", policy, "--gpus-per-replica", "2",
		"--accelerator", "H100", "--engine", "sglang", "--engine-args", "--max-model-len=8192 --dp=2", "--kube-state-metrics",
		"--step", "30s", "--timeout", "7s"})
	ok, err := parse(fs, args)
	require.True(t, ok)
	require.NoError(t, err)
	require.NoError(t, f.check(fs))
	opts, err := f.windowOptions()
	require.NoError(t, err)
	require.NotNil(t, opts.Policy)
	assert.InDelta(t, 0.6, opts.Policy.ScaleUpThreshold, 0)
	assert.Equal(t, 2, opts.GPUsPerReplica)
	assert.Equal(t, "H100", opts.Accelerator)
	assert.Equal(t, inferenceengine.EngineSGLang, opts.Engine)
	assert.Equal(t, []string{"--max-model-len=8192", "--dp=2"}, opts.EngineArgs)
	assert.True(t, opts.KubeStateMetrics)
	assert.Equal(t, 30*time.Second, opts.Step)
	assert.Equal(t, sweepNS, opts.Namespace)
	assert.Equal(t, 64, opts.BatchCap)
	assert.Equal(t, 7*time.Second, f.window.timeout)

	// And in a real capture, the window line holds the GPUs and the
	// accelerator the flags gave.
	srv := sweepServerFor(t, inferenceengine.EngineSGLang)
	out := filepath.Join(t.TempDir(), "c.gz")
	text, err := runCLI(slices.Concat([]string{"capture"}, sweepWindow(srv.URL),
		[]string{"--gpus-per-replica", "2", "--accelerator", "H100", "--engine", "sglang", "-o", out})...)
	require.NoError(t, err)
	assert.Contains(t, text, "m/m-dec  decode  2 GPU H100")
}

// A capture's warnings go to stderr, so that -o json is still one object.
func TestPrometheus_WarningsGoToStderr(t *testing.T) {
	srv := sweepServer(t)
	var out, warn bytes.Buffer
	args := slices.Concat([]string{"replicas-to-slo"}, sweepWindow(srv.URL), []string{"--engine-args", "--max-num-seqs=64", "-o", "json"})
	require.NoError(t, run(context.Background(), args, &out, &warn))
	assert.Contains(t, warn.String(), "both set the batch cap")
	decodeOutput(t, out.String())
}

// Help lists the --prometheus mode's flags beside the question's.
func TestPrometheus_HelpListsTheWindowFlags(t *testing.T) {
	for _, cmd := range []string{"replicas-to-slo", "slo-to-rate"} {
		text, code := runCmd(t, cmd, "-h")
		require.Equal(t, 0, code)
		for _, name := range windowFlags() {
			assert.Regexp(t, "(?m)^  -"+name+"( |$)", text, "%s help lists -%s", cmd, name)
		}
		assert.Contains(t, text, "-recording")
	}
	// windowFlags is every flag capture has but --prometheus, --namespace,
	// --batch-cap and -o: a flag added to capture is refused without
	// --prometheus.
	text, code := runCmd(t, "capture", "-h")
	require.Equal(t, 0, code)
	var got []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "  -") {
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "  -"), " ")
			if !slices.Contains([]string{"prometheus", "namespace", "batch-cap", "o"}, name) {
				got = append(got, name)
			}
		}
	}
	want := windowFlags()
	slices.Sort(want)
	assert.Equal(t, want, got)
	assert.NotContains(t, want, "prometheus")
	assert.Len(t, want, 15)
}

// queryOf is a registered query's PromQL in the sweep's namespace.
func queryOf(t *testing.T, name string) string {
	t.Helper()
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	s, err := reg.Get("prometheus").QueryList().Build(name, map[string]string{source.ParamNamespace: sweepNS})
	require.NoError(t, err)
	return s
}
