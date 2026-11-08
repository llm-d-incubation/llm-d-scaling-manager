package whatif

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

func bucket(u, r float64) domain.LengthBucket { return domain.LengthBucket{UpperBound: u, Rate: r} }

// rowsSpread is the spread of rows' pooled histograms at mean.
func rowsSpread(rows []domain.ReplicaMetrics, mean float64) PromptSpread {
	return ReadPromptSpread(PoolComputedBuckets(nil, rows), mean)
}

func TestReadPromptSpread(t *testing.T) {
	inf := math.Inf(1)
	withBuckets := func(sets ...[]domain.LengthBucket) []domain.ReplicaMetrics {
		rows := make([]domain.ReplicaMetrics, len(sets))
		for i, b := range sets {
			rows[i] = domain.ReplicaMetrics{Ready: true, ComputedTokensBuckets: b}
		}
		return rows
	}
	// 2000-token prompts of which half hit 1800 tokens: I_eff 2000 or 200, a
	// cv of 0.82, where the raw prompts have none.
	got := rowsSpread(withBuckets([]domain.LengthBucket{bucket(200, 1), bucket(2000, 2), bucket(inf, 2)}), 1100)
	assert.False(t, got.Assumed)
	assert.False(t, got.Clamped)
	assert.GreaterOrEqual(t, got.CV, 0.818)

	// A bucketed sample reads at or above the true cv.
	r := rand.New(rand.NewPCG(1, 2))
	bounds := []float64{100, 200, 500, 1000, 2000, 5000, 10000, 20000, 50000, 100000}
	for range 200 {
		xs := make([]float64, 0, 400)
		for range 400 {
			xs = append(xs, 100+r.Float64()*r.Float64()*40000)
		}
		var sum, sq float64
		b := make([]domain.LengthBucket, 0, len(bounds)+1)
		for _, x := range xs {
			sum += x
			sq += x * x
		}
		for _, u := range bounds {
			n := 0.0
			for _, x := range xs {
				if x <= u {
					n++
				}
			}
			b = append(b, bucket(u, n))
		}
		b = append(b, bucket(inf, float64(len(xs))))
		n := float64(len(xs))
		mean := sum / n
		truth := math.Sqrt(sq/n/(mean*mean) - 1)
		assert.GreaterOrEqual(t, rowsSpread(withBuckets(b), mean).CV, truth*(1-1e-12))
	}

	// Pooled over pods: each pod constant, the pool spread.
	a := []domain.LengthBucket{bucket(1000, 1), bucket(10000, 1), bucket(inf, 1)}
	b := []domain.LengthBucket{bucket(1000, 0), bucket(10000, 1), bucket(inf, 1)}
	pooled := rowsSpread(withBuckets(a, b), 5500)
	assert.InDelta(t, math.Sqrt((1e6+1e8)/2/(5500.0*5500)-1), pooled.CV, 1e-9)

	// A not-Ready pod's histogram is not read.
	rows := withBuckets(a, b)
	rows[1].Ready = false
	assert.InDelta(t, 0, rowsSpread(rows, 1000).CV, 1e-9)

	// Mass past the last bound has no upper bound.
	clamped := rowsSpread(withBuckets([]domain.LengthBucket{bucket(1000, 9), bucket(inf, 10)}), 800)
	assert.Equal(t, PromptSpread{CV: 3, Clamped: true}, clamped)
	// A cv above 3.
	over := rowsSpread(withBuckets([]domain.LengthBucket{bucket(100, 99), bucket(100000, 100), bucket(inf, 100)}), 1000)
	assert.Equal(t, PromptSpread{CV: 3, Clamped: true}, over)

	// Constant 3000-token prompts read about 1.3 in the 2000-5000 bucket.
	coarse := rowsSpread(withBuckets([]domain.LengthBucket{bucket(2000, 0), bucket(5000, 1), bucket(inf, 1)}), 3000)
	assert.InDelta(t, 1.33, coarse.CV, 0.01)
}

