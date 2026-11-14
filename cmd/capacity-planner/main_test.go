package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

var cliFrom = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fleetServer is a Prometheus with one decode Pod of model m in namespace
// llm, reporting at every instant.
func fleetServer(t *testing.T) *promtest.Server {
	t.Helper()
	srv := promtest.New()
	t.Cleanup(srv.Close)
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	q := func(name string) string {
		s, err := reg.Get("prometheus").QueryList().Build(name, map[string]string{source.ParamNamespace: "llm"})
		require.NoError(t, err)
		return s
	}
	labels := map[string]string{"model_name": "m", "pod": "m-dec-7c9d-x1", "instance": "10.0.0.1:8000", "namespace": "llm"}
	srv.Set(q(registration.QueryKvCacheUsage), promtest.Const(labels, 0.4))
	srv.Set(q(registration.QueryQueueLength), promtest.Const(labels, 1))
	return srv
}

// runCLI runs capacity-planner with args and returns its output and error; what it
// warns about is dropped (runCLIWarn keeps it).
func runCLI(args ...string) (string, error) {
	var out bytes.Buffer
	err := run(context.Background(), args, &out, io.Discard)
	return out.String(), err
}

func captureArgs(prom, out string, extra ...string) []string {
	return append([]string{"capture", "--prometheus", prom, "--namespace", "llm", "--role-of", "decode=app=m-dec",
		"--from", cliFrom.Format(time.RFC3339), "--to", cliFrom.Add(time.Minute).Format(time.RFC3339), "-o", out}, extra...)
}

func TestCapture_WritesTheWindowAtItsStep(t *testing.T) {
	srv := fleetServer(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "run.capture.jsonl.gz")
	text, err := runCLI(captureArgs(srv.URL, out, "--step", "30s")...)
	require.NoError(t, err)
	assert.Contains(t, text, "captured 3 steps of llm")
	assert.Contains(t, text, "m/m-dec  decode  1 GPU")
	assert.Contains(t, text, "flag: roles inferred from Pod names")

	// Every request a range query over exactly the window, at the step.
	reqs := srv.Requests()
	require.NotEmpty(t, reqs)
	for _, r := range reqs {
		assert.Equal(t, "/api/v1/query_range", r.Path)
		assert.True(t, r.Start.Equal(cliFrom), "start %s", r.Start)
		assert.True(t, r.End.Equal(cliFrom.Add(time.Minute)), "end %s", r.End)
		assert.Equal(t, 30*time.Second, r.Step)
	}

	capture, err := readMaybeGzipped(out)
	require.NoError(t, err)
	read, err := learnrec.Read(capture)
	require.NoError(t, err)
	require.Len(t, read.Events, 2+3)
	var window learnreplay.CaptureWindow
	require.NoError(t, read.Events[1].DecodeData(&window))
	assert.Equal(t, 30*time.Second, window.Step)
	for i, ev := range read.Events[2:] {
		assert.True(t, ev.Now.Equal(cliFrom.Add(time.Duration(i)*30*time.Second)), "cycle %d at %s", i, ev.Now)
	}

	// generate turns it into a recording that replays at this commit.
	rec := filepath.Join(dir, "run.jsonl")
	text, err = runCLI("generate", "-in", out, "-o", rec)
	require.NoError(t, err)
	assert.Contains(t, text, "generated 3 steps")
	assert.Contains(t, text, "caveat: roles inferred from Pod names")
	b, err := os.ReadFile(rec)
	require.NoError(t, err)
	rep, err := learnreplay.Replay(context.Background(), b, learnreplay.Options{Mode: learnreplay.Fidelity})
	require.NoError(t, err)
	assert.Empty(t, rep.Failed())
	assert.Equal(t, 3, rep.Analyzes[saturation.KindAnalyze])

	// A .gz output is gzipped.
	gz := filepath.Join(dir, "run.jsonl.gz")
	_, err = runCLI("generate", "-in", out, "-o", gz)
	require.NoError(t, err)
	b, err = readMaybeGzipped(gz)
	require.NoError(t, err)
	assert.Equal(t, string(rep.Recording), string(b))
}

func TestCapture_UnixSecondsAndAPolicy(t *testing.T) {
	srv := fleetServer(t)
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.yaml")
	require.NoError(t, os.WriteFile(policy, []byte("analyzers:\n- name: saturation\n- name: throughput\n"), 0o600))
	out := filepath.Join(dir, "c.gz")
	from := strconv.FormatInt(cliFrom.Unix(), 10)
	to := strconv.FormatInt(cliFrom.Add(15*time.Second).Unix(), 10)
	_, err := runCLI("capture", "--prometheus", srv.URL, "--namespace", "llm", "--role-of", "decode=app=m-dec",
		"--from", from, "--to", to, "--policy", policy, "--batch-cap", "32", "--engine-args", "--max-model-len=4096", "-o", out)
	require.NoError(t, err)
	capture, err := readMaybeGzipped(out)
	require.NoError(t, err)
	read, err := learnrec.Read(capture)
	require.NoError(t, err)
	var start learnrec.Start
	require.NoError(t, read.Events[0].DecodeData(&start))
	assert.True(t, start.Throughput, "the policy lists throughput")
	var window learnreplay.CaptureWindow
	require.NoError(t, read.Events[1].DecodeData(&window))
	require.Len(t, window.Variants, 1)
	assert.Equal(t, int64(32), window.Variants[0].Params.MaxNumSeqs)
	assert.Equal(t, int64(4096), window.Variants[0].Params.MaxModelLen)
	assert.Len(t, read.Events, 2+2)
}

