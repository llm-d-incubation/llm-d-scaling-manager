package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// concReport runs at-replicas -o json and decodes it.
func concReport(t *testing.T, args ...string) whatifapi.ReportResponse {
	t.Helper()
	out, code := runCmd(t, append(args, "-o", "json")...)
	require.Contains(t, []int{0, 3}, code, out)
	var rep whatifapi.ReportResponse
	require.NoError(t, json.Unmarshal([]byte(out), &rep), out)
	return rep
}

// rowOf is the shape's row for load.
func rowOf(t *testing.T, s whatifapi.ReportShape, load string) whatifapi.ReportRow {
	t.Helper()
	for _, r := range s.Table {
		if r.Load == load {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", load, s.Table)
	return whatifapi.ReportRow{}
}

// Every row of the report carries its mean concurrency, rate x e2e mean;
// a concurrency asked as a load (--load 64c) prices at the rate that holds
// it, which reads the same concurrency back; one past what the fleet holds
// below its knee prices at the knee, flagged, where the concurrency is
// unbounded.
func TestAtReplicas_Concurrency(t *testing.T) {
	rep := concReport(t, atStair("--load", "50%", "--load", "100%")...)
	require.Len(t, rep.Shapes, 1)
	s := rep.Shapes[0]
	half := rowOf(t, s, "50%")
	require.NotNil(t, half.Concurrency)
	require.NotNil(t, half.Concurrency.Fleet.Value, "%+v", half.Concurrency.Fleet.Refused)
	require.NotNil(t, half.E2EMeanSum.Value)
	assert.InDelta(t, half.Rate**half.E2EMeanSum.Value, *half.Concurrency.Fleet.Value, 1e-9, "Little's law on the row's own figures")
	require.NotNil(t, half.Concurrency.Decode)
	require.NotNil(t, half.Concurrency.DecodePerPod)
	assert.InDelta(t, *half.Concurrency.Decode.Value/float64(rep.Fleet.Decode), *half.Concurrency.DecodePerPod.Value, 1e-9)
	full := rowOf(t, s, "100%")
	require.NotNil(t, full.Concurrency)
	assert.True(t, full.Concurrency.Unbounded, "at the knee the queue has no mean")
	assert.Nil(t, full.Concurrency.Fleet.Value)
	require.NotNil(t, s.Capacity.FleetKnee.Concurrency)
	assert.True(t, s.Capacity.FleetKnee.Concurrency.Unbounded)
	require.NotNil(t, s.Capacity.Operating.Concurrency)
	assert.NotNil(t, s.Capacity.Operating.Concurrency.Fleet.Value, "the operating point, under the knee, holds a mean")

	n := *half.Concurrency.Fleet.Value
	asked := strconv.FormatFloat(n, 'f', -1, 64) + "c"
	rep = concReport(t, atStair("--load", asked, "--load", "100000c")...)
	s = rep.Shapes[0]
	assert.Empty(t, s.Untranslated)
	back := rowOf(t, s, asked)
	require.NotNil(t, back.FromConcurrency)
	assert.False(t, back.FromConcurrency.AtKnee)
	assert.InDelta(t, half.Rate, back.Rate, 1e-4*half.Rate, "the 50%% row's concurrency translates back into its rate")
	require.NotNil(t, back.Concurrency.Fleet.Value)
	assert.InDelta(t, n, *back.Concurrency.Fleet.Value, 1e-3*n)
	// A huge concurrency: the staircase's TTFT is a queue's (the KV pull on
	// the decode pods saturates short of the fleet knee), so it is reached
	// just under where the queue saturates; were it not, the row would be
	// the knee, flagged.
	big := rowOf(t, s, "100000c")
	require.NotNil(t, big.FromConcurrency)
	if big.FromConcurrency.AtKnee {
		assert.True(t, strings.HasSuffix(big.Label, "saturates"), big.Label)
		assert.True(t, big.Concurrency.Unbounded)
	} else {
		assert.Less(t, big.Rate, *s.Capacity.FleetKnee.Value)
		require.NotNil(t, big.Concurrency.Fleet.Value)
		assert.InDelta(t, 1e5, *big.Concurrency.Fleet.Value, 1e-3*1e5)
	}

	text, _ := runCmd(t, atStair("--load", "64c")...)
	assert.Contains(t, text, "in system (mean)")
	assert.Contains(t, text, "64c concurrent")
}

// Where the mean TTFT is refused (no prefill speed learned), a concurrency
// load has no time in system to translate with: its row is left out and the
// shape says why, and each row's concurrency is refused with the TTFT.
func TestAtReplicas_ConcurrencyRefused(t *testing.T) {
	rep := concReport(t, "at-replicas", "--recording", outputSpreadRun, "--tpot", "5ms", "--load", "50%", "--load", "16c")
	s := rep.Shapes[0]
	require.Len(t, s.Untranslated, 1)
	assert.Equal(t, "16c", s.Untranslated[0].Load)
	assert.Contains(t, s.Untranslated[0].Reason, "the mean time in the system is refused")
	for _, r := range s.Table {
		assert.NotEqual(t, "16c", r.Load)
	}
	half := rowOf(t, s, "50%")
	require.NotNil(t, half.Concurrency)
	assert.Nil(t, half.Concurrency.Fleet.Value)
	assert.NotNil(t, half.Concurrency.Fleet.Refused)
	assert.NotNil(t, half.Concurrency.Decode, "decode's occupancy stands on TPOT alone")
}

// --load reads a concurrency, and refuses one that is not a count above 0.
func TestAtReplicas_ConcurrencyFlag(t *testing.T) {
	for _, bad := range []string{"0c", "-3c", "c", "2000000c"} {
		out, code := runCmd(t, atStair("--load", bad)...)
		assert.Equal(t, 2, code, "--load %s: %s", bad, out)
	}
}

// A knee priced from a lower bound of V_P is not known to be where requests
// queue without bound: its concurrency reads "not known", not unbounded, and
// a concurrency asked is never called saturating there (#280). One it holds
// is a rate of at least its figure, its time in system an upper bound.
func TestAtReplicas_ConcurrencyAtLowerBoundKnee(t *testing.T) {
	rec := lowerBoundRecording(t)
	rep := concReport(t, "at-replicas", "--recording", rec, "--load-seen", "8", "--load", "50%", "--load", "1000000c")
	s := rep.Shapes[0]
	require.True(t, s.Capacity.PrefillKnee.LowerBound)
	require.True(t, s.Capacity.FleetKnee.LowerBound, "the prefill knee sets the fleet's")
	for _, k := range []whatifapi.ReportRate{s.Capacity.PrefillKnee, s.Capacity.FleetKnee} {
		require.NotNil(t, k.Concurrency)
		assert.False(t, k.Concurrency.Unbounded)
		require.NotNil(t, k.Concurrency.Fleet.Refused)
		assert.Equal(t, "PastLowerBoundKnee", k.Concurrency.Fleet.Refused.Reason)
	}
	// The time in system priced from the lower bound grows without bound at
	// the lower-bound knee, so a huge concurrency is reached just under it:
	// a rate of at least that, never one called saturating. Were it not
	// reached, the load would be left untranslated, never priced at the knee.
	if len(s.Untranslated) == 1 {
		assert.Contains(t, s.Untranslated[0].Reason, "lower bound of V_P")
	} else {
		big := rowOf(t, s, "1000000c")
		require.NotNil(t, big.FromConcurrency)
		assert.False(t, big.FromConcurrency.AtKnee)
		assert.True(t, big.FromConcurrency.LowerBound)
		assert.Less(t, big.Rate, *s.Capacity.FleetKnee.Value)
	}

	half := rowOf(t, s, "50%")
	require.NotNil(t, half.Concurrency.Fleet.Value)
	require.True(t, half.Concurrency.Fleet.UpperBound)
	asked := strconv.FormatFloat(*half.Concurrency.Fleet.Value, 'f', -1, 64) + "c"
	rep = concReport(t, "at-replicas", "--recording", rec, "--load-seen", "8", "--load", asked)
	back := rowOf(t, rep.Shapes[0], asked)
	require.NotNil(t, back.FromConcurrency)
	assert.False(t, back.FromConcurrency.AtKnee)
	assert.True(t, back.FromConcurrency.LowerBound)

	text, _ := runCmd(t, "at-replicas", "--recording", rec, "--load-seen", "8", "--load", asked)
	assert.Contains(t, text, "not known")
	assert.Contains(t, text, asked+": at least")
}
