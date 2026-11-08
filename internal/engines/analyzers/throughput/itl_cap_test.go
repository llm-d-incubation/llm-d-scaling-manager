package throughput

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
)

// The line's cap admission (learner PR 4b-3, SwitchITLAdmitsAtSequenceCap),
// each through Analyze, with the switch on and off. The property cases are
// in property_cap_test.go; the probe's case in closed_loop_probe_test.go.

// capLineRow is a replica of v1 above the band at k on the probe's line, its
// batch at 64 on its one rank, requests waiting, no preemption, read.
func capLineRow(pod string, k float64) domain.ReplicaMetrics {
	row := lineRow(pod, k, probeITL(k), lineStart.Add(-time.Hour))
	row.RunningRequests, row.RunningRequestsKnown, row.QueueLength = 64, true, 5
	row.PreemptionRate, row.PreemptionRateKnown = 0, true
	return row
}

// paramsStore is a capacity store holding p for v1.
func paramsStore(p capacity.EngineParams) *capacity.Store {
	store := capacity.NewStore()
	store.Update("ns", "m", "v1", capacity.Record{EngineParams: &p}, lineStart)
	return store
}

// lineRead is what v1's line read on the last cycle, and the cap its
// admission carried.
func lineRead(a *ThroughputAnalyzer) (itl.Reading, int64, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.variantStates[variantKey("ns", "m", "v1")]
	return st.reading, st.batchCap, st.batchCapSource
}

// A replica above the band at the cap its engine states is a reading of the
// band's line, whichever way the cap is stated: the flags' max_num_seqs, the
// flags' max_num_seqs lowered by --max-num-active-seqs, or the
// llm-d.ai/max-num-seqs annotation. Off, the line 4b-2 built refuses it, as
// does an analyzer with no store.
func TestITLCap_AReplicaAtItsStatedCapIsAReading(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params capacity.EngineParams
		source string
	}{
		{"flags", capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, MaxNumSeqsSet: true}, batchCapFlags},
		{"flags lowered by the active cap", capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 128,
			MaxNumSeqsSet: true, MaxNumActiveSeqs: 64}, batchCapFlags},
		{"the annotation", capacity.EngineParams{Engine: inferenceengine.EngineVLLM, DeclaredBatchCap: 64}, batchCapDeclared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.EqualValues(t, 64, tc.params.StatedBatchCap(), "premise: the cap is stated")
			read := func(a *ThroughputAnalyzer) (itl.Reading, int64, string) {
				newLineRun(t, a).step([]domain.ReplicaMetrics{capLineRow("pod-a", 0.88)})
				return lineRead(a)
			}
			on, capOn, source := read(NewThroughputAnalyzer().WithCapacityStore(paramsStore(tc.params)))
			require.Equal(t, 1, on.Offered, "the capped reading was refused")
			require.Equal(t, 1, on.Capped)
			require.Zero(t, on.AboveBand)
			require.EqualValues(t, 64, capOn)
			require.Equal(t, tc.source, source)

			off, capOff, sourceOff := read(withSwitchOff(SwitchITLAdmitsAtSequenceCap)().WithCapacityStore(paramsStore(tc.params)))
			require.Zero(t, off.Offered, "negative control: with the switch off the capped reading is the band's")
			require.Equal(t, 1, off.AboveBand)
			require.Zero(t, capOff)
			require.Equal(t, batchCapNone, sourceOff)

			bare, _, _ := read(NewThroughputAnalyzer())
			require.Zero(t, bare.Offered, "an analyzer with no store knows no cap")
		})
	}
}

// The cap is read only on the tracker: the parent's window (the steady gate
// off) reads the band alone, with the cap admission on or off.
func TestITLCap_TheParentsWindowReadsNoCap(t *testing.T) {
	a := newParentLineAnalyzer()
	adm := a.lineAdmission(a.admission(itlPolicy{}, "ns", "m"), "ns", "m",
		variantEngine{batchCap: 64, batchCapSource: batchCapFlags})
	require.Zero(t, adm.MaxNumSeqs)

	a = newParentLineAnalyzer().WithCapacityStore(paramsStore(capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64}))
	newLineRun(t, a).step([]domain.ReplicaMetrics{capLineRow("pod-a", 0.88)})
	st, _ := a.VariantState("m", "ns", "v1")
	require.Zero(t, st.SampleCount, "the parent's window kept a reading above the band")
}

