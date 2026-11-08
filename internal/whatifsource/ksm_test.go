package whatifsource

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// ksmPod is one Pod as kube-state-metrics exports it.
type ksmPod struct {
	name, deployment, hash, node, role string
	ready                              bool
	created, started, readySince       time.Time
	gpus                               float64
	// during says at which instants kube-state-metrics lists the Pod; nil
	// for always.
	during func(at time.Time) bool
}

// addKSMPod sets every kube-state-metrics series of p.
func addKSMPod(srv *promtest.Server, p ksmPod) {
	k := KSMQueries(testNS)
	at := func(v float64) func(time.Time) (float64, bool) {
		return func(t time.Time) (float64, bool) { return v, p.during == nil || p.during(t) }
	}
	series := func(q string, v float64, labels ...string) {
		l := map[string]string{"namespace": testNS, "pod": p.name}
		for i := 0; i+1 < len(labels); i += 2 {
			l[labels[i]] = labels[i+1]
		}
		srv.Add(q, promtest.Series{Labels: l, Value: at(v)})
	}
	unix := func(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }
	series(k.PodNode, 1, "node", p.node)
	labels := []string{}
	if p.role != "" {
		labels = append(labels, "label_llm_d_ai_role", p.role)
	}
	series(k.PodLabels, 1, labels...)
	rs := p.deployment + "-" + p.hash
	series(k.PodOwner, 1, "owner_kind", "ReplicaSet", "owner_name", rs)
	srv.Add(k.ReplicaSetOwner, promtest.Const(map[string]string{"namespace": testNS, "replicaset": rs,
		"owner_kind": "Deployment", "owner_name": p.deployment}, 1))
	series(k.PodRunning, 1)
	ready := 0.0
	if p.ready {
		ready = 1
		series(k.PodReadyTime, unix(p.readySince))
	}
	series(k.PodReady, ready)
	series(k.PodCreated, unix(p.created))
	series(k.PodStarted, unix(p.started))
	if p.gpus > 0 {
		series(k.PodGPUs, p.gpus)
	}
}

// addKSMNode labels node with its accelerator.
func addKSMNode(srv *promtest.Server, node, accel string) {
	srv.Add(KSMQueries(testNS).NodeLabels, promtest.Const(map[string]string{"node": node,
		"label_nvidia_com_gpu_product": accel}, 1))
}

// addKSMDeployment sets a Deployment's spec replicas over time.
func addKSMDeployment(srv *promtest.Server, name string, replicas func(at time.Time) float64) {
	srv.Add(KSMQueries(testNS).DeploymentReplicas, promtest.Series{
		Labels: map[string]string{"namespace": testNS, "deployment": name},
		Value:  func(at time.Time) (float64, bool) { return replicas(at), true }})
}

