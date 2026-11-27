package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// An OVER-PROVISIONED verdict never cuts decode below the count the decode
// knee's error bound needs (#363). Two prefill and three decode replicas at
// 20 r/s on the staircase: two decode replicas hold at the point estimate,
// but three are needed if the decode knee reads 99% high, as it may outside
// its validated region. Decode has no spare, prefill has none either, so the
// fleet is RIGHT-SIZED, and the role says the count the bound needs.
func TestAtReplicas_OverKeepsDecodeAtItsErrorBound(t *testing.T) {
	v := stairReport(t, stairQuery(2, 3, 20)).Shapes[0].Verdict
	assert.Equal(t, whatifapi.VerdictRight, v.Verdict, v.Summary)
	assert.Empty(t, v.Shrink, v.Summary)
	require.Len(t, v.Roles, 2)
	for _, r := range v.Roles {
		assert.False(t, r.Spare, "%s: %s", r.Role, r.Detail)
		if r.Role == roleDecode {
			assert.Contains(t, r.Detail, "no spare: 3 decode if the decode knee reads 99% high", v.Summary)
		}
	}
	assert.NotContains(t, v.Summary, "needs 2 prefill + 2 decode (has 2 + 3)")
}

// Across fleets and loads, a needed fleet that cuts decode cuts it no lower
// than the decode count at the knee's error bound: DecodeAtErrorBound is
// never above a cut decode count. A decode spare is the same rule: one fewer
// decode replica still meets the bound. The sweep must see a decode cut,
// or it proves nothing.
func TestAtReplicas_NoDecodeCutBelowTheBound(t *testing.T) {
	cuts := 0
	for _, load := range []float64{20, 0} {
		for np := 2; np <= 4; np++ {
			for nd := 2; nd <= 7; nd++ {
				name := fmt.Sprintf("%d+%d@%g", np, nd, load)
				v := stairReport(t, stairQuery(np, nd, load)).Shapes[0].Verdict
				if v.Needed == nil || v.Needed.Unmet != "" || v.Needed.Decode >= nd {
					continue
				}
				cuts++
				assert.LessOrEqual(t, v.Needed.DecodeAtErrorBound, v.Needed.Decode, "%s: %s", name, v.Summary)
				for _, r := range v.Roles {
					if r.Role == roleDecode && r.Spare {
						assert.LessOrEqual(t, v.Needed.DecodeAtErrorBound, nd-1, "%s: %s", name, v.Summary)
					}
				}
			}
		}
	}
	assert.Positive(t, cuts, "no fleet in the sweep cut decode")
}
