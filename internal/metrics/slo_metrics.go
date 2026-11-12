package metrics

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var (
	sloTarget       *prometheus.GaugeVec
	sloObserved     *prometheus.GaugeVec
	sloAttainment   *prometheus.GaugeVec
	sloObjective    *prometheus.GaugeVec
	sloFill         *prometheus.GaugeVec
	sloPredictedITL *prometheus.GaugeVec
	sloSource       *prometheus.GaugeVec
	sloRequests     *prometheus.GaugeVec
	sloExcluded     *prometheus.GaugeVec

	sloPredictedTTFT   *prometheus.GaugeVec
	sloTTFTUnmet       *prometheus.GaugeVec
	sloTTFTReplicaRate *prometheus.GaugeVec
	sloTTFTVPSource    *prometheus.GaugeVec
	sloTTFTReason      *prometheus.GaugeVec
	sloTTFTRefused     *prometheus.GaugeVec
	sloTTFTFlag        *prometheus.GaugeVec

	sloTTFTJointInput  *prometheus.GaugeVec
	sloTTFTJointMean   *prometheus.GaugeVec
	sloTTFTJointReason *prometheus.GaugeVec
)

// registerSLOMetrics creates and registers the SLO analyzer's gauges. Called
// from InitMetrics, after controllerInstance is set.
func registerSLOMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	objectiveLabels := withInstance(constants.LabelNamespace, constants.LabelModelName,
		constants.LabelObjective, constants.LabelStatistic)
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	}
	sloTarget = gauge(constants.LLMSMSLOTargetSeconds,
		"SLO analyzer: the latency target a model's scaling policy declares, in seconds.",
		objectiveLabels)
	sloObserved = gauge(constants.LLMSMSLOObservedSeconds,
		"SLO analyzer: the latency the router observed over its window, in seconds -- the mean for "+
			"TPOT, the configured percentile for TTFT. Absent when too few requests were seen to judge.",
		objectiveLabels)
	sloAttainment = gauge(constants.LLMSMSLOAttainmentRatio,
		"SLO analyzer: the share of completed streaming requests whose TTFT met the target, as the "+
			"router measured it (including its queue). Absent below the sample minimum.",
		objectiveLabels)
	sloObjective = gauge(constants.LLMSMSLOObjectiveRatio,
		"SLO analyzer: the share of requests a TTFT objective promises within its target, 0.90 for p90. "+
			"TTFT only.",
		objectiveLabels)
	sloRequests = gauge(constants.LLMSMSLOWindowRequests,
		"SLO analyzer: requests the router recorded for the objective over its window. Present "+
			"whenever the router reported anything for the model.",
		objectiveLabels)
	sloFill = gauge(constants.LLMSMSLOFill,
		"SLO analyzer: the KV fill, as a fraction of physical KV, at which a variant's mean "+
			"inter-token latency meets the TPOT target. Absent when the variant could not be priced.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName,
			constants.LabelObjective))
	sloPredictedITL = gauge(constants.LLMSMSLOPredictedITLSeconds,
		"SLO analyzer: the mean inter-token latency a variant's line predicts at the fill the fleet "+
			"would run at after the scale-up headroom, in seconds.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName))
	sloSource = gauge(constants.LLMSMSLOSource,
		"SLO analyzer: 1 for the source a variant's SLO capacity came from, 0 for every other.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName,
			constants.LabelSource))
	// Fresh registry, fresh gauges: forget the series a previous registry held.
	sloPublishedMu.Lock()
	sloPublished = map[string]*sloModelSeries{}
	sloPublishedMu.Unlock()
	sloExcluded = gauge(constants.LLMSMSLOExcluded,
		"SLO analyzer: per objective, 1 for the reason its router window is not judged this cycle (a wake "+
			"from zero, a replica starting), 0 for the others; all 0 while it is judged.",
		withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelObjective, constants.LabelReason))
	ttftLabels := func(more ...string) []string {
		return withInstance(append([]string{constants.LabelNamespace, constants.LabelModelName,
			constants.LabelVariantName, constants.LabelStatistic}, more...)...)
	}
	sloPredictedTTFT = gauge(constants.LLMSMSLOPredictedTTFTSeconds,
		"SLO analyzer, shadow only: the TTFT predicted for a prefill variant at its current rate, in "+
			"seconds, at the objective's percentile. "+constants.SLOTTFTPredictionCaveat+" Absent when the "+
			"model refuses the rate or llmsm_slo_ttft_reason is a refusal.",
		ttftLabels())
	sloTTFTUnmet = gauge(constants.LLMSMSLOTTFTUnmet,
		"SLO analyzer, shadow only: 1 when a prefill variant's current rate misses the TTFT target, "+
			"including a rate at or over the prefill knee or the decode onset, and a target below the floor, "+
			"0 when met. Absent when not priced.",
		ttftLabels())
	sloTTFTReplicaRate = gauge(constants.LLMSMSLOTTFTReplicaRequestsPerSecond,
		"SLO analyzer, shadow only: the requests per second per prefill replica at which the bounded "+
			"TTFT meets the target, at the current replica count. Absent unless llmsm_slo_ttft_reason is priced.",
		ttftLabels())
	sloTTFTVPSource = gauge(constants.LLMSMSLOTTFTVPSource,
		"SLO analyzer: 1 for where a prefill variant's capacity V_P came from, 0 for the others.",
		ttftLabels(constants.LabelVPSource))
	sloTTFTReason = gauge(constants.LLMSMSLOTTFTReason,
		"SLO analyzer: 1 for the reason of a prefill variant's TTFT pricing, 0 for the others.",
		ttftLabels(constants.LabelReason))
	sloTTFTRefused = gauge(constants.LLMSMSLOTTFTRefused,
		"SLO analyzer: 1 for why the model refused a prefill variant's current rate as past what the fleet "+
			"serves, 0 for the other. The reason series stays the answer at the target.",
		ttftLabels(constants.LabelReason))
	sloTTFTFlag = gauge(constants.LLMSMSLOTTFTFlag,
		"SLO analyzer: 1 for each flag on a prefill variant's TTFT answer, 0 for the others.",
		ttftLabels(constants.LabelFlag))
	sloTTFTJointInput = gauge(constants.LLMSMSLOTTFTJointInput,
		"SLO analyzer, shadow only: one input of the joint prefill and decode TTFT model, learned from the "+
			"engines' histograms. Absent while the input is not learned. Nothing acts on it.",
		ttftLabels(constants.LabelJointInput))
	sloTTFTJointMean = gauge(constants.LLMSMSLOTTFTJointMeanSeconds,
		"SLO analyzer, shadow only: the joint prefill and decode model's mean TTFT at the running fleet and "+
			"rate, in seconds. Absent unless llmsm_slo_ttft_joint_reason is priced.",
		ttftLabels())
	sloTTFTJointReason = gauge(constants.LLMSMSLOTTFTJointReason,
		"SLO analyzer, shadow only: 1 for why the joint TTFT model has no mean, or priced, 0 for the others.",
		ttftLabels(constants.LabelReason))
	for _, g := range []*prometheus.GaugeVec{sloTarget, sloObserved, sloAttainment, sloObjective, sloFill,
		sloPredictedITL, sloSource, sloRequests, sloExcluded, sloPredictedTTFT, sloTTFTUnmet, sloTTFTReplicaRate,
		sloTTFTVPSource, sloTTFTReason, sloTTFTRefused, sloTTFTFlag, sloTTFTJointInput, sloTTFTJointMean,
		sloTTFTJointReason} {
		if err := registry.Register(g); err != nil {
			return err
		}
	}
	return nil
}

