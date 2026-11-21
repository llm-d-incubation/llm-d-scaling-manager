package whatifapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// samplesAt is one cycle a rate, each read.
func samplesAt(rates ...float64) []SeriesSample {
	out := make([]SeriesSample, len(rates))
	for i, r := range rates {
		out[i] = SeriesSample{Rate: r, RateKnown: true}
	}
	return out
}

// A window that holds the start or the end of a load near the knee is a
// burst; a steady one is not, bursty arrivals included, and neither is a
// change of load far under the knee.
func TestBurstIn(t *testing.T) {
	const knee = 4.08
	for name, c := range map[string]struct {
		rates []float64
		want  bool
	}{
		"the warm-up's 09:15, a burst starting":  {[]float64{0.04, 4.02, 4.16, 4.28, 4.12}, true},
		"the warm-up's 09:25, a burst ending":    {[]float64{4.14, 4.14, 2.72, 0, 0.42}, true},
		"a steady step near the knee":            {[]float64{3.5, 3.7, 3.4, 3.9, 3.6}, false},
		"bursty arrivals near the knee (Ca^2 4)": {[]float64{1.6, 4.16, 3.2, 3.9, 2.8}, false},
		"a change of load far under the knee":    {[]float64{0.05, 2.0, 2.1, 2.2}, false},
		"one cycle":                              {[]float64{4.2}, false},
		"a peak just past the share of the knee": {[]float64{3.3, 0}, true},
	} {
		lo, hi, ok := burstIn(samplesAt(c.rates...), knee)
		assert.Equal(t, c.want, ok, name)
		if ok {
			assert.LessOrEqual(t, lo, BurstTroughShare*hi, name)
			assert.GreaterOrEqual(t, hi, BurstPeakKneeShare*knee, name)
		}
	}

	// A cycle with no rate is not read, and a knee that is not one admits nothing.
	s := samplesAt(4.2, 0)
	s[1].RateKnown = false
	_, _, ok := burstIn(s, knee)
	assert.False(t, ok, "the 0 was not read")
	_, _, ok = burstIn(samplesAt(4.2, 0), 0)
	assert.False(t, ok)
}
