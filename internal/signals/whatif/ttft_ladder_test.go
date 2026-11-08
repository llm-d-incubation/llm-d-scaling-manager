package whatif

import (
	"fmt"
	"math"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The staircase run's frozen ladder (docs/proposals/slo-capacity-planner-ttft.md,
// "As built: the decode term"): P1's estimate and the decode line as the run's
// warm-up recording left them, on two pods a role, flow control off. The
// recording is not committed here, so its figures are literals.
//
// ladderTerm is what a window learned over that warm-up, past its latch: D as
// DecodeTermWindow published it at the last cycle. The warm-up recording is not
// committed (the four phases are, and TestGoldens_TheDecodeTermIsLearned holds a
// window on them), so its figures are literals here. R is the warm-up's fit of
// the router's _sum and _count, which no learner recording holds: it was read
// from the run's Prometheus. Both come from the warm-up alone; no staircase
// level went into them.
var ladderTerm = DecodeTerm{
	Engine:  PromptLine{Base: -0.0021163385366780585, PerToken: 4.089790884615122e-06},
	PerStep: 6.7792790425064675,
	Hop:     PromptLine{Base: 0.00461, PerToken: 0.511e-6},
}

// ladderBucket is one shape of the staircase, with the Ca^2 it was sent at and
// the recorded transfer the old sum read.
type ladderBucket struct {
	prompt, output, kvPerRequest, ca2, transfer float64
}

var ladderBuckets = map[string]ladderBucket{
	"A": {prompt: 1000, output: 1000, kvPerRequest: 1500, transfer: 0.0080 / 1000},
	"B": {prompt: 8000, output: 200, kvPerRequest: 8100, transfer: 0.0314 / 8500},
	"C": {prompt: 8000, output: 200, kvPerRequest: 8100, ca2: 4, transfer: 0.0314 / 8500},
}

// ladderFleet is the fleet of one bucket on the old sum, which the term
// replaces.
func ladderFleet(b ladderBucket) TTFTFleet {
	return TTFTFleet{
		Prefill: Prefill{
			VP: 288287.9, FixedLatency: 0.01205735519529711, VPPromptTokens: 8001.162709910401,
			CurveSlope: 4.61284350108191e-07, CurveCurvature: 2.2578832669308503e-10, VPMargin: DefaultVPMargin,
			Imbalance: 1.192798939189922, Replicas: 2, PromptTokens: b.prompt,
			PromptCV: 0.2 / math.Sqrt(12), ArrivalCV2: b.ca2, TransferSecondsPerToken: b.transfer,
		},
		Decode: &Decode{
			Line: itl.Model{A: 0.03002838165320376, B: 0.0020791987727279727}, Band: DefaultBand(), Tier: "pinned-B",
			CapacityTokens: 1162240, KVPerRequest: b.kvPerRequest, OutputTokens: b.output, ReplicaCap: 256, Replicas: 2,
		},
		FlowControl:     FlowControl{State: FlowControlOff},
		LegacyDecodeSum: true,
	}
}

// ladderBurstyNow is bucket C's percentiles at this tail, rate and q to old and
// new sum. The frozen run priced Ca^2 4 at Kingman's 1/theta, which the stretched
// Poisson tail replaced (prefillPlan.engineWait); ladderFigures keeps what was
// frozen, the staircase's record. Means do not move: they read Kingman's theta.
var ladderBurstyNow = map[[2]float64][2]float64{
	{17.581, 0.9}:  {0.19719, 0.22269},
	{17.581, 0.95}: {0.23378, 0.25927},
	{24.174, 0.9}:  {0.23402, 0.26574},
	{24.174, 0.95}: {0.28170, 0.31341},
	{30.767, 0.9}:  {0.28699, 0.32975},
	{30.767, 0.95}: {0.35057, 0.39333},
	{33.404, 0.9}:  {0.32913, 0.37882},
	{33.404, 0.95}: {0.40512, 0.45481},
	{36.042, 0.9}:  {0.38758, 0.44624},
	{36.042, 0.95}: {0.48051, 0.53917},
	{38.679, 0.9}:  {0.44461, 0.51743},
	{38.679, 0.95}: {0.55455, 0.62737},
	{41.316, 0.9}:  {0.63295, 0.73149},
	{41.316, 0.95}: {1.05750, 1.15603},
}

// The ladder before and after the term. Before is the frozen run of main at
// 59772fce2, to its 5 decimals: the old sum must reproduce it, bucket C's
// percentiles at the tail that replaced the frozen one. After pins the term.
// Every figure rises, the floors with them.
func TestLadder_BeforeAndAfterTheTerm(t *testing.T) {
	const tol = 6e-6 // half the last decimal printed, and a little
	for _, l := range ladderFigures {
		t.Run(fmt.Sprintf("%s at %g q %g", l.bucket, l.rate, l.q), func(t *testing.T) {
			fl := ladderFleet(ladderBuckets[l.bucket])
			oldSeconds, newSeconds := l.oldSeconds, l.newSeconds
			if now, ok := ladderBurstyNow[[2]float64{l.rate, l.q}]; ok && l.bucket == "C" {
				oldSeconds, newSeconds = now[0], now[1]
			}
			old := mustTTFT(t, ttftAt(t, fl, l.rate, l.q))
			if math.Abs(old.Seconds-oldSeconds) > tol || math.Abs(old.Floor-l.oldFloor) > tol {
				t.Errorf("before: %.5f floor %.5f, frozen %.5f floor %.5f", old.Seconds, old.Floor, oldSeconds, l.oldFloor)
			}
			fl.LegacyDecodeSum = false
			term := ladderTerm
			fl.DecodeTerm = &term
			now := mustTTFT(t, ttftAt(t, fl, l.rate, l.q))
			if math.Abs(now.Seconds-newSeconds) > tol || math.Abs(now.Floor-l.newFloor) > tol {
				t.Errorf("after: %.5f floor %.5f, pinned %.5f floor %.5f", now.Seconds, now.Floor, newSeconds, l.newFloor)
			}
			if !(now.Seconds > old.Seconds) || !(now.Floor > old.Floor) {
				t.Errorf("fell: %.5f -> %.5f, floor %.5f -> %.5f", old.Seconds, now.Seconds, old.Floor, now.Floor)
			}
		})
	}
}

// On the staircase's own fleet a target between the two floors refuses, where
// the old sum answered: bucket A's p90 floor is 31 ms by the old sum and 40 ms
// by the term.
func TestLadder_ATargetUnderTheCorrectedFloorRefuses(t *testing.T) {
	fl := ladderFleet(ladderBuckets["A"])
	target := TTFTTarget{Seconds: 0.035, Percentile: 0.9}
	if a := mustAnswer(t, rateForTTFT(t, fl, target)); !(a.Value > 0) {
		t.Fatalf("the old sum gave %g", a.Value)
	}
	fl.LegacyDecodeSum = false
	term := ladderTerm
	fl.DecodeTerm = &term
	u, ok := rateForTTFT(t, fl, target).(Unanswerable)
	if !ok || u.Reason != TargetBelowFloor {
		t.Errorf("want TargetBelowFloor, got %#v", rateForTTFT(t, fl, target))
	}
}

// ladderFigures is every level of the staircase, at the mean, p90 and p95, in
// seconds.
var ladderFigures = []struct {
	bucket                                     string
	rate, q                                    float64
	oldSeconds, oldFloor, newSeconds, newFloor float64
}{
	{"A", 17.066, 0, 0.03017, 0.03011, 0.04374, 0.03672},
	{"A", 17.066, 0.9, 0.03112, 0.03097, 0.04439, 0.03732},
	{"A", 17.066, 0.95, 0.03157, 0.03122, 0.04472, 0.03750},
	{"A", 23.466, 0, 0.03020, 0.03011, 0.04856, 0.03672},
	{"A", 23.466, 0.9, 0.03119, 0.03097, 0.04923, 0.03732},
	{"A", 23.466, 0.95, 0.03189, 0.03122, 0.04990, 0.03750},
	{"A", 29.865, 0, 0.03022, 0.03011, 0.05619, 0.03672},
	{"A", 29.865, 0.9, 0.03128, 0.03097, 0.05691, 0.03732},
	{"A", 29.865, 0.95, 0.03237, 0.03122, 0.05842, 0.03750},
	{"B", 17.581, 0, 0.08166, 0.07594, 0.10712, 0.09320},
	{"B", 17.581, 0.9, 0.10672, 0.08022, 0.13230, 0.09802},
	{"B", 17.581, 0.95, 0.11589, 0.08149, 0.14147, 0.09945},
	{"B", 24.174, 0, 0.08523, 0.07594, 0.11692, 0.09320},
	{"B", 24.174, 0.9, 0.11586, 0.08022, 0.14764, 0.09802},
	{"B", 24.174, 0.95, 0.12781, 0.08149, 0.15959, 0.09945},
	{"B", 30.767, 0, 0.09072, 0.07594, 0.13346, 0.09320},
	{"B", 30.767, 0.9, 0.12916, 0.08022, 0.17197, 0.09802},
	{"B", 30.767, 0.95, 0.14509, 0.08149, 0.18790, 0.09945},
	{"B", 33.404, 0, 0.09581, 0.07594, 0.14549, 0.09320},
	{"B", 33.404, 0.9, 0.14040, 0.08022, 0.19013, 0.09802},
	{"B", 33.404, 0.95, 0.15944, 0.08149, 0.20918, 0.09945},
	{"B", 36.042, 0, 0.10379, 0.07594, 0.16244, 0.09320},
	{"B", 36.042, 0.9, 0.15667, 0.08022, 0.21536, 0.09802},
	{"B", 36.042, 0.95, 0.17996, 0.08149, 0.23866, 0.09945},
	{"B", 38.679, 0, 0.11221, 0.07594, 0.18502, 0.09320},
	{"B", 38.679, 0.9, 0.17282, 0.08022, 0.24567, 0.09802},
	{"B", 38.679, 0.95, 0.20037, 0.08149, 0.27322, 0.09945},
	{"B", 41.316, 0, 0.15888, 0.07594, 0.25740, 0.09320},
	{"B", 41.316, 0.9, 0.22333, 0.08022, 0.32189, 0.09802},
	{"B", 41.316, 0.95, 0.34792, 0.08149, 0.44648, 0.09945},
	{"C", 17.581, 0, 0.09874, 0.07594, 0.12420, 0.09320},
	{"C", 17.581, 0.9, 0.15969, 0.08022, 0.18517, 0.09802},
	{"C", 17.581, 0.95, 0.21399, 0.08149, 0.23947, 0.09945},
	{"C", 24.174, 0, 0.11299, 0.07594, 0.14468, 0.09320},
	{"C", 24.174, 0.9, 0.20438, 0.08022, 0.23609, 0.09802},
	{"C", 24.174, 0.95, 0.26856, 0.08149, 0.30026, 0.09945},
	{"C", 30.767, 0, 0.13451, 0.07594, 0.17726, 0.09320},
	{"C", 30.767, 0.9, 0.26260, 0.08022, 0.30536, 0.09802},
	{"C", 30.767, 0.95, 0.34179, 0.08149, 0.38454, 0.09945},
	{"C", 33.404, 0, 0.15199, 0.07594, 0.20167, 0.09320},
	{"C", 33.404, 0.9, 0.30634, 0.08022, 0.35603, 0.09802},
	{"C", 33.404, 0.95, 0.39785, 0.08149, 0.44754, 0.09945},
	{"C", 36.042, 0, 0.17722, 0.07594, 0.23587, 0.09320},
	{"C", 36.042, 0.9, 0.36624, 0.08022, 0.42490, 0.09802},
	{"C", 36.042, 0.95, 0.47462, 0.08149, 0.53328, 0.09945},
	{"C", 38.679, 0, 0.20273, 0.07594, 0.27554, 0.09320},
	{"C", 38.679, 0.9, 0.42473, 0.08022, 0.49755, 0.09802},
	{"C", 38.679, 0.95, 0.54995, 0.08149, 0.62277, 0.09945},
	{"C", 41.316, 0, 0.27142, 0.07594, 0.36994, 0.09320},
	{"C", 41.316, 0.9, 0.62747, 0.08022, 0.72600, 0.09802},
	{"C", 41.316, 0.95, 1.08740, 0.08149, 1.18593, 0.09945},
}
