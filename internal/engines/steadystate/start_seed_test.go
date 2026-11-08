package steadystate

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scalingpolicy"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// startSeedEngine is an engine with an empty registry and a reporter.
func startSeedEngine() (*Engine, *registry.Registry) {
	reg := registry.New(0)
	return &Engine{Variants: reg, policies: scalingpolicy.NewChangeReporter()}, reg
}

// observeStartSeed registers ns/v with the given replicaStartSeconds, ""
// for none.
func observeStartSeed(reg *registry.Registry, raw string) {
	md := map[string]string{registry.ModelIDKey: "m"}
	if raw != "" {
		md[registry.ReplicaStartSecondsKey] = raw
	}
	reg.Observe("ns", "v", md)
}

// The seed travels trigger metadata -> registry -> the variant's own state,
// set with its presence bit. Under P/D the seeded roles are not first: the
// unseeded prefill comes before them, and the two seeded variants carry
// different figures, so a seed written to the wrong state fails. A variant
// with none, or one the registry does not hold, is left unset.
func TestApplyStartSeeds_PutsTheSeedOnItsVariant(t *testing.T) {
	e, reg := startSeedEngine()
	reg.Observe("ns", "prefill", map[string]string{registry.ModelIDKey: "m"})
	reg.Observe("ns", "decode", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "120"})
	reg.Observe("ns", "decode-big", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "300"})
	states := []domain.VariantReplicaState{
		{VariantName: "prefill"}, {VariantName: "decode"}, {VariantName: "decode-big"}, {VariantName: "unknown"},
	}

	e.applyStartSeeds(context.Background(), "ns", states)

	want := map[string]float64{"prefill": 0, "decode": 120, "decode-big": 300, "unknown": 0}
	for _, s := range states {
		assert.Equal(t, want[s.VariantName] > 0, s.StartSeedSet, "%s: presence bit", s.VariantName)
		assert.Equal(t, want[s.VariantName], s.StartSeedSeconds, "%s: seed", s.VariantName)
	}
}

// The warning's once-per-change record is per variant: a bad seed beside a
// good one is reported once, the same bad value on a second variant is
// reported for that variant too, and neither repeats on the next cycle.
func TestApplyStartSeeds_TheWarningIsPerVariant(t *testing.T) {
	var lines []string
	ctx := logr.NewContext(context.Background(),
		funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	e, reg := startSeedEngine()
	reg.Observe("ns", "good", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "120"})
	reg.Observe("ns", "bad", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "2m"})
	reg.Observe("ns", "bad-too", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "2m"})
	cycle := func() []domain.VariantReplicaState {
		states := []domain.VariantReplicaState{{VariantName: "good"}, {VariantName: "bad"}, {VariantName: "bad-too"}}
		e.applyStartSeeds(ctx, "ns", states)
		return states
	}

	states := cycle()
	assert.True(t, states[0].StartSeedSet)
	assert.False(t, states[1].StartSeedSet)
	assert.False(t, states[2].StartSeedSet)
	require.Len(t, lines, 2, "one line for each bad variant, none for the good one")
	assert.Contains(t, lines[0], `"variant"="bad"`)
	assert.Contains(t, lines[1], `"variant"="bad-too"`)

	cycle()
	assert.Len(t, lines, 2, "the next cycle repeats neither")
}

// The logged value is quoted, so a newline in it shows, and cut at 64 bytes,
// so a value of any length does not flood the log.
func TestApplyStartSeeds_TheLoggedValueIsQuotedAndCapped(t *testing.T) {
	var lines []string
	ctx := logr.NewContext(context.Background(),
		funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	e, reg := startSeedEngine()
	long := strings.Repeat("9", 200)
	reg.Observe("ns", "long", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: long})
	reg.Observe("ns", "newline", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "120\n"})
	e.applyStartSeeds(ctx, "ns", []domain.VariantReplicaState{{VariantName: "long"}, {VariantName: "newline"}})

	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], `"replicaStartSeconds"="\"`+strings.Repeat("9", 64)+`\"..."`)
	assert.NotContains(t, lines[0], strings.Repeat("9", 65))
	assert.Contains(t, lines[1], `"replicaStartSeconds"="\"120\\n\""`)
}

// Another namespace's entry of the same name does not seed this one.
func TestApplyStartSeeds_IsKeyedByNamespace(t *testing.T) {
	e, reg := startSeedEngine()
	observeStartSeed(reg, "120")
	states := []domain.VariantReplicaState{{VariantName: "v"}}
	e.applyStartSeeds(context.Background(), "other", states)
	assert.False(t, states[0].StartSeedSet)
}