// The wired analyzer of TestTheWiredAnalyzerReadsTheFleetAsABareOneDoes,
// with the cap admission on, admits that fleet's capped replica, and only
// it, above the band: the one row the equality there turns the switch off
// for.
func TestITLCap_TheWiredLineAdmitsTheCappedReplica(t *testing.T) {
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	store := capacity.NewStore()
	store.FileScaleTarget("ns", "m", "v1", "H100", 1,
		capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, MaxNumSeqsSet: true, Image: "img:1"},
		false, start)
	tracker := settle.NewTracker()
	clock := clocktesting.NewFakePassiveClock(start)
	wired := NewThroughputAnalyzer().WithClock(clock).WithSettleView(tracker).WithCapacityStore(store).
		WithOptimizationInterval(15 * time.Second).WithSlopeHoldFor(24 * time.Hour)
	rows := neutralRows(start, 0)
	tracker.Observe("ns/m", settlerow.Of(rows, nil), start)
	_, err := wired.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns", ReplicaMetrics: rows,
		Config: &config.ScalingPolicy{}, ArrivalRate: 8,
		VariantStates: []domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: len(rows), GPUsPerReplica: 1}}})
	require.NoError(t, err)
	r, _, _ := lineRead(wired)
	require.Equal(t, 1, r.Capped, "pod-cap is admitted at its cap")
	require.Equal(t, 1, r.AboveBand, "pod-high, above the band and not at a cap, is not")
}

// Which window an at-cap reading goes to, against the above-band fallback
// of learner PR 4b-1. A variant with one replica at its cap at k 0.92,
// another above the band at k 0.90 and not at its cap (its batch one short),
// and one below the band. On: the capped replica's readings enter the
// band's window, so the band's line prices the variant from the tenth on
// (T2-default) and the fallback, which keeps the uncapped replica's
// readings in its own window, is never used. Off: nothing is the band's,
// and the fallback prices it (T2-above-band). And with the capped replica
// preempting, no reading is the band's either way: the fallback prices it,
// as it does for any key with no line from the band.
func TestITLCap_AVariantAtItsCapPricesFromTheBandNotTheFallback(t *testing.T) {
	store := func() *capacity.Store {
		return paramsStore(capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64})
	}
	run := func(a *ThroughputAnalyzer, preempt float64) (map[string]int, itl.Reading) {
		r := newLineRun(t, a.WithCapacityStore(store()))
		reasons := map[string]int{}
		for range 20 {
			capped := capLineRow("pod-cap", 0.92)
			capped.PreemptionRate = preempt
			short := capLineRow("pod-short", 0.90)
			short.RunningRequests = 63
			vc, has := r.step([]domain.ReplicaMetrics{capped, short,
				lineRow("pod-low", 0.08, probeITL(0.08), lineStart.Add(-time.Hour))})
			if !has {
				reasons[itlReasonT2Failed]++
				continue
			}
			reasons[vc.Reason]++
		}
		read, _, _ := lineRead(a)
		return reasons, read
	}
	on, onRead := run(NewThroughputAnalyzer(), 0)
	t.Logf("on: %v, the band's window %d, the fallback's %d", on, onRead.Held, onRead.AboveBandHeld)
	require.Equal(t, map[string]int{itlReasonT2Failed: 9, itlReasonT2Default: 11}, on,
		"the band's line, from the capped replica's tenth reading")
	require.Equal(t, 20, onRead.AboveBandHeld, "premise: the fallback's window held readings it was not asked for")

	off, _ := run(withSwitchOff(SwitchITLAdmitsAtSequenceCap)(), 0)
	t.Logf("negative control, the cap admission off: %v", off)
	require.Zero(t, off[itlReasonT2Default])
	require.Positive(t, off[itlReasonT2AboveBand], "negative control: off, the fallback prices the variant")

	preempting, read := run(NewThroughputAnalyzer(), 0.5)
	t.Logf("the capped replica preempting: %v", preempting)
	require.Zero(t, read.Capped)
	require.Zero(t, preempting[itlReasonT2Default], "a preempting replica's reading was the band's")
	require.Positive(t, preempting[itlReasonT2AboveBand], "with nothing in the band, the fallback still prices")
}
