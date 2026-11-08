package steadystate

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

// TestCompositeHoldLogSaysWhy: the line a held role logs every cycle names
// the rule that holds it. It looked the same for all of them, so a role
// held because nothing listed sizes it read like one waiting out an
// analyzer's grace.
func TestCompositeHoldLogSaysWhy(t *testing.T) {
	reasons := func(named, eligible []allocation.NamedAnalyzerResult) map[string]string {
		t.Helper()
		ctx, logs := zapObserverCtx(t)
		buildComposite(ctx, named, eligible, 0.85, 0.7)
		out := map[string]string{}
		for _, l := range logs.FilterMessage("composite holds a role").All() {
			out[fmt.Sprint(l.ContextMap()["role"])] = fmt.Sprint(l.ContextMap()["reason"])
		}
		return out
	}

	t.Run("nothing listed sizes the role", func(t *testing.T) {
		sat := pdEntry(domain.SaturationAnalyzerName,
			map[string]float64{domain.RolePrefill: 400, domain.RoleDecode: 900}, 100)
		other := pdEntry("throughput", map[string]float64{domain.RoleDecode: 900}, 100)
		got := reasons([]allocation.NamedAnalyzerResult{sat, other}, []allocation.NamedAnalyzerResult{other})
		assert.Equal(t, map[string]string{
			domain.RolePrefill: "[" + holdUnsized + "]",
			domain.RoleDecode:  "[" + holdOtherRoleUnsized + "]",
		}, got)
	})

	t.Run("a listed analyzer is inside its grace", func(t *testing.T) {
		sat := liveEntry(domain.SaturationAnalyzerName, 0, 200)
		grace := inGrace("throughput")
		got := reasons([]allocation.NamedAnalyzerResult{sat, grace}, []allocation.NamedAnalyzerResult{sat, grace})
		assert.Equal(t, map[string]string{domain.RoleBoth: "[" + holdAnalyzerInGrace + "throughput]"}, got)
	})

	t.Run("saturation holds a role another analyzer prices", func(t *testing.T) {
		sat := pdEntry(domain.SaturationAnalyzerName,
			map[string]float64{domain.RolePrefill: 500, domain.RoleDecode: 900}, 100)
		sat.Result.HeldRoleDemand = map[string]float64{domain.RolePrefill: 300}
		other := pdEntry("slo", map[string]float64{domain.RolePrefill: 600}, 100)
		named := []allocation.NamedAnalyzerResult{sat, other}
		got := reasons(named, named)
		assert.Equal(t, map[string]string{domain.RolePrefill: "[" + holdSaturationHolds + "]"}, got)
	})

	t.Run("two rules hold one role", func(t *testing.T) {
		sat := pdEntry(domain.SaturationAnalyzerName,
			map[string]float64{domain.RolePrefill: 500, domain.RoleDecode: 900}, 100)
		sat.Result.HeldRoleDemand = map[string]float64{domain.RolePrefill: 300}
		other := pdEntry("slo", map[string]float64{domain.RolePrefill: 600}, 100)
		grace := inGrace("throughput")
		named := []allocation.NamedAnalyzerResult{sat, other, grace}
		got := reasons(named, named)
		require.Contains(t, got, domain.RolePrefill)
		assert.Equal(t, "["+holdAnalyzerInGrace+"throughput "+holdSaturationHolds+"]", got[domain.RolePrefill])
	})
}