// A bad value leaves the variant unseeded and is logged once each time the
// value written changes: not every cycle, and again when a fixed value turns
// bad once more.
func TestApplyStartSeeds_ABadSeedIsIgnoredAndLoggedOncePerChange(t *testing.T) {
	var lines []string
	ctx := logr.NewContext(context.Background(),
		funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	e, reg := startSeedEngine()
	run := func(raw string) domain.VariantReplicaState {
		observeStartSeed(reg, raw)
		states := []domain.VariantReplicaState{{VariantName: "v"}}
		e.applyStartSeeds(ctx, "ns", states)
		return states[0]
	}

	for _, step := range []struct {
		raw   string
		set   bool
		lines int
	}{
		{raw: "garbage", lines: 1},
		{raw: "garbage", lines: 1}, // the next cycle: the same value, not logged again
		{raw: "garbage", lines: 1},
		{raw: "-5", lines: 2}, // changed, still bad
		{raw: "NaN", lines: 3},
		{raw: "0", lines: 4},
		{raw: "120", set: true, lines: 4}, // fixed: nothing to say
		{raw: "", lines: 4},               // removed: nothing to say
		{raw: "garbage", lines: 5},        // bad again after the fix: said again
		{raw: " 120", lines: 6},           // a stray space: ignored, and said
		{raw: "0.5", lines: 7},            // under a second
	} {
		st := run(step.raw)
		assert.Equal(t, step.set, st.StartSeedSet, "seed %q", step.raw)
		if !step.set {
			assert.Zero(t, st.StartSeedSeconds, "seed %q", step.raw)
		}
		require.Len(t, lines, step.lines, "after seed %q", step.raw)
	}
	assert.True(t, strings.Contains(lines[0], `"replicaStartSeconds"="\"garbage\""`), lines[0])
	assert.True(t, strings.Contains(lines[0], `"variant"="v"`), lines[0])
	assert.True(t, strings.Contains(lines[0], `"maxSeconds"=3600`), lines[0])
}

// A bad seed never turns into a refused trigger: the rest of the variant's
// metadata is read, so the model keeps scaling.
func TestApplyStartSeeds_ABadSeedIsNotATriggerError(t *testing.T) {
	_, err := registry.ParseMeta(map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "garbage"})
	assert.NoError(t, err)
}

// No registry, no seed, and no panic: a test-built engine has none.
func TestApplyStartSeeds_NoRegistry(t *testing.T) {
	states := []domain.VariantReplicaState{{VariantName: "v"}}
	assert.NotPanics(t, func() { (&Engine{}).applyStartSeeds(context.Background(), "ns", states) })
	assert.False(t, states[0].StartSeedSet)
}

// seededVA is model m's variant name, scaled by Deployment d-<name>, and
// that Deployment with one Ready replica.
func seededVA(name string) (llmdvariant.Variant, client.Object) {
	va := parkedVA()
	va.Name = name
	va.Spec.ScaleTargetRef.Name = "d-" + name
	d := deployWithArgs("--max-num-seqs=128")
	d.Name = "d-" + name
	one := int32(1)
	d.Spec.Replicas = &one
	d.Status.Replicas, d.Status.ReadyReplicas = 1, 1
	return va, d
}

// The seed reaches the analyzer input through prepareModelData, not only
// through applyStartSeeds called by hand, and lands on its own variant: a
// P/D model whose unseeded prefill comes first and whose two decode variants
// seed 120 s and 300 s. Calling it before the states are built, not at all,
// or writing a seed to another variant's state, fails here.
func TestPrepareModelData_CarriesTheStartSeed(t *testing.T) {
	e, _ := engineConfigEngine(t)
	reg := registry.New(0)
	reg.Observe("ns", "prefill", map[string]string{registry.ModelIDKey: "m"})
	reg.Observe("ns", "decode", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "120"})
	reg.Observe("ns", "decode-big", map[string]string{registry.ModelIDKey: "m", registry.ReplicaStartSecondsKey: "300"})
	e.Variants, e.policies = reg, scalingpolicy.NewChangeReporter()

	names := []string{"prefill", "decode", "decode-big"}
	vas := make([]llmdvariant.Variant, 0, len(names))
	objs := make([]client.Object, 0, len(names))
	pods := make([]string, 0, len(names))
	for _, name := range names {
		va, d := seededVA(name)
		vas, objs, pods = append(vas, va), append(objs, d), append(pods, name+podSuffix)
	}
	k8s := fake.NewClientBuilder().WithObjects(objs...).Build()
	e.ReplicaMetricsCollector = collector.NewReplicaMetricsCollector(
		replicaSource{modelID: "m", pods: pods}, k8s, nil, nil, variantOfPodLocator{})

	md, err := e.prepareModelData(context.Background(), "m", vas, k8s)
	require.NoError(t, err)
	require.NotNil(t, md, "precondition: the model has replica rows, so it is not skipped")
	require.Len(t, md.variantStates, 3)
	require.Equal(t, "prefill", md.variantStates[0].VariantName, "precondition: the unseeded variant is first")

	want := map[string]float64{"prefill": 0, "decode": 120, "decode-big": 300}
	for _, s := range md.variantStates {
		assert.Equal(t, want[s.VariantName] > 0, s.StartSeedSet, "%s: presence bit", s.VariantName)
		assert.Equal(t, want[s.VariantName], s.StartSeedSeconds, "%s: seed", s.VariantName)
	}
}
