package slo

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The line in a rollout (issue #481), through Analyze, with the switch on and
// off.

// newRevision is the revision a rollout brings.
const newRevision = "rev-2"

// rolloutRig runs Analyze over a store holding agg's engine.
type rolloutRig struct {
	t     *testing.T
	a     *Analyzer
	store *capacity.Store
	now   time.Time
	// seqs is the engine's stated cap, and last the last cycle's result.
	seqs int64
	last *domain.AnalyzerResult
}

func newRolloutRig(t *testing.T, switches map[string]bool) *rolloutRig {
	t.Helper()
	r := &rolloutRig{t: t, store: capacity.NewStore(), now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), seqs: 64}
	r.file(false)
	r.a = New(nil, r.store)
	r.a.now = func() time.Time { return r.now }
	require.NoError(t, r.a.SetSwitchesForReplay(switches))
	return r
}

// file files agg's engine, rolling out or not.
func (r *rolloutRig) file(rolling bool) {
	r.store.Update("ns", "m", "agg", capacity.Record{EngineParams: &capacity.EngineParams{MaxNumSeqs: r.seqs, MaxNumSeqsSet: true},
		RollingOut: rolling}, r.now)
}

// cycle analyzes rows, and returns how many readings the line's window holds.
func (r *rolloutRig) cycle(rows []domain.ReplicaMetrics) int {
	r.t.Helper()
	res, err := r.a.Analyze(context.Background(), domain.AnalyzerInput{Namespace: "ns", ModelID: "m",
		Config: policy(44*time.Millisecond, 0), VariantStates: []domain.VariantReplicaState{settleAgg}, ReplicaMetrics: rows})
	require.NoError(r.t, err)
	r.last = res
	r.now = r.now.Add(settleCycle)
	return r.a.lines.WindowLen(windowKey("ns", "m", settleAgg))
}

func revised(rows []domain.ReplicaMetrics, revision string) []domain.ReplicaMetrics {
	for i := range rows {
		rows[i].Revision = revision
	}
	return rows
}

// While the scale target says it rolls out, the window takes nothing; when
// the rollout ends it reads again. Off, it takes every row.
func TestRollout_ThePublishedRolloutOffersNoRows(t *testing.T) {
	run := func(switches map[string]bool) (before, during, after int) {
		r := newRolloutRig(t, switches)
		before = r.cycle(revised(settleFleet(), "rev-1"))
		r.file(true)
		during = r.cycle(revised(settleFleet(), "rev-1"))
		r.file(false)
		after = r.cycle(revised(settleFleet(), newRevision))
		return before, during, after
	}
	before, during, after := run(nil)
	require.Equal(t, 3, before, "premise: the window takes the rows")
	assert.Equal(t, before, during, "the window took a reading mid-rollout")
	assert.Equal(t, 3, after, "the old readings go, and the new revision's come")

	_, during, _ = run(map[string]bool{SwitchITLRolloutGuard: false})
	assert.Equal(t, 6, during, "negative control: off, the window takes them")
}

// Revisions that disagree are a rollout whatever the status says, and the new
// pods go on teaching the line.
func TestRollout_MixedRevisionsOfferTheNewestOnly(t *testing.T) {
	run := func(switches map[string]bool) int {
		r := newRolloutRig(t, switches)
		rows := aged(revised(settleFleet(), "rev-1"), r.now.Add(-time.Hour))
		rows[1].Revision, rows[1].ReadySince = newRevision, r.now
		return r.cycle(rows)
	}
	assert.Equal(t, 1, run(nil))
	assert.Equal(t, 3, run(map[string]bool{SwitchITLRolloutGuard: false}), "negative control")
}

// The guard is per variant: a sibling rolling out leaves this line reading,
// and an analyzer with no store still reads the revisions.
func TestRollout_OneVariantsRolloutIsNotAnother(t *testing.T) {
	r := newRolloutRig(t, nil)
	r.store.Update("ns", "m", "other", capacity.Record{RollingOut: true}, r.now)
	assert.Equal(t, 3, r.cycle(revised(settleFleet(), "rev-1")))

	bare := New(nil, nil)
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m"}
	rows, held := bare.lineRows(in, fleetReading{mixed: map[string]bool{"agg": true}}, "k", "agg")
	assert.Nil(t, rows)
	assert.True(t, held)
}

func aged(rows []domain.ReplicaMetrics, ready time.Time) []domain.ReplicaMetrics {
	for i := range rows {
		rows[i].ReadySince = ready
	}
	return rows
}

// stall warms a line, rolls out and stalls for cycles at 15 s, and counts the
// cycles the variant had no price.
func stall(t *testing.T, hold time.Duration, c stallCase, cycles int) (int, *rolloutRig) {
	t.Helper()
	r := newRolloutRig(t, nil)
	r.a.WithSlopeHoldFor(hold)
	early := r.now.Add(-time.Hour)
	for range 8 {
		r.cycle(aged(revised(settleFleet(), "rev-1"), early))
	}
	require.Contains(t, r.last.VariantCapacities[0].Reason, "slo-", "premise: priced")
	r.seqs = 64
	if c.argsChange {
		r.seqs = 128
	}
	r.file(true)
	first := r.now
	silent := 0
	for range cycles {
		rows := aged(revised(settleFleet(), "rev-1"), early)
		if c.newPod {
			pod := row("agg", "agg-new", 0.45)
			pod.Revision, pod.ReadySince = newRevision, first
			rows = append(rows, pod)
		}
		r.cycle(rows)
		if r.last.VariantCapacities[0].Reason == domain.ReasonNoData {
			silent++
		}
	}
	return silent, r
}

