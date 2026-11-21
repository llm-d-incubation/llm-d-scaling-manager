package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The KV pull is named as the larger wait in TTFT only where it is, on its
// own, larger than the prefill side's wait and than decode admission's
// (issue #301: decode idle by its KV fill, yet the pull is the larger wait in TTFT).
func TestKVPull_SetsTTFTOnlyAsTheLargerWait(t *testing.T) {
	for _, c := range []struct {
		name string
		p    whatif.TTFTParts
		want bool
	}{
		{"pull beats prefill and admission", whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.05, DecodeWait: 0.001}, true},
		{"prefill is the larger wait", whatif.TTFTParts{EngineWait: 0.08, KVPullWait: 0.05}, false},
		{"admission is the decode side's larger part", whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.02, DecodeWait: 0.03}, false},
		{"the router's wait counts on the prefill side", whatif.TTFTParts{EngineWait: 0.01, RouterWait: 0.03, BurstWait: 0.02, KVPullWait: 0.05}, false},
		{"no pull", whatif.TTFTParts{EngineWait: 0.01}, false},
		{"the decode side beats prefill only with admission added", whatif.TTFTParts{EngineWait: 0.04, KVPullWait: 0.03, DecodeWait: 0.02}, false},
	} {
		assert.Equal(t, c.want, pullSetsTTFT(c.p), c.name)
	}
}

// A missed TTFT names the pull, not decode admission, where the pull is the
// decode side's larger wait.
func TestKVPull_ARoleWordNamesThePull(t *testing.T) {
	role, why := ttftRole(whatif.TTFT{Parts: whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.05, DecodeWait: 0.001}}, fleetEval{})
	assert.Equal(t, roleDecodeWord, role)
	assert.True(t, strings.HasPrefix(why, "the KV pull on the decode pods"), why)
	role, why = ttftRole(whatif.TTFT{Parts: whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.01, DecodeWait: 0.03}}, fleetEval{})
	assert.Equal(t, roleDecodeWord, role)
	assert.True(t, strings.HasPrefix(why, "the decode side, a 40 ms mean wait (admission 30 ms, the KV pull 10 ms)"), why)
	role, why = ttftRole(whatif.TTFT{Parts: whatif.TTFTParts{EngineWait: 0.02, DecodeWait: 0.03}}, fleetEval{})
	assert.Equal(t, roleDecodeWord, role)
	assert.True(t, strings.HasPrefix(why, "decode admission"), why)
	role, _ = ttftRole(whatif.TTFT{Parts: whatif.TTFTParts{EngineWait: 0.04, KVPullWait: 0.03, DecodeWait: 0.02}}, fleetEval{})
	assert.Equal(t, roleDecodeWord, role, "the decode side is still the larger wait, though the pull alone is not")
}

// Decode's utilization carries a KV pull row where the pull is priced, and
// says when it sets TTFT; with none priced there is no row.
func TestKVPull_TheUtilizationRow(t *testing.T) {
	var s whatifapi.ReportShape
	wait := 0.05
	s.Utilization.Decode.KVPull = &whatifapi.ReportKVPull{Utilization: 0.62, Wait: &wait, SetsTTFT: true}
	var b strings.Builder
	shapeText(&b, s)
	assert.Contains(t, b.String(), "  KV pull       utilization 0.620 per decode pod, mean wait 50.0 ms: the larger mean wait in TTFT sits on the decode pods\n")

	s.Utilization.Decode.KVPull.SetsTTFT = false
	b.Reset()
	shapeText(&b, s)
	assert.Contains(t, b.String(), "  KV pull       utilization 0.620 per decode pod, mean wait 50.0 ms\n")

	s.Utilization.Decode.KVPull.Wait, s.Utilization.Decode.KVPull.Unpriced = nil, whatifapi.KVPullTTFTRefused
	b.Reset()
	shapeText(&b, s)
	assert.Contains(t, b.String(), "  KV pull       utilization 0.620 per decode pod, mean wait unpriced (TTFT refused)\n")

	s.Utilization.Decode.KVPull.Unpriced = whatifapi.KVPullFirstCome
	b.Reset()
	shapeText(&b, s)
	assert.Contains(t, b.String(), "  KV pull       utilization 0.620 per decode pod, mean wait unpriced (TTFT priced first come: prompts too uneven for processor sharing)\n")

	s.Utilization.Decode.KVPull.Utilization = 1.25
	b.Reset()
	shapeText(&b, s)
	assert.Contains(t, b.String(), "  KV pull       utilization 1.250 per decode pod, saturated: TTFT is refused\n")

	s.Utilization.Decode.KVPull = nil
	b.Reset()
	shapeText(&b, s)
	assert.NotContains(t, b.String(), "KV pull")
}

