package saturation

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
)

// Issue llm-d-incubation/llm-d-scaling-manager#97: below the knee a P/D
// prefill learns no mu and no k2, and had no throughput floor. The busy-share
// bound (prefill_busy_share.go) gives it one.

// busyPrefillRow is one prefill replica of variant p below the knee: its own
// queue empty, completing rate requests a second at serviceTime each, holding
// resident tokens.
func busyPrefillRow(pod string, rate, serviceTime float64, resident int64) domain.ReplicaMetrics {
	rm := satRow(pod, resident, 0, rate)
	rm.VariantName = "p"
	rm.AvgServiceTime = serviceTime
	rm.TotalKvCapacityTokens = propKv
	rm.KvCacheUsage = float64(resident) / float64(propKv)
	return rm
}

// belowTheKneeStates is a P/D fleet of the given number of prefill replicas
// (variant p) and two decode replicas (variant v).
func belowTheKneeStates(prefill int) []domain.VariantReplicaState {
	return []domain.VariantReplicaState{decodeStates(2)[0],
		{VariantName: "p", AcceleratorName: "H200", CurrentReplicas: prefill, GPUsPerReplica: 1, Role: domain.RolePrefill}}
}

// belowTheKneeCycle is one cycle's rows: two decode replicas at 30 % of their
// KV with nothing queued, and prefill replicas each completing lambda/prefill
// requests a second at serviceTime x (1 + up to 1 %), the jitter a one-minute
// mean carries (an exact repeat is not a new sample).
func belowTheKneeCycle(r *rand.Rand, prefill int, lambda, serviceTime float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, 2+prefill)
	rows = append(rows, fullDecode("d0", 0.3, 0), fullDecode("d1", 0.3, 0))
	for p := range prefill {
		rows = append(rows, busyPrefillRow(fmt.Sprintf("p%d", p), lambda/float64(prefill),
			serviceTime*(1+0.01*r.Float64()), propKv/20))
	}
	return rows
}

// prefillImplied is the prefill replicas res's role demand implies at the
// scale-up threshold the analyzer sizes with: demand / P / target.
func prefillImplied(res *domain.AnalyzerResult) (float64, error) {
	for _, vc := range res.VariantCapacities {
		if vc.VariantName == "p" && vc.PerReplicaCapacity > 0 {
			return res.RoleDemand[domain.RolePrefill] / vc.PerReplicaCapacity / floorTarget, nil
		}
	}
	return 0, errors.New("no prefill variant capacity in the result")
}

// checkFloorPrefillBelowTheKnee: a P/D fleet of 2 to 4 prefill replicas
// whose engine queues read empty and whose router queue is empty, the case
// liveness/k2/flow-control-on is the decode-only half of. Each prefill
// replica serves one prompt in 1/mu seconds and is busy 30 to 70 % of the
// time. After three minutes prefill is priced at the floor's estimand,
// lambda / (mu x target) replicas, within propTruthTol: not on its resident KV
// alone, and not over. stub, when set, edits the analyzer first (a mutant).
func checkFloorPrefillBelowTheKnee(stub func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		prefill := 2 + r.IntN(3)
		mu := 5 + 25*r.Float64()
		lambda := float64(prefill) * mu * (0.3 + 0.4*r.Float64())
		rig := newPropRig()
		if stub != nil {
			stub(rig.a)
		}
		var res *domain.AnalyzerResult
		for range 13 {
			var err error
			if res, err = rig.analyze(belowTheKneeCycle(r, prefill, lambda, 1/mu), belowTheKneeStates(prefill), lambda, propCycle); err != nil {
				return err
			}
		}
		got, err := prefillImplied(res)
		if err != nil {
			return err
		}
		want := lambda / (mu * floorTarget)
		if got < want*(1-propTruthTol) {
			return violation(kindNotLearned, "%d prefill replicas below the knee at lambda %.1f, mu %.1f: priced %.2f replicas against %.2f",
				prefill, lambda, mu, got, want)
		}
		return holds(got <= want*(1+propTruthTol), kindOverPriced, "%d prefill replicas below the knee at lambda %.1f, mu %.1f: priced %.2f replicas against %.2f",
			prefill, lambda, mu, got, want)
	}
}

