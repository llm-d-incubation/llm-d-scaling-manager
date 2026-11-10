package saturation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The rules that bound one slow pod (mu_slow_pod.go): the weighed sample
// (SwitchMuWeighsFleet), the yield's cap (SwitchMuYieldCappedAtMeasurement)
// and the unread replica's credit (SwitchMuUnreadAtTarget). The closed-loop
// probe holds them together
// (TestClosedLoopProbe_ASlowReplicaInsideTheServedBoundStaysUnderTheBound);
// these hold each edge alone, each run on and off.

// slowPodRows is four replicas of variant v at 1000 in, 1000 out: the
// first queued with a full cache, serving slow, and the other three
// unqueued at half their cache, serving rate.
func slowPodRows(slow, rate float64) []domain.ReplicaMetrics {
	const n, kv = 4, 0.5
	rows := make([]domain.ReplicaMetrics, 0, n)
	for i := range n {
		if i == 0 {
			rows = append(rows, imbRow(fmt.Sprintf("r%d", i), "v", 0.95, 10, 1000, 1000, slow))
			continue
		}
		rows = append(rows, imbRow(fmt.Sprintf("r%d", i), "v", kv, 0, 1000, 1000, rate))
	}
	return rows
}

// slowPodRig is a rig with switches set.
func slowPodRig(t *testing.T, switches map[string]bool) *propRig {
	t.Helper()
	rig := newPropRig()
	require.NoError(t, rig.a.SetSwitchesForReplay(switches))
	return rig
}

// An unread replica under the KV line counts at what it serves over the
// target; one with a full cache, at what it serves; and a target outside
// (0, 1), the switch off, credits nothing.
func TestMuSlowPod_AnUnreadReplicaUnderTheKVLineCountsAtWhatItServesOverTheTarget(t *testing.T) {
	served := servedRates{pods: map[string]servedPod{
		"slow": {rate: 0.4, queued: true, full: true},
		"open": {rate: 0.6},
		"full": {rate: 0.9, full: true},
	}}
	reads := map[string]float64{"slow": 0.4}
	for _, c := range []struct {
		target, want float64
	}{
		{0.85, (0.6/0.85 + 0.9) / 2},
		{0, (0.6 + 0.9) / 2},
		{1, (0.6 + 0.9) / 2},
		{1.2, (0.6 + 0.9) / 2},
	} {
		s := served
		s.target = c.target
		got, n := s.unread(reads)
		require.Equal(t, 2, n)
		require.InDelta(t, c.want, got, 1e-12, "target %v", c.target)
	}
}

// Through Analyze: one replica queued at 0.4 beside three unqueued at half
// their cache serving 0.6. The sample counts the three at 0.6 / 0.85, so mu
// is (0.4 + 3 x 0.706) / 4 = 0.629. With the credit off it counts them at
// 0.6, 0.550: the floor would then charge the target on a rate they were
// only offered.
func TestMuSlowPod_TheCreditReachesTheSample(t *testing.T) {
	for _, c := range []struct {
		on   bool
		want float64
	}{
		{true, (0.4 + 3*0.6/floorTarget) / 4},
		{false, (0.4 + 3*0.6) / 4},
	} {
		rig := slowPodRig(t, map[string]bool{SwitchMuUnreadAtTarget: c.on})
		for range 13 {
			_, err := rig.analyze(slowPodRows(0.4, 0.6), aggStates(4), 2.2, propCycle)
			require.NoError(t, err)
		}
		got := rig.measuredMu()
		t.Logf("credit %v: mu %.4f, want %.4f", c.on, got, c.want)
		require.InDelta(t, c.want, got, 1e-3*c.want, "credit %v", c.on)
	}
}

