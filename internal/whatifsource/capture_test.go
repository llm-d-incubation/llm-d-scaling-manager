package whatifsource

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// stepValue is a series whose value at each step is the step's index.
func stepValue(labels map[string]string) promtest.Series {
	return promtest.Series{Labels: labels, Value: func(at time.Time) (float64, bool) {
		return float64(at.Sub(t0)/(15*time.Second)) / 100, true
	}}
}

func TestCapture_EveryStepReadsItsOwnSamplesAcrossChunkedRangeQueries(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	srv.Set(promQL(t, registration.QueryKvCacheUsage), stepValue(podLabels(testPod)))
	srv.Set(promQL(t, registration.QueryQueueLength), promtest.Const(podLabels(testPod), 1))
	opts := roleOfOpts(t, srv)
	opts.To = t0.Add(24 * 15 * time.Second)
	opts.RangeQueryPoints = 10
	d, sum := capture(t, opts)

	require.Equal(t, 25, sum.Steps)
	require.Len(t, d.cycles, 25)
	for i, c := range d.cycles {
		assert.True(t, c.At.Equal(t0.Add(time.Duration(i)*15*time.Second)), "cycle %d at %s", i, c.At)
		require.Len(t, c.Input.ReplicaMetrics, 1)
		assert.InDelta(t, float64(i)/100, c.Input.ReplicaMetrics[0].KvCacheUsage, 1e-12, "cycle %d read another step's sample", i)
		// Stamped at the step, so the rows read fresh.
		require.NotNil(t, c.Input.ReplicaMetrics[0].Metadata)
		assert.True(t, c.Input.ReplicaMetrics[0].Metadata.CollectedAt.Equal(c.At))
		assert.Equal(t, "fresh", c.Input.ReplicaMetrics[0].Metadata.FreshnessStatus)
	}

	// Every request is a range query, at the capture's step, and the window
	// is asked in three parts of at most ten steps, joined with no gap.
	kv := promQL(t, registration.QueryKvCacheUsage)
	var parts [][2]time.Time
	for _, r := range srv.Requests() {
		require.Equal(t, "/api/v1/query_range", r.Path, "a capture asks range queries only")
		assert.Equal(t, 15*time.Second, r.Step)
		if r.Query == kv {
			parts = append(parts, [2]time.Time{r.Start, r.End})
		}
	}
	require.Len(t, parts, 3, "the KV query is asked once per part, not once per step")
	assert.True(t, parts[0][0].Equal(t0))
	assert.True(t, parts[2][1].Equal(opts.To))
	for i := 1; i < len(parts); i++ {
		assert.Equal(t, 15*time.Second, parts[i][0].Sub(parts[i-1][1]), "part %d does not follow part %d by one step", i, i-1)
	}
	assert.Equal(t, len(srv.Queries()), sum.RangeQueries, "each expression counts once, whatever its parts")
}

func TestCapture_StartWindowAndPolicy(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	baseFleet(t, srv, testPod)
	opts := roleOfOpts(t, srv, "decode=app=m-dec")
	opts.Step = 30 * time.Second
	opts.EngineArgs = []string{"--max-model-len=8192"}
	opts.BatchCap = 48
	opts.Accelerator = "NVIDIA-H100-80GB-HBM3"
	opts.GPUsPerReplica = 2
	p, err := config.DecodeScalingPolicy("analyzers:\n- name: saturation\n- name: slo\n- name: throughput\n")
	require.NoError(t, err)
	opts.Policy = &p
	d, _ := capture(t, opts)

	// The start line: horizons, the borrow gap a 30 s interval sets
	// (max(BorrowedK2TickGap, 3 x 30 s)), the slo analyzer judging, and
	// throughput registered because the policy lists it, as cmd/main
	// registers it.
	assert.Equal(t, max(saturation.BorrowedK2TickGap, 90*time.Second), d.start.Saturation.BorrowTickGap)
	require.NotNil(t, d.start.SLO)
	assert.True(t, d.start.SLO.Judging)
	assert.True(t, d.start.Throughput)
	require.NotNil(t, d.start.ThroughputConfig)
	assert.Equal(t, 30*time.Second, d.start.ThroughputConfig.OptimizationInterval)
	assert.Zero(t, d.start.StoreRecords)

	assert.Equal(t, testNS, d.window.Namespace)
	assert.Equal(t, 30*time.Second, d.window.Step)
	assert.Equal(t, "role-of", d.window.Roles)
	assert.Contains(t, d.window.Flags, FlagRolesInferred)
	assert.Contains(t, d.window.Flags, FlagNoReadiness)
	assert.Contains(t, d.window.Flags, FlagParamsFromFlags)
	require.Len(t, d.window.Variants, 1)
	v := d.window.Variants[0]
	assert.Equal(t, "m-dec", v.Name)
	assert.Equal(t, testModel, v.ModelID)
	assert.Equal(t, domain.RoleDecode, v.Role)
	assert.Equal(t, 2, v.GpuCount)
	assert.Equal(t, "NVIDIA-H100-80GB-HBM3", v.Accelerator)
	assert.Equal(t, int64(48), v.Params.MaxNumSeqs)
	assert.Equal(t, int64(8192), v.Params.MaxModelLen)

	require.Len(t, d.cycles, 1)
	in := d.cycles[0].Input
	assert.Equal(t, "*config.ScalingPolicy", in.ConfigType)
	require.NotNil(t, in.Config)
	assert.True(t, in.Config.AnalyzerEnabled("slo"), "the cycle carries the merged policy")
	require.Len(t, in.VariantStates, 1)
	assert.Equal(t, domain.VariantReplicaState{VariantName: "m-dec", CurrentReplicas: 1, DesiredReplicas: 1,
		GPUsPerReplica: 2, Role: domain.RoleDecode, AcceleratorName: "NVIDIA-H100-80GB-HBM3"}, in.VariantStates[0])
	// Without kube-state-metrics every Pod is Ready, with no Ready time.
	row := in.ReplicaMetrics[0]
	assert.True(t, row.Ready)
	assert.True(t, row.ReadySince.IsZero())
	assert.Equal(t, "m-dec", row.VariantName)
}

