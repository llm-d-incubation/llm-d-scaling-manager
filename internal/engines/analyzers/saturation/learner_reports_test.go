package saturation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// The learners' reports reaching the engine, restored on their lines, and mu
// on learn's list (analyzer-learner.md 6.1 to 6.3, 3.6).

func oneFull() []domain.ReplicaMetrics {
	return []domain.ReplicaMetrics{fullRow("p0", "v", restartT0.Add(-time.Hour))}
}

// unqueuedRow is the replica of oneFull with nothing queued: nothing is admitted.
func unqueuedRow() []domain.ReplicaMetrics {
	row := fullRow("p0", "v", restartT0.Add(-time.Hour))
	row.QueueLength = 0
	return []domain.ReplicaMetrics{row}
}

// TakeLearnerReports hands the reports over: a second take is empty, so a
// cycle whose Analyze fails publishes nothing twice and a forgotten key is
// reported once. The next cycle files anew.
func TestTakeLearnerReports_HandsOverAndKeepsNone(t *testing.T) {
	r := newRestartRig(t, restartT0)
	r.cycle(oneFull(), decodeVariants("v"))
	got := r.a.TakeLearnerReports()
	assert.NotEmpty(t, reportsOf(got["test-ns|test-model"], learn.K2))
	assert.NotEmpty(t, reportsOf(got["test-ns|test-model"], learn.Mu))
	assert.Empty(t, r.a.TakeLearnerReports(), "taken once")
	assert.Empty(t, r.a.LearnerReports(), "and none kept")

	r.cycle(oneFull(), decodeVariants("v"))
	assert.NotEmpty(t, r.a.TakeLearnerReports()["test-ns|test-model"], "the next cycle files anew")
}

// lineFields is each line's fields, in order.
func lineFields(entries []observer.LoggedEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ContextMap())
	}
	return out
}

// k2 and mu say restored: true on their lines from the restart until a
// reading is admitted under the key, and keep their sources: a restore is a
// field, not a source (6.1). Reading a window does not clear it: the cycles
// inside the settle read the restored window and write nothing.
func TestLearnerWrite_K2AndMuSayRestoredUntilAReadingIsAdmitted(t *testing.T) {
	before := snapshotRig(t, restartT0, "v")
	settled(t, before, oneFull, decodeVariants("v"))
	r := restartFromSnapshot(t, before, false, "v")

	var k2Lines, muLines, histLines []map[string]any
	rate := 3000.0
	cycle := func() {
		ctx, logs := observedCtx(t)
		rows := oneFull()
		rows[0].GenerationTokenRate = rate
		r.cycleCtx(ctx, rows, decodeVariants("v"))
		k2Lines = append(k2Lines, lineFields(learnerWrites(logs, learn.K2))...)
		muLines = append(muLines, lineFields(learnerWrites(logs, learn.Mu))...)
		for _, f := range lineFields(logs.FilterMessage("k2-decision").All()) {
			if f["priority"] == capacity.K2SrcHistorical.String() {
				histLines = append(histLines, f)
			}
		}
	}
	cycle()
	require.Len(t, k2Lines, 1, "a restored key logs its first write on the first cycle")
	assert.Equal(t, true, k2Lines[0]["restored"])
	assert.Equal(t, true, k2Lines[0]["firstWrite"])
	assert.Equal(t, capacity.K2SrcHistorical.String(), k2Lines[0]["source"], "and keeps its source")
	require.Len(t, muLines, 1)
	assert.Equal(t, true, muLines[0]["restored"])
	assert.Equal(t, domain.ServiceRateSourceMeasured, muLines[0]["source"], "and keeps its source")
	require.NotEmpty(t, histLines, "premise: the settle prices from the restored window")
	assert.Equal(t, true, histLines[0]["restored"], "the line the report's Restored column reads")

	// Past the settle k2 admits its reading. Mu reads the same rate again:
	// an exact repeat is use, not a write (SwitchMuRepeatIsUse), so its
	// window is still the snapshot's a heartbeat later.
	k2Lines, muLines = nil, nil
	for range waitWindow + 2 {
		cycle()
	}
	r.now = r.now.Add(LearnerHeartbeat)
	cycle()
	require.NotEmpty(t, k2Lines)
	require.NotEmpty(t, muLines)
	assert.Equal(t, false, k2Lines[len(k2Lines)-1]["restored"], "k2 admitted a reading")
	assert.Equal(t, true, muLines[len(muLines)-1]["restored"], "mu admitted none: only repeats")

	// A rate that moves is admitted, and the next heartbeat says so.
	rate = 3100
	for range 8 {
		rate += 10
		cycle()
	}
	muLines = nil
	r.now = r.now.Add(LearnerHeartbeat)
	cycle()
	require.NotEmpty(t, muLines, "the heartbeat")
	assert.Equal(t, false, muLines[len(muLines)-1]["restored"], "mu admitted a reading")
}