func TestCapture_KubeStateMetricsGivesRolesReadinessGPUsAndParking(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	created := t0.Add(-10 * time.Minute)
	step1 := t0.Add(15 * time.Second)
	ready := ksmPod{name: "m-dec-7c9d-x1", deployment: "m-dec", hash: "7c9d", node: "n1", role: "decode", ready: true,
		created: created, started: created.Add(5 * time.Second), readySince: created.Add(80 * time.Second), gpus: 2}
	starting := ksmPod{name: "m-dec-7c9d-x2", deployment: "m-dec", hash: "7c9d", node: "n1", role: "decode",
		created: t0.Add(-20 * time.Second), started: t0.Add(-15 * time.Second), gpus: 2}
	prefill := ksmPod{name: "m-pre-1a2b-y1", deployment: "m-pre", hash: "1a2b", node: "n2", role: "prefill", ready: true,
		created: created, started: created, readySince: created.Add(time.Minute), gpus: 1,
		during: func(at time.Time) bool { return at.Equal(t0) }}
	for _, p := range []ksmPod{ready, starting, prefill} {
		addKSMPod(srv, p)
		srv.Add(promQL(t, "kv_cache_usage"), promtest.Series{Labels: podLabels(p.name),
			Value: func(at time.Time) (float64, bool) { return 0.5, p.during == nil || p.during(at) }})
	}
	addKSMNode(srv, "n1", "NVIDIA-H100-80GB-HBM3")
	addKSMNode(srv, "n2", "NVIDIA-H200")
	addKSMDeployment(srv, "m-dec", func(time.Time) float64 { return 3 })
	addKSMDeployment(srv, "m-pre", func(at time.Time) float64 {
		if at.Before(step1) {
			return 1
		}
		return 0
	})

	opts := Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: step1, Step: 15 * time.Second,
		KubeStateMetrics: true}
	d, _ := capture(t, opts)

	assert.Equal(t, "kube-state-metrics", d.window.Roles)
	assert.NotContains(t, d.window.Flags, FlagNoReadiness)
	assert.NotContains(t, d.window.Flags, FlagRolesInferred)
	assert.NotContains(t, d.window.Flags, FlagNoRoleLabel)
	require.Len(t, d.window.Variants, 2)
	dec, pre := d.window.Variants[0], d.window.Variants[1]
	assert.Equal(t, "m-dec", dec.Name)
	assert.Equal(t, domain.RoleDecode, dec.Role)
	assert.Equal(t, 2, dec.GpuCount)
	assert.Equal(t, "NVIDIA-H100-80GB-HBM3", dec.Accelerator)
	assert.Equal(t, "m-pre", pre.Name)
	assert.Equal(t, domain.RolePrefill, pre.Role)
	assert.Equal(t, "NVIDIA-H200", pre.Accelerator)

	require.Len(t, d.cycles, 2)
	first := d.cycles[0]
	require.Len(t, first.Input.ReplicaMetrics, 3)
	byPod := map[string]domain.ReplicaMetrics{}
	for _, r := range first.Input.ReplicaMetrics {
		byPod[r.PodName] = r
	}
	r := byPod["m-dec-7c9d-x1"]
	assert.True(t, r.Ready)
	assert.True(t, r.ReadySince.Equal(ready.readySince), "Ready time from kube_pod_status_ready_time")
	assert.InDelta(t, 80, r.StartSeconds, 1e-9, "start from created to Ready")
	assert.Equal(t, "7c9d", r.Revision, "revision from the ReplicaSet's hash")
	assert.False(t, byPod["m-dec-7c9d-x2"].Ready)
	assert.Equal(t, []domain.VariantReplicaState{
		{VariantName: "m-dec", CurrentReplicas: 2, DesiredReplicas: 3, PendingReplicas: 1, GPUsPerReplica: 2,
			Role: domain.RoleDecode, AcceleratorName: "NVIDIA-H100-80GB-HBM3"},
		{VariantName: "m-pre", CurrentReplicas: 1, DesiredReplicas: 1, GPUsPerReplica: 1,
			Role: domain.RolePrefill, AcceleratorName: "NVIDIA-H200"},
	}, first.Input.VariantStates)

	// At the second step m-pre's Deployment is at zero: parked, and its Pod,
	// which kube-state-metrics no longer lists, is gone from the rows.
	second := d.cycles[1]
	assert.Equal(t, []string{"m-pre"}, second.Parked)
	require.Len(t, second.Input.VariantStates, 1)
	for _, r := range second.Input.ReplicaMetrics {
		assert.NotEqual(t, "m-pre", r.VariantName)
	}
}

func TestCapture_KubeStateMetricsWithoutRoleLabelsIsFlagged(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	addKSMPod(srv, ksmPod{name: testPod, deployment: "m-dec", hash: "7c9d", node: "n1", ready: true,
		created: t0.Add(-time.Hour), started: t0.Add(-time.Hour), readySince: t0.Add(-50 * time.Minute)})
	baseFleet(t, srv, testPod)
	d, _ := capture(t, Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0, Step: 15 * time.Second,
		KubeStateMetrics: true})
	assert.Contains(t, d.window.Flags, FlagNoRoleLabel)
	assert.Contains(t, d.window.Flags, FlagGPUsFromFlags)
	assert.Contains(t, d.window.Flags, FlagAcceleratorFromFlags)
	require.Len(t, d.window.Variants, 1)
	assert.Empty(t, d.window.Variants[0].Role)
	assert.Equal(t, 1, d.window.Variants[0].GpuCount, "the flag's default")
}