func TestCapture_TheSameWindowCapturesToTheSameBytes(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	for _, pod := range []string{"m-dec-7c9d-x1", "m-dec-7c9d-x2", "m-dec-7c9d-x3", "m-pre-1a2b-y1"} {
		baseFleet(t, srv, pod)
	}
	opts := roleOfOpts(t, srv, "decode=app=m-dec", "prefill=app=m-pre")
	opts.To = t0.Add(time.Minute)
	var a, b bytes.Buffer
	_, err := Capture(context.Background(), opts, &a)
	require.NoError(t, err)
	_, err = Capture(context.Background(), opts, &b)
	require.NoError(t, err)
	assert.Equal(t, gunzip(t, a.Bytes()), gunzip(t, b.Bytes()))
	d := decodeCapture(t, a.Bytes())
	rows := d.cycles[0].Input.ReplicaMetrics
	require.Len(t, rows, 4)
	assert.Equal(t, []string{"m-dec-7c9d-x1", "m-dec-7c9d-x2", "m-dec-7c9d-x3", "m-pre-1a2b-y1"},
		[]string{rows[0].PodName, rows[1].PodName, rows[2].PodName, rows[3].PodName}, "rows in variant and Pod order")
}

func TestCapture_Errors(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	base := func() Options { return roleOfOpts(t, srv) }
	for _, c := range []struct {
		name string
		edit func(o *Options)
		want string
	}{
		{"no role map", func(o *Options) { o.RoleOf = nil }, "cannot tell prefill from decode"},
		{"no namespace", func(o *Options) { o.Namespace = "" }, "no namespace"},
		{"no step", func(o *Options) { o.Step = 0 }, "not positive"},
		{"ends before it starts", func(o *Options) { o.To = o.From.Add(-time.Second) }, "before it starts"},
		{"no Prometheus", func(o *Options) { o.Prometheus = nil }, "no Prometheus"},
		{"nothing attributed", func(o *Options) {}, "no Pod the role map attributes had a KV-cache series, in namespace llm"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := base()
			c.edit(&o)
			var buf bytes.Buffer
			_, err := Capture(context.Background(), o, &buf)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
	o := base()
	o.RoleOf = nil
	_, err := Capture(context.Background(), o, &bytes.Buffer{})
	assert.True(t, errors.Is(err, ErrNoRoleMap))

	t.Run("a failed query fails the capture", func(t *testing.T) {
		srv := promtest.New()
		defer srv.Close()
		baseFleet(t, srv, testPod)
		srv.Fail(promQL(t, registration.QueryKvCacheUsage), "query timed out")
		_, err := Capture(context.Background(), roleOfOpts(t, srv), &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "query timed out")
	})
}

func TestCapture_ParkedVariantsWithoutKubeStateMetrics(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	// m-pre's Pod reports only at the first step.
	srv.Add(promQL(t, registration.QueryKvCacheUsage), promtest.Const(podLabels(testPod), 0.5),
		promtest.Series{Labels: podLabels("m-pre-1a2b-y1"), Value: func(at time.Time) (float64, bool) { return 0.3, at.Equal(t0) }})
	srv.Add(promQL(t, registration.QueryQueueLength), promtest.Const(podLabels(testPod), 0))
	opts := roleOfOpts(t, srv, "decode=app=m-dec", "prefill=app=m-pre")
	opts.To = t0.Add(15 * time.Second)
	d, _ := capture(t, opts)
	require.Len(t, d.cycles, 2)
	assert.Empty(t, d.cycles[0].Parked)
	assert.Len(t, d.cycles[0].Input.VariantStates, 2)
	assert.Equal(t, []string{"m-pre"}, d.cycles[1].Parked)
	require.Len(t, d.cycles[1].Input.VariantStates, 1)
	assert.Equal(t, "m-dec", d.cycles[1].Input.VariantStates[0].VariantName)
}