// A mean above what the buckets allow proves two populations, so the reading
// is refused, not read as no spread. A two-pod variant in a rollout: one pod
// carries the histogram, its prompts all at or under 1000, and the pooled mean
// is 5000. second / mean^2 is 0.04, under the 1 a single population cannot go
// below.
func TestReadPromptSpread_AMeanTheBucketsCannotHold(t *testing.T) {
	inf := math.Inf(1)
	assumed := PromptSpread{CV: AssumedPromptCV, Assumed: true}
	onePod := []domain.ReplicaMetrics{
		{Ready: true, ComputedTokensBuckets: []domain.LengthBucket{bucket(500, 0), bucket(1000, 2), bucket(inf, 2)}},
		{Ready: true},
	}
	assert.Equal(t, assumed, rowsSpread(onePod, 5000))

	// Just above 0 is the same fault. 90% at or under 100 and 10% at or under
	// 10000: the second moment alone passes a mean of 3100, at 1.04 mean^2, and
	// would read cv 0.2. But the upper-bound mean is 1090.
	wide := map[float64]float64{100: 90, 10000: 100, inf: 100}
	assert.Equal(t, assumed, ReadPromptSpread(wide, 3100))
	assert.False(t, ReadPromptSpread(wide, 1090).Assumed, "a mean on the bound is a reading")

	// The bound itself is a reading: constant prompts on a bucket's bound.
	assert.Equal(t, PromptSpread{CV: 0}, ReadPromptSpread(map[float64]float64{1000: 3, inf: 3}, 1000))
	assert.Equal(t, assumed, ReadPromptSpread(map[float64]float64{1000: 3, inf: 3}, 1000.1))
	// A low mean reads high, the safe side, and stays a reading.
	assert.False(t, ReadPromptSpread(map[float64]float64{1000: 3, inf: 3}, 10).Assumed)
}

// Past the check cv is at least the buckets' own spread about their upper-bound
// mean, so an inconsistent mean cannot land it near 0 unless they have none.
func TestReadPromptSpread_TheFloorPastTheCheck(t *testing.T) {
	pooled := map[float64]float64{100: 1, 10000: 2, math.Inf(1): 2}
	const upper = (100.0 + 10000) / 2
	floor := math.Sqrt((100.0*100+10000.0*10000)/2/(upper*upper) - 1)
	for _, mean := range []float64{upper, upper * 0.9, upper / 2} {
		assert.GreaterOrEqual(t, ReadPromptSpread(pooled, mean).CV, floor*(1-1e-12), "mean %v", mean)
	}
}

// The reviewed worked example, at the default 15 s cycle over a one-minute
// rate: 98% of prompts at 1800 tokens, 2% at 20000, 40 requests a minute, true
// cv 1.2. One cycle's histogram misses the long prompts in 45% of minutes and
// reads 0.48 with no flag. The window misses them about 1% of the time at 15 s
// a cycle, when it spans under 6 minutes, and at 60 s a cycle not at all. A
// restart has no such span: at the minimum the window has counted 200 requests,
// and the long prompts are in them all but about 1.8% of the time.
func TestPromptWindow_ARareLongPrompt(t *testing.T) {
	const slices15s, perSlice = 4000, 10
	r := rand.New(rand.NewPCG(7, 11))
	long := make([]int, slices15s)
	for i := range long {
		for range perSlice {
			if r.Float64() < 0.02 {
				long[i]++
			}
		}
	}
	// row is the one-minute rates at slice i.
	at := func(i int) time.Time { return time.Unix(int64(15*i), 0) }
	row := func(i int) []domain.ReplicaMetrics {
		n, l := 0.0, 0.0
		for j := i - 3; j <= i; j++ {
			n += perSlice
			l += float64(long[j])
		}
		rate := func(x float64) float64 { return x / 60 }
		return []domain.ReplicaMetrics{{Ready: true,
			PrefillComputedTokenRate: rate((n-l)*1800 + l*20000), PrefillComputedTokenRateKnown: true,
			RequestRate: rate(n), RequestRateKnown: true,
			ComputedTokensBuckets: []domain.LengthBucket{
				bucket(2000, rate(n-l)), bucket(20000, rate(n)), bucket(math.Inf(1), rate(n))}}}
	}
	// missed is a read with no long prompt in it, the 0.48; low is under 1.
	const missed, low = 0.5, 1.0
	for _, tc := range []struct {
		name      string
		stride    int
		maxMissed float64
		maxLow    float64
	}{
		{"15 s cycle", 1, 0.02, 0.15},
		{"60 s cycle", 4, 0, 0.02},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var window PromptWindow
			oneMissed, windowMissed, windowLow, cycles := 0, 0, 0, 0
			for i, n := 3, 0; i < slices15s; i, n = i+tc.stride, n+1 {
				rows := row(i)
				window.Note(rows, i, at(i))
				if n < PromptWindowCycles {
					continue
				}
				mean := rows[0].PrefillComputedTokenRate / rows[0].RequestRate
				cycles++
				if ReadPromptSpread(PoolComputedBuckets(nil, rows), mean).CV < missed {
					oneMissed++
				}
				cv := window.Spread().CV
				if cv < missed {
					windowMissed++
				}
				if cv < low {
					windowLow++
				}
			}
			of := func(n int) float64 { return float64(n) / float64(cycles) }
			assert.InDelta(t, 0.45, of(oneMissed), 0.05, "the one-cycle read this test exists for")
			assert.LessOrEqual(t, of(windowMissed), tc.maxMissed)
			assert.LessOrEqual(t, of(windowLow), tc.maxLow)
			t.Logf("%d cycles: one cycle misses %.3f; the window misses %.4f, reads under %.1f %.4f",
				cycles, of(oneMissed), of(windowMissed), low, of(windowLow))
		})
	}

	// A restart at every 15 s slice: the first reading a fresh window gives.
	for _, tc := range []struct {
		name       string
		stride     int
		firstCycle int
	}{
		{"restart, 15 s cycle", 1, 17},
		{"restart, 60 s cycle", 4, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starts, firstMissed := 0, 0
			for start := 3; start+tc.stride*PromptWindowCycles < slices15s; start++ {
				var w PromptWindow
				for n := 1; ; n++ {
					i := start + (n-1)*tc.stride
					w.Note(row(i), i, at(i))
					if n < tc.firstCycle {
						require.True(t, w.Spread().Assumed, "cycle %d of a restart at %d", n, start)
						continue
					}
					s := w.Spread()
					require.False(t, s.Assumed, "cycle %d", n)
					if s.CV < missed {
						firstMissed++
					}
					break
				}
				starts++
			}
			got := float64(firstMissed) / float64(starts)
			assert.Less(t, got, 0.04)
			t.Logf("%d restarts, first read at cycle %d: misses the long prompts %.4f", starts, tc.firstCycle, got)
		})
	}
}

