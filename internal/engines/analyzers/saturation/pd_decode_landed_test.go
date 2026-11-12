package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Issue #43. vLLM keeps a decode request deferred from the moment its KV pull
// starts until it is scheduled, and promotes a landed pull only inside its
// waiting loop, which stops at max_num_seqs or when a block cannot be
// allocated. Where vLLM (v0.32.0 and later) says which pulls have landed
// (num_requests_kv_fetch_by_stage{stage="completed_waiting"}), those wait on
// this replica, and the decode's queue is the capacity part plus them.

// withLanded gives rm n deferred requests whose KV has arrived.
func withLanded(rm domain.ReplicaMetrics, n int) domain.ReplicaMetrics {
	rm.LandedQueueKnown = true
	rm.LandedQueueLength = n
	rm.MaxRankLandedQueueLength = n
	return rm
}

// A decode capped only by vLLM's default max_num_seqs: the cap is not known,
// so the full queue is not charged, and without the stage the landed pulls
// were dropped with the in-flight ones.
func TestPDDecodeChargesLandedPullsAtADefaultCap(t *testing.T) {
	const landed = 40
	run := func(t *testing.T, rm domain.ReplicaMetrics) (*SaturationAnalyzer, *domain.AnalyzerResult) {
		t.Helper()
		params := deploymentParams()
		params.MaxNumSeqs, params.MaxNumSeqsSet = 256, false
		store := capacity.NewStore()
		store.Update("test-ns", "test-model", decodeVariant, capacity.Record{
			GpuCount: 1, EngineParams: params, LearnedFrom: "deployment",
		}, time.Now())
		rm.RunningRequests = 256
		return analyzePDWith(t, store, []domain.ReplicaMetrics{rm, pdPrefillRow()}, pdStates)
	}

	a, res := run(t, withReason(pdDecodeRow(halfCache), 0))
	require.False(t, recordedK2(a), "precondition: without the stage the landed pulls read as in flight")
	require.InDelta(t, float64(halfCache), decodeDemand(t, res), 1, "precondition: and are not charged")

	a, res = run(t, withLanded(withReason(pdDecodeRow(halfCache), 0), landed))
	assert.True(t, recordedK2(a), "40 landed pulls waiting for a batch slot are over the threshold of 5")
	assert.InDelta(t, float64(halfCache)+landed*(pdIn+pdOut), decodeDemand(t, res), 1,
		"the landed pulls are charged; the rest of the 768 are still in flight")
}

// A full decode below its cap: landed pulls cannot get their next block, and
// they are charged beside the requests that wait for capacity.
func TestPDDecodeChargesLandedPullsOnAFullDecode(t *testing.T) {
	const capacityWait, landed = 10, 30
	full := int64(runKvCapacity * 95 / 100)
	_, res := analyzePD(t, []domain.ReplicaMetrics{withLanded(withReason(pdDecodeRow(full), capacityWait), landed), pdPrefillRow()}, pdStates)
	assert.InDelta(t, float64(full)+(capacityWait+landed)*(pdIn+pdOut), decodeDemand(t, res), 1,
		"capacity + completed_waiting")
}

// Without the stage -- vLLM before v0.32.0 -- nothing changes: a landed count
// that is not known is not read, and the result is the one before.
func TestPDDecodeWithoutTheLandedStageIsUnchanged(t *testing.T) {
	for name, base := range map[string]domain.ReplicaMetrics{
		"with the reason":    withReason(pdDecodeRow(halfCache), 40),
		"without the reason": pdDecodeRow(halfCache),
	} {
		t.Run(name, func(t *testing.T) {
			stray := base
			stray.LandedQueueLength, stray.MaxRankLandedQueueLength = 500, 500 // not known: not a reading
			aBase, resBase := analyzePD(t, []domain.ReplicaMetrics{base, pdPrefillRow()}, pdStates)
			aStray, resStray := analyzePD(t, []domain.ReplicaMetrics{stray, pdPrefillRow()}, pdStates)
			assert.Equal(t, resBase, resStray, "the whole result")
			assert.Equal(t, recordedK2(aBase), recordedK2(aStray))
			view := decodeCapacityView(stray, domain.RoleDecode, 0)
			view.LandedQueueLength, view.MaxRankLandedQueueLength = 0, 0
			assert.Equal(t, decodeCapacityView(base, domain.RoleDecode, 0), view,
				"the view reads the row as it read it before")
		})
	}
}

func TestDecodeCapacityViewWithLandedPulls(t *testing.T) {
	multi := domain.ReplicaMetrics{
		VariantName: "d",
		QueueLength: 800, MaxRankQueueLength: 300, EngineRanks: 8,
		CapacityQueueKnown: true, CapacityQueueLength: 24, MaxRankCapacityQueueLength: 9,
		LandedQueueKnown: true, LandedQueueLength: 40, MaxRankLandedQueueLength: 11,
	}
	v := decodeCapacityView(multi, domain.RoleDecode, 0)
	assert.Equal(t, 64, v.QueueLength, "capacity + completed_waiting")
	assert.Equal(t, 20, v.BusiestRankQueue(), "the busiest rank's of each, never more than the full queue's")

	// The two are one-minute peaks: their sum is capped at the full count.
	over := multi
	over.QueueLength, over.MaxRankQueueLength = 50, 15
	v = decodeCapacityView(over, domain.RoleDecode, 0)
	assert.Equal(t, 50, v.QueueLength)
	assert.Equal(t, 15, v.BusiestRankQueue())

	// Landed without the reason: the full count, as without either.
	noReason := multi
	noReason.CapacityQueueKnown = false
	assert.Equal(t, noReason, decodeCapacityView(noReason, domain.RoleDecode, 0))

	// At a stated cap: the full count, landed or not.
	capped := multi
	capped.RunningRequests = 8 * 256
	assert.Equal(t, capped, decodeCapacityView(capped, domain.RoleDecode, 256))

	for _, role := range []string{domain.RolePrefill, domain.RoleBoth, ""} {
		assert.Equal(t, multi, decodeCapacityView(multi, role, 0), "role %q keeps its row", role)
	}
}

// Control: a model with no prefill variant reads its decode's whole queue,
// landed stage or not.
func TestDecodeWithoutPrefillIgnoresTheLandedStage(t *testing.T) {
	rm := withLanded(withReason(pdDecodeRow(halfCache), 0), 40)
	_, res := analyzePD(t, []domain.ReplicaMetrics{rm}, decodeOnlyStates)
	assert.InDelta(t, float64(halfCache)+queueCharge, decodeDemand(t, res), 1, "the whole queue is charged")
}
