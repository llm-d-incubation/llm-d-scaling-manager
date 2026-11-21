package whatif

import (
	"math"
	"math/rand/v2"
	"slices"
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
	// A cv above 3: the longest prompt is still bounded, 100000 over 1000.
	over := rowsSpread(withBuckets([]domain.LengthBucket{bucket(100, 99), bucket(100000, 100), bucket(inf, 100)}), 1000)
	assert.Equal(t, PromptSpread{CV: 3, MaxRatio: 100, Clamped: true}, over)

	// Constant 3000-token prompts read 0.47 in the 2000-5000 bucket: 2/3 of
	// them at 2000 and 1/3 at 5000 also have that mean, and that spread.
	coarse := rowsSpread(withBuckets([]domain.LengthBucket{bucket(2000, 0), bucket(5000, 1), bucket(inf, 1)}), 3000)
	assert.InDelta(t, math.Sqrt((7000.0*3000-2000*5000)/(3000.0*3000)-1), coarse.CV, 1e-9)
	assert.InDelta(t, 0.471, coarse.CV, 0.001)
}

// upperEdgeCV is the reading before the mean bounded it: every request at its
// bucket's upper bound.
func upperEdgeCV(pooled map[float64]float64, mean float64) float64 {
	bounds := make([]float64, 0, len(pooled))
	for b := range pooled {
		if !math.IsInf(b, 1) {
			bounds = append(bounds, b)
		}
	}
	slices.Sort(bounds)
	var second, below float64
	for _, u := range bounds {
		second += max(pooled[u]-below, 0) * u * u
		below = max(below, pooled[u])
	}
	return math.Sqrt(max(second/pooled[math.Inf(1)]/(mean*mean)-1, 0))
}

// The staircase's fixed 8k prompts (issue #266): every one in the (5000,
// 10000] bucket at a mean of 8000. Every request at the upper bound read cv
// 0.75 (sqrt(10000^2/8000^2 - 1)), which set SpreadBeyondProcessorSharing.
// With the mean held, the most spread the bucket can hold is 40% at 5000 and
// 60% at 10000: cv 0.31.
func TestReadPromptSpread_TheMeanBoundsTheSpread(t *testing.T) {
	pooled := map[float64]float64{2000: 0, 5000: 0, 10000: 100, math.Inf(1): 100}
	got := ReadPromptSpread(pooled, 8000)
	assert.False(t, got.Assumed)
	assert.False(t, got.Clamped)
	assert.InDelta(t, math.Sqrt((0.4*5000*5000+0.6*10000*10000)/(8000.0*8000)-1), got.CV, 1e-9)
	assert.InDelta(t, 0.306, got.CV, 0.001)
	assert.InDelta(t, 0.75, upperEdgeCV(pooled, 8000), 1e-9)
}

// A mean below every request at its bucket's lower bound is two populations
// as well; it reads every request at its upper bound, high, as before.
func TestReadPromptSpread_AMeanBelowTheLowerEdges(t *testing.T) {
	pooled := map[float64]float64{1000: 0, 2000: 3, math.Inf(1): 3}
	got := ReadPromptSpread(pooled, 900)
	assert.False(t, got.Assumed)
	assert.InDelta(t, upperEdgeCV(pooled, 900), got.CV, 1e-12)
	assert.InDelta(t, math.Sqrt(2000.0*2000/(900*900)-1), got.CV, 1e-12)

	// The step at the lower edges is the price of reading two populations
	// high: at 1000 every request is on its lower edge, cv 0; just under it
	// every request is on its upper edge.
	assert.InDelta(t, 0, ReadPromptSpread(pooled, 1000).CV, 1e-12)
	assert.InDelta(t, math.Sqrt(2000.0*2000/(999.0*999)-1), ReadPromptSpread(pooled, 999).CV, 1e-12)
}

