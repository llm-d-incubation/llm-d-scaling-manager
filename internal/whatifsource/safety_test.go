package whatifsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// tooManySamples is the error Prometheus answers a query over its
// query.max-samples with.
const tooManySamples = "query processing would load too many samples into memory in query execution"

// A query other than the KV and kube-state-metrics ones failing fails the
// capture, naming the query, and at once: the collector's retries stop on
// the first failure instead of sleeping through their backoff each step.
func TestCapture_AnyFailedQueryFailsTheCaptureAtOnce(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	baseFleet(t, srv, testPod)
	ttft := promQL(t, registration.QueryTTFTBuckets)
	srv.Fail(ttft, tooManySamples)
	opts := roleOfOpts(t, srv)
	opts.To = t0.Add(4 * 15 * time.Second)

	began := time.Now()
	sum, err := Capture(context.Background(), opts, &bytes.Buffer{})
	took := time.Since(began)
	require.Error(t, err, "the capture went on without the TTFT histogram: %+v", sum)
	assert.Contains(t, err.Error(), tooManySamples)
	assert.Contains(t, err.Error(), fmt.Sprintf("%q", ttft), "the error names the query")
	assert.Contains(t, err.Error(), "every query a capture asks is required")
	assert.Less(t, took, 3*time.Second, "a failed query must not be retried with backoff")
	asked := 0
	for _, r := range srv.Requests() {
		if r.Query == ttft {
			asked++
		}
	}
	assert.Equal(t, 1, asked, "the failed query was asked again")
}

// --role-of selectors that can pick the same Pod are refused: nested name
// prefixes before anything is asked, and a Pod two selectors' labels match
// when it is read.
func TestCapture_OverlappingRoleOfIsRefused(t *testing.T) {
	t.Run("nested name prefixes", func(t *testing.T) {
		srv := promtest.New()
		defer srv.Close()
		baseFleet(t, srv, testPod)
		opts := roleOfOpts(t, srv, "decode=app=m", "prefill=app=m-dec")
		_, err := Capture(context.Background(), opts, &bytes.Buffer{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRoleOverlap), "%v", err)
		assert.Contains(t, err.Error(), `"m-"`)
		assert.Empty(t, srv.Requests(), "refused before any query")
	})
	t.Run("a Pod two label selectors match", func(t *testing.T) {
		srv := promtest.New()
		defer srv.Close()
		baseFleet(t, srv, testPod)
		addKSMPod(srv, ksmPod{name: testPod, deployment: "m-dec", hash: "7c9d", node: "n1", ready: true,
			created: t0.Add(-time.Hour), started: t0.Add(-time.Hour), readySince: t0.Add(-time.Hour), gpus: 1})
		srv.Add(KSMQueries(testNS).PodLabels, promtest.Const(map[string]string{"namespace": testNS, "pod": testPod,
			"label_app": "m-dec", "label_tier": "gpu"}, 1))
		opts := roleOfOpts(t, srv, "decode=app=m-dec", "prefill=tier=gpu")
		opts.KubeStateMetrics = true
		_, err := Capture(context.Background(), opts, &bytes.Buffer{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRoleOverlap), "%v", err)
		assert.Contains(t, err.Error(), testPod)
		assert.Contains(t, err.Error(), "decode=app=m-dec and prefill=tier=gpu")
	})
}

// A variant's replicas are its engine Pods, the ones with a KV series: the
// router's Pod a name prefix also picks is not one.
func TestCapture_CurrentReplicasCountsEnginePodsOnly(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	engine := ksmPod{name: "m-7c9d-x1", deployment: "m", hash: "7c9d", node: "n1", ready: true,
		created: t0.Add(-time.Hour), started: t0.Add(-time.Hour), readySince: t0.Add(-time.Hour), gpus: 1}
	router := ksmPod{name: "m-epp-5f6-z1", deployment: "m-epp", hash: "5f6", node: "n1", ready: true,
		created: t0.Add(-time.Hour), started: t0.Add(-time.Hour), readySince: t0.Add(-time.Hour)}
	addKSMPod(srv, engine)
	addKSMPod(srv, router)
	baseFleet(t, srv, engine.name)
	opts := roleOfOpts(t, srv, "decode=app=m")
	opts.KubeStateMetrics = true
	d, _ := capture(t, opts)
	require.Len(t, d.cycles, 1)
	require.Len(t, d.cycles[0].Input.VariantStates, 1)
	vs := d.cycles[0].Input.VariantStates[0]
	assert.Equal(t, "m", vs.VariantName)
	assert.Equal(t, 1, vs.CurrentReplicas, "the router's Pod counted as a replica")
	assert.Equal(t, 1, vs.DesiredReplicas)
}

// What a capture cannot see is flagged: rollouts always, and multi-Pod
// groups where kube-state-metrics shows them.
func TestCapture_FlagsRolloutsAndMultiPodGroups(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	pod := "m-lws-0-1"
	baseFleet(t, srv, pod)
	k := KSMQueries(testNS)
	l := map[string]string{"namespace": testNS, "pod": pod}
	srv.Add(k.PodNode, promtest.Const(map[string]string{"namespace": testNS, "pod": pod, "node": "n1"}, 1))
	srv.Add(k.PodOwner, promtest.Const(map[string]string{"namespace": testNS, "pod": pod,
		"owner_kind": "StatefulSet", "owner_name": "m-lws-0"}, 1))
	srv.Add(k.PodRunning, promtest.Const(l, 1))
	srv.Add(k.PodReady, promtest.Const(l, 1))
	srv.Add(k.PodGPUs, promtest.Const(map[string]string{"pod": pod}, 4))
	opts := roleOfOpts(t, srv, "decode=app=m-lws")
	opts.KubeStateMetrics = true
	d, sum := capture(t, opts)
	assert.Contains(t, d.window.Flags, FlagNoRollouts)
	assert.Contains(t, d.window.Flags, "multi-Pod groups in 1 variants: GPUs per replica read from one Pod's request, not the group's")
	assert.Equal(t, d.window.Flags, sum.Flags)

	// A Deployment's Pods are not in groups.
	srv2 := promtest.New()
	defer srv2.Close()
	baseFleet(t, srv2, testPod)
	d, _ = capture(t, roleOfOpts(t, srv2))
	assert.Contains(t, d.window.Flags, FlagNoRollouts)
	for _, f := range d.window.Flags {
		assert.NotContains(t, f, "multi-Pod")
	}
}