type stallCase struct {
	name         string
	argsChange   bool
	newPod       bool
	silentAtMost int
}

// The guard and the forget together on a rollout that stalls: priced on every
// cycle while the old pods serve, whether or not the arguments changed, and a
// new pod brings the line back. As first built, an arguments change left this
// line without a price for all 720 cycles.
func TestRollout_AStalledRolloutDoesNotSilenceTheLine(t *testing.T) {
	for _, c := range []stallCase{
		{name: "image only, no new pod"},
		{name: "new arguments, no new pod", argsChange: true},
		{name: "image only, a new pod", newPod: true},
		// Forgotten when the new pod comes: its settle, then 10 readings.
		{name: "new arguments, a new pod", argsChange: true, newPod: true, silentAtMost: 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			silent, r := stall(t, 7*24*time.Hour, c, 720)
			assert.LessOrEqual(t, silent, c.silentAtMost)
			assert.NotEqual(t, domain.ReasonNoData, r.last.VariantCapacities[0].Reason)
		})
	}
}

// Past the hold the variant has no price, and the composite reads no need into
// it: not eligible, so it neither sizes the role nor votes it down.
func TestRollout_PastTheHoldTheCompositeReadsNoNeed(t *testing.T) {
	hold := time.Hour
	cycles := int((itl.DefaultObservationMaxAge + hold + 10*time.Minute) / settleCycle)
	silent, r := stall(t, hold, stallCase{argsChange: true}, cycles)
	assert.Positive(t, silent)
	nr := allocation.NamedAnalyzerResult{Name: domain.SLOAnalyzerName, Result: r.last, Live: true}
	assert.False(t, allocation.Eligible(nr), "no price read as a figure")
	assert.False(t, allocation.ResolveSO([]allocation.NamedAnalyzerResult{nr}, nil, "agg").OK)
}

// A line resting on its held slope, the window aged out under light load, goes
// on through a rollout that changes the arguments: the slope is the old
// engine's, and the old engine serves.
func TestRollout_AHeldSlopeOutlivesNewArgumentsWithNoNewPod(t *testing.T) {
	r := newRolloutRig(t, nil)
	r.a.WithSlopeHoldFor(7 * 24 * time.Hour)
	early := r.now.Add(-time.Hour)
	for range 8 {
		r.cycle(aged(revised(settleFleet(), "rev-1"), early))
	}
	r.now = r.now.Add(2 * itl.DefaultObservationMaxAge)
	light := aged(revised([]domain.ReplicaMetrics{row("agg", "agg-a", 0.001)}, "rev-1"), early)
	require.Zero(t, r.cycle(light), "premise: the window aged out")
	require.NotEqual(t, domain.ReasonNoData, r.last.VariantCapacities[0].Reason, "premise: the slope prices")
	r.seqs = 128
	r.file(true)
	r.cycle(light)
	assert.NotEqual(t, domain.ReasonNoData, r.last.VariantCapacities[0].Reason, "the hold refused the slope under the new arguments")
}

// A held line's change of arguments waits for a new pod, and is filed then.
func TestRollout_TheForgetWaitsForANewPod(t *testing.T) {
	_, r := stall(t, time.Hour, stallCase{argsChange: true}, 8)
	assert.Empty(t, r.a.TakeLearnerReports()["ns|m"][0].Forgotten, "forgotten with the old pods serving")
	pod := row("agg", "agg-new", 0.45)
	pod.Revision, pod.ReadySince = newRevision, r.now
	r.cycle(append(aged(revised(settleFleet(), "rev-1"), r.now.Add(-time.Hour)), pod))
	reports := r.a.TakeLearnerReports()["ns|m"]
	assert.Equal(t, []learn.Forgotten{{Key: windowKey("ns", "m", settleAgg), By: learn.ByArguments}}, reports[0].Forgotten)
}

// The first rows of a new revision take the tier latch with the window: what
// put it at the full fit was old pods' readings. Off, it stands.
func TestRollout_TheLatchGoesWhenItEnds(t *testing.T) {
	run := func(switches map[string]bool) bool {
		r := newRolloutRig(t, switches)
		key := windowKey("ns", "m", settleAgg)
		for range 8 {
			r.cycle(revised(settleFleet(), "rev-1"))
		}
		require.True(t, r.a.lines.Readings(key).OLSLatched, "premise: the line stands at the full fit")
		r.file(true)
		r.cycle(revised(settleFleet(), "rev-1"))
		require.True(t, r.a.lines.Readings(key).OLSLatched, "the tier moved mid-rollout, with no new reading")
		r.file(false)
		r.cycle(revised(settleFleet(), newRevision))
		return r.a.lines.Readings(key).OLSLatched
	}
	assert.False(t, run(nil))
	assert.True(t, run(map[string]bool{SwitchITLRolloutGuard: false}), "negative control")
}

// The line is priced from what it holds through a rollout: a window of the
// old engine's readings under the same arguments is not forgotten.
func TestRollout_TheLineFitsFromWhatItHolds(t *testing.T) {
	r := newRolloutRig(t, nil)
	var got int
	for range settleCycles + 8 {
		got = r.cycle(revised(settleFleet(), "rev-1"))
	}
	r.file(true)
	assert.Equal(t, got, r.cycle(revised(settleFleet(), "rev-1")))
	reading := r.a.lines.Observe(windowKey("ns", "m", settleAgg), "agg", nil, admission(policy(44*time.Millisecond, 0), nil, true), r.now)
	assert.NotEmpty(t, reading.Tier, "no line from a full window")
}
