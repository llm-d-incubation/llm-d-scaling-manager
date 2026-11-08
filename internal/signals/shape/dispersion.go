package shape

import (
	"math"
)

// How the spread of output lengths enters the KV a resident request holds
// (issue ev-shindin/llm-scaling-manager#276).
//
// A request that generates O tokens is resident for O decode steps and holds
// IL + a tokens at age a. Averaged over the time requests are resident, which
// is what a KV snapshot samples, a long request counts in proportion to how
// long it stays:
//
//	KVreq = ILeff + E[O^2] / (2 E[O]) = ILeff + D * E[O] / 2
//
// where D = E[O^2] / E[O]^2 = 1 + cv^2 is the dispersion of the output length.
// D is 1 only when every request generates the same number of tokens, which is
// what ILeff + OL/2 assumes. At IL = 2 OL it under-states KVreq by 6.7% for
// uniform lengths, 20% for exponential ones and 43% for a lognormal with cv
// 1.5, and over-counts the resident requests by as much.
//
// E[O^2] is read as a lower bound from the output-length histogram
// (domain.SquaredMeanLowerBound).

// Dispersion returns D = squaredMean / mean^2, the factor the spread of
// lengths multiplies the generated half of the KV per request by. It is at
// least 1, and 1 when either figure is not a positive finite number: no
// reading of the spread is read as no spread, which is the KV per request as
// it was priced before (ILeff + OL/2).
func Dispersion(mean, squaredMean float64) float64 {
	if !(mean > 0) || math.IsInf(mean, 0) || !(squaredMean > 0) || math.IsInf(squaredMean, 0) {
		return 1
	}
	return max(squaredMean/(mean*mean), 1)
}

// WithOutputDispersion returns s with the spread of its output lengths priced
// into the KV a resident request holds: KVreq = ILeff + d * OL / 2. A d below
// 1, or not a finite number, is read as 1, the shape as New prices it.
func (s Shape) WithOutputDispersion(d float64) Shape {
	if !(d >= 1) || math.IsInf(d, 0) {
		d = 1
	}
	s.OutputDispersion = d
	s.KVreq = s.ILeff + d*s.AvgOutputTokens/2
	return s
}