// SLOObjectiveReading is one objective of one model, as PublishSLO takes it.
// A NaN Observed, Attainment or Requests is not published: absence is the
// honest value for "not enough requests to judge" (Observed, Attainment) and
// for "the router reported nothing" (Requests). An infinite Observed -- a
// percentile in the router's +Inf bucket -- is published as +Inf.
type SLOObjectiveReading struct {
	Objective     string
	Statistic     string
	TargetSeconds float64
	Observed      float64
	Attainment    float64
	// ObjectiveRatio is the share of requests the objective promises within
	// its target, 0.90 for p90; published only when above 0, so a TPOT mean,
	// which promises no share, leaves it out.
	ObjectiveRatio float64
	Requests       float64
	// WindowSeconds is the router window the figures were taken over; zero
	// when the router reported nothing.
	WindowSeconds float64
	// Excluded is why the objective's window is not judged this cycle, one of
	// constants.SLOExcludedReasons, or "" when it is.
	Excluded string
}

// SLOVariantReading is one generating variant of one model, as PublishSLO
// takes it. A NaN field is not published, and an empty Source publishes no
// source series.
type SLOVariantReading struct {
	Variant      string
	Source       string
	Fill         float64
	PredictedITL float64
}

// SLOTTFTReading is one prefill variant's TTFT pricing, as PublishSLO takes it.
// A shadow figure: nothing reads it to size a fleet. A NaN Predicted or
// PerReplica is not published. llmsm_slo_ttft_unmet reads 1 with Unmet, 0 with
// Predicted present and Unmet false, and is absent otherwise: no prediction and
// no miss is not priced. Unmet with no prediction is a rate the model refused
// (Refused), or a target below the floor (Reason).
type SLOTTFTReading struct {
	Variant, Statistic    string
	Predicted, PerReplica float64
	Unmet                 bool
	VPSource, Reason      string
	// Refused is why the model refused the current rate as past what the
	// fleet serves, one of constants.SLOTTFTRefusedReasons, or "". Reason is the
	// answer at the target, so this is what says why a prediction is absent
	// while Unmet is set.
	Refused string `learnrec:"omitzero"`
	// Flags is the flags on the answer, in constants.SLOTTFTFlags order.
	Flags []string
	// Joint is the joint prefill and decode model in shadow; nil with the
	// slo-ttft-joint-inputs replay switch off, or not one variant per role.
	Joint *SLOTTFTJointReading `learnrec:"omitzero"`
}

