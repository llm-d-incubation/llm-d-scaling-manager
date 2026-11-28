package whatif

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// acceptedInput is one decode variant "d" with one speculating row that
// accepted alpha drafts a step of 40 ms, and one row that does not speculate.
func acceptedInput(alpha float64) domain.AnalyzerInput {
	spec := domain.ReplicaMetrics{PodName: "d-0", VariantName: "d", Ready: true, RequestRate: 2, RequestRateKnown: true,
		AvgITL: 0.04, AvgITLKnown: true, SpecAcceptedPerStep: alpha, SpecAcceptedPerStepKnown: true}
	plain := domain.ReplicaMetrics{PodName: "d-1", VariantName: "d", Ready: true, RequestRate: 1, RequestRateKnown: true,
		AvgITL: 0.02, AvgITLKnown: true}
	return domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: []domain.ReplicaMetrics{spec, plain},
		VariantStates: []domain.VariantReplicaState{{VariantName: "d", Role: domain.RoleDecode}}}
}

// A reading over k, the most a step accepts, by more than a rounding error
// gives no group: its step time over an unknown count of tokens is no token
// time. One over k by a rounding error is read at k; one at or under k, a
// variant whose flags state no k, and a nil MaxAccepted read it as it is.
func TestDecodeCycleAcceptedAtMost_WithholdsTheDraftsOverK(t *testing.T) {
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	k3 := func(string) int { return 3 }
	cases := []struct {
		name     string
		alpha    float64
		k        MaxAccepted
		kept     bool
		accepted float64
	}{
		{"over k: withheld", 4.5, k3, false, 0},
		{"over k by rounding: k", 3.000001, k3, true, 3},
		{"at k: as read", 3, k3, true, 3},
		{"under k: as read", 1.2, k3, true, 1.2},
		{"no k stated: as read", 4.5, func(string) int { return 0 }, true, 4.5},
		{"no MaxAccepted: as read", 4.5, nil, true, 4.5},
	}
	for _, tc := range cases {
		c := DecodeCycleAcceptedAtMost(acceptedInput(tc.alpha), at, "d", tc.k)
		if !tc.kept {
			require.Len(t, c.Groups, 1, tc.name)
			assert.False(t, c.Groups[0].AcceptedKnown, tc.name+": only the row that does not speculate")
			assert.InDelta(t, 0.02, c.Groups[0].TokenTime, 1e-12, tc.name)
			continue
		}
		require.Len(t, c.Groups, 2, tc.name)
		assert.True(t, c.Groups[0].AcceptedKnown, tc.name)
		assert.InDelta(t, tc.accepted, c.Groups[0].Accepted, 1e-12, tc.name)
		assert.InDelta(t, 0.04/(1+tc.accepted), c.Groups[0].TokenTime, 1e-12, tc.name)
		assert.False(t, c.Groups[1].AcceptedKnown, tc.name)
		assert.InDelta(t, 0.02, c.Groups[1].TokenTime, 1e-12, tc.name+": no speculation, the step is a token")
	}
}

// DecodeCycleOf and FitCycleOf read every row as it is: they are the nil
// MaxAccepted.
func TestDecodeCycleOf_ReadsTheDraftsAsTheyAre(t *testing.T) {
	at := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	in := acceptedInput(4.5)
	assert.Equal(t, DecodeCycleAcceptedAtMost(in, at, "d", nil), DecodeCycleOf(in, at, "d"))
	assert.Equal(t, FitCycleAcceptedAtMost(in, nil, at, "", "d", nil), FitCycleOf(in, nil, at, "", "d"))
	read := FitCycleAcceptedAtMost(in, nil, at, "", "d", func(string) int { return 3 })
	assert.Len(t, read.Decode.Groups, 1, "the row over k gives no group")
}

func TestAcceptedAtMost(t *testing.T) {
	for _, tc := range []struct {
		alpha float64
		k     int
		want  float64
		ok    bool
	}{{3.0000001, 3, 3, true}, {7, 2, 0, false}, {2.01, 2, 0, false}, {1.5, 2, 1.5, true}, {7, 0, 7, true}} {
		got, ok := AcceptedAtMost(tc.alpha, tc.k)
		assert.Equal(t, tc.ok, ok, "%v at k %d", tc.alpha, tc.k)
		assert.InDelta(t, tc.want, got, 0, "%v at k %d", tc.alpha, tc.k)
	}
}