func TestCapture_RoleOfOverKubeStateMetricsLabels(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	// The role label is exported, and the selector matches it rather than
	// the Pod's name.
	addKSMPod(srv, ksmPod{name: "worker-0", deployment: "m-dec", hash: "7c9d", node: "n1", role: "decode", ready: true,
		created: t0.Add(-time.Hour), started: t0.Add(-time.Hour), readySince: t0.Add(-50 * time.Minute)})
	baseFleet(t, srv, "worker-0")
	opts := roleOfOpts(t, srv, "decode=llm-d.ai/role=decode")
	opts.KubeStateMetrics = true
	d, _ := capture(t, opts)
	assert.Equal(t, "kube-state-metrics+role-of", d.window.Roles)
	assert.NotContains(t, d.window.Flags, FlagRolesInferred, "matched on the exported label")
	require.Len(t, d.window.Variants, 1)
	assert.Equal(t, "decode", d.window.Variants[0].Name, "a --role-of variant is named by its selector's value")
	assert.Equal(t, domain.RoleDecode, d.window.Variants[0].Role)
}

func TestParseRoleOf(t *testing.T) {
	r, err := ParseRoleOf("prefill=app=m-prefill")
	require.NoError(t, err)
	assert.Equal(t, domain.RolePrefill, r.Role)
	assert.Equal(t, "m-prefill", r.Variant)
	assert.Equal(t, "prefill=app=m-prefill", r.String())
	ok, inferred := r.matches("m-prefill-5d4-abc", nil)
	assert.True(t, ok)
	assert.True(t, inferred)
	ok, _ = r.matches("m-prefill2-5d4-abc", nil)
	assert.False(t, ok, "a name prefix ends at a dash")
	ok, inferred = r.matches("anything", map[string]string{"label_app": "m-prefill"})
	assert.True(t, ok)
	assert.False(t, inferred)
	ok, _ = r.matches("m-prefill-5d4-abc", map[string]string{"label_app": "other"})
	assert.False(t, ok, "the exported label decides")
	ok, inferred = r.matches("m-prefill-5d4-abc", map[string]string{"label_llm_d_ai_role": "prefill"})
	assert.True(t, ok, "labels without the selector's key say nothing of it: the name decides")
	assert.True(t, inferred)

	multi, err := ParseRoleOf("decode=app=m,tier in (a,b)")
	require.NoError(t, err)
	ok, _ = multi.matches("m-x", nil)
	assert.False(t, ok, "a selector of more than one requirement infers nothing from a name")

	for _, bad := range []string{"prefill", "prefill=", "worker=app=x", "decode=app in (", "decode=!"} {
		_, err := ParseRoleOf(bad)
		assert.Error(t, err, bad)
	}
}

func TestCapture_AStepWithoutKubeStateMetricsFallsBackToTheRoleMap(t *testing.T) {
	srv := promtest.New()
	defer srv.Close()
	// kube-state-metrics is scraped at the first step only.
	addKSMPod(srv, ksmPod{name: testPod, deployment: "m-dec", hash: "7c9d", node: "n1", ready: false,
		created: t0.Add(-time.Hour), started: t0.Add(-time.Hour), during: func(at time.Time) bool { return at.Equal(t0) }})
	baseFleet(t, srv, testPod)
	opts := roleOfOpts(t, srv, "decode=app=m-dec")
	opts.KubeStateMetrics = true
	opts.To = t0.Add(15 * time.Second)
	d, _ := capture(t, opts)
	assert.Contains(t, d.window.Flags, "kube-state-metrics listed no Pod at 1 of 2 steps: those steps' Pods attributed by --role-of alone and read as Ready")
	require.Len(t, d.cycles, 2)
	require.Len(t, d.cycles[0].Input.ReplicaMetrics, 1)
	assert.False(t, d.cycles[0].Input.ReplicaMetrics[0].Ready, "kube-state-metrics says not Ready")
	require.Len(t, d.cycles[1].Input.ReplicaMetrics, 1, "the gap drops no row")
	assert.True(t, d.cycles[1].Input.ReplicaMetrics[0].Ready, "no listing: read as Ready, as the collector fails open")
	assert.Equal(t, 1, d.cycles[1].Input.VariantStates[0].CurrentReplicas, "counted from the rows")
}