// The two ledger cases ev-shindin/llm-scaling-manager#495 cleared, each failing with its switch off, as
// they did on the parent: the slow reading refused (SwitchMuWeighsFleet) and
// the yield to the full siblings' median (SwitchMuYieldCappedAtMeasurement).
func TestMuSlowPod_TheClearedLedgerCasesFailWithTheirSwitchOff(t *testing.T) {
	for _, c := range []struct {
		name  string
		check func(map[string]bool) func(uint64) error
		off   string
	}{
		{"half-served-max-slow-pod", checkMuHalfServedWith, SwitchMuWeighsFleet},
		{"sibling-yield-slow-pod", checkMuSiblingYieldWith, SwitchMuYieldCappedAtMeasurement},
	} {
		for seed := uint64(1); seed <= 3; seed++ {
			require.NoError(t, c.check(nil)(seed), "%s seed %d with the switch on", c.name, seed)
			err := c.check(map[string]bool{c.off: false})(seed)
			var v *learnertest.Violation
			require.ErrorAs(t, err, &v, "%s seed %d with %s off", c.name, seed, c.off)
			require.Equal(t, kindOverPriced, v.Kind)
			t.Logf("%s, %s off, seed %d: %s", c.name, c.off, seed, v.Msg)
		}
	}
}

// The sibling yield never prices mu above the measurement it replaces
// (SwitchMuYieldCappedAtMeasurement). The fixture is the ttft-three-pods
// cycle the uncapped fleet yield moved: a variant measured at 3.39 whose full
// replicas served 5.18 unqueued. Capped, there is no yield; uncapped, the
// negative control, it priced 5.18, 53 % over the measurement.
func TestMuSlowPod_TheYieldNeverPricesAboveTheMeasurement(t *testing.T) {
	tn := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	served := servedRates{max: 5.18, saturated: 5.18, unqueued: true}
	for _, capped := range []bool{true, false} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchMuYieldCappedAtMeasurement: capped}))
		rate, yielded := a.siblingYield(propMuKey, throughputReading{rate: 3.39}, served, tn)
		t.Logf("capped %v: yielded %v at %.2f against a measured 3.39", capped, yielded, rate)
		if capped {
			require.False(t, yielded, "a yield priced above the measurement")
		} else {
			require.True(t, yielded, "premise: uncapped, the yield prices the siblings")
			require.Greater(t, rate, 3.39)
		}
	}

	// Through Analyze, the weighing off so the yield alone decides: p0 queued
	// at 3.39, two full siblings unqueued at 5.18.
	bucket := func(switches map[string]bool) string {
		a := newE1Analyzer(t, false)
		require.NoError(t, a.SetSwitchesForReplay(switches))
		var b map[string]string
		for i := range 20 {
			rows := make([]e1Row, 0, 3)
			rows = append(rows, e1Row{"p0", 30, 330_000, 3.39, 3.39*498 + float64(i%3)})
			for _, p := range []string{"p1", "p2"} {
				rows = append(rows, e1Row{p, 0, 330_000, 5.18, 5.18*498 + float64(i%3)})
			}
			b, _ = tunedE1(t, a, e1Start.Add(time.Duration(i)*15*time.Second), rows, nil)
		}
		return b["p0"]
	}
	require.NotEqual(t, servedBucket, bucket(map[string]bool{SwitchMuWeighsFleet: false}), "capped, p0 priced its siblings")
	require.Equal(t, servedBucket, bucket(map[string]bool{SwitchMuWeighsFleet: false, SwitchMuYieldCappedAtMeasurement: false}),
		"premise: uncapped, p0 yields to its siblings")
	require.NotEqual(t, servedBucket, bucket(nil), "all on, p0 priced its siblings")
}

// The credit's target is the one the floor charges on mu: the saturation
// analyzer's own scaleUpThreshold (config.AnalyzerThresholds), not the
// policy-wide one. A policy at 0.7 with the analyzer overridden to 0.95:
// one replica queued at 0.4 beside three unqueued at half their cache
// serving 0.6 reads (0.4 + 3 x 0.6 / 0.95) / 4 = 0.574. Negative control:
// credited at the policy-wide 0.7, as first built, it read
// (0.4 + 3 x 0.6 / 0.7) / 4 = 0.743, 29 % over, priced over what the floor
// then takes back off at 0.95.
func TestMuSlowPod_TheCreditUsesTheSaturationAnalyzersTarget(t *testing.T) {
	rig := slowPodRig(t, nil)
	up := 0.95
	rig.policy = func(p *config.ScalingPolicy) {
		p.ScaleUpThreshold = 0.7
		p.Analyzers = []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName, ScaleUpThreshold: &up}}
	}
	for range 13 {
		_, err := rig.analyze(slowPodRows(0.4, 0.6), aggStates(4), 2.2, propCycle)
		require.NoError(t, err)
	}
	got := rig.measuredMu()
	want, global := (0.4+3*0.6/up)/4, (0.4+3*0.6/0.7)/4
	t.Logf("mu %.4f; at the analyzer's 0.95 %.4f, at the policy's 0.7 %.4f", got, want, global)
	require.InDelta(t, want, got, 1e-3*want, "the credit's target is the saturation analyzer's")
}