// Each minimum holds alone: traffic past the count over too few cycles, and
// cycles past the count of too few requests. The count takes the time since
// the last cycle, not the rate's whole minute, so overlapping samples count
// once, and a gap past a minute counts a minute: the rate says nothing of the
// rest.
func TestPromptWindow_TheMinimums(t *testing.T) {
	inf := math.Inf(1)
	row := func(rate float64) []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{{Ready: true, PrefillComputedTokenRate: rate * 1000, PrefillComputedTokenRateKnown: true,
			RequestRate: rate, RequestRateKnown: true,
			ComputedTokensBuckets: []domain.LengthBucket{bucket(2000, rate), bucket(inf, rate)}}}
	}
	at := func(s int) time.Time { return time.Unix(int64(s), 0) }
	reads := func(rate float64, cycles, gap int) bool {
		var w PromptWindow
		for i := range cycles {
			w.Note(row(rate), i, at(i*gap))
		}
		return !w.Spread().Assumed
	}
	assert.False(t, reads(1000, 4, 60), "the count passed, too few cycles")
	assert.True(t, reads(1000, 5, 60))
	// 1 a second: 60 from the first cycle and 15 from each after it is 195 at
	// cycle 10, under, and 210 at cycle 11. A count of each whole minute, the
	// overlap counted again, would read at cycle 4.
	assert.False(t, reads(1, 10, 15), "195 requests")
	assert.True(t, reads(1, 11, 15), "210 requests")
	// 42 a minute at a cycle an hour apart: 42 a cycle, not 2520.
	assert.False(t, reads(0.7, 4, 3600), "168 requests")
	assert.True(t, reads(0.7, 5, 3600), "210 requests")
	assert.False(t, reads(0.3, 5, 3600), "90 requests, enough cycles")
	// A clock that steps back adds nothing, and takes nothing away.
	assert.True(t, reads(5, PromptWindowMinCycles, -15), "300 requests from the first cycle")
}

