package slo

import (
	"maps"
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The predicted latency of a model the v2 models run for, published by
// Analyze beside the default model's, under the learner label
// (llmsm_slo_predicted_ttft_seconds, llmsm_slo_predicted_tpot_seconds):
//
//   - TTFT, default (learner slo): the analyzer's own TTFT model at the
//     current rate, its objective's percentile as before and the mean, p90
//     and p95 beside it (defaultTTFTStatistics).
//   - TTFT, v2 (learner ttft-mixture): the rank mixture at the current rate
//     and serving prefill pods (priceModelLocked).
//   - TPOT, default (learner slo): the mean is the analyzer's ITL line at the
//     decode variant's current fill, the p90 and p95 that mean times R_q, the
//     capacity planner's default ratio read from the window's router TPOT
//     histograms (whatif.TPOTRatioFromWindows).
//   - TPOT, v2 (learner tpot-distribution): p90 and p95, the distribution's
//     ratio times the same mean. The distribution is placed around the
//     default's mean, so its mean is the default's and is not published
//     twice.
//
// Only a model observed on the analyzer's own cycle is published, so a
// replay, which never observes, publishes what it did before.

// modelObservedLocked is the model's v2 learners where they were observed on
// this cycle, nil otherwise. Callers hold a.mu.
func (a *Analyzer) modelObservedLocked(namespace, modelID string) *modelWindow {
	w := a.models[reportModel(namespace, modelID)]
	if w == nil || w.observed != a.cycle {
		return nil
	}
	return w
}

// modelObserved says the model's v2 learners were observed on this cycle.
func (a *Analyzer) modelObserved(namespace, modelID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.modelObservedLocked(namespace, modelID) != nil
}

// defaultTTFTStatistics is the default TTFT model's mean, p90 and p95 at
// rate, for a variant whose objective's percentile it priced (in, as
// gateOrPrice left it): each statistic it refuses is left out.
func defaultTTFTStatistics(in ttftInputs, rate float64) map[string]float64 {
	fl, _ := ttftFleet(in, in.pods)
	out := map[string]float64{}
	for i, q := range []float64{0, 0.9, 0.95} {
		if t, ok := whatif.TTFTAt(fl, rate, q).(whatif.TTFT); ok && whatif.Finite(t.Seconds) {
			out[constants.SLOPredictedStatistics[i]] = t.Seconds
		}
	}
	return out
}

// currentFill is the variant's serving rows' mean instantaneous KV fill, the
// k its ITL line is read at; false with none read.
func currentFill(rows []domain.ReplicaMetrics, variant string) (float64, bool) {
	var sum float64
	n := 0
	for i := range rows {
		rm := &rows[i]
		if rm.VariantName == variant && prefill.Serving(*rm) && rm.KvUsageInstantKnown && whatif.Finite(rm.KvUsageInstant) {
			sum += rm.KvUsageInstant
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// modelPredictions is the TTFT v2 and the TPOT figures of a model observed
// this cycle (the default TTFT's are on its TTFT readings): lines is each
// generating variant's ITL line, as priced this cycle. Nil for a model not
// observed.
func (a *Analyzer) modelPredictions(input domain.AnalyzerInput, lines map[string]itl.Model) []metrics.SLOPrediction {
	a.mu.Lock()
	w := a.modelObservedLocked(input.Namespace, input.ModelID)
	if w == nil {
		a.mu.Unlock()
		return nil
	}
	prefillVariant, decodeVariant := w.prefill, w.decode
	ttft := w.now.ttft
	var v2Ratio [2]float64
	if !w.tpot.fittedAt.IsZero() {
		v2Ratio = [2]float64{w.tpot.fit.Ratio(0.9), w.tpot.fit.Ratio(0.95)}
	}
	defaultRatio := w.tpot.defaultRatio
	a.mu.Unlock()

	var out []metrics.SLOPrediction
	// The mean, p90, p95 and the objective's percentile where it is another
	// (priceModel), in a fixed order.
	for _, s := range slices.Sorted(maps.Keys(ttft)) {
		if v := ttft[s]; whatif.Finite(v) {
			out = append(out, metrics.SLOPrediction{Objective: constants.SLOObjectiveTTFT, Variant: prefillVariant, Statistic: s,
				Learner: learn.TTFTMixture, Seconds: v})
		}
	}
	for _, vs := range input.VariantStates {
		line, ok := lines[vs.VariantName]
		if !ok {
			continue
		}
		k, kok := currentFill(input.ReplicaMetrics, vs.VariantName)
		if !kok {
			continue
		}
		mean := line.ITLAt(k)
		if !(mean > 0) || !whatif.Finite(mean) {
			continue
		}
		tpot := func(statistic, learner string, v float64) {
			if v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) {
				out = append(out, metrics.SLOPrediction{Objective: constants.SLOObjectiveTPOT, Variant: vs.VariantName,
					Statistic: statistic, Learner: learner, Seconds: v})
			}
		}
		tpot(constants.SLOStatisticMean, learn.SLO, mean)
		tpot(constants.SLOStatisticP90, learn.SLO, mean*defaultRatio[0])
		tpot(constants.SLOStatisticP95, learn.SLO, mean*defaultRatio[1])
		if vs.VariantName == decodeVariant {
			tpot(constants.SLOStatisticP90, learn.TPOTDistribution, mean*v2Ratio[0])
			tpot(constants.SLOStatisticP95, learn.TPOTDistribution, mean*v2Ratio[1])
		}
	}
	return out
}

// modelReports is the v2 learners' reports of a model observed this cycle,
// nil otherwise.
func (a *Analyzer) modelReports(namespace, modelID string) []learn.Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.modelObservedLocked(namespace, modelID)
	if w == nil || !a.itlFilesReports {
		return nil
	}
	return a.modelReportsLocked(namespace, modelID, w)
}