// On the 0.6B staircase at 9 prefill (#301's case), at 30 r/s, decode admits
// every request at once while the pull on 2 decode pods is the larger wait
// in TTFT; at 4 the pull's utilization halves.
func TestKVPull_TheStaircaseNamesThePullWhileDecodeIdles(t *testing.T) {
	use := func(nd int) whatifapi.ReportDecodeUse {
		q := stairQuery(9, nd, 30)
		q.TTFT = &whatifapi.ReportTarget{Seconds: 0.2, Percentile: 0.95}
		s := stairReport(t, q).Shapes[0]
		require.NotNil(t, s.Utilization.Decode.KVPull, "%d decode: %s; %+v", nd, s.Verdict.Summary, s.Table)
		return s.Utilization.Decode
	}
	two, four := use(2), use(4)
	require.NotNil(t, two.WaitProbability.Value)
	assert.Less(t, *two.WaitProbability.Value, 0.01, "decode admits every request at once")
	assert.True(t, two.KVPull.SetsTTFT)
	require.NotNil(t, two.KVPull.Wait)
	require.NotNil(t, four.KVPull.Wait)
	assert.Greater(t, *two.KVPull.Wait, *four.KVPull.Wait)
	assert.InEpsilon(t, two.KVPull.Utilization/2, four.KVPull.Utilization, 1e-9)
}

// Where the pull saturates the decode pods the TTFT is refused, and the row
// stays: the pull's utilization, at or above 1, with no wait (review of
// #301's row: it vanished exactly where it binds).
func TestKVPull_TheRowStandsWhereThePullSaturates(t *testing.T) {
	q := stairQuery(9, 2, 30)
	q.TTFT = &whatifapi.ReportTarget{Seconds: 0.2, Percentile: 0.95}
	at30 := stairReport(t, q).Shapes[0].Utilization.Decode.KVPull
	require.NotNil(t, at30)
	rate := 30 * 1.2 / at30.Utilization
	q = stairQuery(9, 2, rate)
	q.TTFT = &whatifapi.ReportTarget{Seconds: 0.2, Percentile: 0.95}
	kv := stairReport(t, q).Shapes[0].Utilization.Decode.KVPull
	require.NotNil(t, kv, "at %.1f r/s", rate)
	assert.InDelta(t, 1.2, kv.Utilization, 1e-9)
	assert.Nil(t, kv.Wait)
	assert.False(t, kv.SetsTTFT)
}

// At a prompt spread past processor sharing's (whatif.MaxPSPromptCV) the
// TTFT is the larger of processor sharing's and first come's; where first
// come's is larger it prices no pull stage, and its pull wait of 0 is no
// reading: the row keeps the utilization and gives no wait, rather than a
// mean wait of 0.0 ms (review of #301's row).
func TestKVPull_NoZeroWaitWhereFirstComePrices(t *testing.T) {
	// One prefill pod at 5 r/s, prompt cv 2: first come's queue is the
	// larger; at cv 1 processor sharing prices the pull's wait.
	// The mean TTFT at the seen load answers at both cvs: a refused TTFT
	// also leaves the wait out, and would pass this test for that reason.
	at := func(cv float64) (*whatifapi.ReportKVPull, whatifapi.ReportStat) {
		q := stairQuery(1, 2, 5)
		q.TTFT = &whatifapi.ReportTarget{Seconds: 0.2, Percentile: 0.95}
		q.TTFTPercentiles = []float64{0}
		q.Assume.PromptCV = &cv
		s := stairReport(t, q).Shapes[0]
		kv := s.Utilization.Decode.KVPull
		require.NotNil(t, kv, "cv %g", cv)
		i := slices.IndexFunc(s.Table, func(r whatifapi.ReportRow) bool { return r.Load == "seen" })
		require.GreaterOrEqual(t, i, 0, "cv %g: no seen row", cv)
		require.Len(t, s.Table[i].TTFT, 1)
		mean := s.Table[i].TTFT[0]
		require.Equal(t, "mean", mean.Statistic)
		require.Nil(t, mean.Refused, "cv %g: the mean TTFT answers", cv)
		require.NotNil(t, mean.Value, "cv %g", cv)
		return kv, mean
	}
	ps, psMean := at(1)
	require.NotNil(t, ps.Wait)
	assert.Greater(t, *ps.Wait, 0.0)
	fc, fcMean := at(2)
	assert.InDelta(t, ps.Utilization, fc.Utilization, 1e-12, "the pull's load does not depend on the queue priced")
	assert.Contains(t, fcMean.Flags, string(whatif.SpreadBeyondProcessorSharing), "cv 2 prices first come")
	assert.Greater(t, *fcMean.Value, *psMean.Value, "first come's mean is the larger at cv 2")
	assert.Nil(t, fc.Wait, "first come prices no pull wait")
	assert.Equal(t, whatifapi.KVPullFirstCome, fc.Unpriced)
	assert.Empty(t, ps.Unpriced)
	assert.False(t, fc.SetsTTFT)
}