func TestPrefillBusyShareRate(t *testing.T) {
	serving := admission{}
	prefill := func(rate, svc float64, ranks int) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{RequestRate: rate, AvgServiceTime: svc, EngineRanks: ranks}
	}
	for _, c := range []struct {
		name string
		rm   domain.ReplicaMetrics
		role string
		adm  admission
		want float64
	}{
		{"one prompt at a time: 1/service time", prefill(4, 0.05, 1), domain.RolePrefill, serving, 20},
		{"busier than one at a time: what it served", prefill(25, 0.05, 1), domain.RolePrefill, serving, 25},
		{"two data-parallel ranks serve two at a time", prefill(4, 0.05, 2), domain.RolePrefill, serving, 40},
		{"decode has no busy-share bound", prefill(4, 0.05, 1), domain.RoleDecode, serving, 0},
		{"no service time read", prefill(4, 0, 1), domain.RolePrefill, serving, 0},
		{"no request rate", prefill(0, 0.05, 1), domain.RolePrefill, serving, 0},
		{"a NaN service time", prefill(4, math.NaN(), 1), domain.RolePrefill, serving, 0},
		{"mid-rollout", prefill(4, 0.05, 1), domain.RolePrefill, admission{rollout: true}, 0},
		{"not Ready", prefill(4, 0.05, 1), domain.RolePrefill, admission{why: "not Ready"}, 0},
	} {
		if got := prefillBusyShareRate(c.rm, c.role, c.adm); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Below the knee, with the router queue empty, prefill is priced at the
// floor; with the switch off (the control) on its resident KV alone, as
// before.
func TestPrefillBelowTheKneeGetsAFloor(t *testing.T) {
	if err := checkFloorPrefillBelowTheKnee(nil)(1); err != nil {
		t.Errorf("on: %v", err)
	}
	err := checkFloorPrefillBelowTheKnee(func(a *SaturationAnalyzer) { a.prefillBusyShare = false })(1)
	if v, ok := err.(*propViolation); !ok || v.Kind != kindNotLearned {
		t.Errorf("off (the control): %v, want prefill priced on its resident KV alone (%s)", err, kindNotLearned)
	}
}

// busyBoundTokens is the busy-share bound the analyzer prices from: the
// larger of its window's mean and the held figure, in tokens/s; 0 with no
// window.
func (r *propRig) busyBoundTokens() float64 {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	for k, ra := range r.a.saturatedThroughput {
		if strings.Contains(k, "|"+prefillBusyRole+"|") && ra.Len() > 0 {
			return max(ra.Average(), r.a.prefillBoundHeld[k].tokens)
		}
	}
	return 0
}

// Under load prompts share their steps and each takes longer, so one reading
// falls under what a replica sustains. The held figure keeps what a quieter
// period read: twenty minutes at twice the service time do not pull the
// bound down to them.
func TestPrefillBusyShareKeepsTheQuieterReading(t *testing.T) {
	const mu, prefill, prompt = 10.0, 2, 1000.0
	r := propRand(7)
	rig := newPropRig()
	for range 4 * 10 {
		if _, err := rig.analyze(belowTheKneeCycle(r, prefill, 6, 1/mu), belowTheKneeStates(prefill), 6, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	quiet := rig.busyBoundTokens()
	if quiet < 0.97*mu*prompt {
		t.Fatalf("precondition: ten quiet minutes at mu %.0f read %.0f tokens/s, want about %.0f", mu, quiet, mu*prompt)
	}
	// Busy: each replica serves 8 req/s at twice the lone service time, so
	// one reading is max(8, 5) = 8 req/s.
	for range 4 * 20 {
		if _, err := rig.analyze(belowTheKneeCycle(r, prefill, 16, 2/mu), belowTheKneeStates(prefill), 16, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	if got := rig.busyBoundTokens(); got < 0.97*mu*prompt {
		t.Errorf("after twenty busy minutes the bound read %.0f tokens/s, want the quiet %.0f kept (one reading is %.0f)",
			got, quiet, 8*prompt)
	}
}

// A prefill priced on a lower bound keeps its share of the router queue only
// up to one doubling of its ready supply, as a prefill with no mu does:
// kept whole beside the floor's term, a runaway's router queue priced it at
// 20 replicas from 2. With the switch off (the control) the share is kept
// whole, as before.
func TestPrefillOnALowerBoundKeepsTheRouterShareCapped(t *testing.T) {
	const k1, resident, share = 900_000.0, 200_000.0, 16_000_000.0
	price := func(a *SaturationAnalyzer) float64 {
		vcs := []domain.VariantCapacity{
			{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 2, PerReplicaCapacity: k1, TotalDemand: resident},
			{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: k1, TotalDemand: 400_000},
		}
		bound := capacity.ReplicaCapacity{VariantName: "p", SaturatedThroughput: 20, SaturatedThroughputSamples: 3,
			SaturatedThroughputLowerBound: true}
		rcs := []capacity.ReplicaCapacity{bound, bound,
			{VariantName: "d", PodName: "d0", SaturatedThroughput: 6, SaturatedThroughputSamples: 2},
			{VariantName: "d", PodName: "d1", SaturatedThroughput: 6, SaturatedThroughputSamples: 2},
		}
		rcs[0].PodName, rcs[1].PodName = "p0", "p1"
		roleDemand := map[string]float64{domain.RolePrefill: resident + share, domain.RoleDecode: 400_000 + 400_000}
		eppByRole := map[string]float64{domain.RolePrefill: share, domain.RoleDecode: 400_000}
		in := makeAnalyzerInput(nil, nil)
		in.ArrivalRate = 30
		a.applyThroughputFloor(in, in.Config.(*config.ScalingPolicy), rcs, vcs, resident+share+800_000, roleDemand, eppByRole,
			2000, false, logr.Discard())
		return roleDemand[domain.RolePrefill]
	}
	scaleUp, _ := makeAnalyzerInput(nil, nil).Config.(*config.ScalingPolicy).AnalyzerThresholds(domain.SaturationAnalyzerName)
	growth := floor.LowerBoundGrowthCap(scaleUp, 2*k1)
	if on := price(NewSaturationAnalyzer(capacity.NewStore())); on > growth*(1+1e-9) {
		t.Errorf("on: prefill priced %.0f, over one doubling of its ready supply, %.0f", on, growth)
	}
	off := NewSaturationAnalyzer(capacity.NewStore())
	off.prefillBusyShare = false
	if got := price(off); got < resident+share {
		t.Errorf("off (the control): prefill priced %.0f, want the share kept whole, at least %.0f", got, resident+share)
	}
}