// A k2 or mu that wrote its own window says restored: false from its first
// line: without a restart there is nothing to say.
func TestLearnerWrite_AFigureLearnedHereIsNotRestored(t *testing.T) {
	r := newRestartRig(t, restartT0)
	ctx, logs := observedCtx(t)
	for range waitWindow + 2 {
		r.cycleCtx(ctx, oneFull(), decodeVariants("v"))
	}
	requireLogged(t, logs, "learner-write")
	for _, learner := range []string{learn.K2, learn.Mu} {
		lines := lineFields(learnerWrites(logs, learner))
		require.NotEmpty(t, lines, learner)
		assert.Equal(t, false, lines[0]["restored"], learner)
	}
}

// The ITL line says restored: true from the restart while its window keeps
// no reading, on a line held from the snapshot, and false from the first
// reading kept.
func TestLearnerWrite_TheITLLineSaysRestoredUntilAReadingIsKept(t *testing.T) {
	const variant = "variant-a"
	states := []domain.VariantReplicaState{{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H200",
		CurrentReplicas: 1, GPUsPerReplica: 1}}
	replica := func(k, itlSec float64) []domain.ReplicaMetrics {
		rm := makeReplicaMetrics("pod-0", variant, int64(k*16000), 16000, 0, 1000, 500)
		rm.KvUsageInstant = k
		rm.AvgITL, rm.AvgITLKnown = itlSec, itlSec > 0
		return []domain.ReplicaMetrics{rm}
	}
	clock := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	process := func() *SaturationAnalyzer {
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", variant, capacity.Record{EngineParams: mtParams(64, "vllm:v1")}, time.Now())
		a := NewSaturationAnalyzer(store)
		a.now = func() time.Time { return clock }
		return a
	}
	cycle := func(a *SaturationAnalyzer, rows []domain.ReplicaMetrics) []map[string]any {
		t.Helper()
		ctx, logs := observedCtx(t)
		_, err := a.Analyze(ctx, makeAnalyzerInput(rows, states))
		require.NoError(t, err)
		return lineFields(learnerWrites(logs, learn.ITL))
	}

	first := process()
	own := make([]map[string]any, 0, itl.DefaultMinSamples+6)
	for range itl.DefaultMinSamples {
		clock = clock.Add(15 * time.Second)
		own = append(own, cycle(first, replica(0.40, 0.03))...)
	}
	for i := range 6 {
		clock = clock.Add(15 * time.Second)
		own = append(own, cycle(first, replica(0.20+0.1*float64(i), 0.01+0.05*(0.20+0.1*float64(i))))...)
	}
	require.NotEmpty(t, own, "premise: the line logged")
	assert.Equal(t, false, own[0]["restored"], "a line this process fitted is not restored")
	loadEnd := clock
	for clock.Sub(loadEnd) < itl.DefaultObservationMaxAge+2*time.Minute {
		clock = clock.Add(time.Minute)
		cycle(first, replica(0.06, 0.01))
	}
	// The window has aged out and the line rests on its slope: as old as the
	// fit, not as old as can be.
	itlReport := func(a *SaturationAnalyzer) learn.Report {
		t.Helper()
		reports := reportsOf(a.LearnerReports()["test-ns|test-model"], learn.ITL)
		require.Len(t, reports, 1)
		return reports[0]
	}
	quiet := itlReport(first)
	require.Zero(t, quiet.Samples, "premise: the window has aged out")
	require.Equal(t, itlSourceHeld, quiet.Source, "premise: a held slope")
	assert.False(t, quiet.LastAdmitted.Before(loadEnd), "the last fit")
	assert.True(t, quiet.LastAdmitted.Before(clock), "the last fit")

	raw, err := json.Marshal(first.ExportLearnedState())
	require.NoError(t, err)
	var doc LearnedState
	require.NoError(t, json.Unmarshal(raw, &doc))

	second := process()
	second.RestoreLearnedState(doc)
	clock = clock.Add(time.Minute)
	lines := cycle(second, replica(0.06, 0.01)) // under the band: nothing kept
	require.Len(t, lines, 1, "a restored line logs its first write")
	assert.Equal(t, true, lines[0]["restored"])
	assert.Equal(t, itlSourceHeld, lines[0]["source"], "and keeps its source")
	assert.True(t, quiet.LastAdmitted.Equal(itlReport(second).LastAdmitted), "a restored slope keeps its fit's age")

	clock = clock.Add(LearnerHeartbeat)
	lines = cycle(second, replica(0.06, 0.01))
	require.Len(t, lines, 1, "the heartbeat")
	assert.Equal(t, true, lines[0]["restored"], "nothing was kept, so it stays restored")

	clock = clock.Add(LearnerHeartbeat)
	lines = cycle(second, replica(0.40, 0.03)) // in the band: a reading kept
	require.Len(t, lines, 1)
	assert.Equal(t, false, lines[0]["restored"])
}

