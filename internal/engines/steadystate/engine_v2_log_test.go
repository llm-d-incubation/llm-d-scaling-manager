package steadystate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

func zapObserverCtx(t *testing.T) (context.Context, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	return ctx, logs
}

func TestLogAnalyzerResult_EmitsRequiredFields(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	nr := allocation.NamedAnalyzerResult{
		Name:              "saturation",
		ScaleUpThreshold:  1.2,
		ScaleDownBoundary: 0.7,
		TotalSupply:       100000,
		Utilization:       0.8,
		RequiredCapacity:  0,
		SpareCapacity:     20000,
		Result: &domain.AnalyzerResult{
			TotalDemand: 80000,
			VariantCapacities: []domain.VariantCapacity{
				{
					VariantName:        "primary",
					PerReplicaCapacity: 50000,
					Role:               domain.RoleDecode,
					Reason:             "P2-hist",
				},
			},
		},
	}

	logAnalyzerResult(ctx, "mymodel", "ns", nr)

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "analyzer-result", entry.Message)

	fields := entry.ContextMap()
	for _, key := range []string{"modelID", "namespace", "analyzer", "live", "supply", "demand", "util", "rc", "sc", "scaleUpThreshold", "scaleDownBoundary", "variants"} {
		assert.Contains(t, fields, key, "missing field %q", key)
	}
	assert.Equal(t, "mymodel", fields["modelID"])
	assert.Equal(t, "ns", fields["namespace"])
	assert.Equal(t, "saturation", fields["analyzer"])

	// Verify variants entries contain "reason" but not "cost".
	b, err := json.Marshal(fields["variants"])
	require.NoError(t, err)
	variantsJSON := string(b)
	assert.Contains(t, variantsJSON, `"reason"`, "variants entry must include reason field")
	assert.Contains(t, variantsJSON, "P2-hist", "label value must be present")
	assert.NotContains(t, variantsJSON, `"cost"`, "cost must not appear in variants entry")

	// V2 charges waiting requests by P/D role, so demand is not interpretable
	// without the resolved role on this line.
	assert.Contains(t, variantsJSON, `"role"`, "variants entry must include role field")
	assert.Contains(t, variantsJSON, domain.RoleDecode, "resolved role value must be present")
}

// Live must be on the line for every entry, matching or not: it is the only
// field that says whether THIS analyzer currently contributes to the
// composite aggregation and the scale-down veto (allocation.Eligible). The
// line itself has no Live guard, so a reader must be able to see both a
// non-live analyzer's real numbers AND that they are not currently being
// counted -- omitting the field would make "why didn't the composite pick
// this signal up" undiagnosable from the log alone.
func TestLogAnalyzerResult_EmitsLiveField(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	live := allocation.NamedAnalyzerResult{
		Name: "saturation",
		Live: true,
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 100}},
		},
	}
	logAnalyzerResult(ctx, "m", "ns", live)
	require.Equal(t, 1, logs.Len())
	assert.Equal(t, true, logs.All()[0].ContextMap()["live"])
	logs.TakeAll()

	stale := allocation.NamedAnalyzerResult{
		Name: "throughput",
		Live: false,
		Result: &domain.AnalyzerResult{
			TotalDemand:       12345, // a non-live analyzer's real numbers still appear on the line
			VariantCapacities: []domain.VariantCapacity{{VariantName: "v", PerReplicaCapacity: 200}},
		},
	}
	logAnalyzerResult(ctx, "m", "ns", stale)
	require.Equal(t, 1, logs.Len())
	fields := logs.All()[0].ContextMap()
	assert.Equal(t, false, fields["live"])
	assert.Equal(t, float64(12345), fields["demand"], "a non-live analyzer's real demand must still be visible")
}

