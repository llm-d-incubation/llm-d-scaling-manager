package saturation

import (
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// A role's start estimate folds its variants two ways: the fastest for the
// throughput floor's projection, the slowest for a consumer that must not act
// before a replica serves (AnalyzerResult.RoleStartSeconds). Decode has a
// fast and a slow variant; prefill has one, and a role with none measured is
// absent from both.
func TestStartSecondsByRoleFastestAndSlowest(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	input := domain.AnalyzerInput{Namespace: "ns", ModelID: "m", VariantStates: []domain.VariantReplicaState{
		{VariantName: "d-fast", AcceleratorName: "H200", GPUsPerReplica: 1},
		{VariantName: "d-slow", AcceleratorName: "H200", GPUsPerReplica: 8},
		{VariantName: "p", AcceleratorName: "H200", GPUsPerReplica: 1},
		{VariantName: "unmeasured", AcceleratorName: "H200", GPUsPerReplica: 1},
	}}
	roleOf := map[string]string{"d-fast": domain.RoleDecode, "d-slow": domain.RoleDecode,
		"p": domain.RolePrefill, "unmeasured": domain.RoleBoth}
	for variant, s := range map[string]float64{"d-fast": 70, "d-slow": 600, "p": 90} {
		vs := input.VariantStates[0]
		for _, v := range input.VariantStates {
			if v.VariantName == variant {
				vs = v
			}
		}
		a.startSeconds[a.itlWindowKey("ns", "m", variant, vs.AcceleratorName, vs.GPUsPerReplica)] = s
	}

	fastest := a.startSecondsByRole(input, roleOf).sized
	slowest, _ := a.slowestStartSecondsByRole(input, roleOf)
	if fastest[domain.RoleDecode] != 70 || slowest[domain.RoleDecode] != 600 {
		t.Fatalf("decode: fastest %v, slowest %v; want 70 and 600", fastest[domain.RoleDecode], slowest[domain.RoleDecode])
	}
	if fastest[domain.RolePrefill] != 90 || slowest[domain.RolePrefill] != 90 {
		t.Fatalf("prefill, one variant: fastest %v, slowest %v; want 90 both", fastest[domain.RolePrefill], slowest[domain.RolePrefill])
	}
	if _, ok := slowest[domain.RoleBoth]; ok {
		t.Fatal("a role with no measured variant has a start estimate")
	}
}
