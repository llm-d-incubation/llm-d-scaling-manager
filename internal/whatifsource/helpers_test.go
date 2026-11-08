package whatifsource

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"testing"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

const (
	testNS    = "llm"
	testModel = "acme/m"
	// testPod is a decode Pod of variant m-dec, named as its Deployment
	// names it.
	testPod = "m-dec-7c9d-x1"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// promQL is the PromQL the collector asks for the query registered under
// name, in testNS.
func promQL(t testing.TB, name string) string {
	t.Helper()
	reg := source.NewSourceRegistry()
	require.NoError(t, reg.Register("prometheus", source.NewNoOpSource()))
	registration.RegisterSaturationQueries(reg)
	registration.RegisterQueueingModelQueries(reg)
	registration.RegisterSLOQueries(reg)
	registration.RegisterFlowControlPoolQueries(reg)
	registration.RegisterAlwaysOnQueries(reg)
	params := map[string]string{source.ParamNamespace: testNS}
	if q := reg.Get("prometheus").QueryList().Get(name); q != nil && len(q.Params) == 0 {
		params = nil
	}
	s, err := reg.Get("prometheus").QueryList().Build(name, params)
	require.NoError(t, err, name)
	return s
}

// podLabels is a per-Pod series' labels for pod.
func podLabels(pod string, extra ...string) map[string]string {
	l := map[string]string{"model_name": testModel, "instance": "10.0.0.1:8000", "pod": pod, "namespace": testNS}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

// apiFor is a Prometheus client of srv.
func apiFor(t testing.TB, srv *promtest.Server) promv1.API {
	t.Helper()
	c, err := api.NewClient(api.Config{Address: srv.URL})
	require.NoError(t, err)
	return promv1.NewAPI(c)
}

// baseFleet sets the two series that define a Pod's row: its KV cache usage
// and its queue.
func baseFleet(t testing.TB, srv *promtest.Server, pod string) {
	t.Helper()
	srv.Add(promQL(t, registration.QueryKvCacheUsage), promtest.Const(podLabels(pod), 0.5))
	srv.Add(promQL(t, registration.QueryQueueLength), promtest.Const(podLabels(pod), 2))
}

// roleOfOpts is a capture of one step with the role map from --role-of.
func roleOfOpts(t testing.TB, srv *promtest.Server, roles ...string) Options {
	t.Helper()
	if len(roles) == 0 {
		roles = []string{"decode=app=m-dec"}
	}
	opts := Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0, Step: 15 * time.Second}
	for _, r := range roles {
		parsed, err := ParseRoleOf(r)
		require.NoError(t, err)
		opts.RoleOf = append(opts.RoleOf, parsed)
	}
	return opts
}

// decodedCycle is a capture's cycle as a test reads it.
type decodedCycle struct {
	At    time.Time
	Input struct {
		ModelID          string
		Namespace        string
		ReplicaMetrics   []domain.ReplicaMetrics
		VariantStates    []domain.VariantReplicaState
		Config           *config.ScalingPolicy
		ConfigType       string
		SchedulerQueue   *domain.SchedulerQueueMetrics
		ArrivalRate      float64
		RouterLatency    *domain.RouterLatency
		FlowControlPools []domain.FlowControlPool
	}
	Parked []string
}

// decoded is a capture as a test reads it.
type decoded struct {
	raw    []byte
	start  learnrec.Start
	window learnreplay.CaptureWindow
	cycles []decodedCycle
}

func gunzip(t testing.TB, b []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return out
}

// decodeCapture reads a gzipped capture.
func decodeCapture(t testing.TB, gz []byte) decoded {
	t.Helper()
	raw := gunzip(t, gz)
	read, err := learnrec.Read(raw)
	require.NoError(t, err)
	require.Empty(t, read.TornTail)
	require.GreaterOrEqual(t, len(read.Events), 2)
	d := decoded{raw: raw}
	require.Equal(t, learnrec.KindStart, read.Events[0].Kind)
	require.NoError(t, read.Events[0].DecodeData(&d.start))
	require.Equal(t, learnreplay.KindCaptureWindow, read.Events[1].Kind)
	require.NoError(t, read.Events[1].DecodeData(&d.window))
	for _, ev := range read.Events[2:] {
		require.Equal(t, learnreplay.KindCaptureCycle, ev.Kind)
		var c decodedCycle
		require.NoError(t, ev.DecodeData(&c))
		c.At = ev.Now
		d.cycles = append(d.cycles, c)
	}
	return d
}

// capture runs a capture and reads it back.
func capture(t testing.TB, opts Options) (decoded, *Summary) {
	t.Helper()
	var buf bytes.Buffer
	sum, err := Capture(context.Background(), opts, &buf)
	require.NoError(t, err)
	return decodeCapture(t, buf.Bytes()), sum
}

// onlyRow is the one row of the capture's one cycle.
func onlyRow(t testing.TB, d decoded) domain.ReplicaMetrics {
	t.Helper()
	require.Len(t, d.cycles, 1)
	require.Len(t, d.cycles[0].Input.ReplicaMetrics, 1)
	return d.cycles[0].Input.ReplicaMetrics[0]
}