// The window keeps the latest cycles with traffic and no others, reads a nil
// window as nothing, and reads cv at the mean of the cycles that carried a
// histogram, not of those before it arrived.
func TestPromptWindow(t *testing.T) {
	inf := math.Inf(1)
	row := func(computed, rate float64, b ...domain.LengthBucket) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{Ready: true, PrefillComputedTokenRate: computed, PrefillComputedTokenRateKnown: true,
			RequestRate: rate, RequestRateKnown: true, ComputedTokensBuckets: b}
	}
	var none *PromptWindow
	assert.True(t, none.Spread().Assumed)
	assert.Nil(t, none.Clone())

	at := func(i int) time.Time { return time.Unix(int64(60*i), 0) }
	var w PromptWindow
	w.Note([]domain.ReplicaMetrics{row(0, 0)}, 1, at(1))
	_, _, _, n := w.Prompt()
	assert.Zero(t, n, "no traffic is no cycle")

	for i := range PromptWindowCycles + 3 {
		w.Note([]domain.ReplicaMetrics{row(100, 1)}, i+2, at(i+2))
	}
	tokens, from, to, n := w.Prompt()
	assert.Equal(t, []int{5, 24, PromptWindowCycles}, []int{from, to, n})
	assert.InDelta(t, 100, tokens, 1e-9)

	// Cycles at 100 tokens with no histogram, then cycles at 900 with one: the
	// spread reads at 900, where the window's mean would be under it, and the
	// cycles with no histogram count toward no minimum.
	for i := range PromptWindowMinCycles {
		w.Note([]domain.ReplicaMetrics{row(900, 1, bucket(1000, 1), bucket(inf, 1))}, 30+i, at(30+i))
	}
	assert.InDelta(t, math.Sqrt(1e6/(900.0*900)-1), w.Spread().CV, 1e-9)

	copied := w.Clone()
	w.Note([]domain.ReplicaMetrics{row(9000, 1)}, 40, at(40))
	_, _, last, _ := copied.Prompt()
	assert.Equal(t, 34, last, "a clone is not reached by a later Note")

	// A row not serving, or with one rate unread, does not count, nor does its
	// histogram: the mean and the pool hold the same rows.
	skipped := row(100000, 1, bucket(1000, 5), bucket(inf, 5))
	skipped.RequestRateKnown = false
	var mixed PromptWindow
	mixed.Note([]domain.ReplicaMetrics{row(500, 1, bucket(1000, 1), bucket(inf, 1)), skipped}, 1, at(1))
	assert.Equal(t, map[float64]float64{1000: 1, inf: 1}, mixed.samples[0].buckets)
}

// With no histogram, cv 2.
func TestReadPromptSpread_Assumed(t *testing.T) {
	assumed := PromptSpread{CV: 2, Assumed: true}
	assert.Equal(t, assumed, rowsSpread(nil, 1000))
	assert.Equal(t, assumed, rowsSpread([]domain.ReplicaMetrics{{Ready: true}}, 1000))
	noTotal := []domain.ReplicaMetrics{{Ready: true, ComputedTokensBuckets: []domain.LengthBucket{bucket(1000, 1)}}}
	assert.Equal(t, assumed, rowsSpread(noTotal, 1000), "no +Inf bucket: no total to read a share of")
	zero := []domain.ReplicaMetrics{{Ready: true, ComputedTokensBuckets: []domain.LengthBucket{bucket(math.Inf(1), 0)}}}
	assert.Equal(t, assumed, rowsSpread(zero, 1000), "no request in the window")
}

// A pooled histogram holds what it was given: nil starts a new map, one
// passed in is added to, and a row with none, a NaN bound or a rate that is no
// reading adds nothing, so a pool of such rows reads as no histogram.
func TestPoolComputedBuckets(t *testing.T) {
	inf := math.Inf(1)
	serving := func(b ...domain.LengthBucket) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{Ready: true, ComputedTokensBuckets: b}
	}
	pooled := PoolComputedBuckets(nil, []domain.ReplicaMetrics{serving(bucket(100, 1), bucket(inf, 2))})
	assert.Equal(t, map[float64]float64{100: 1, inf: 2}, pooled)
	pooled = PoolComputedBuckets(pooled, []domain.ReplicaMetrics{serving(bucket(inf, 3)), {Ready: true}})
	assert.Equal(t, map[float64]float64{100: 1, inf: 5}, pooled)

	junk := PoolComputedBuckets(nil, []domain.ReplicaMetrics{serving(
		bucket(math.NaN(), 1), bucket(100, math.NaN()), bucket(200, inf), bucket(300, -1))})
	assert.Empty(t, junk)
	assert.Equal(t, PromptSpread{CV: AssumedPromptCV, Assumed: true}, ReadPromptSpread(junk, 1000))
}
