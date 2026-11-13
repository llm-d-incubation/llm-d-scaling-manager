package saturation

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

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
// x (1 - up to 1 %) requests a second at serviceTime x (1 + up to 1 %): the
// jitter a one-minute mean carries. Both carry it, since either may be the
// reading, and an exact repeat is not a new sample.
func belowTheKneeCycle(r *rand.Rand, prefill int, lambda, serviceTime float64) []domain.ReplicaMetrics {
	rows := make([]domain.ReplicaMetrics, 0, 2+prefill)
	rows = append(rows, fullDecode("d0", 0.3, 0), fullDecode("d1", 0.3, 0))
	for p := range prefill {
		rows = append(rows, busyPrefillRow(fmt.Sprintf("p%d", p), lambda/float64(prefill)*(1-0.01*r.Float64()),
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

// withTTFT is rm with its time to first token, and its queue time when
// known.
func withTTFT(rm domain.ReplicaMetrics, ttft, queue float64, queueKnown bool) domain.ReplicaMetrics {
	rm.AvgTTFT = ttft
	rm.AvgQueueTime, rm.AvgQueueTimeKnown = queue, queueKnown
	return rm
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
		{"no service time read (SGLang's clamp at zero)", prefill(4, 0, 1), domain.RolePrefill, serving, 0},
		{"no request rate", prefill(0, 0.05, 1), domain.RolePrefill, serving, 0},
		{"a NaN service time", prefill(4, math.NaN(), 1), domain.RolePrefill, serving, 0},
		{"an infinite service time", prefill(4, math.Inf(1), 1), domain.RolePrefill, serving, 0},
		{"a service time a quarter of TTFT: 1/service time", withTTFT(prefill(4, 0.05, 1), 0.2, 0, false), domain.RolePrefill, serving, 20},
		{"a service time under a quarter of TTFT", withTTFT(prefill(4, 0.001, 1), 0.2, 0, false), domain.RolePrefill, serving, 0},
		{"TTFT mostly queue time, read: 1/service time", withTTFT(prefill(4, 0.05, 1), 2.0, 1.9, true), domain.RolePrefill, serving, 20},
		{"TTFT mostly queue time, not read", withTTFT(prefill(4, 0.05, 1), 2.0, 0, false), domain.RolePrefill, serving, 0},
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
// 20 replicas from 2. With SwitchLowerBoundCapsRouterShare off (the control)
// the share is kept whole, as before.
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
	off.lowerBoundCapsShare = false
	if got := price(off); got < resident+share {
		t.Errorf("off (the control): prefill priced %.0f, want the share kept whole, at least %.0f", got, resident+share)
	}
}

// sharedStepCycle is one cycle's rows: two decode replicas at 30 % of their
// KV with nothing queued, and prefill replicas of ranks engine ranks each
// completing lambda/prefill requests a second at serviceTime x (1 + up to
// 1 %).
func sharedStepCycle(r *rand.Rand, prefill, ranks int, lambda, serviceTime float64) []domain.ReplicaMetrics {
	rows := belowTheKneeCycle(r, prefill, lambda, serviceTime)
	for i := range rows {
		if rows[i].VariantName == "p" {
			rows[i].EngineRanks = ranks
		}
	}
	return rows
}

// checkFloorPrefillSharedSteps: below the knee, as
// checkFloorPrefillBelowTheKnee, but prompts share steps, so the service
// time grows with load: S = S0 x (1 + c x rho) at a utilisation rho of the
// true rate, c from 0 to 2, and a batch computes g times what one prompt
// alone does (mu = g / S0, g from 1 to 3). Five minutes at rho 0.2, then
// eight at rho 0.3 to 0.8. Every reading is then under the true rate, so the
// truth is one-sided: prefill is never priced under lambda / (mu x target),
// and never over the growth cap, LowerBoundMaxGrowth times its ready
// replicas.
func checkFloorPrefillSharedSteps(stub func(*SaturationAnalyzer)) func(uint64) error {
	return func(seed uint64) error {
		r := propRand(seed)
		prefill := 2 + r.IntN(3)
		s0 := 1 / (5 + 25*r.Float64())
		g := 1 + 2*r.Float64()
		c := 2 * r.Float64()
		mu := g / s0
		rig := newPropRig()
		if stub != nil {
			stub(rig.a)
		}
		run := func(rho float64, cycles int) (*domain.AnalyzerResult, float64, error) {
			lambda := float64(prefill) * mu * rho
			var res *domain.AnalyzerResult
			for range cycles {
				var err error
				if res, err = rig.analyze(sharedStepCycle(r, prefill, 1, lambda, s0*(1+c*rho)),
					belowTheKneeStates(prefill), lambda, propCycle); err != nil {
					return nil, 0, err
				}
			}
			return res, lambda, nil
		}
		if _, _, err := run(0.2, 20); err != nil {
			return err
		}
		rho := 0.3 + 0.5*r.Float64()
		res, lambda, err := run(rho, 32)
		if err != nil {
			return err
		}
		got, err := prefillImplied(res)
		if err != nil {
			return err
		}
		want := lambda / (mu * floorTarget)
		if got < want*(1-propTruthTol) {
			return violation(kindWrongSide, "%d prefill replicas sharing steps (g %.2f, c %.2f) at rho %.2f: priced %.2f replicas, under the %.2f the true rate needs",
				prefill, g, c, rho, got, want)
		}
		capReplicas := 2.0 * float64(prefill) // floor.LowerBoundMaxGrowth
		return holds(got <= max(want*(1+propTruthTol), capReplicas)*(1+1e-9), kindOverPriced,
			"%d prefill replicas sharing steps (g %.2f, c %.2f) at rho %.2f: priced %.2f replicas, over the growth cap %.0f",
			prefill, g, c, rho, got, capReplicas)
	}
}

// checkFloorPrefillDataParallel: checkFloorPrefillBelowTheKnee on prefill
// pods of two data-parallel engine ranks, through the whole analyzer: each
// rank serves one prompt in 1/mu seconds, so a pod serves 2 x mu, and
// prefill is priced at lambda / (2 x mu x target) within propTruthTol.
func checkFloorPrefillDataParallel(seed uint64) error {
	r := propRand(seed)
	prefill := 2 + r.IntN(3)
	mu := 5 + 25*r.Float64()
	lambda := float64(prefill) * 2 * mu * (0.3 + 0.4*r.Float64())
	rig := newPropRig()
	var res *domain.AnalyzerResult
	for range 13 {
		var err error
		if res, err = rig.analyze(sharedStepCycle(r, prefill, 2, lambda, 1/mu), belowTheKneeStates(prefill), lambda, propCycle); err != nil {
			return err
		}
	}
	got, err := prefillImplied(res)
	if err != nil {
		return err
	}
	want := lambda / (2 * mu * floorTarget)
	return holds(relErr(got, want) <= propTruthTol, kindBiased, "%d two-rank prefill pods at lambda %.1f, mu %.1f a rank: priced %.2f replicas against %.2f",
		prefill, lambda, mu, got, want)
}

// busyTestKey is a prefill throughput key, nine fields, role prefill.
const busyTestKey = "ns|v|model|H200|1|prefill|ilong|noout|q5"

// busyDirect calls notePrefillBusyShare once for a replica completing rate
// requests a second at svc x (1 + up to 0.1 %) seconds each, at prompt
// length ileff, and moves the clock a minute.
func busyDirect(a *SaturationAnalyzer, clock *time.Time, r *rand.Rand, rate, svc, ileff float64) prefillBoundReading {
	rm := domain.ReplicaMetrics{PodName: "p0", VariantName: "p", RequestRate: rate,
		AvgServiceTime: svc * (1 + 0.001*r.Float64())}
	got := a.notePrefillBusyShare(rm, domain.RolePrefill, busyTestKey, "", ileff, admission{})
	*clock = clock.Add(time.Minute)
	return got
}

// The held figure is a lower bound at the prompt length it was taken at.
// Ten minutes at 1000-token prompts read 10,000 tokens/s; then the prompts
// grow to 1500 tokens, each costing more per token, and fifteen minutes read
// 7500 tokens/s. The bound is the new length's, 5 req/s: the old held
// figure, read back at 1500 tokens, would say 6.7.
func TestPrefillBusyShareHeldFigureFollowsThePromptLength(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	clock := propStart
	a.now = func() time.Time { return clock }
	r := propRand(3)
	for range 10 {
		busyDirect(a, &clock, r, 5, 0.1, 1000)
	}
	var got prefillBoundReading
	for range 15 {
		got = busyDirect(a, &clock, r, 3, 0.2, 1500)
	}
	if got.rate > 5*1.01 {
		t.Errorf("after the prompts grew to 1500 tokens the bound read %.2f req/s, want at most the new length's 5 (%.2f is the old held figure)",
			got.rate, 10000.0/1500)
	}
	// A move within the tolerance keeps the held figure.
	for range 3 {
		got = busyDirect(a, &clock, r, 3, 0.2, 1600)
	}
	a.mu.Lock()
	h := a.prefillBoundHeld[prefillBusyKey(busyTestKey)]
	a.mu.Unlock()
	if !(h.tokens > 0) || h.prompt != 1500 {
		t.Errorf("held figure %.0f tokens/s at prompt %.0f, want one kept at 1500 across a move to 1600", h.tokens, h.prompt)
	}
}

// A prompt length that is not a finite positive figure records nothing and
// prices nothing.
func TestPrefillBusyShareIgnoresAnUnreadPromptLength(t *testing.T) {
	for _, ileff := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		a := NewSaturationAnalyzer(capacity.NewStore())
		clock := propStart
		a.now = func() time.Time { return clock }
		got := busyDirect(a, &clock, propRand(1), 5, 0.1, ileff)
		a.mu.Lock()
		windows := len(a.saturatedThroughput)
		a.mu.Unlock()
		if got.rate != 0 || windows != 0 {
			t.Errorf("prompt length %v: bound %v, %d windows, want none", ileff, got.rate, windows)
		}
	}
}

// A restart under load keeps the held figure: ten quiet minutes, twenty
// busy ones whose readings are under the quiet figure, a snapshot through
// its JSON, and the restored analyzer prices from the quiet figure, not
// from the mean of the busy readings.
func TestPrefillBusyShareHeldFigureSurvivesARestart(t *testing.T) {
	const mu, prefill, prompt = 10.0, 2, 1000.0
	r := propRand(7)
	// The engine arguments on record, which a restore attributes a variant's
	// learned state by: with none, nothing is restored.
	withParams := func(rig *propRig) *propRig {
		for _, v := range []string{"p", "v"} {
			rig.a.capacityStore.Update("test-ns", "test-model", v, capacity.Record{EngineParams: mtParams(256, "vllm:v1")}, rig.clock)
		}
		return rig
	}
	rig := withParams(newPropRig())
	for range 4 * 10 {
		if _, err := rig.analyze(belowTheKneeCycle(r, prefill, 6, 1/mu), belowTheKneeStates(prefill), 6, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	for range 4 * 20 {
		if _, err := rig.analyze(belowTheKneeCycle(r, prefill, 16, 2/mu), belowTheKneeStates(prefill), 16, propCycle); err != nil {
			t.Fatal(err)
		}
	}
	before := rig.busyBoundTokens()
	if before < 0.97*mu*prompt {
		t.Fatalf("precondition: the bound read %.0f tokens/s before the restart, want about %.0f", before, mu*prompt)
	}
	raw, err := json.Marshal(rig.a.ExportLearnedState())
	if err != nil {
		t.Fatal(err)
	}
	var state LearnedState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	next := newPropRig()
	next.clock = rig.clock
	withParams(next)
	if rep := next.a.RestoreLearnedState(state); rep.Unattributable > 0 {
		t.Fatalf("precondition: %d entries of the snapshot could not be attributed to a variant", rep.Unattributable)
	}
	if _, err := next.analyze(belowTheKneeCycle(r, prefill, 16, 2/mu), belowTheKneeStates(prefill), 16, propCycle); err != nil {
		t.Fatal(err)
	}
	if got := next.busyBoundTokens(); got < 0.97*mu*prompt {
		t.Errorf("after a restart under load the bound read %.0f tokens/s, want the quiet %.0f kept (the busy readings are %.0f)",
			got, before, 8*prompt)
	}
}

// Prefill of two variants, one with a rate of its own and one priced on its
// busy-share bound: the floor prices prefill on the own rate alone, and does
// not call it a lower bound.
func TestPrefillBusyShareYieldsToASiblingsOwnRate(t *testing.T) {
	const k1 = 900_000.0
	vcs := []domain.VariantCapacity{
		{VariantName: "pa", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: k1},
		{VariantName: "pb", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: k1},
		{VariantName: "d", Role: domain.RoleDecode, ReplicaCount: 2, PerReplicaCapacity: k1},
	}
	rcs := []capacity.ReplicaCapacity{
		{VariantName: "pa", PodName: "pa0", SaturatedThroughput: 8, SaturatedThroughputSamples: 2},
		{VariantName: "pb", PodName: "pb0", SaturatedThroughput: 30, SaturatedThroughputSamples: 3,
			SaturatedThroughputLowerBound: true},
		{VariantName: "d", PodName: "d0", SaturatedThroughput: 6, SaturatedThroughputSamples: 2},
	}
	tf := floor.Estimate(12, rcs, vcs, nil, floor.BacklogDrainSeconds, floorTarget, false, 0, nil)
	term, ok := tf.Terms[domain.RolePrefill]
	if !ok || term.Mu != 8 || term.LowerBound {
		t.Errorf("prefill priced at mu %v (lower bound %v, present %v), want the own rate 8 alone", term.Mu, term.LowerBound, ok)
	}
}
