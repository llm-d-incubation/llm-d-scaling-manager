package steadystate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// roleFixture is one variant of a P/D model whose Deployment may carry a
// role label, and the reading of its one pod.
type roleFixture struct {
	name, label string
	row         domain.ReplicaMetrics
}

// roleCycle is what the engine holds for fixtures at one cycle: the
// variants, scale targets, rows, and the states and metadata discovery
// resolves (the label, else both).
type roleCycle struct {
	variants map[string]*llmdvariant.Variant
	targets  map[string]scaletarget.ScaleTargetAccessor
	rows     []domain.ReplicaMetrics
	states   []domain.VariantReplicaState
	metas    []domain.VariantMetadata
}

func roleInputs(fixtures ...roleFixture) roleCycle {
	c := roleCycle{variants: map[string]*llmdvariant.Variant{}, targets: map[string]scaletarget.ScaleTargetAccessor{},
		rows:   make([]domain.ReplicaMetrics, 0, len(fixtures)),
		states: make([]domain.VariantReplicaState, 0, len(fixtures)),
		metas:  make([]domain.VariantMetadata, 0, len(fixtures))}
	for _, f := range fixtures {
		va := &llmdvariant.Variant{ObjectMeta: metav1.ObjectMeta{Name: f.name, Namespace: "ns"}}
		va.Spec.ScaleTargetRef.Name = f.name
		c.variants[utils.GetNamespacedKey("ns", f.name)] = va
		deploy := &appsv1.Deployment{}
		if f.label != "" {
			deploy.Spec.Template.Labels = map[string]string{variantmeta.RoleLabel: f.label}
		}
		st := scaletarget.NewDeploymentAccessor(deploy)
		c.targets[utils.GetNamespacedKey("ns", f.name)] = st
		row := f.row
		row.ModelID, row.VariantName, row.PodName = "m", f.name, f.name+"-pod"
		c.rows = append(c.rows, row)
		role := variantmeta.RoleFromScaleTarget(st)
		c.states = append(c.states, domain.VariantReplicaState{VariantName: f.name, Role: role})
		c.metas = append(c.metas, domain.VariantMetadata{VariantName: f.name, Role: role})
	}
	return c
}

var (
	prefillRow = domain.ReplicaMetrics{AvgOutputTokens: 1, RequestRate: 6.7, RequestRateKnown: true,
		PrefillComputedTokenRate: 669, PrefillComputedTokenRateKnown: true}
	decodeRow = domain.ReplicaMetrics{AvgOutputTokens: 1198, RequestRate: 5.9, RequestRateKnown: true,
		PrefillComputedTokenRateKnown: true,
		KVTransferRates:               []domain.KVTransferRate{{Kind: domain.KVTransferNIXL, ByteRate: 2e8, SecondsRate: 0.1}}}
	bothRow = domain.ReplicaMetrics{AvgOutputTokens: 300, RequestRate: 2, RequestRateKnown: true,
		PrefillComputedTokenRate: 1800, PrefillComputedTokenRateKnown: true}
	// idleRow is a pod that served nothing in the last minute and moved no
	// KV: its signals decide nothing.
	idleRow = domain.ReplicaMetrics{AvgOutputTokens: 1198, RequestRateKnown: true, PrefillComputedTokenRateKnown: true}
)

// roleEngine is an engine with the switch as given, on a clock the test
// moves.
func roleEngine(on bool, now *time.Time) *Engine {
	return &Engine{rolesFromSignals: on, observeNow: func() time.Time { return *now }}
}

func (e *Engine) resolveCycle(ctx context.Context, c roleCycle) ([]domain.VariantReplicaState, []domain.VariantMetadata) {
	return e.resolveRoles(ctx, "ns", "m", c.rows, c.states, c.metas, c.targets, c.variants)
}

func rolesByName(states []domain.VariantReplicaState, metas []domain.VariantMetadata) (map[string]string, map[string]string) {
	s, m := map[string]string{}, map[string]string{}
	for _, v := range states {
		s[v.VariantName] = v.Role
	}
	for _, v := range metas {
		m[v.VariantName] = v.Role
	}
	return s, m
}

func TestResolveRoles_SwitchOffLeavesDiscoverysRoles(t *testing.T) {
	now := time.Unix(0, 0)
	c := roleInputs(roleFixture{name: "p", row: prefillRow}, roleFixture{name: "d", row: decodeRow},
		roleFixture{name: "a", row: bothRow})
	e := roleEngine(false, &now)
	gotStates, gotMetas := e.resolveCycle(context.Background(), c)
	// The very slices discovery built: nothing copied, nothing changed.
	require.Equal(t, len(c.states), len(gotStates))
	assert.Same(t, &c.states[0], &gotStates[0], "the states are discovery's own")
	assert.Same(t, &c.metas[0], &gotMetas[0], "the metadata is discovery's own")
	s, m := rolesByName(gotStates, gotMetas)
	want := map[string]string{"p": domain.RoleBoth, "d": domain.RoleBoth, "a": domain.RoleBoth}
	assert.Equal(t, want, s, "an unlabelled variant reads both, as before")
	assert.Equal(t, want, m)
	assert.Equal(t, domain.RoleBoth, e.variantRole("ns", "d", c.targets[utils.GetNamespacedKey("ns", "d")]))
	assert.False(t, (&Engine{}).rolesFromSignals, "off unless turned on")
}