// shifted answers range queries with every sample moved off the step grid,
// as a query frontend that aligns start to step does.
type shifted struct {
	promv1.API
	by time.Duration
}

func (s shifted) QueryRange(ctx context.Context, q string, r promv1.Range, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	v, w, err := s.API.QueryRange(ctx, q, r, opts...)
	if m, ok := v.(model.Matrix); ok {
		for _, stream := range m {
			for i := range stream.Values {
				stream.Values[i].Timestamp = stream.Values[i].Timestamp.Add(s.by)
			}
		}
	}
	return v, w, err
}

// Samples off the step grid fail the capture, naming the cause; a start
// finer than a millisecond is truncated to one, onto the grid.
func TestCapture_TheStepGrid(t *testing.T) {
	t.Run("off the grid", func(t *testing.T) {
		srv := promtest.New()
		defer srv.Close()
		baseFleet(t, srv, testPod)
		opts := roleOfOpts(t, srv)
		opts.Prometheus = shifted{API: opts.Prometheus, by: 5 * time.Second}
		_, err := Capture(context.Background(), opts, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "off the step grid")
		assert.Contains(t, err.Error(), "aligns start to step")
	})
	t.Run("a start finer than a millisecond", func(t *testing.T) {
		srv := promtest.New()
		defer srv.Close()
		baseFleet(t, srv, testPod)
		opts := roleOfOpts(t, srv)
		opts.From = t0.Add(400 * time.Microsecond)
		opts.To = t0.Add(15*time.Second + 400*time.Microsecond)
		d, _ := capture(t, opts)
		require.Len(t, d.cycles, 2)
		assert.True(t, d.cycles[0].At.Equal(t0), "cycle at %s", d.cycles[0].At)
		assert.True(t, d.window.From.Equal(t0), "window from %s", d.window.From)
		require.Len(t, d.cycles[1].Input.ReplicaMetrics, 1)
	})
	t.Run("a step finer than a millisecond", func(t *testing.T) {
		srv := promtest.New()
		defer srv.Close()
		opts := roleOfOpts(t, srv)
		opts.Step = 15*time.Second + time.Microsecond
		_, err := Capture(context.Background(), opts, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "whole number of milliseconds")
	})
}

// A window of more steps times queries than MaxStepQueries is refused
// before anything is asked, unless forced.
func TestCapture_AWindowTooLargeIsRefused(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	opts := roleOfOpts(t, srv)
	opts.Step = time.Second
	opts.To = t0.Add(MaxStepQueries * time.Second)
	_, err := Capture(context.Background(), opts, &bytes.Buffer{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWindowTooLarge), "%v", err)
	assert.Contains(t, err.Error(), "--force")
	assert.Empty(t, srv.Requests())
}

// The samples a capture holds are counted as they arrive, and a capture
// past MaxSamples stops, unless forced.
func TestCapture_TheSamplesHeldAreBounded(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	baseFleet(t, srv, testPod)
	opts := roleOfOpts(t, srv)
	opts.To = t0.Add(4 * 15 * time.Second)
	opts.MaxSamples = 6
	_, err := Capture(context.Background(), opts, &bytes.Buffer{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWindowTooLarge), "%v", err)
	assert.True(t, IsInputError(err))
	assert.Contains(t, err.Error(), "past 6 samples")

	opts.Force = true
	_, sum := capture(t, opts)
	assert.Greater(t, sum.Samples, 6, "the forced capture held more than the bound")
}

// Each error a capture fails with is the caller's input or Prometheus'.
func TestCapture_ErrorsSayWhoseFaultTheyAre(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	for _, c := range []struct {
		name  string
		edit  func(o *Options)
		is    error
		input bool
	}{
		{"no namespace", func(o *Options) { o.Namespace = "" }, ErrInvalidOptions, true},
		{"a step under a millisecond", func(o *Options) { o.Step = time.Microsecond }, ErrInvalidOptions, true},
		{"no role map", func(o *Options) { o.RoleOf = nil }, ErrNoRoleMap, true},
		{"nested prefixes", func(o *Options) {
			r, err := ParseRoleOf("prefill=app=m-dec-p")
			require.NoError(t, err)
			o.RoleOf = append(o.RoleOf, r)
		}, ErrRoleOverlap, true},
		{"nothing attributed", func(*Options) {}, ErrNothingAttributed, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := roleOfOpts(t, srv)
			c.edit(&o)
			_, err := Capture(context.Background(), o, &bytes.Buffer{})
			require.Error(t, err)
			assert.True(t, errors.Is(err, c.is), "%v", err)
			assert.Equal(t, c.input, IsInputError(err))
		})
	}
	t.Run("a failed query", func(t *testing.T) {
		srv := promtest.New()
		defer srv.Close()
		baseFleet(t, srv, testPod)
		srv.Fail(promQL(t, registration.QueryAvgTTFT), "query timed out")
		_, err := Capture(context.Background(), roleOfOpts(t, srv), &bytes.Buffer{})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrQueryFailed), "%v", err)
		assert.False(t, IsInputError(err))
	})
}