// An analyzer that leaves Role unset is treated as "both" downstream, so the log
// line must say "both" rather than omitting the field — that is precisely the
// case a reader needs to distinguish from an explicitly-roled variant.
func TestLogAnalyzerResult_DefaultRoleRendersAsBoth(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	logAnalyzerResult(ctx, "mymodel", "ns", allocation.NamedAnalyzerResult{
		Name: "saturation",
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "primary", PerReplicaCapacity: 50000, Role: ""},
			},
		},
	})

	require.Equal(t, 1, logs.Len())
	b, err := json.Marshal(logs.All()[0].ContextMap()["variants"])
	require.NoError(t, err)
	assert.Contains(t, string(b), `"role":"both"`, "unset role must render as the canonical %q", domain.RoleBoth)
}

// The model-level rc/sc net to a blend of both roles and can read 0 even
// while one role's own RequiredCapacity is still positive -- exactly the
// case that made a variant pinned at MaxReplicas for many cycles
// undiagnosable from this line alone (see engine_v2.go's roleRC/roleSC
// comment). roleRC/roleSC must expose each role's own figure so that case is
// visible in the log directly.
func TestLogAnalyzerResult_EmitsPerRoleRequiredAndSpareCapacity(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	nr := allocation.NamedAnalyzerResult{
		Name: "saturation",
		Result: &domain.AnalyzerResult{
			TotalDemand: 140000,
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "decode", PerReplicaCapacity: 447563, Role: domain.RoleDecode},
				{VariantName: "prefill", PerReplicaCapacity: 468446, Role: domain.RolePrefill},
			},
		},
		RequiredCapacity: 0, // model-level nets to 0
		SpareCapacity:    6149145,
		RoleCapacities: map[string]domain.RoleCapacity{
			domain.RoleDecode:  {Role: domain.RoleDecode, RequiredCapacity: 1200, SpareCapacity: 0},
			domain.RolePrefill: {Role: domain.RolePrefill, RequiredCapacity: 0, SpareCapacity: 468446},
		},
	}

	logAnalyzerResult(ctx, "mymodel", "ns", nr)

	require.Equal(t, 1, logs.Len())
	fields := logs.All()[0].ContextMap()
	require.Contains(t, fields, "roleRC")
	require.Contains(t, fields, "roleSC")

	roleRC, ok := fields["roleRC"].(map[string]float64)
	require.True(t, ok, "roleRC must be a map[string]float64, got %T", fields["roleRC"])
	assert.Equal(t, 1200.0, roleRC[domain.RoleDecode], "decode's own RC must be visible despite model-level rc reading 0")
	assert.Equal(t, 0.0, roleRC[domain.RolePrefill])

	roleSC, ok := fields["roleSC"].(map[string]float64)
	require.True(t, ok, "roleSC must be a map[string]float64, got %T", fields["roleSC"])
	assert.Equal(t, 0.0, roleSC[domain.RoleDecode])
	assert.Equal(t, 468446.0, roleSC[domain.RolePrefill])
}

// A non-disaggregated model has no RoleCapacities at all; roleRC/roleSC must
// stay absent rather than render as an empty map, so the line still
// distinguishes "not a P/D model" from "P/D model, both roles read zero".
func TestLogAnalyzerResult_NoRoleCapacitiesOmitsPerRoleFields(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	logAnalyzerResult(ctx, "mymodel", "ns", allocation.NamedAnalyzerResult{
		Name: "saturation",
		Result: &domain.AnalyzerResult{
			VariantCapacities: []domain.VariantCapacity{
				{VariantName: "primary", PerReplicaCapacity: 50000, Role: domain.RoleBoth},
			},
		},
	})

	require.Equal(t, 1, logs.Len())
	fields := logs.All()[0].ContextMap()
	// NotContains, not Nil: a map lookup returns nil both for an absent key and
	// for a key present with a nil value, so assert.Nil passes either way and
	// cannot fail for the reason this test names.
	assert.NotContains(t, fields, "roleRC")
	assert.NotContains(t, fields, "roleSC")
}

func TestLogAnalyzerResult_NilResultSkipped(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	logAnalyzerResult(ctx, "m", "ns", allocation.NamedAnalyzerResult{
		Name:   "saturation",
		Result: nil,
	})

	assert.Equal(t, 0, logs.Len(), "nil result should emit no log line")
}

