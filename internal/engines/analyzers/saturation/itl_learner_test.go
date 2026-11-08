package saturation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The ITL line as a learner (itl_learner.go, analyzer-learner.md 6.1-6.3):
// its learn.Report per decode variant, its learner-write line, and Forget
// reaching it by name.

// itlLearnerRig is an analyzer alone (its own tracker: nothing waits) over
// the three-replica fleet, one cycle per call.
type itlLearnerRig struct {
	a   *SaturationAnalyzer
	now time.Time
}

func newITLLearnerRig() *itlLearnerRig {
	r := &itlLearnerRig{a: NewSaturationAnalyzer(capacity.NewStore()), now: itlSettleT0}
	r.a.now = func() time.Time { return r.now }
	return r
}

func (r *itlLearnerRig) cycle(t *testing.T, ctx context.Context, rows []domain.ReplicaMetrics) learn.Report {
	t.Helper()
	_, err := r.a.Analyze(ctx, makeAnalyzerInput(rows, decodeVariants("v")))
	require.NoError(t, err)
	r.now = r.now.Add(restartCycle)
	reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.ITL)
	require.Len(t, reports, 1)
	return reports[0]
}

// The report names what the window was offered and what priced the
// variant: the key once its window holds readings, the tier of its line,
// and when the newest reading was taken.
func TestITLLearner_TheReportNamesTheLineAndItsReadings(t *testing.T) {
	r := newITLLearnerRig()
	first := r.cycle(t, context.Background(), itlSettleFleet())
	assert.Equal(t, learn.ITL, first.Learner)
	assert.Equal(t, domain.RoleDecode, first.Role)
	assert.EqualValues(t, 3, first.Counts["offered"])
	assert.EqualValues(t, 3, first.Writes)
	assert.Equal(t, 3, first.Samples)
	assert.Empty(t, first.PricingKey, "no line of its own priced it")
	assert.Equal(t, itlSettleT0, first.LastAdmitted)
	assert.Empty(t, first.Source, "three readings fit no line yet")

	var last learn.Report
	for range 4 {
		last = r.cycle(t, context.Background(), itlSettleFleet())
	}
	assert.NotEmpty(t, last.Source, "a line once the window holds ten readings")
	assert.NotEmpty(t, last.PricingKey, "its own line priced it")
}

// learner-write logs a line at V(3) when it is first set, when it changes
// tier, and when A or B moves ITLWriteBand: once per key per cycle, and
// never at the default verbosity.
func TestITLLearner_LearnerWriteLogsAMovedLineOnce(t *testing.T) {
	r := newITLLearnerRig()
	ctx, logs := observedCtx(t)
	for range 8 {
		r.cycle(t, ctx, itlSettleFleet())
	}
	writes := learnerWrites(logs, learn.ITL)
	require.Len(t, writes, 1, "the first line is logged once; the same line again is not")
	f := writes[0].ContextMap()
	assert.Equal(t, true, f["firstWrite"])
	assert.Equal(t, learn.ITL, f["learner"])
	assert.Equal(t, int8(-perReplicaLogLevel), int8(writes[0].Level), "at V(3)")

	// Every reading's ITL 30 % higher: the line moves past the band.
	slower := itlSettleFleet()
	for i := range slower {
		slower[i].AvgITL *= 1.3
	}
	before := len(learnerWrites(logs, learn.ITL))
	for range 12 {
		r.cycle(t, ctx, slower)
	}
	assert.Greater(t, len(learnerWrites(logs, learn.ITL)), before, "a line that moved is logged again")

	quiet, quietLogs := infoOnlyCtx()
	r.cycle(t, quiet, slower)
	assert.Empty(t, learnerWrites(quietLogs, learn.ITL), "nothing at the default verbosity")
}

// Forget reaches the ITL line by name: its keys are reported forgotten by
// the operator on the variant's next report, once, and the line is gone.
// The variant's forget drops as many entries as it did before the line was
// on learn's list: the ITL keys counted once, by the learner's code.
func TestITLLearner_ForgetReportsTheLinesKeys(t *testing.T) {
	r := newITLLearnerRig()
	var last learn.Report
	for range 6 {
		last = r.cycle(t, context.Background(), itlSettleFleet())
	}
	require.NotEmpty(t, last.Source, "premise: a line")
	key := last.PricingKey

	r.a.mu.Lock()
	windows := 0
	for k := range r.a.itlTracker.WindowLens() {
		if k == key {
			windows++
		}
	}
	_, hasBaseline := r.a.itlTracker.Baseline(key)
	r.a.mu.Unlock()
	require.Equal(t, 1, windows)

	n := r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
	assert.GreaterOrEqual(t, n, windows+btoi(hasBaseline), "the window and intercept are among what the forget counts")

	idle := itlSettleFleet()
	for i := range idle {
		idle[i].KvUsageInstant = 0.05 // below the band: no reading, the window stays empty
	}
	got := r.cycle(t, context.Background(), idle)
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, got.Forgotten)
	assert.Empty(t, got.PricingKey, "no window remains")
	assert.Empty(t, r.cycle(t, context.Background(), idle).Forgotten, "reported once")
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Review r2: a key whose line drops to none logs that once, with an empty
// source and the line it had as previous; a key that never had a line logs
// nothing, and a line coming back logs again.
func TestITLLearner_LearnerWriteLogsALineThatDrops(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	ctx, logs := observedCtx(t)
	logger := logr.FromContextOrDiscard(ctx)
	line := itl.Reading{Model: itl.Model{A: 0.04, B: 0.008}, Tier: itl.TierOLS}
	var none itl.Reading
	for i, r := range []itl.Reading{line, none, none, line} {
		a.noteITLLearner("ns", "m", "v", domain.RoleDecode, "ns|m|v|H200|1", r, itlSettleT0.Add(time.Duration(i)*restartCycle), logger)
	}
	a.noteITLLearner("ns", "m", "w", domain.RoleDecode, "ns|m|w|H200|1", none, itlSettleT0, logger)
	writes := learnerWrites(logs, learn.ITL)
	require.Len(t, writes, 3, "the first line, its drop once, and its return")
	drop := writes[1].ContextMap()
	assert.Equal(t, "", fmt.Sprint(drop["source"]))
	assert.Equal(t, "ols", fmt.Sprint(drop["previousSource"]))
	assert.Equal(t, false, drop["firstWrite"])
	assert.Equal(t, "ols", fmt.Sprint(writes[2].ContextMap()["source"]))
}