func TestResolveRoles_UnlabelledVariantsTakeTheSignalsRole(t *testing.T) {
	now := time.Unix(0, 0)
	c := roleInputs(roleFixture{name: "p", row: prefillRow}, roleFixture{name: "d", row: decodeRow},
		roleFixture{name: "a", row: bothRow})
	gotStates, gotMetas := roleEngine(true, &now).resolveCycle(context.Background(), c)
	want := map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode, "a": domain.RoleBoth}
	s, m := rolesByName(gotStates, gotMetas)
	assert.Equal(t, want, s, "variant states")
	assert.Equal(t, want, m, "variant metadata: the role join reads the same role")
	for _, v := range c.states {
		assert.Equal(t, domain.RoleBoth, v.Role, "the caller's slice is not written")
	}
}

func TestResolveRoles_OneTokenAloneIsNotPrefill(t *testing.T) {
	// An aggregated model serving one-token requests reads like prefill,
	// with no decode variant beside it: it keeps both.
	now := time.Unix(0, 0)
	c := roleInputs(roleFixture{name: "a", row: prefillRow})
	gotStates, _ := roleEngine(true, &now).resolveCycle(context.Background(), c)
	assert.Equal(t, domain.RoleBoth, gotStates[0].Role)
}

func TestResolveRoles_ALabelWins(t *testing.T) {
	now := time.Unix(0, 0)
	c := roleInputs(roleFixture{name: "p", label: domain.RolePrefill, row: decodeRow},
		roleFixture{name: "d", label: domain.RoleDecode, row: decodeRow})
	gotStates, gotMetas := roleEngine(true, &now).resolveCycle(context.Background(), c)
	s, m := rolesByName(gotStates, gotMetas)
	want := map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}
	assert.Equal(t, want, s)
	assert.Equal(t, want, m)
}

func TestResolveRoles_AnIdleCycleKeepsTheDecidedRole(t *testing.T) {
	now := time.Unix(0, 0)
	e := roleEngine(true, &now)
	busy := roleInputs(roleFixture{name: "p", row: prefillRow}, roleFixture{name: "d", row: decodeRow})
	s, _ := rolesByName(e.resolveCycle(context.Background(), busy))
	require.Equal(t, map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}, s)

	// A cycle later both are idle: nothing is decided, the roles stay.
	now = now.Add(30 * time.Second)
	idle := roleInputs(roleFixture{name: "p", row: idleRow}, roleFixture{name: "d", row: idleRow})
	s, m := rolesByName(e.resolveCycle(context.Background(), idle))
	assert.Equal(t, map[string]string{"p": domain.RolePrefill, "d": domain.RoleDecode}, s, "kept through an idle cycle")
	assert.Equal(t, s, m)
	assert.Equal(t, domain.RoleDecode, e.variantRole("ns", "d", idle.targets[utils.GetNamespacedKey("ns", "d")]),
		"replicas_not_arriving reads the same role")

	// Undecided past the TTL: the default again.
	now = now.Add(roleMemoryTTL + time.Second)
	s, _ = rolesByName(e.resolveCycle(context.Background(), idle))
	assert.Equal(t, map[string]string{"p": domain.RoleBoth, "d": domain.RoleBoth}, s)
}

func TestResolveRoles_ADisagreementIsLoggedOncePerChange(t *testing.T) {
	now := time.Unix(0, 0)
	var infos []string
	ctx := ctrl.LoggerInto(context.Background(), funcr.New(func(_, args string) { infos = append(infos, args) },
		funcr.Options{Verbosity: 0}))
	e := roleEngine(true, &now)
	mislabelled := roleInputs(roleFixture{name: "p", label: domain.RolePrefill, row: decodeRow})
	for range 3 {
		e.resolveCycle(ctx, mislabelled)
	}
	count := func() int {
		n := 0
		for _, l := range infos {
			if strings.Contains(l, "labelled prefill, engine signals read decode") {
				n++
			}
		}
		return n
	}
	assert.Equal(t, 1, count(), "once while it lasts: %q", infos)
	agreeing := roleInputs(roleFixture{name: "p", label: domain.RolePrefill, row: prefillRow},
		roleFixture{name: "d", label: domain.RoleDecode, row: decodeRow})
	e.resolveCycle(ctx, agreeing)
	e.resolveCycle(ctx, mislabelled)
	assert.Equal(t, 2, count(), "again once it starts again")
}

func TestResolveRoles_NoScaleTargetKeepsTheRole(t *testing.T) {
	now := time.Unix(0, 0)
	c := roleInputs(roleFixture{name: "d", row: decodeRow})
	c.targets, c.variants = nil, nil
	gotStates, _ := roleEngine(true, &now).resolveCycle(context.Background(), c)
	assert.Equal(t, domain.RoleBoth, gotStates[0].Role, "nothing says the role was not declared")
}

func TestEngineSetSwitchesForReplay(t *testing.T) {
	e := &Engine{}
	require.NoError(t, e.SetSwitchesForReplay(map[string]bool{SwitchRolesFromSignals: true}))
	assert.True(t, e.rolesFromSignals)
	assert.Error(t, e.SetSwitchesForReplay(map[string]bool{"no-such-switch": true}))
	assert.True(t, e.rolesFromSignals, "an unknown name sets nothing")
}

func TestDeclaredRole(t *testing.T) {
	for label, want := range map[string]bool{"": false, "unknown": false, domain.RoleBoth: true,
		domain.RolePrefill: true, domain.RoleDecode: true} {
		deploy := &appsv1.Deployment{}
		if label != "" {
			deploy.Spec.Template.Labels = map[string]string{variantmeta.RoleLabel: label}
		}
		_, ok := variantmeta.DeclaredRole(scaletarget.NewDeploymentAccessor(deploy))
		assert.Equal(t, want, ok, "label %q", label)
	}
	_, ok := variantmeta.DeclaredRole(nil)
	assert.False(t, ok)
}