// Over random prompts, narrow and wide, the reading is never below the true
// cv and never above every request at its upper bound.
func TestReadPromptSpread_BetweenTheTruthAndTheUpperEdges(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	bounds := []float64{128, 256, 512, 1000, 2000, 5000, 10000, 20000, 50000}
	tighter := 0
	for i := range 2000 {
		xs := make([]float64, 0, 300)
		center := 50 + r.Float64()*30000
		width := []float64{0.001, 0.05, 0.3, 1, 3}[i%5]
		for range 300 {
			x := center * (1 + width*(r.Float64()-0.5))
			if r.Float64() < 0.05 {
				x *= 1 + 4*r.Float64()
			}
			xs = append(xs, min(max(x, 1), 49999))
		}
		pooled := map[float64]float64{}
		var sum, sq float64
		for _, x := range xs {
			sum += x
			sq += x * x
			for _, u := range bounds {
				if x <= u {
					pooled[u]++
				}
			}
			pooled[math.Inf(1)]++
		}
		// Every bound is a bucket, an empty one too, as the collector reads it.
		for _, u := range bounds {
			pooled[u] += 0
		}
		n := float64(len(xs))
		mean := sum / n
		truth := math.Sqrt(max(sq/n/(mean*mean)-1, 0))
		got := ReadPromptSpread(pooled, mean)
		require.False(t, got.Assumed, "case %d", i)
		require.False(t, got.Clamped, "case %d", i)
		old := upperEdgeCV(pooled, mean)
		require.GreaterOrEqual(t, got.CV, truth*(1-1e-9)-1e-12, "case %d", i)
		require.LessOrEqual(t, got.CV, old*(1+1e-9)+1e-12, "case %d", i)
		if got.CV < old*0.9 {
			tighter++
		}
	}
	assert.Greater(t, tighter, 500, "the mean tightens the reading on narrow prompts")
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
	assert.Equal(t, PromptSpread{CV: 0, MaxRatio: 1}, ReadPromptSpread(map[float64]float64{1000: 3, inf: 3}, 1000))
	assert.Equal(t, assumed, ReadPromptSpread(map[float64]float64{1000: 3, inf: 3}, 1000.1))
	// A low mean reads high, the safe side, and stays a reading.
	assert.False(t, ReadPromptSpread(map[float64]float64{1000: 3, inf: 3}, 10).Assumed)
}

// The +Inf rate can read under the last bound's: each series' rate is
// extrapolated on its own. The reading is then taken over the requests the
// buckets hold, so it does not fall under the widest spread they allow: the
// 8k case 1% short on +Inf reads 0.306, as it does when the two agree.
func TestReadPromptSpread_AnInfRateUnderTheLastBound(t *testing.T) {
	short := map[float64]float64{5000: 0, 10000: 101, math.Inf(1): 100}
	assert.InDelta(t, math.Sqrt((0.4*5000*5000+0.6*10000*10000)/(8000.0*8000)-1), ReadPromptSpread(short, 8000).CV, 1e-9)
	one := map[float64]float64{1000: 0, 2000: 110, math.Inf(1): 100}
	assert.InDelta(t, math.Sqrt((3000.0*1500-1000*2000)/(1500.0*1500)-1), ReadPromptSpread(one, 1500).CV, 1e-9)
}

// The reviewed worked example, at the default 15 s cycle over a one-minute
// rate: 98% of prompts at 1800 tokens, 2% at 20000, 40 requests a minute, true
// cv 1.2. One cycle's histogram misses the long prompts in 45% of minutes and
// reads 0.33 with no flag. The window misses them about 1.5% of the time at 15 s
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
	// missed is a read with no long prompt in it, the 0.33; low is under 1.
	const missed, low = 0.5, 1.0
	for _, tc := range []struct {
		name      string
		stride    int
		maxMissed float64
		maxLow    float64
	}{
		// The window reads under 1 in 17.9% of its 15 s cycles, against
		// 11.1% with every request at its upper bound: its few long prompts
		// are the cv, and the upper bounds padded it. The case is seeded, so
		// 0.2 leaves 2.1 points of margin and cannot flake.
		{"15 s cycle", 1, 0.02, 0.2},
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
	assert.InDelta(t, math.Sqrt(1000.0*900/(900*900)-1), w.Spread().CV, 1e-9)

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
