package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// A cycle's fill is the mean of its serving decode rows' ITL fill: the
// minute's mean where it was read, else the instant; prefill rows, rows not
// Ready, bridges and rows with no fill read are left out.
func TestDecodeFill(t *testing.T) {
	roles := map[string]string{"d": "decode", "p": "prefill"}
	row := func(variant string, mean, instant float64, meanOK, instantOK bool) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{VariantName: variant, Ready: true, KvUsageMean: mean, KvUsageMeanKnown: meanOK,
			KvUsageInstant: instant, KvUsageInstantKnown: instantOK}
	}
	notReady := row("d", 0.9, 0.9, true, true)
	notReady.Ready = false
	bridge := row("d", 0.9, 0.9, true, true)
	bridge.FromWarmPool = true
	rows := []domain.ReplicaMetrics{
		row("d", 0.2, 0.9, true, true),   // the minute's mean, 0.2
		row("d", 0, 0.4, false, true),    // the instant, 0.4
		row("d", 0.9, 0.9, false, false), // no fill read
		row("p", 0.9, 0.9, true, true),   // prefill
		notReady, bridge,
	}
	k, ok := decodeFill(rows, roles, nil)
	assert.True(t, ok)
	assert.InDelta(t, 0.3, k, 1e-12)

	_, ok = decodeFill([]domain.ReplicaMetrics{row("p", 0.5, 0.5, true, true)}, roles, nil)
	assert.False(t, ok, "a cycle with no decode row has no fill")
}