// The weighing's hold (muChoice.unread): the unread replicas count at the
// lower of two consecutive samples' rates at most, held while the readers sit
// under the tolerance of them. One queued replica at 0.4 beside three
// unqueued at half their cache serving 0.6, then one minute's drain burst at
// 0.8, then the three falling to 0.55, 0.5 and 0.45 as a growing fleet would
// share the load. The burst lifts its own sample and no other, and the
// falling minutes hold the 0.6 the three showed. Read fresh (h := mean in
// weighUnreadReplicas) the falling minutes fall with them, which orders more,
// which lowers them again: the 0.3 probe peaked at 28 that way, against 24.
func TestMuSlowPod_TheWeighingHoldsAFallingFleetAndCarriesNoBurst(t *testing.T) {
	rig := slowPodRig(t, nil)
	served := []float64{0.6, 0.6, 0.6, 0.6, 0.8, 0.6, 0.55, 0.5, 0.45}
	for _, s := range served {
		for range 4 { // a minute, one sample
			_, err := rig.analyze(slowPodRows(0.4, s), aggStates(4), 0.4+3*s, propCycle)
			require.NoError(t, err)
		}
	}
	var samples []float64
	rig.a.mu.Lock()
	for _, ra := range rig.a.saturatedThroughput {
		samples = ra.Snapshot().Values
	}
	rig.a.mu.Unlock()
	t.Logf("samples %.4f", samples)
	require.GreaterOrEqual(t, len(samples), 5, "premise: the window holds the burst and the falling minutes")
	held := (0.4 + 3*0.6/floorTarget) / 4
	over := 0
	for _, v := range samples {
		if v > held*(1+1e-9) {
			over++
		}
	}
	require.Equal(t, 1, over, "the burst lifts its own sample only")
	for _, v := range samples[len(samples)-3:] {
		require.InDelta(t, held, v, 1e-9, "a falling fleet's samples hold what it showed")
	}
}

// How the weighing moves demand through the floor (analyzer-learner.md, "One
// slow pod, as built"), pinned on the start-seed-restart-2 golden's cycle at
// 21:22:34, the replay's own numbers: occupancy's demand 8193 tokens, of
// which 48 resident, 2.177 req/s arriving, and the floor's term at mu 0.3506
// with the weighing on and 0.3657 off. The floor's tokens rise as mu falls
// (53.2 against 51.1), but the trust blend reads lambda x P / mu against the
// resident KV, which a lower mu raises, so more of the queue's residency
// charge is swapped for the floor's drain price: 699 against 1527.
// localQueueBacklog is not the term: the capped backlog is 55.99 requests
// either way, and in replicas the cap is min(q / (mu x T), 1) per replica,
// which never falls as mu does.
func TestMuSlowPod_TheFloorTrustIsHowALowerMuLowersDemand(t *testing.T) {
	tn := tuning{AnalyzerTuning: tuningDefault.WithDefaults()}
	const measured, resident, lambda = 8193.0, 48.0, 2.1772658136774585
	at := func(mu, perReplica, replicas float64) float64 {
		tokens := replicas * perReplica
		return tn.flooredDemand(measured, resident, tokens, lambda*perReplica/mu, true)
	}
	on := at(0.3506359494741492, 6.0000310445465095, 8.87084602889296)
	off := at(0.3657028708413113, 6.011312673169893, 8.505368067863964)
	t.Logf("floored demand %.2f with the weighing, %.2f without", on, off)
	require.InDelta(t, 699.28948785588, on, 0.01)
	require.InDelta(t, 1527.275814274085, off, 0.01)
	// Without the swap, the same terms floor at occupancy's demand either way.
	require.Equal(t, measured, tn.flooredDemand(measured, resident, 53.2, lambda*6/0.35, false))
}