func TestLogAnalyzerResult_EmptyVariants(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	nr := allocation.NamedAnalyzerResult{
		Name:             "throughput",
		TotalSupply:      0,
		RequiredCapacity: 15000,
		Result: &domain.AnalyzerResult{
			TotalDemand:       0,
			VariantCapacities: []domain.VariantCapacity{},
		},
	}

	logAnalyzerResult(ctx, "m", "ns", nr)

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "analyzer-result", entry.Message)
	assert.Contains(t, entry.ContextMap(), "variants")
}

func TestLogScalingDecisions_EmitsPerModel(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	requests := []allocation.ModelScalingRequest{
		{ModelID: "model-a", Namespace: "ns"},
		{ModelID: "model-b", Namespace: "ns"},
	}
	decisions := []domain.VariantDecision{
		{ModelID: "model-a", Namespace: "ns", VariantName: "v1", CurrentReplicas: 1, TargetReplicas: 2, Action: domain.ActionScaleUp},
		{ModelID: "model-a", Namespace: "ns", VariantName: "v2", CurrentReplicas: 1, TargetReplicas: 1, Action: domain.ActionNoChange},
		{ModelID: "model-b", Namespace: "ns", VariantName: "v1", CurrentReplicas: 2, TargetReplicas: 1, Action: domain.ActionScaleDown},
	}

	logScalingDecisions(ctx, requests, decisions, nil)

	require.Equal(t, 2, logs.Len(), "expected one log line per model")
	msgs := map[string]bool{}
	for _, e := range logs.All() {
		assert.Equal(t, "scaling-decision", e.Message)
		modelID, _ := e.ContextMap()["modelID"].(string)
		msgs[modelID] = true
	}
	assert.True(t, msgs["model-a"], "expected log for model-a")
	assert.True(t, msgs["model-b"], "expected log for model-b")
}

// O2: while borrowing is counted, every model's scaling-decision line says how
// many GPUs it draws from the cohorts' shared pools, 0 included; without it,
// the field is absent.
func TestLogScalingDecisions_Borrowed(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	requests := []allocation.ModelScalingRequest{{ModelID: "a", Namespace: "ns"}, {ModelID: "b", Namespace: "ns"}}
	decisions := []domain.VariantDecision{
		{ModelID: "a", Namespace: "ns", VariantName: "a1", CurrentReplicas: 1, TargetReplicas: 2, Action: domain.ActionScaleUp},
		{ModelID: "b", Namespace: "ns", VariantName: "b1", CurrentReplicas: 1, TargetReplicas: 1, Action: domain.ActionNoChange},
	}
	logScalingDecisions(ctx, requests, decisions, map[string]int{"ns/a": 8})
	require.Equal(t, 2, logs.Len())
	got := map[string]any{}
	for _, e := range logs.All() {
		got[e.ContextMap()["modelID"].(string)] = e.ContextMap()["borrowed"]
	}
	assert.EqualValues(t, 8, got["a"])
	assert.EqualValues(t, 0, got["b"])

	ctx, logs = zapObserverCtx(t)
	logScalingDecisions(ctx, requests, decisions, nil)
	for _, e := range logs.All() {
		assert.NotContains(t, e.ContextMap(), "borrowed", "no borrowing counted: no field")
	}
}

