package metrics

import (
	"fmt"
	"maps"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

var (
	replicasSuggested     *prometheus.GaugeVec
	replicasProvided      *prometheus.GaugeVec
	replicasShort         *prometheus.GaugeVec
	replicasBorrowed      *prometheus.GaugeVec
	quotaAfterSuggested   *prometheus.GaugeVec
	replicaGapPublished   = map[*prometheus.GaugeVec]map[string]prometheus.Labels{}
	replicaGapPublishedMu sync.Mutex
)

// registerReplicaGapMetrics creates and registers the suggested/provided
// series. Called from InitMetrics, after controllerInstance is set.
func registerReplicaGapMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	variantLabels := []string{constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName, constants.LabelRole,
		constants.LabelMode}
	gauge := func(name, help string, labels []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, withInstance(labels...))
	}
	replicasSuggested = gauge(constants.LLMSMReplicasSuggested,
		"Replicas the analyzers ask for (X), before the limiters, the variant's maxReplicas ceiling and "+
			"the utilization share. learner is empty for the analyzers' own ask; ttft-mixture is what they "+
			"would ask for a passive or shadow model's prefill variant with the slo analyzer sizing it on the "+
			"v2 TTFT model (would-be only).", append(append([]string{}, variantLabels...), constants.LabelLearner))
	replicasProvided = gauge(constants.LLMSMReplicasProvided,
		"Replicas the variant gets (Y), after the limiters and the utilization share. With mode active, the "+
			"value of llmsm_desired_replicas; with mode passive or shadow nothing is applied and it is what "+
			"the controller would provide.", variantLabels)
	replicasShort = gauge(constants.LLMSMReplicasShort,
		"Replicas suggested and not provided, by reason (quota, gpu, node-fit, max-replicas, share-lent). "+
			"limiter_name names the quota entry for reason quota. Present only while positive.",
		append(append([]string{}, variantLabels...), constants.LabelReason, constants.LabelLimiterName))
	replicasBorrowed = gauge(constants.LLMSMReplicasBorrowed,
		"Replicas provided above the suggestion because the utilization share gave the variant another "+
			"model's GPUs (Y - X), 0 otherwise.", variantLabels)
	quotaAfterSuggested = gauge(constants.LLMSMQuotaRemainingAfterSuggestedGPUs,
		"GPUs a quota pool would have left with every variant it bounds at its suggestion: negative when "+
			"the suggestions do not fit.",
		[]string{constants.LabelLimiterName, constants.LabelScope, constants.LabelAccelerator})
	for name, c := range map[string]prometheus.Collector{
		constants.LLMSMReplicasSuggested:                replicasSuggested,
		constants.LLMSMReplicasProvided:                 replicasProvided,
		constants.LLMSMReplicasShort:                    replicasShort,
		constants.LLMSMReplicasBorrowed:                 replicasBorrowed,
		constants.LLMSMQuotaRemainingAfterSuggestedGPUs: quotaAfterSuggested,
	} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register %s: %w", name, err)
		}
	}
	replicaGapPublishedMu.Lock()
	replicaGapPublished = map[*prometheus.GaugeVec]map[string]prometheus.Labels{}
	replicaGapPublishedMu.Unlock()
	return nil
}

// ReplicaShort is the part of a variant's shortfall one reason accounts for.
type ReplicaShort struct {
	// Reason is a constants.ReplicasShortReason* value; LimiterName names the
	// quota entry for constants.ReplicasShortReasonQuota, "" otherwise.
	Reason, LimiterName string
	Replicas            int
}

// ReplicaGap is one variant's suggestion against what it is provided.
type ReplicaGap struct {
	Namespace, ModelName, VariantName, Role string
	// Mode is a constants.ReplicasMode* value: whether Provided is applied.
	Mode string
	// Learner is empty for the analyzers' own suggestion; a v2 learner's
	// name (learn.TTFTMixture) for what the analyzers would suggest with
	// the slo analyzer sizing on it, where only Suggested is published.
	Learner string
	// Suggested is X, Provided is Y. Suggested - Provided is the sum of Short
	// less Borrowed.
	Suggested, Provided int
	Short               []ReplicaShort
	Borrowed            int
}

// QuotaAfterSuggested is what one quota pool would have left with every
// variant it bounds at its suggestion.
type QuotaAfterSuggested struct {
	// LimiterName is the quota entry; Scope a namespace, or
	// constants.UtilizationShareClusterScope.
	LimiterName, Scope, Accelerator string
	GPUs                            int
}

// PublishReplicaGaps makes the suggested/provided series exactly this cycle's:
// it sets every current series, then deletes those a variant, reason or quota
// pool that went had left. Called with nothing, it clears them all.
func PublishReplicaGaps(gaps []ReplicaGap, quotas []QuotaAfterSuggested) {
	if replicasSuggested == nil {
		return
	}
	replicaGapPublishedMu.Lock()
	defer replicaGapPublishedMu.Unlock()
	current := map[*prometheus.GaugeVec]map[string]prometheus.Labels{}
	set := func(g *prometheus.GaugeVec, l prometheus.Labels, v float64) {
		if controllerInstance != "" {
			l[constants.LabelControllerInstance] = controllerInstance
		}
		g.With(l).Set(v)
		if current[g] == nil {
			current[g] = map[string]prometheus.Labels{}
		}
		current[g][seriesKey(l)] = l
	}
	for _, gap := range gaps {
		vl := prometheus.Labels{
			constants.LabelNamespace:   gap.Namespace,
			constants.LabelModelName:   gap.ModelName,
			constants.LabelVariantName: gap.VariantName,
			constants.LabelRole:        gap.Role,
			constants.LabelMode:        gap.Mode,
		}
		sl := maps.Clone(vl)
		sl[constants.LabelLearner] = gap.Learner
		set(replicasSuggested, sl, float64(gap.Suggested))
		if gap.Learner != "" {
			continue
		}
		set(replicasProvided, maps.Clone(vl), float64(gap.Provided))
		set(replicasBorrowed, maps.Clone(vl), float64(gap.Borrowed))
		for _, s := range gap.Short {
			if s.Replicas <= 0 {
				continue
			}
			sl := maps.Clone(vl)
			sl[constants.LabelReason] = s.Reason
			sl[constants.LabelLimiterName] = s.LimiterName
			set(replicasShort, sl, float64(s.Replicas))
		}
	}
	for _, q := range quotas {
		set(quotaAfterSuggested, prometheus.Labels{
			constants.LabelLimiterName: q.LimiterName,
			constants.LabelScope:       q.Scope,
			constants.LabelAccelerator: q.Accelerator,
		}, float64(q.GPUs))
	}
	for g, prev := range replicaGapPublished {
		for k, l := range prev {
			if _, ok := current[g][k]; !ok {
				g.Delete(l)
			}
		}
	}
	replicaGapPublished = current
}
