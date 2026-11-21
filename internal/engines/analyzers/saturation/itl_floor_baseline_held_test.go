package saturation

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The held floor B is on from the constructor, and a replay turns it off.
func TestITLFloorBaselineHeld_OnFromTheConstructor(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	fields := a.switchFields()
	require.Contains(t, fields, SwitchITLFloorBaselineHeld)
	assert.True(t, *fields[SwitchITLFloorBaselineHeld], "on from the constructor")
	assert.True(t, a.floorBaselineHeldOn())

	off := NewSaturationAnalyzer(capacity.NewStore())
	require.NoError(t, off.SetSwitchesForReplay(map[string]bool{SwitchITLFloorBaselineHeld: false}))
	assert.False(t, off.floorBaselineHeldOn(), "a replay turns it off")
}

// heldRun is one decode variant's line and service rate per cycle through
// Analyze: 20 cycles below the band, then n inside it at a spread too narrow
// for a full fit, none of its replicas below it, on tracedModel throughout.
type heldRun struct {
	b, a, rate []float64
	logs       []string
}

func runHeldFleet(t *testing.T, switches map[string]bool, n int) heldRun {
	t.Helper()
	const v = "decode-held"
	states := []domain.VariantReplicaState{
		{VariantName: v, Role: domain.RoleDecode, AcceleratorName: srAccel, CurrentReplicas: 12, GPUsPerReplica: 1},
	}
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	clk := clocktesting.NewFakeClock(start)
	a := srAnalyzer(v).WithClock(clk)
	if switches != nil {
		require.NoError(t, a.SetSwitchesForReplay(switches))
	}
	ctx, logs := observedCtx(t)
	var out heldRun
	for c := range 20 + n {
		lo, hi := 0.02, 0.10
		if c >= 20 {
			lo, hi = 0.30, 0.36
		}
		rows := lowFillReadings(v, 12, lo, hi)
		for i := range rows {
			rows[i].ReadySince = start.Add(-time.Hour)
		}
		res, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
		require.NoError(t, err)
		if c >= 20 {
			sig := srSignal(t, res, v)
			require.True(t, sig.HasLine, "cycle %d: premise: a line", c)
			out.b = append(out.b, sig.LineIntercept)
			out.a = append(out.a, sig.LineSlope)
			out.rate = append(out.rate, sig.Rate)
		}
		clk.Step(15 * time.Second)
	}
	for _, e := range logs.FilterMessage("itl-fit").All() {
		kv := make([]string, 0, len(e.Context))
		for _, f := range e.Context {
			kv = append(kv, f.Key)
		}
		out.logs = append(out.logs, strings.Join(kv, ","))
	}
	return out
}

// Through Analyze, with the constructor's switches: a decode fleet held inside
// the band for an hour after it ran below it keeps the floor's B, its slope
// and its service rate on every cycle. With the switch off (the negative
// control) the floor ages out after 30 minutes, B steps to the 6 ms default,
// and the slope and service rate step with it, with no change in load
// (issue #333).
func TestITLFloorBaselineHeld_AnalyzeHoldsTheFloorThroughAnHourInTheBand(t *testing.T) {
	const hour = 240
	on := runHeldFleet(t, nil, hour)
	for c := range on.b {
		require.InDelta(t, tracedModel.B, on.b[c], 0.0002, "on, cycle %d (%s in the band): B", c, time.Duration(c)*15*time.Second)
		require.InDelta(t, on.a[0], on.a[c], 1e-9, "on, cycle %d: the slope stepped", c)
		require.InDelta(t, on.rate[0], on.rate[c], 1e-9*on.rate[0], "on, cycle %d: the service rate stepped", c)
	}
	held := 0
	for _, l := range on.logs {
		if strings.Contains(l, "floorBHeld") {
			held++
		}
	}
	assert.Positive(t, held, "the itl-fit line names the held B")

	off := runHeldFleet(t, map[string]bool{SwitchITLFloorBaselineHeld: false}, hour)
	stepAt := -1
	for c := range off.b {
		if off.b[c] == itl.DefaultBaselineSec {
			stepAt = c
			break
		}
	}
	require.Positive(t, stepAt, "negative control: off, B never left the floor's")
	stepTime := time.Duration(stepAt) * 15 * time.Second
	require.True(t, stepTime >= 25*time.Minute && stepTime <= 31*time.Minute,
		"negative control: off, the step at %s, want it where the floor ages out", stepTime)
	assert.InDelta(t, tracedModel.B, off.b[stepAt-1], 0.0002, "off: the floor's B until it aged out")
	// tracedModel's B is under the default, so here the step flattens the
	// slope and raises the service rate (fewer replicas); over it, as on the
	// GPU check's fleet, it steepens the slope and lowers the rate.
	assert.Greater(t, math.Abs(off.a[stepAt]/off.a[stepAt-1]-1), 0.2, "off: the slope steps")
	assert.Greater(t, math.Abs(off.rate[stepAt]/off.rate[stepAt-1]-1), 0.1, "off: the service rate steps")
	for _, l := range off.logs {
		assert.NotContains(t, l, "floorBHeld", "off: the itl-fit line is as it was")
	}
	t.Logf("off at %s in the band: B %.4f -> %.4f, A %.4f -> %.4f (%+.0f %%), rate %.3f -> %.3f; on: flat over %d cycles",
		time.Duration(stepAt)*15*time.Second, off.b[stepAt-1], off.b[stepAt], off.a[stepAt-1], off.a[stepAt],
		(off.a[stepAt]/off.a[stepAt-1]-1)*100, off.rate[stepAt-1], off.rate[stepAt], len(on.b))
	if math.IsNaN(off.rate[stepAt]) {
		t.Errorf("off: a NaN service rate")
	}
}

// A floor slope's stamp goes out in the snapshot and comes back with it; a
// stamp past the clock skew comes back as unknown, never younger than now.
func TestITLFloorBaselineHeld_ASnapshotKeepsTheStamp(t *testing.T) {
	running := mtParams(64, "vllm:v1")
	now := time.Now()
	for _, c := range []struct {
		name       string
		at, wantAt time.Time
	}{
		{"stamped", now.Add(-2 * time.Hour), now.Add(-2 * time.Hour)},
		{"no stamp", time.Time{}, time.Time{}},
		{"far future", now.Add(24 * time.Hour), time.Time{}},
	} {
		a, _, _ := waProcess(running)
		key, _, _ := itlStashKeyAndArgs(a)
		a.RestoreLearnedState(LearnedState{
			Config:        map[string]string{"test-ns/variant-a": running.Fingerprint()},
			Args:          map[string]string{"test-ns/variant-a": running.ArgsFingerprint()},
			VariantSeenAt: map[string]time.Time{key: now},
			ITLSlopes: map[string]SlopeSnapshot{key: {A: 0.05, FittedAt: now.Add(-time.Hour),
				Args: running.ArgsFingerprint(), FloorB: 0.012, FloorBAt: c.at}},
		})
		_, done := a.installRestored("test-ns", "test-model", "variant-a")
		require.True(t, done, "%s: precondition: the variant's entries were pending", c.name)
		got, installed := a.itlTracker.Slopes()[key]
		require.True(t, installed, c.name)
		assert.True(t, got.FloorBAt.Equal(c.wantAt), "%s: restored stamp %v, want %v", c.name, got.FloorBAt, c.wantAt)
		out := a.exportLearnedState().ITLSlopes[key]
		assert.True(t, out.FloorBAt.Equal(c.wantAt), "%s: exported stamp %v, want %v", c.name, out.FloorBAt, c.wantAt)
	}
}
