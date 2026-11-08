package throughput

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The line's reports (issue ev-shindin/llm-scaling-manager#495, analyzer-learner.md 6.2 and 6.3).

// A variant with a line files one report a cycle, of its own name, saying
// which key priced it and when its newest reading was admitted; and taking
// keeps nothing.
func TestLearnerReports_ALineFilesItsReport(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	warm(t, r, 4, spreadFleet("rev-1"))
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 1, "one report a cycle, not one a cycle kept")
	rep := reports[0]
	assert.Equal(t, learn.Throughput, rep.Learner)
	assert.Contains(t, learn.Reporters(), rep.Learner)
	assert.Equal(t, "v1", rep.Variant)
	assert.Equal(t, variantKey("ns", "m", "v1"), rep.PricingKey)
	assert.Equal(t, learn.Source(itlReasonT1OLS), rep.Source)
	assert.Equal(t, 12, rep.Samples)
	assert.EqualValues(t, 3, rep.Writes)
	assert.Equal(t, lineStart.Add(3*15*time.Second), rep.LastAdmitted)
	assert.Empty(t, r.a.TakeLearnerReports(), "taken once")
}

// With no line, no key priced the variant, so its series is left as it stands.
func TestLearnerReports_NoLineIsNoPricingKey(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	r.step(spreadFleet("rev-1"))
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 1)
	assert.Empty(t, reports[0].PricingKey)
}

// A line fitted above the band has an empty main window, and its age is its
// newest reading there, not "as old as can be".
func TestLearnerReports_AnAboveBandLineHasAnAge(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	early := lineStart.Add(-time.Hour)
	fleet := []domain.ReplicaMetrics{lineRow("pod-a", 0.92, lineAt(0.92), early),
		lineRow("pod-b", 0.93, lineAt(0.93), early), lineRow("pod-c", 0.94, lineAt(0.94), early)}
	var last itl.Reading
	for range 16 {
		r.step(fleet)
		last, _, _ = lineRead(r.a)
	}
	require.True(t, last.AboveBandFit, "premise: fitted above the band")
	rep := r.a.TakeLearnerReports()["ns|m"][0]
	assert.Equal(t, learn.Source(itlReasonT2AboveBand), rep.Source)
	assert.Equal(t, r.now().Add(-15*time.Second), rep.LastAdmitted)
}

// Off, the line files nothing: the negative control of every test here.
func TestLearnerReports_TheSwitchOffFilesNothing(t *testing.T) {
	r := newLineRun(t, withSwitchOff(SwitchITLFilesReports)())
	warm(t, r, 4, spreadFleet("rev-1"))
	r.a.ForgetVariant(context.Background(), "ns", "m", "v1")
	assert.Empty(t, r.a.TakeLearnerReports())
}

// A forget says which key went, by the operator, ahead of the cycle's own
// report, so a key refilled on the same cycle stays published.
func TestLearnerReports_AForgetIsReported(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	warm(t, r, 4, spreadFleet("rev-1"))
	r.a.TakeLearnerReports()
	r.a.ForgetVariant(context.Background(), "ns", "m", "v1")
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 1)
	key := variantKey("ns", "m", "v1")
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, reports[0].Forgotten)

	r.a.ForgetVariant(context.Background(), "ns", "m", "v1")
	assert.Empty(t, r.a.TakeLearnerReports(), "a second forget takes nothing, and says nothing")

	warm(t, r, 4, spreadFleet("rev-1"))
	r.a.ForgetVariant(context.Background(), "ns", "m", "v1")
	warm(t, r, 4, spreadFleet("rev-1"))
	reports = r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 2)
	assert.NotEmpty(t, reports[0].Forgotten)
	assert.Equal(t, key, reports[1].PricingKey)
}

// The sweep says what it evicted, against the variant the key belongs to.
func TestLearnerReports_AnEvictionIsReported(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	warm(t, r, 4, spreadFleet("rev-1"))
	r.a.TakeLearnerReports()
	key := variantKey("ns", "m", "v1")
	r.a.observeDefaultBand(context.Background(), lineStart.Add(3*itl.DefaultObservationMaxAge), "m2", "ns", nil)
	reports := r.a.TakeLearnerReports()["ns|m"]
	require.Len(t, reports, 1)
	assert.Equal(t, "ns", reports[0].Namespace)
	assert.Equal(t, "m", reports[0].ModelID)
	assert.Equal(t, "v1", reports[0].Variant)
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByEvicted}}, reports[0].Forgotten)
}

// Each model's reports are overwritten by its next cycle: with no take they
// stay one cycle's.
func TestLearnerReports_StayBoundedUntaken(t *testing.T) {
	r := newLineRun(t, NewThroughputAnalyzer())
	warm(t, r, 50, spreadFleet("rev-1"))
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	assert.Len(t, r.a.learnerReports["ns|m"], 1)
}