// atMax is what separates "the scaling manager chose this size" from "the scaling manager was not allowed to go
// higher". Without it both read as a steady target, which is how a benchmark
// pinned at 4 while demand implied 12 produced numbers that looked like a
// decision.
func TestLogScalingDecisions_AtMaxReplicas(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	four, ten := 4, 10
	requests := []allocation.ModelScalingRequest{{
		ModelID: "model-a", Namespace: "ns",
		VariantStates: []domain.VariantReplicaState{
			{VariantName: "pinned", MaxReplicas: &four},
			{VariantName: "roomy", MaxReplicas: &ten},
			{VariantName: "unbounded"}, // MaxReplicas nil: no ceiling to be at
		},
	}}
	decisions := []domain.VariantDecision{
		{ModelID: "model-a", Namespace: "ns", VariantName: "pinned", CurrentReplicas: 4, TargetReplicas: 4, Action: domain.ActionNoChange},
		{ModelID: "model-a", Namespace: "ns", VariantName: "roomy", CurrentReplicas: 2, TargetReplicas: 4, Action: domain.ActionScaleUp},
		{ModelID: "model-a", Namespace: "ns", VariantName: "unbounded", CurrentReplicas: 9, TargetReplicas: 99, Action: domain.ActionScaleUp},
	}

	logScalingDecisions(ctx, requests, decisions, nil)

	require.Equal(t, 1, logs.Len())

	// Asserted through JSON rather than by type-asserting the anonymous struct:
	// that assertion depends on field order and tags matching the production
	// type exactly, so it breaks on edits that change nothing an operator sees.
	// The JSON *is* what they see.
	raw, err := json.Marshal(logs.All()[0].ContextMap()["decisions"])
	require.NoError(t, err)
	var entries []map[string]any
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.Len(t, entries, 3)

	got := map[string]any{}
	for _, e := range entries {
		got[e["name"].(string)] = e["atMax"]
	}
	assert.Equal(t, true, got["pinned"], "target 4 of max 4 is at the ceiling")
	// omitempty: false is absent rather than present-and-false, which is the
	// shape the log actually carries.
	assert.Nil(t, got["roomy"], "target 4 of max 10 has headroom")
	assert.Nil(t, got["unbounded"], "no MaxReplicas means no ceiling to sit on")
}

// A troubleshooting runbook starts from scaling-decision: a variant a limiter
// held back must say which, as the ResourceConstrained Event does; one that is
// not limited (the utilization share clears WasLimited) carries no name.
func TestLogScalingDecisions_LimitedBy(t *testing.T) {
	ctx, logs := zapObserverCtx(t)
	requests := []allocation.ModelScalingRequest{{ModelID: "model-a", Namespace: "ns"}}
	decisions := []domain.VariantDecision{
		{ModelID: "model-a", Namespace: "ns", VariantName: "held", CurrentReplicas: 1, TargetReplicas: 1,
			Action: domain.ActionNoChange, WasLimited: true, LimitedBy: "team-quota"},
		{ModelID: "model-a", Namespace: "ns", VariantName: "overridden", CurrentReplicas: 1, TargetReplicas: 2,
			Action: domain.ActionScaleUp, LimitedBy: "team-quota"},
		{ModelID: "model-a", Namespace: "ns", VariantName: "free", CurrentReplicas: 1, TargetReplicas: 2,
			Action: domain.ActionScaleUp},
	}

	logScalingDecisions(ctx, requests, decisions, nil)

	require.Equal(t, 1, logs.Len())
	raw, err := json.Marshal(logs.All()[0].ContextMap()["decisions"])
	require.NoError(t, err)
	var entries []map[string]any
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.Len(t, entries, 3)
	got := map[string]any{}
	for _, e := range entries {
		got[e["name"].(string)] = e["limitedBy"]
	}
	assert.Equal(t, "team-quota", got["held"])
	assert.Nil(t, got["overridden"], "not limited: no name")
	assert.Nil(t, got["free"])
}

func TestLogScalingDecisions_NoDecisionsSkipsModel(t *testing.T) {
	ctx, logs := zapObserverCtx(t)

	requests := []allocation.ModelScalingRequest{
		{ModelID: "model-a", Namespace: "ns"},
		{ModelID: "model-b", Namespace: "ns"},
	}
	// Only model-a has a decision; model-b has none.
	decisions := []domain.VariantDecision{
		{ModelID: "model-a", Namespace: "ns", VariantName: "v1", Action: domain.ActionNoChange},
	}

	logScalingDecisions(ctx, requests, decisions, nil)

	require.Equal(t, 1, logs.Len(), "model with no decisions should emit no log line")
	assert.Equal(t, "model-a", logs.All()[0].ContextMap()["modelID"])
}
