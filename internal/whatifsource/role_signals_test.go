package whatifsource

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifsource/promtest"
)

// signalPod is one engine Pod and what its engine does, as a P/D lab run
// reads it: a prefill Pod makes one token a request and computes the
// prompts; a decode Pod pulls the KV through NIXL and computes none; an
// aggregated Pod computes the prompts and generates.
type signalPod struct {
	name, deployment, role                  string
	outputTokens, requestRate, computedRate float64
	nixlBytes, nixlSeconds                  float64
}

var (
	prefillPod = signalPod{name: "fleet-a-1a2b-x1", deployment: "fleet-a", outputTokens: 1, requestRate: 6.7,
		computedRate: 669}
	decodePod = signalPod{name: "fleet-b-3c4d-y1", deployment: "fleet-b", outputTokens: 1198, requestRate: 5.9,
		nixlBytes: 2e8, nixlSeconds: 0.1}
	bothPod = signalPod{name: "fleet-c-5e6f-z1", deployment: "fleet-c", outputTokens: 300, requestRate: 2,
		computedRate: 1800}
)

// signalFleet serves pods, each as kube-state-metrics shows it (with its
// role label when it has one) and with its engine series.
func signalFleet(t *testing.T, pods ...signalPod) *promtest.Server {
	t.Helper()
	srv := promtest.New()
	t.Cleanup(srv.Close)
	for i, p := range pods {
		node := "n" + string(rune('1'+i))
		addKSMPod(srv, ksmPod{name: p.name, deployment: p.deployment, hash: strings.Split(p.name, "-")[2], node: node,
			role: p.role, ready: true, created: t0.Add(-time.Hour), started: t0.Add(-time.Hour),
			readySince: t0.Add(-50 * time.Minute), gpus: 1})
		addKSMNode(srv, node, "NVIDIA-H200")
		for q, v := range map[string]float64{
			registration.QueryKvCacheUsage:             0.5,
			registration.QueryQueueLength:              0,
			registration.QueryAvgOutputTokens:          p.outputTokens,
			registration.QueryRequestRate:              p.requestRate,
			registration.QueryPrefillComputedTokenRate: p.computedRate,
			registration.QueryNIXLByteRate:             p.nixlBytes,
			registration.QueryNIXLSecondsRate:          p.nixlSeconds,
		} {
			srv.Add(promQL(t, q), promtest.Const(podLabels(p.name), v))
		}
	}
	return srv
}

func signalCapture(t *testing.T, pods ...signalPod) decoded {
	t.Helper()
	srv := signalFleet(t, pods...)
	d, _ := capture(t, Options{Prometheus: apiFor(t, srv), Namespace: testNS, From: t0, To: t0.Add(30 * time.Second),
		Step: 15 * time.Second, KubeStateMetrics: true})
	return d
}

// rolesOf is each window variant's role, and each cycle's variant states'
// roles, which must agree with it.
func rolesOf(t *testing.T, d decoded) map[string]string {
	t.Helper()
	roles := map[string]string{}
	for _, v := range d.window.Variants {
		roles[v.Name] = v.Role
	}
	require.NotEmpty(t, d.cycles)
	for _, c := range d.cycles {
		for _, vs := range c.Input.VariantStates {
			assert.Equal(t, roles[vs.VariantName], vs.Role, "cycle at %s, variant %s", c.At, vs.VariantName)
		}
	}
	return roles
}

func TestCapture_UnlabelledPodsTakeTheRoleTheirEngineSignalsRead(t *testing.T) {
	d := signalCapture(t, prefillPod, decodePod, bothPod)
	assert.Equal(t, map[string]string{"fleet-a": domain.RolePrefill, "fleet-b": domain.RoleDecode,
		"fleet-c": domain.RoleBoth}, rolesOf(t, d), "the names say nothing of the roles; the signals do")
	assert.Contains(t, d.window.Flags, FlagNoRoleLabel)
	// The evidence is named per Pod, in the window line's flags: the
	// recording carries them, and a report made from it shows them.
	for _, want := range []string{
		"variant fleet-a: role prefill from engine signals, no role label: fleet-a-1a2b-x1 prefill at 3 of 3 readings",
		"variant fleet-b: role decode from engine signals, no role label: fleet-b-3c4d-y1 decode at 3 of 3 readings",
		"variant fleet-c: role both from engine signals, no role label: fleet-c-5e6f-z1 both at 3 of 3 readings",
	} {
		assert.True(t, hasFlagPrefix(d.window.Flags, want), "no flag starting %q in %q", want, d.window.Flags)
	}
}

func TestCapture_ARoleLabelWinsAndADisagreementIsFlagged(t *testing.T) {
	agree, mislabelled := prefillPod, decodePod
	agree.role, mislabelled.role = domain.RolePrefill, domain.RolePrefill
	d := signalCapture(t, agree, mislabelled)
	assert.Equal(t, map[string]string{"fleet-a": domain.RolePrefill, "fleet-b": domain.RolePrefill}, rolesOf(t, d),
		"a label is the source of truth")
	assert.NotContains(t, d.window.Flags, FlagNoRoleLabel)
	assert.True(t, hasFlagPrefix(d.window.Flags,
		"variant fleet-b: labelled prefill, engine signals read decode: fleet-b-3c4d-y1 decode at 3 of 3 readings"),
		"%q", d.window.Flags)
	for _, f := range d.window.Flags {
		assert.NotContains(t, f, "variant fleet-a:", "a label the signals agree with is not flagged")
	}
}

func TestCapture_RoleOfByNameWinsOverTheSignals(t *testing.T) {
	// --role-of picked the Pods by name, with no labels to match: its role
	// is the operator's statement and wins; the disagreement is flagged.
	srv := signalFleet(t, decodePod)
	opts := roleOfOpts(t, srv, "prefill=app=fleet-b")
	d, _ := capture(t, opts)
	assert.Contains(t, d.window.Flags, FlagRolesInferred)
	assert.Equal(t, map[string]string{"fleet-b": domain.RolePrefill}, rolesOf(t, d))
	assert.True(t, hasFlagPrefix(d.window.Flags, "variant fleet-b: labelled prefill, engine signals read decode: "),
		"%q", d.window.Flags)
}

func TestCapture_OneTokenRequestsAloneDoNotReadPrefill(t *testing.T) {
	// An aggregated fleet serving one-token requests (max_tokens=1, an
	// output_len=1 benchmark) reads like prefill; with no decode variant
	// beside it and no KV moving, it reads no role, and so both.
	d := signalCapture(t, prefillPod)
	assert.Equal(t, map[string]string{"fleet-a": ""}, rolesOf(t, d))
	assert.True(t, hasFlagPrefix(d.window.Flags,
		"variant fleet-a: no role label and engine signals read no role, read as both: fleet-a-1a2b-x1 prefill at 3 of 3"),
		"%q", d.window.Flags)
}

func hasFlagPrefix(flags []string, prefix string) bool {
	for _, f := range flags {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}