// Only a line with no reading and no slope has no stamp.
func TestITLLearner_AnInterceptWithNoSlopeHasNoStamp(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.noteITLLearner("ns", "m", "v", domain.RoleDecode, "ns|m|v|k", itl.Reading{Tier: itl.TierPinnedB},
		restartT0, logr.Discard())
	require.Len(t, a.itlCycles["ns|m"], 1)
	assert.NotEmpty(t, a.itlCycles["ns|m"][0].PricingKey)
	assert.True(t, a.itlCycles["ns|m"][0].LastAdmitted.IsZero())
}

// muForgetRig is a variant with a mu window and a line logged of it, and the
// switch set; key is the window's.
func muForgetRig(t *testing.T, on bool) (r *restartRig, key string) {
	t.Helper()
	r = newRestartRig(t, restartT0)
	require.NoError(t, r.a.SetSwitchesForReplay(map[string]bool{SwitchMuForgetReported: on}))
	settled(t, r, oneFull, decodeVariants("v"))
	r.cycle(oneFull(), decodeVariants("v"))
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for k := range r.a.saturatedThroughput {
		if !isPrefillBoundKey(k) {
			key = k
		}
	}
	require.NotEmpty(t, key, "premise: a mu window")
	require.Contains(t, r.a.muLogged, key, "premise: learner-write logged it")
	return r, key
}