// SLOTTFTJointReading is the joint prefill and decode TTFT model of one
// prefill variant, in shadow: its inputs, and its mean at the running fleet.
// A NaN figure is not learned and not published.
type SLOTTFTJointReading struct {
	// Inputs is each input by its constants.SLOTTFTJointInputs name.
	Inputs map[string]float64
	// MeanSeconds is the mean TTFT at the running fleet and rate, NaN
	// unless Reason is constants.SLOTTFTJointReasonPriced.
	MeanSeconds float64
	// Reason is one of constants.SLOTTFTJointReasons.
	Reason string
	// Flags is the flags on the mean, in constants.SLOTTFTFlags order.
	Flags []string
}

// SLOModelSnapshot is everything the SLO analyzer publishes for one model in
// one cycle.
type SLOModelSnapshot struct {
	Namespace  string
	ModelName  string
	Objectives []SLOObjectiveReading
	Variants   []SLOVariantReading
	// TTFT is each prefill variant's TTFT pricing; nil with no TTFT target,
	// no prefill variant, or the slo-ttft-price replay switch off.
	TTFT []SLOTTFTReading `learnrec:"omitzero"`
}

// sloStaleAfter is how long a model's SLO series outlive the last cycle that
// published them. The analyzer is only called for models whose policy lists
// it, so a model that left -- or whose policy stopped listing the analyzer --
// is noticed only by its silence, which SweepSLO reads every cycle.
const sloStaleAfter = 10 * time.Minute

type sloModelSeries struct {
	series   map[*prometheus.GaugeVec]map[string]prometheus.Labels
	lastSeen time.Time
}

var (
	sloPublished   = map[string]*sloModelSeries{}
	sloPublishedMu sync.Mutex
)

