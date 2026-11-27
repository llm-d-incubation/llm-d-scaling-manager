package slo

import (
	"math"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The v2 learners' cost on a large fleet (docs/proposals/
// predicted-latency-in-controller.md, "Cost per cycle"): a refit of one
// model on a full window, the pricing of one cycle with the prefill ask, and
// the memory of one model's window. The window is the largest a learner
// keeps (ModelWindow at a 15 s cycle: 3,120 cycles) of a P/D model with
// eight prefill ranks, four decode replicas and 20-bucket histograms, under a
// load that rises and falls through the day, so that every fit runs to its
// end. Run with
//
//	go test ./internal/engines/analyzers/slo/ -run '^$' -bench 'ModelLearners' -benchtime 5x
//
// and TestModelLearners_WindowMemoryIsBounded, which also runs in make test.

// costInterval is the controller's cycle the cost is measured at.
const costInterval = 15 * time.Second

// costEdges is a 20-bucket histogram's upper edges from lo, doubling every
// two buckets, and +Inf.
func costEdges(lo float64) []float64 {
	out := make([]float64, 0, 21)
	for i := range 20 {
		out = append(out, lo*math.Pow(2, float64(i)/2))
	}
	return append(out, math.Inf(1))
}

// costHist is a cumulative histogram of rate requests a second, lognormal
// around mean with log-spread s, on edges.
func costHist(edges []float64, rate, mean, s float64) map[float64]float64 {
	out := make(map[float64]float64, len(edges))
	for _, e := range edges {
		if math.IsInf(e, 1) {
			out[e] = rate
			continue
		}
		z := (math.Log(e) - math.Log(mean)) / s
		out[e] = rate * 0.5 * math.Erfc(-z/math.Sqrt2)
	}
	return out
}

// costBuckets is costHist as latency buckets.
func costBuckets(edges []float64, rate, mean, s float64) []domain.LatencyBucket {
	h := costHist(edges, rate, mean, s)
	out := make([]domain.LatencyBucket, 0, len(edges))
	for _, e := range edges {
		out = append(out, domain.LatencyBucket{UpperBound: e, Rate: h[e]})
	}
	return out
}

// costCycles is n cycles of the synthetic model, costInterval apart.
func costCycles(n int) []whatif.FitCycle {
	t0 := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	tokens, secs := costEdges(64), costEdges(0.005)
	out := make([]whatif.FitCycle, n)
	for i := range out {
		at := t0.Add(time.Duration(i) * costInterval)
		phase := 2 * math.Pi * float64(i) / float64(n)
		rate := 6 + 4*math.Sin(phase)
		prompt := 1500 + 500*math.Sin(3*phase)
		c := whatif.FitCycle{At: at, Rate: rate, RateKnown: true, EngineRate: rate, Latency: true}
		c.Mixture = whatif.MixtureCycle{At: at, Computed: costHist(tokens, rate, prompt*0.6, 0.8),
			Prompt: costHist(tokens, rate, prompt, 0.8), Queue: costHist(secs, rate, 0.02*rate, 1)}
		for r := range 8 {
			share := (1 + 0.1*float64(r%3)) / 8.4
			rr := rate * share
			pf := 0.02 + prompt*0.6/40000
			rho := math.Min(rr*pf, 0.9)
			q := pf * rho / (1 - rho)
			c.Mixture.Ranks = append(c.Mixture.Ranks, whatif.MixtureRankReading{Key: "p-" + strconv.Itoa(r),
				MixtureRank: whatif.MixtureRank{Rate: rr, PrefillTime: pf, QueueTime: q, TTFT: pf + q, Computed: prompt * 0.6, Prompt: prompt}})
		}
		ttft := 0.06 + 0.01*rate
		c.Router = &domain.RouterLatency{WindowSeconds: 300, TTFTBuckets: costBuckets(secs, rate, ttft, 0.6),
			TTFTSumRate: rate * ttft, TTFTCountRate: rate, TPOTBuckets: costBuckets(secs, rate, 0.03, 0.3),
			TPOTSumRate: rate * 0.03, TPOTCountRate: rate}
		c.Decode = whatif.DecodeCycle{At: at, Output: costHist(tokens, rate, 300, 1), MeanRunning: 4 * rate, MostRunning: 5 * rate,
			RanksReported: 4}
		for range 4 {
			c.Decode.Groups = append(c.Decode.Groups, whatif.DecodeGroup{Rate: rate / 4, TokenTime: 0.03})
		}
		out[i] = c
	}
	return out
}

// costWindow is the cycles of a full window.
func costWindow() []whatif.FitCycle { return costCycles(int(ModelWindow / costInterval)) }

// BenchmarkModelLearners_Refit is one model's refit on a full window: both
// fits and the default R_q, as refitModel runs them off the lock.
func BenchmarkModelLearners_Refit(b *testing.B) {
	cycles := costWindow()
	for b.Loop() {
		whatif.FitMixtureWindow(cycles, ModelStep, 8, modelHeldOutTolerance, modelMixtureSwitches)
		whatif.FitTPOTWindow(cycles, ModelStep, 256, 0, whatif.TPOTRenewalLognormal, 0)
		var windows []whatif.TPOTWindow
		for i := range cycles {
			if w, ok := whatif.TPOTWindowOf(cycles[i].Router); ok {
				windows = append(windows, w)
			}
		}
		for _, q := range []float64{0.9, 0.95} {
			_, _, _ = whatif.TPOTRatioFromWindows(windows, q)
		}
	}
}

// BenchmarkModelLearners_PriceCycle is one pricing of a model with a fit in
// use (at most one per ModelPriceEvery): the mean, p90, p95 and the
// objective's percentile from one set of draws, and the prefill ask searched
// outward from the serving pods.
func BenchmarkModelLearners_PriceCycle(b *testing.B) {
	a := New(nil, nil)
	tt := &ttftLearner{fit: pricedMixture(), prefill: 8}
	for b.Loop() {
		_ = a.priceModel(tt, 6, 8, 0.9, 0.2)
	}
}

// One model's window at its largest stays under 16 MiB: the ModelMaxWindows
// windows the analyzer keeps at once fit in the controller's 512 MiB limit
// beside the rest of its state. The figure is logged for the cost table.
func TestModelLearners_WindowMemoryIsBounded(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	cycles := costWindow()
	runtime.GC()
	runtime.ReadMemStats(&after)
	bytes := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(cycles)
	t.Logf("one model's window: %d cycles, %.1f MiB", len(cycles), float64(bytes)/(1<<20))
	if bytes > 16<<20 {
		t.Fatalf("one model's window holds %.1f MiB, over 16 MiB", float64(bytes)/(1<<20))
	}
}