// Forget reaches mu by name: its keys are reported forgotten by the operator
// and what learner-write last logged of them goes, so the next line is a
// first write with no previous value. The entries dropped are the entries a
// forget dropped before mu was on the list. Off, as before: nothing reported,
// and the logged value survives the forget.
func TestMuLearner_ForgetReportsItsKeysAndClearsWhatWasLogged(t *testing.T) {
	dropped := map[bool]int{}
	for _, on := range []bool{true, false} {
		r, key := muForgetRig(t, on)
		dropped[on] = r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
		r.a.mu.Lock()
		_, window := r.a.saturatedThroughput[key]
		_, logged := r.a.muLogged[key]
		r.a.mu.Unlock()
		assert.False(t, window, "the window goes either way")
		assert.Equal(t, !on, logged, "what was logged of it, on=%v", on)

		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.Mu)
		require.Len(t, reports, 1)
		if on {
			assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, reports[0].Forgotten)
		} else {
			assert.Empty(t, reports[0].Forgotten)
		}
		assert.Empty(t, reports[0].PricingKey, "no window remains")

		r.cycle(unqueuedRow(), decodeVariants("v"))
		assert.Empty(t, reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.Mu)[0].Forgotten, "reported once")

		// The same reading admitted again, the settle past: with the logged
		// value kept it is inside the band of a value from before the forget.
		ctx, logs := observedCtx(t)
		for range 4 {
			r.cycleCtx(ctx, oneFull(), decodeVariants("v"))
		}
		lines := lineFields(learnerWrites(logs, learn.Mu))
		measured := func(f map[string]any) bool { return f["source"] == domain.ServiceRateSourceMeasured }
		var first map[string]any
		for _, f := range lines {
			if measured(f) {
				first = f
				break
			}
		}
		if on {
			require.NotNil(t, first, "lines %v", lines)
			assert.Zero(t, first["previousValue"], "no stale previous value")
		} else if first != nil {
			assert.NotZero(t, first["previousValue"], "negative control: off, the value from before the forget")
		}
	}
	assert.Positive(t, dropped[true])
	assert.Equal(t, dropped[false], dropped[true], "the same entries, counted the same")
}

// Forget leaves the prefill bound's windows out of mu's report: they share
// mu's map and are not mu. They still go with the variant.
func TestMuLearner_ForgetDoesNotReportThePrefillBound(t *testing.T) {
	r, key := muForgetRig(t, true)
	bound := "test-ns|v|test-model|H200|1|" + prefillBoundRole + "|imedium|medium|q5"
	require.True(t, isPrefillBoundKey(bound), "premise")
	r.a.mu.Lock()
	r.a.saturatedThroughput[bound] = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)
	r.a.saturatedThroughput[bound].AddAt(2, r.now)
	r.a.mu.Unlock()
	r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
	r.a.mu.Lock()
	_, ok := r.a.saturatedThroughput[bound]
	r.a.mu.Unlock()
	assert.False(t, ok, "the bound goes with the variant")
	r.cycle(unqueuedRow(), decodeVariants("v"))
	reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.Mu)
	require.Len(t, reports, 1)
	assert.Equal(t, []string{key}, learn.Keys(reports[0].Forgotten))
}

// A forget of mu that has no variant in the cycle still files a report, to
// say so, and a key mu's sweep evicts is reported as evicted. A prefill bound
// the sweep evicts is not.
func TestMuLearner_EvictionIsReported(t *testing.T) {
	for _, on := range []bool{true, false} {
		r, key := muForgetRig(t, on)
		bound := "test-ns|v|test-model|H200|1|" + prefillBoundRole + "|imedium|medium|q5"
		r.a.mu.Lock()
		aged := r.now.Add(-r.a.horizons.BucketRetention - time.Hour)
		r.a.saturatedThroughput[key].ObservedAt(aged)
		r.a.saturatedThroughput[key].TouchAt(aged)
		r.a.saturatedThroughput[bound] = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, aged)
		r.a.saturatedThroughput[bound].AddAt(2, aged)
		r.a.mu.Unlock()
		r.a.EvictStaleHistory()
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.Mu)
		require.Len(t, reports, 1)
		if on {
			assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByEvicted}}, reports[0].Forgotten)
		} else {
			assert.Empty(t, reports[0].Forgotten)
		}
	}
}

// A variant mu did not see this cycle still reports what was forgotten of it.
func TestMuLearner_AForgottenVariantOutsideTheCycleStillReports(t *testing.T) {
	r, key := muForgetRig(t, true)
	r.a.mu.Lock()
	r.a.forgetMuLocked("test-ns", "test-model", "v", learn.ByOperator)
	r.a.mu.Unlock()
	r.a.beginMuCycle("test-ns", "test-model")
	r.a.closeMuCycle(t.Context(), "test-ns", "test-model", nil)
	reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learn.Mu)
	var got []learn.Forgotten
	for _, rep := range reports {
		if rep.Variant == "v" {
			got = append(got, rep.Forgotten...)
		}
	}
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, got)
}