// PublishSLO makes one model's SLO series exactly this snapshot: it sets every
// current series first and only then deletes those the model no longer has, so
// a scrape never lands on a gap.
func PublishSLO(snap SLOModelSnapshot, now time.Time) {
	if sloTarget == nil {
		return
	}
	sloPublishedMu.Lock()
	defer sloPublishedMu.Unlock()

	current := map[*prometheus.GaugeVec]map[string]prometheus.Labels{}
	set := func(g *prometheus.GaugeVec, l prometheus.Labels, v float64) {
		if math.IsNaN(v) {
			return
		}
		g.With(l).Set(v)
		if current[g] == nil {
			current[g] = map[string]prometheus.Labels{}
		}
		current[g][seriesKey(l)] = l
	}
	labels := func(pairs ...string) prometheus.Labels {
		l := prometheus.Labels{
			constants.LabelNamespace: snap.Namespace,
			constants.LabelModelName: snap.ModelName,
		}
		for i := 0; i+1 < len(pairs); i += 2 {
			l[pairs[i]] = pairs[i+1]
		}
		if controllerInstance != "" {
			l[constants.LabelControllerInstance] = controllerInstance
		}
		return l
	}

	for _, o := range snap.Objectives {
		ol := labels(constants.LabelObjective, o.Objective, constants.LabelStatistic, o.Statistic)
		set(sloTarget, ol, o.TargetSeconds)
		set(sloObserved, ol, o.Observed)
		set(sloAttainment, ol, o.Attainment)
		if o.ObjectiveRatio > 0 {
			set(sloObjective, ol, o.ObjectiveRatio)
		}
		set(sloRequests, ol, o.Requests)
		for _, reason := range constants.SLOExcludedReasons {
			on := 0.0
			if reason == o.Excluded {
				on = 1
			}
			set(sloExcluded, labels(constants.LabelObjective, o.Objective, constants.LabelReason, reason), on)
		}
	}
	for _, v := range snap.Variants {
		set(sloFill, labels(constants.LabelVariantName, v.Variant,
			constants.LabelObjective, constants.SLOObjectiveTPOT), v.Fill)
		set(sloPredictedITL, labels(constants.LabelVariantName, v.Variant), v.PredictedITL)
		if v.Source == "" {
			continue
		}
		for _, s := range constants.SLOSources {
			on := 0.0
			if s == v.Source {
				on = 1
			}
			set(sloSource, labels(constants.LabelVariantName, v.Variant, constants.LabelSource, s), on)
		}
	}

	for _, t := range snap.TTFT {
		tl := func(pairs ...string) prometheus.Labels {
			return labels(append([]string{constants.LabelVariantName, t.Variant, constants.LabelStatistic, t.Statistic}, pairs...)...)
		}
		set(sloPredictedTTFT, tl(), t.Predicted)
		set(sloTTFTReplicaRate, tl(), t.PerReplica)
		switch {
		case t.Unmet:
			set(sloTTFTUnmet, tl(), 1)
		case !math.IsNaN(t.Predicted):
			set(sloTTFTUnmet, tl(), 0)
		}
		oneHot := func(g *prometheus.GaugeVec, label string, values []string, on func(string) bool) {
			for _, v := range values {
				x := 0.0
				if on(v) {
					x = 1
				}
				set(g, tl(label, v), x)
			}
		}
		oneHot(sloTTFTVPSource, constants.LabelVPSource, constants.SLOTTFTVPSources, func(v string) bool { return v == t.VPSource })
		oneHot(sloTTFTReason, constants.LabelReason, constants.SLOTTFTReasons, func(v string) bool { return v == t.Reason })
		oneHot(sloTTFTRefused, constants.LabelReason, constants.SLOTTFTRefusedReasons, func(v string) bool { return v == t.Refused })
		oneHot(sloTTFTFlag, constants.LabelFlag, constants.SLOTTFTFlags, func(v string) bool {
			return slices.Contains(t.Flags, v) || t.Joint != nil && slices.Contains(t.Joint.Flags, v)
		})
		if j := t.Joint; j != nil {
			for _, name := range constants.SLOTTFTJointInputs {
				if v, ok := j.Inputs[name]; ok {
					set(sloTTFTJointInput, tl(constants.LabelJointInput, name), v)
				}
			}
			set(sloTTFTJointMean, tl(), j.MeanSeconds)
			oneHot(sloTTFTJointReason, constants.LabelReason, constants.SLOTTFTJointReasons, func(v string) bool { return v == j.Reason })
		}
	}

	key := snap.Namespace + "/" + snap.ModelName
	if prev := sloPublished[key]; prev != nil {
		deleteGone(prev.series, current)
	}
	sloPublished[key] = &sloModelSeries{series: current, lastSeen: now}
}

// SweepSLO deletes every series of a model not published for sloStaleAfter.
// The engine calls it every cycle, whether or not any model runs the analyzer:
// a sweep that ran only inside PublishSLO never ran once the last model
// stopped publishing, and that model's series stayed for ever.
func SweepSLO(now time.Time) {
	if sloTarget == nil {
		return
	}
	sloPublishedMu.Lock()
	defer sloPublishedMu.Unlock()
	for k, m := range sloPublished {
		if now.Sub(m.lastSeen) > sloStaleAfter {
			deleteGone(m.series, nil)
			delete(sloPublished, k)
		}
	}
}

// DeleteSLOModel deletes every SLO series of one model at once. The engine
// calls it when the model departs, as it deletes the model's other analyzer
// series; SweepSLO's timeout is then only the backstop for a model that stays
// but stops publishing.
func DeleteSLOModel(namespace, modelName string) {
	if sloTarget == nil {
		return
	}
	sloPublishedMu.Lock()
	defer sloPublishedMu.Unlock()
	key := namespace + "/" + modelName
	if m := sloPublished[key]; m != nil {
		deleteGone(m.series, nil)
		delete(sloPublished, key)
	}
}

// deleteGone deletes every series in prev that current does not hold.
func deleteGone(prev, current map[*prometheus.GaugeVec]map[string]prometheus.Labels) {
	for g, series := range prev {
		for k, l := range series {
			if _, ok := current[g][k]; !ok {
				g.Delete(l)
			}
		}
	}
}
