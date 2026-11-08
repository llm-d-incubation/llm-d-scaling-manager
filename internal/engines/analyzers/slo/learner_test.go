package slo

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The line as a learner (issue ev-shindin/llm-scaling-manager#495, analyzer-learner.md 6.1): its report, its
// learner-write line, and what a forget and an eviction say.

// learnerCycles runs n cycles of the fleet through Analyze at V(3), and
// returns the learner-write lines.
func learnerCycles(t *testing.T, r *rolloutRig, n int, rows func(i int) []domain.ReplicaMetrics) []observer.LoggedEntry {
	t.Helper()
	core, logs := observer.New(zapcore.Level(-3))
	ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)))
	for i := range n {
		_, err := r.a.Analyze(ctx, domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: policy(44*time.Millisecond, 0),
			VariantStates: []domain.VariantReplicaState{settleAgg}, ReplicaMetrics: rows(i)})
		require.NoError(t, err)
		r.now = r.now.Add(settleCycle)
	}
	return logs.FilterMessage("learner-write").All()
}

func steadyRows(int) []domain.ReplicaMetrics { return settleFleet() }

// A variant with a line files one report a cycle, of its own name, saying
// which key priced it and when its newest reading was admitted; and taking
// keeps nothing.
func TestLearner_ALineFilesItsReport(t *testing.T) {
	r := newRolloutRig(t, nil)
	key := windowKey("ns", "m", settleAgg)
	learnerCycles(t, r, 4, steadyRows)
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 1, "one report a cycle, not one a cycle kept")
	rep := reports[0]
	assert.Equal(t, learn.SLO, rep.Learner)
	assert.Contains(t, learn.Reporters(), rep.Learner)
	assert.Equal(t, "agg", rep.Variant)
	assert.Equal(t, domain.RoleBoth, rep.Role)
	assert.Equal(t, key, rep.PricingKey)
	assert.Equal(t, learn.Source(constants.SLOSourceOwn), rep.Source)
	assert.Equal(t, 12, rep.Samples)
	assert.EqualValues(t, 3, rep.Writes)
	assert.EqualValues(t, 3, rep.Counts["offered"])
	assert.Equal(t, r.now.Add(-settleCycle), rep.LastAdmitted)

	assert.Empty(t, r.a.TakeLearnerReports(), "taken once")
}

// A variant with no line yet reports that it priced nothing, so its series is
// left as it stands, and a prefill variant, which has no line, reports nothing.
func TestLearner_NoLineIsNoPricingKey(t *testing.T) {
	r := newRolloutRig(t, nil)
	learnerCycles(t, r, 1, func(int) []domain.ReplicaMetrics { return nil })
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 1)
	assert.Empty(t, reports[0].PricingKey)
	assert.True(t, reports[0].LastAdmitted.IsZero())

	var d pricingDetail
	r.a.noteLineLearner(logr.Discard(), "ns", "m", domain.VariantReplicaState{VariantName: "p", Role: domain.RolePrefill},
		"k", d, r.now)
	assert.Empty(t, r.a.TakeLearnerReports())
}

// Off, the line files nothing and logs nothing: the negative control of every
// test here.
func TestLearner_TheSwitchOffFilesNothing(t *testing.T) {
	r := newRolloutRig(t, map[string]bool{SwitchITLFilesReports: false})
	assert.Empty(t, learnerCycles(t, r, 8, steadyRows), "learner-write on a line that fitted")
	require.NotEmpty(t, r.a.lines.Readings(windowKey("ns", "m", settleAgg)).All, "premise: the line has readings")
	r.a.ForgetVariant(context.Background(), "ns", "m", "agg")
	assert.Empty(t, r.a.TakeLearnerReports())
	assert.Len(t, learnerCycles(t, newRolloutRig(t, nil), 8, steadyRows), 1, "negative control: on, it logs")
}

// learner-write logs a key's first line, then only what moves it: a steady
// line past its first cycle logs nothing, and the heartbeat logs it once.
func TestLearner_WriteLogsTheFirstLineAndTheHeartbeat(t *testing.T) {
	r := newRolloutRig(t, nil)
	lines := learnerCycles(t, r, 8, steadyRows)
	require.Len(t, lines, 1, "a line that does not move logs once")
	f := lines[0].ContextMap()
	assert.Equal(t, learn.ITL, f["learner"])
	assert.Equal(t, domain.SLOAnalyzerName, f["analyzer"])
	assert.Equal(t, "agg", f["variant"])
	assert.Equal(t, domain.RoleBoth, f["role"])
	assert.Equal(t, windowKey("ns", "m", settleAgg), f["key"])
	assert.Equal(t, true, f["firstWrite"])
	assert.Equal(t, constants.SLOSourceOwn, f["source"])

	r.now = r.now.Add(LearnerHeartbeat)
	lines = learnerCycles(t, r, 2, steadyRows)
	require.Len(t, lines, 1, "the heartbeat, once")
	assert.Equal(t, false, lines[0].ContextMap()["firstWrite"])
}