// What mu holds pending goes with a model the sweep lets go: a model that
// will not report again leaves no forgotten keys behind.
func TestMuLearner_TheSweepLetsAGoneModelsForgottenKeysGo(t *testing.T) {
	r, _ := muForgetRig(t, true)
	r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
	r.a.mu.Lock()
	require.NotEmpty(t, r.a.muForgotten, "premise: a key pending")
	delete(r.a.k2Settle, "test-ns|test-model") // what gone reads
	r.a.sweepLearnerMapsLocked()
	assert.Empty(t, r.a.muForgotten)
	r.a.mu.Unlock()
}

// Every learner's Forgotten names the key its report named as PricingKey:
// the engine deletes the variant's series on that match, so a key spelled two
// ways would leave the series up for good.
func TestForgottenKeyIsThePricingKeyPublished(t *testing.T) {
	t.Run("k2 and mu", func(t *testing.T) {
		r, _ := muForgetRig(t, true)
		reports := r.a.LearnerReports()["test-ns|test-model"]
		k2, mu := reportsOf(reports, learn.K2)[0].PricingKey, reportsOf(reports, learn.Mu)[0].PricingKey
		require.NotEmpty(t, k2)
		require.NotEmpty(t, mu)
		r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports = r.a.LearnerReports()["test-ns|test-model"]
		assert.Contains(t, learn.Keys(reportsOf(reports, learn.K2)[0].Forgotten), k2)
		assert.Contains(t, learn.Keys(reportsOf(reports, learn.Mu)[0].Forgotten), mu)
	})
	t.Run("itl", func(t *testing.T) {
		r := newITLLearnerRig()
		var last learn.Report
		for range 5 {
			last = r.cycle(t, t.Context(), itlSettleFleet())
		}
		require.NotEmpty(t, last.PricingKey, "premise: its own line priced it")
		r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
		after := r.cycle(t, t.Context(), itlSettleFleet())
		assert.Contains(t, learn.Keys(after.Forgotten), last.PricingKey)
	})
}

// stashRig is muForgetRig with k2's and mu's windows put aside under other
// arguments, as a rollout puts them, and their keys.
func stashRig(t *testing.T, on bool) (r *restartRig, k2, mu string) {
	t.Helper()
	r, mu = muForgetRig(t, on)
	k2 = firstKey(r.a)
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	r.a.k2Stash[stashKey(k2, "old")] = r.a.computeCapacityHistory[k2]
	delete(r.a.computeCapacityHistory, k2)
	r.a.throughputStash[stashKey(mu, "old")] = stashedThroughput{window: r.a.saturatedThroughput[mu]}
	delete(r.a.saturatedThroughput, mu)
	return r, k2, mu
}

// reportedForgotten is what one cycle's reports of a learner say was forgotten.
func reportedForgotten(t *testing.T, r *restartRig, learner string) []learn.Forgotten {
	t.Helper()
	r.cycle(unqueuedRow(), decodeVariants("v"))
	reports := reportsOf(r.a.LearnerReports()["test-ns|test-model"], learner)
	require.Len(t, reports, 1, learner)
	return reports[0].Forgotten
}

// A key whose only window is in a stash is still the key last published: a
// forget names it, or the variant's series stands for good.
func TestForget_NamesAKeyWhoseWindowIsInAStash(t *testing.T) {
	r, k2, mu := stashRig(t, true)
	r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
	r.a.mu.Lock()
	assert.Empty(t, r.a.k2Stash)
	assert.Empty(t, r.a.throughputStash)
	r.a.mu.Unlock()
	r.cycle(unqueuedRow(), decodeVariants("v"))
	reports := r.a.LearnerReports()["test-ns|test-model"]
	assert.Equal(t, []learn.Forgotten{{Key: k2, By: learn.ByOperator}}, reportsOf(reports, learn.K2)[0].Forgotten)
	assert.Equal(t, []learn.Forgotten{{Key: mu, By: learn.ByOperator}}, reportsOf(reports, learn.Mu)[0].Forgotten)
}