func TestCapture_UsageErrors(t *testing.T) {
	srv := fleetServer(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "c.gz")
	from, to := cliFrom.Format(time.RFC3339), cliFrom.Add(time.Minute).Format(time.RFC3339)
	badPolicy := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(badPolicy, []byte("scaleUpThreshhold: 0.9\n"), 0o600))
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "usage: capacity-planner"},
		{"unknown command", []string{"answer"}, "unknown command"},
		{"no prometheus", []string{"capture", "--namespace", "llm", "--role-of", "decode=app=x", "--from", from, "--to", to, "-o", out}, "--prometheus"},
		{"no namespace", []string{"capture", "--prometheus", srv.URL, "--role-of", "decode=app=x", "--from", from, "--to", to, "-o", out}, "--namespace"},
		{"no window", []string{"capture", "--prometheus", srv.URL, "--namespace", "llm", "--role-of", "decode=app=x", "-o", out}, "--from and --to"},
		{"no output", []string{"capture", "--prometheus", srv.URL, "--namespace", "llm", "--role-of", "decode=app=x", "--from", from, "--to", to}, "-o"},
		{"no role map", []string{"capture", "--prometheus", srv.URL, "--namespace", "llm", "--from", from, "--to", to, "-o", out}, "cannot tell prefill from decode"},
		{"a bad role", captureArgs(srv.URL, out, "--role-of", "worker=app=x"), "is not prefill, decode or both"},
		{"a bad time", []string{"capture", "--prometheus", srv.URL, "--namespace", "llm", "--role-of", "decode=app=x", "--from", "yesterday", "--to", to, "-o", out}, "--from"},
		{"to before from", []string{"capture", "--prometheus", srv.URL, "--namespace", "llm", "--role-of", "decode=app=x", "--from", to, "--to", from, "-o", out}, "is not after --from"},
		{"zero step", captureArgs(srv.URL, out, "--step", "0s"), "not positive"},
		{"a bad step", captureArgs(srv.URL, out, "--step", "soon"), "invalid value"},
		{"a bad engine", captureArgs(srv.URL, out, "--engine", "tgi"), "not vllm or sglang"},
		{"no GPUs", captureArgs(srv.URL, out, "--gpus-per-replica", "0"), "at least 1"},
		{"a bad URL", captureArgs("ftp://prom", out), "not an http or https URL"},
		{"a missing policy", captureArgs(srv.URL, out, "--policy", filepath.Join(dir, "none.yaml")), "none.yaml"},
		{"an unknown policy key", captureArgs(srv.URL, out, "--policy", badPolicy), "scaleUpThreshhold"},
		{"a stray argument", append(captureArgs(srv.URL, out), "extra"), "unexpected argument"},
		{"generate needs its flags", []string{"generate", "-in", out}, "-in and -o"},
		{"generate of a missing file", []string{"generate", "-in", filepath.Join(dir, "none"), "-o", out}, "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := runCLI(c.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
			assert.Equal(t, 2, exitCode(err), "a usage error exits 2")
		})
	}
	_, err := os.Stat(out)
	assert.True(t, os.IsNotExist(err), "no failed capture leaves a file")
}

func TestCapture_APrometheusErrorExitsOneAndLeavesNoFile(t *testing.T) {
	srv := fleetServer(t)
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	kv, err := reg.Get("prometheus").QueryList().Build(registration.QueryKvCacheUsage, map[string]string{source.ParamNamespace: "llm"})
	require.NoError(t, err)
	srv.Fail(kv, "query processing would load too many samples")
	out := filepath.Join(t.TempDir(), "c.gz")
	_, err = runCLI(captureArgs(srv.URL, out)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many samples")
	assert.Equal(t, 1, exitCode(err))
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr))
}

func TestCapture_HelpIsNotAnError(t *testing.T) {
	text, err := runCLI("capture", "-h")
	require.NoError(t, err)
	for _, flag := range []string{"-prometheus", "-namespace", "-kube-state-metrics", "-role-of", "-from", "-to", "-step",
		"-policy", "-bearer-token-file", "-ca-file", "-insecure-skip-verify", "-batch-cap", "-engine-args", "-timeout", "-force"} {
		assert.True(t, strings.Contains(text, flag), "help lists %s", flag)
	}
}