// A key that never had a line logs nothing; one that loses its line logs that
// once.
func TestLearner_WriteLogsALineLostOnce(t *testing.T) {
	r := newRolloutRig(t, nil)
	assert.Empty(t, learnerCycles(t, r, 2, func(int) []domain.ReplicaMetrics { return nil }), "never had a line")

	r = newRolloutRig(t, nil)
	assert.Len(t, learnerCycles(t, r, 4, steadyRows), 1, "premise: the first line")
	r.a.lines.Sweep(r.now, func(string) bool { return true })
	lost := learnerCycles(t, r, 3, func(int) []domain.ReplicaMetrics { return nil })
	require.Len(t, lost, 1)
	assert.Equal(t, constants.SLOSourceOwn, lost[0].ContextMap()["previousSource"])
	assert.Equal(t, constants.SLOSourceNoLine, lost[0].ContextMap()["source"])

	r.now = r.now.Add(LearnerHeartbeat)
	assert.Empty(t, learnerCycles(t, r, 1, func(int) []domain.ReplicaMetrics { return nil }), "no heartbeat for no line")
}

// A forget says which keys went, by the operator, ahead of the cycle's own
// report, so a key refilled on the same cycle stays published; learner-write
// starts over.
func TestLearner_AForgetIsReported(t *testing.T) {
	r := newRolloutRig(t, nil)
	key := windowKey("ns", "m", settleAgg)
	learnerCycles(t, r, 4, steadyRows)
	r.a.TakeLearnerReports()
	r.a.ForgetVariant(context.Background(), "ns", "m", "agg")
	lines := learnerCycles(t, r, 4, steadyRows)
	require.Len(t, lines, 1)
	assert.Equal(t, true, lines[0].ContextMap()["firstWrite"])
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 2)
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, reports[0].Forgotten)
	assert.Empty(t, reports[0].PricingKey)
	assert.Equal(t, key, reports[1].PricingKey)
}

// A forget of a variant that does not report again still says so.
func TestLearner_AForgetIsReportedWithNoCycleAfterIt(t *testing.T) {
	r := newRolloutRig(t, nil)
	learnerCycles(t, r, 1, steadyRows)
	r.a.TakeLearnerReports()
	r.a.ForgetVariant(context.Background(), "ns", "m", "agg")
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 1)
	assert.Equal(t, "agg", reports[0].Variant)
	require.Len(t, reports[0].Forgotten, 1)
	assert.Empty(t, r.a.TakeLearnerReports(), "said once")
}

// The sweep says what it evicted, against the variant the key belongs to, a
// model with a separator in its name included.
func TestLearner_AnEvictionIsReported(t *testing.T) {
	r := newRolloutRig(t, nil)
	key := windowKey("ns", "a|b", settleAgg)
	for range 4 {
		_, err := r.a.Analyze(context.Background(), domain.AnalyzerInput{Namespace: "ns", ModelID: "a|b", Config: policy(44*time.Millisecond, 0),
			VariantStates: []domain.VariantReplicaState{settleAgg}, ReplicaMetrics: settleFleet()})
		require.NoError(t, err)
		r.now = r.now.Add(settleCycle)
	}
	require.Contains(t, r.a.lineLogged, key, "premise: learner-write logged the line")
	r.a.TakeLearnerReports()
	ev := r.a.evictStale(r.now.Add(r.a.stateTimeout + time.Minute))
	require.Positive(t, ev.Lines, "premise: the key was swept")
	reports := r.a.TakeLearnerReports()["ns|a|b"]
	require.Len(t, reports, 1)
	assert.Equal(t, "ns", reports[0].Namespace)
	assert.Equal(t, "a|b", reports[0].ModelID)
	assert.Equal(t, "agg", reports[0].Variant)
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByEvicted}}, reports[0].Forgotten)
	assert.NotContains(t, r.a.lineLogged, key)
}

// A second Analyze overwrites the model's reports: without the engine taking
// them they stay one cycle's.
func TestLearner_ReportsStayBoundedUntaken(t *testing.T) {
	r := newRolloutRig(t, nil)
	learnerCycles(t, r, 100, steadyRows)
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	assert.Len(t, r.a.learnerReports["ns|m"], 1)
}