// ageStashes puts every stashed window past the retention.
func ageStashes(r *restartRig) {
	aged := r.now.Add(-r.a.horizons.BucketRetention - time.Hour)
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for _, ra := range r.a.k2Stash {
		ra.TouchAt(aged)
	}
	for _, st := range r.a.throughputStash {
		st.window.TouchAt(aged)
	}
}

// The sweep evicting the last window of a key reports it, by eviction. A key
// with a live window or another stash can still price, and is not reported;
// a prefill bound's stash is not mu's.
func TestStashEviction_NamesAKeyWithNoWindowLeft(t *testing.T) {
	t.Run("the last window", func(t *testing.T) {
		r, k2, mu := stashRig(t, true)
		ageStashes(r)
		r.a.EvictStaleHistory()
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := r.a.LearnerReports()["test-ns|test-model"]
		assert.Equal(t, []learn.Forgotten{{Key: k2, By: learn.ByEvicted}}, reportsOf(reports, learn.K2)[0].Forgotten)
		assert.Equal(t, []learn.Forgotten{{Key: mu, By: learn.ByEvicted}}, reportsOf(reports, learn.Mu)[0].Forgotten)
	})
	t.Run("mu's switch off", func(t *testing.T) {
		r, k2, _ := stashRig(t, false)
		ageStashes(r)
		r.a.EvictStaleHistory()
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := r.a.LearnerReports()["test-ns|test-model"]
		assert.Equal(t, []string{k2}, learn.Keys(reportsOf(reports, learn.K2)[0].Forgotten))
		assert.Empty(t, reportsOf(reports, learn.Mu)[0].Forgotten)
	})
	t.Run("a live window remains", func(t *testing.T) {
		r, key := muForgetRig(t, true)
		k2 := firstKey(r.a)
		r.a.mu.Lock()
		r.a.k2Stash[stashKey(k2, "old")] = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)
		r.a.throughputStash[stashKey(key, "old")] = stashedThroughput{
			window: capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)}
		r.a.mu.Unlock()
		ageStashes(r)
		r.a.EvictStaleHistory()
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := r.a.LearnerReports()["test-ns|test-model"]
		assert.Empty(t, reportsOf(reports, learn.K2)[0].Forgotten)
		assert.Empty(t, reportsOf(reports, learn.Mu)[0].Forgotten)
	})
	t.Run("another stash remains", func(t *testing.T) {
		r, k2, mu := stashRig(t, true)
		ageStashes(r)
		r.a.mu.Lock()
		r.a.k2Stash[stashKey(k2, "newer")] = capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)
		r.a.throughputStash[stashKey(mu, "newer")] = stashedThroughput{
			window: capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)}
		r.a.mu.Unlock()
		r.a.EvictStaleHistory()
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := r.a.LearnerReports()["test-ns|test-model"]
		assert.Empty(t, reportsOf(reports, learn.K2)[0].Forgotten)
		assert.Empty(t, reportsOf(reports, learn.Mu)[0].Forgotten)
	})
	t.Run("the prefill bound", func(t *testing.T) {
		r, _ := muForgetRig(t, true)
		bound := "test-ns|v|test-model|H200|1|" + prefillBoundRole + "|imedium|medium|q5"
		r.a.mu.Lock()
		r.a.throughputStash[stashKey(bound, "old")] = stashedThroughput{
			window: capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)}
		r.a.mu.Unlock()
		ageStashes(r)
		r.a.EvictStaleHistory()
		assert.Empty(t, reportedForgotten(t, r, learn.Mu))
	})
}

