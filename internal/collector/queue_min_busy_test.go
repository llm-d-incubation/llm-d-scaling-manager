package collector

import (
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
)

// A data-parallel prefill pod's busy minutes, from its ranks' rows to V_P:
// the pod collapse sums the ranks' queue minimums and computed-token rates,
// and the prefill observation (prefill.QueueMins, as the saturation analyzer
// feeds it) reads the sum as the pod's. Two ranks at 12000 and 12500 tokens/s
// make a pod at 24500, and five such minutes read V_P at 24500 exactly, a
// lower bound (prefill.DataParallelRanks): the pod has two ranks.
//
//   - Both ranks queued the whole minute (minimums 1 and 2): busy.
//   - One rank's queue emptied (0) while the other's did not (2): the pod's
//     queue never did, and the minute is busy. Its rate is under the pod's
//     capacity if that rank idled, never over, which the flag says.
//   - One rank's minimum was not read: the pod's is not known, and no minute
//     is busy.
func TestQueueMin_ARankSumIsThePodsBusyMinute(t *testing.T) {
	const vp = 24500.0
	for _, tc := range []struct {
		name     string
		mins     [2]float64
		known    [2]bool
		wantBusy int
	}{
		{"both queued", [2]float64{1, 2}, [2]bool{true, true}, 5},
		{"one rank emptied", [2]float64{0, 2}, [2]bool{true, true}, 5},
		{"one rank unread", [2]float64{1, 2}, [2]bool{true, false}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rank := func(rate, qmin float64, known bool) domain.ReplicaMetrics {
				return domain.ReplicaMetrics{
					PodName: "prefill-0", ModelID: "m", Namespace: "ns", VariantName: "prefill", Ready: true,
					PrefillComputedTokenRate: rate, PrefillComputedTokenRateKnown: true,
					RequestRate: rate / 4000, RequestRateKnown: true,
					QueueLength: 3, QueueLengthKnown: true,
					QueueMin: qmin, QueueMinKnown: known,
					Metadata: &domain.ReplicaMetricsMetadata{},
				}
			}
			o := prefill.NewObserver()
			start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			var est prefill.Estimate
			for i := range 5 {
				pods := collapseToPods([]domain.ReplicaMetrics{
					rank(12000, tc.mins[0], tc.known[0]),
					rank(12500, tc.mins[1], tc.known[1]),
				})
				if len(pods) != 1 {
					t.Fatalf("the ranks collapsed to %d pods, want 1", len(pods))
				}
				est = o.Observe(prefill.Cycle{Key: "k", Engine: inferenceengine.EngineVLLM, Disaggregated: true,
					Rows: pods, QueueMin: prefill.QueueMins(pods), Now: start.Add(time.Duration(i) * time.Minute)})
			}
			if est.VPBusyMinutes != tc.wantBusy {
				t.Fatalf("busy minutes = %d, want %d", est.VPBusyMinutes, tc.wantBusy)
			}
			if tc.wantBusy == 0 {
				if est.VPSource == prefill.SourceBusyMinutes {
					t.Errorf("V_P from busy minutes with a rank unread: %v", est.VP)
				}
				return
			}
			if est.VPSource != prefill.SourceBusyMinutes || est.VP != vp {
				t.Errorf("V_P = %v from %q, want %v from busy minutes", est.VP, est.VPSource, vp)
			}
			// The collapse counts the pod's two ranks, so its busy minutes
			// are a lower bound, with the reason.
			if !est.VPLowerBound || est.VPLowerBoundReason != prefill.DataParallelRanks {
				t.Errorf("a two-rank pod's busy minutes: lower bound %v (%q), want true (%q)",
					est.VPLowerBound, est.VPLowerBoundReason, prefill.DataParallelRanks)
			}
		})
	}
}
