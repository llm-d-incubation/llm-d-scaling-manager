package metrics

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

var (
	decodePackingBound    *prometheus.GaugeVec
	decodePackingK2Tokens *prometheus.GaugeVec

	// decodePackingPublished is the llmsm_decode_packing_bound label set last
	// published per namespace|model|variant, so a changed bound or reason
	// deletes the series it replaces and a variant has at most one.
	decodePackingMu        sync.Mutex
	decodePackingPublished = map[string]prometheus.Labels{}
)

// registerDecodePackingMetrics creates and registers the decode packing
// gauges. Called from InitMetrics, after controllerInstance is set.
func registerDecodePackingMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	decodePackingBound = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMDecodePackingBound,
		Help: "Always 1. Whether the TPOT target (slo.tpot) bounded how far a decode variant is packed this " +
			"cycle: bound=itl when it lowered k2, bound=none when it did not, with reason. Published only for a " +
			"decode variant whose policy sets slo.tpot and lists the slo analyzer.",
	}, withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName,
		constants.LabelBound, constants.LabelReason))
	decodePackingK2Tokens = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMDecodePackingK2Tokens,
		Help: "A decode variant's k2 in KV tokens, before the TPOT bound (k2=unbounded) and after it " +
			"(k2=bounded). Same absence as llmsm_decode_packing_bound.",
	}, withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelVariantName,
		constants.LabelK2))
	for name, c := range map[string]prometheus.Collector{
		constants.LLMSMDecodePackingBound:    decodePackingBound,
		constants.LLMSMDecodePackingK2Tokens: decodePackingK2Tokens,
	} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register %s: %w", name, err)
		}
	}
	decodePackingMu.Lock()
	decodePackingPublished = map[string]prometheus.Labels{}
	decodePackingMu.Unlock()
	return nil
}

// decodePackingLabels is one variant's label set without the bound, reason
// or k2 label.
func decodePackingLabels(namespace, modelID, variantName string) prometheus.Labels {
	labels := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelVariantName: variantName,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	return labels
}

// RecordDecodePacking publishes what the TPOT target made of a decode
// variant's k2. A bound or reason that changed is set under its new labels
// before the old series is deleted, so no scrape finds the variant absent.
// Nil-guarded so tests that never call InitMetrics are a no-op.
func (m *MetricsEmitter) RecordDecodePacking(namespace, modelID, variantName string, p domain.DecodePacking) {
	if decodePackingBound == nil {
		return
	}
	labels := decodePackingLabels(namespace, modelID, variantName)
	labels[constants.LabelBound] = p.Bound
	labels[constants.LabelReason] = p.Reason
	decodePackingBound.With(labels).Set(1)
	for k2, v := range map[string]int64{constants.K2Unbounded: p.K2Unbounded, constants.K2Bounded: p.K2} {
		l := decodePackingLabels(namespace, modelID, variantName)
		l[constants.LabelK2] = k2
		decodePackingK2Tokens.With(l).Set(float64(v))
	}

	key := serviceRateKey(namespace, modelID, variantName)
	decodePackingMu.Lock()
	defer decodePackingMu.Unlock()
	if prev, ok := decodePackingPublished[key]; ok &&
		(prev[constants.LabelBound] != p.Bound || prev[constants.LabelReason] != p.Reason) {
		decodePackingBound.Delete(prev)
	}
	decodePackingPublished[key] = labels
}

// DeleteDecodePacking removes one variant's decode packing series. modelID is
// part of the match: a variant name is unique within a model, not within a
// namespace.
func (m *MetricsEmitter) DeleteDecodePacking(namespace, modelID, variantName string) {
	if decodePackingBound == nil {
		return
	}
	match := prometheus.Labels{
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelVariantName: variantName,
	}
	decodePackingBound.DeletePartialMatch(match)
	decodePackingK2Tokens.DeletePartialMatch(match)
	decodePackingMu.Lock()
	delete(decodePackingPublished, serviceRateKey(namespace, modelID, variantName))
	decodePackingMu.Unlock()
}

// DeleteDecodePackingForModel removes every decode packing series of a model
// instance. Used when a model stops being reconciled.
func (m *MetricsEmitter) DeleteDecodePackingForModel(namespace, modelID string) {
	if decodePackingBound == nil {
		return
	}
	match := prometheus.Labels{
		constants.LabelNamespace: namespace,
		constants.LabelModelName: modelID,
	}
	decodePackingBound.DeletePartialMatch(match)
	decodePackingK2Tokens.DeletePartialMatch(match)
	prefix := namespace + "\x00" + modelID + "\x00"
	decodePackingMu.Lock()
	for key := range decodePackingPublished {
		if strings.HasPrefix(key, prefix) {
			delete(decodePackingPublished, key)
		}
	}
	decodePackingMu.Unlock()
}