// A live window and a stash of one key: the sweep names the key once, when the
// last of them goes, and not while a stash still holds it.
func TestEviction_NamesAKeyOnceAndOnlyWhenNothingHoldsIt(t *testing.T) {
	both := func(t *testing.T, stashAt func(r *restartRig) time.Time) (r *restartRig, k2, mu string) {
		t.Helper()
		r, mu = muForgetRig(t, true)
		k2 = firstKey(r.a)
		aged := r.now.Add(-r.a.horizons.BucketRetention - time.Hour)
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		k2Stash := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)
		muStash := capacity.NewRollingAverage(capacity.RollingAverageWindowSize, r.now)
		k2Stash.TouchAt(stashAt(r))
		muStash.TouchAt(stashAt(r))
		r.a.k2Stash[stashKey(k2, "old")] = k2Stash
		r.a.throughputStash[stashKey(mu, "old")] = stashedThroughput{window: muStash}
		r.a.computeCapacityHistory[k2].TouchAt(aged)
		r.a.saturatedThroughput[mu].TouchAt(aged)
		return r, k2, mu
	}
	t.Run("both stale in one sweep", func(t *testing.T) {
		r, k2, mu := both(t, func(r *restartRig) time.Time { return r.now.Add(-r.a.horizons.BucketRetention - time.Hour) })
		r.a.EvictStaleHistory()
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := r.a.LearnerReports()["test-ns|test-model"]
		assert.Equal(t, []learn.Forgotten{{Key: k2, By: learn.ByEvicted}}, reportsOf(reports, learn.K2)[0].Forgotten)
		assert.Equal(t, []learn.Forgotten{{Key: mu, By: learn.ByEvicted}}, reportsOf(reports, learn.Mu)[0].Forgotten)
	})
	t.Run("a stale window beside a fresher stash", func(t *testing.T) {
		r, _, _ := both(t, func(r *restartRig) time.Time { return r.now })
		r.a.EvictStaleHistory()
		r.cycle(unqueuedRow(), decodeVariants("v"))
		reports := r.a.LearnerReports()["test-ns|test-model"]
		assert.Empty(t, reportsOf(reports, learn.K2)[0].Forgotten, "the stash can still price")
		assert.Empty(t, reportsOf(reports, learn.Mu)[0].Forgotten, "the stash can still price")
	})
}

// A key pending from a forget is not filed again before its report.
func TestFileForgotten_NamesAKeyOnce(t *testing.T) {
	pending := map[string]map[string][]learn.Forgotten{}
	fileForgotten(pending, "m", "v", []string{"k"}, learn.ByOperator)
	fileForgotten(pending, "m", "v", []string{"k", "other"}, learn.ByEvicted)
	assert.Equal(t, []learn.Forgotten{{Key: "k", By: learn.ByOperator}, {Key: "other", By: learn.ByEvicted}}, pending["m"]["v"])
}

// The keys a forget names are window keys, not the stash's, and a key with a
// live window and a stash is named once.
func TestForget_KeysAreWindowKeysNamedOnce(t *testing.T) {
	r, key := muForgetRig(t, true)
	assert.False(t, strings.Contains(key, "#"), "premise: a live window's key")
	k2 := firstKey(r.a)
	r.a.mu.Lock()
	r.a.throughputStash[stashKey(key, "other-args")] = stashedThroughput{window: r.a.saturatedThroughput[key]}
	r.a.k2Stash[stashKey(k2, "other-args")] = r.a.computeCapacityHistory[k2]
	r.a.mu.Unlock()
	r.a.ForgetVariant("test-ns", "test-model", "v", "token-1")
	r.a.mu.Lock()
	assert.Empty(t, r.a.throughputStash, "the stash goes with mu")
	got := r.a.muForgotten["test-ns|test-model"]["v"]
	gotK2 := r.a.k2Forgotten["test-ns|test-model"]["v"]
	r.a.mu.Unlock()
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, got)
	assert.Equal(t, []learn.Forgotten{{Key: k2, By: learn.ByOperator}}, gotK2)
}
