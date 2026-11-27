package whatif

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jointRouterEdges is the router's TPOT edges, seconds, and
// jointEngineEdges vLLM's request_time_per_output_token edges up to 100 ms.
var (
	jointRouterEdges = []float64{0.005, 0.01, 0.02, 0.04, 0.06, 0.08}
	jointEngineEdges = []float64{0.01, 0.025, 0.05, 0.075, 0.1}
)

// jointStep is a step whose router TPOT histogram puts the shares f at
// jointRouterEdges over n requests, and, where ef is not nil, whose engines'
// histogram puts the shares ef at jointEngineEdges over ne requests.
func jointStep(f []float64, n float64, ef []float64, ne float64) TPOTDistStep {
	st := TPOTDistStep{Edges: append(append([]float64{}, jointRouterEdges...), math.Inf(1))}
	prev := 0.0
	for _, x := range append(append([]float64{}, f...), 1) {
		st.Counts = append(st.Counts, (x-prev)*n)
		prev = x
	}
	if ef != nil {
		edges := append(append([]float64{}, jointEngineEdges...), math.Inf(1))
		cum := make([]float64, 0, len(edges))
		for _, x := range append(append([]float64{}, ef...), 1) {
			cum = append(cum, x*ne)
		}
		st = st.WithEngine(edges, cum, 1)
	}
	return st
}

// routerF is the router's shares at 5, 10, 20, 40, 60 and 80 ms; engineF the
// engines' at 10, 25, 50, 75 and 100 ms: half their observations under 10 ms
// (a population the router does not time), and above it the router's law,
// anchored at the router's F(10 ms) = 0.30, with F(25 ms) = 0.92.
var (
	routerF = []float64{0.02, 0.30, 0.85, 0.97, 0.99, 0.995}
	engineF = []float64{0.5, 0.5 + 0.5*(0.92-0.3)/0.7, 0.5 + 0.5*(0.98-0.3)/0.7, 0.5 + 0.5*(0.993-0.3)/0.7, 0.5 + 0.5*(0.998-0.3)/0.7}
)

// The router alone puts the p90 in its 20 ms to 40 ms bucket; the engines'
// 25 ms edge, joined above their first edge and anchored at the router's
// F(10 ms), narrows it to [20 ms, 25 ms], and the point is the Pareto
// reading inside: log S linear in log x between F(20 ms) 0.85 and F(25 ms)
// 0.92. The p95 lies in [25 ms, 40 ms].
func TestJointBracket_TheEngineEdgeNarrowsTheRouterBucket(t *testing.T) {
	st := jointStep(routerF, 20000, engineF, 40000)
	b, ok := JointBracket(st, 0.9)
	require.True(t, ok)
	assert.True(t, b.Engine)
	assert.False(t, b.Conflict)
	assert.InDelta(t, 0.020, b.Lo, 1e-12)
	assert.InDelta(t, 0.025, b.Hi, 1e-12)
	want := 0.020 * math.Pow(1.25, math.Log(0.15/0.10)/math.Log(0.15/0.08))
	assert.InDelta(t, want, b.Point, 1e-9, "the Pareto reading")
	assert.Less(t, b.Point, 0.020+0.005*(0.9-0.85)/(0.92-0.85), "below the linear reading")
	assert.Equal(t, BracketInside, b.Side(0.022))
	assert.Equal(t, BracketAbove, b.Side(0.030), "inside the router's bucket, above the joint bracket")
	assert.Equal(t, BracketBelow, b.Side(0.015))

	b, ok = JointBracket(st, 0.95)
	require.True(t, ok)
	assert.InDelta(t, 0.025, b.Lo, 1e-12)
	assert.InDelta(t, 0.040, b.Hi, 1e-12)

	alone, ok := JointBracket(jointStep(routerF, 20000, nil, 0), 0.9)
	require.True(t, ok)
	assert.False(t, alone.Engine)
	assert.InDelta(t, 0.020, alone.Lo, 1e-12)
	assert.InDelta(t, 0.040, alone.Hi, 1e-12)
}

// The engines' histogram joins only above an edge the router also has, and
// only with at least MinEngineAboveFirstEdge requests above it.
func TestJointBracket_TheEngineJoinsOnlyWhereItCan(t *testing.T) {
	few, ok := JointBracket(jointStep(routerF, 20000, engineF, 90), 0.9)
	require.True(t, ok)
	assert.False(t, few.Engine, "45 requests above 10 ms")
	assert.InDelta(t, 0.040, few.Hi, 1e-12)

	st := jointStep(routerF, 20000, engineF, 40000)
	st.EngineEdges[0] = 0.015
	off, ok := JointBracket(st, 0.9)
	require.True(t, ok)
	assert.False(t, off.Engine, "the router has no 15 ms edge to anchor at")
}

// Each share is held to two binomial errors and the allowance either way: at
// 300 requests the router's F(20 ms) = 0.91 does not place the p90 under
// 20 ms, where the bucket alone would.
func TestJointBracket_TheNoiseWidensAThinStep(t *testing.T) {
	f := []float64{0.02, 0.30, 0.91, 0.97, 0.99, 0.995}
	b, ok := JointBracket(jointStep(f, 300, nil, 0), 0.9)
	require.True(t, ok)
	assert.InDelta(t, 0.010, b.Lo, 1e-12)
	assert.InDelta(t, 0.040, b.Hi, 1e-12)
	b, ok = JointBracket(jointStep(f, 1e6, nil, 0), 0.9)
	require.True(t, ok)
	assert.InDelta(t, 0.010, b.Lo, 1e-12)
	assert.InDelta(t, 0.020, b.Hi, 1e-12, "at a million requests the bucket holds")
}

// Where the router and the engines cannot both hold at the quantile (the
// router's F(20 ms) 0.95, the engines' anchored F(25 ms) 0.5) the bracket is
// the router's own bucket, flagged.
func TestJointBracket_AConflictIsTheRouterBucket(t *testing.T) {
	f := []float64{0.02, 0.30, 0.95, 0.97, 0.99, 0.995}
	ef := []float64{0.5, 0.5 + 0.5*(0.5-0.3)/0.7, 0.5 + 0.5*(0.98-0.3)/0.7, 0.5 + 0.5*(0.993-0.3)/0.7, 0.5 + 0.5*(0.998-0.3)/0.7}
	b, ok := JointBracket(jointStep(f, 20000, ef, 40000), 0.9)
	require.True(t, ok)
	assert.True(t, b.Conflict)
	assert.InDelta(t, 0.010, b.Lo, 1e-12)
	assert.InDelta(t, 0.020, b.Hi, 1e-12)
	assert.GreaterOrEqual(t, b.Point, b.Lo)
	assert.LessOrEqual(t, b.Point, b.Hi)
}

// A quantile above the top edge is open above; a step with no router
// histogram has no bracket.
func TestJointBracket_OpenAboveAndUnread(t *testing.T) {
	f := []float64{0.02, 0.30, 0.50, 0.60, 0.70, 0.80}
	b, ok := JointBracket(jointStep(f, 20000, nil, 0), 0.9)
	require.True(t, ok)
	assert.InDelta(t, 0.080, b.Lo, 1e-12)
	assert.True(t, math.IsInf(b.Hi, 1))
	assert.True(t, math.IsInf(b.Point, 1))
	_, ok = JointBracket(TPOTDistStep{}, 0.9)
	assert.False(t, ok)
	_, ok = JointBracket(jointStep(routerF, 20000, nil, 0), 1)
	assert.False(t, ok)
}

// The length groups carry the bucket's mass at 1/L per integer, so their
// weighted 1/(L-1) is InverseOutputLength's, the weights sum to one, and a
// long bucket is cut into at most lengthSubRanges groups.
func TestOutputLengthLaw_MatchesTheMeanInverse(t *testing.T) {
	out := map[float64]float64{1: 2, 2: 3, 10: 8, 50: 20, math.Inf(1): 25}
	law, ok := OutputLengthLaw(out)
	require.True(t, ok)
	inv, ok := InverseOutputLength(out)
	require.True(t, ok)
	var w, m float64
	for _, g := range law {
		w += g.Weight
		m += g.Weight * g.Inv
	}
	assert.InDelta(t, 1, w, 1e-12)
	assert.InDelta(t, inv, m, 1e-12)
	long, ok := OutputLengthLaw(map[float64]float64{1000: 1, 5000: 2})
	require.True(t, ok)
	assert.LessOrEqual(t, len(long), 2*lengthSubRanges)
	assert.Greater(t, len(long), lengthSubRanges, "two long buckets, each cut")
	_, ok = OutputLengthLaw(map[float64]float64{1: 5})
	assert.False(t, ok, "one-token requests have no TPOT")
}

// With one length group the mixture is the one lognormal at that group's
// 1/(L-1); with several its quantile inverts its CDF and its mean is the
// mean.
func TestTPOTDist_PerLengthReducesToTheLognormal(t *testing.T) {
	const mean, inv = 0.012, 0.02
	one := TPOTDist{GroupCV2: 0.01, C: 3}
	mix := TPOTDist{GroupCV2: 0.01, C: 3, PerLength: []LengthGroup{{Weight: 1, Inv: inv}}}
	for _, q := range []float64{0.5, 0.9, 0.95} {
		assert.InEpsilon(t, one.Quantile(mean, inv, q), mix.Quantile(mean, 0.5, q), 1e-9, "inv is not read")
	}
	for _, x := range []float64{0.008, 0.012, 0.02} {
		assert.InDelta(t, one.CDF(mean, inv, x), mix.CDF(mean, 0.5, x), 1e-12)
	}
	two := TPOTDist{GroupCV2: 0.01, C: 3, PerLength: []LengthGroup{{Weight: 0.2, Inv: 0.5}, {Weight: 0.8, Inv: 0.001}}}
	for _, q := range []float64{0.5, 0.9, 0.95} {
		assert.InDelta(t, q, two.CDF(mean, 0, two.Quantile(mean, 0, q)), 1e-9)
	}
	m := 0.0
	const n = 20000
	for i := range n {
		m += two.Quantile(mean, 0, (float64(i)+0.5)/n) / n
	}
	assert.InEpsilon(t, mean, m, 0.005)
	assert.Less(t, two.Quantile(mean, 0, 0.95), TPOTDist{GroupCV2: 0.01, C: 3}.Quantile(mean, 0.2*0.5+0.8*0.001, 0.95),
		"the short requests' wide lognormal keeps most of its mass under the mean: the one lognormal at the mean 1/(L-1) reads the p95 high")
}

// The per-length fit recovers C from steps drawn from the mixture, each at
// its own lengths.
func TestFitTPOTDistPerLength_RecoversC(t *testing.T) {
	edges := []float64{0.005, 0.01, 0.02, 0.04, 0.06, math.Inf(1)}
	steps := make([]TPOTDistStep, 0, 40)
	for i := range 40 {
		law := []LengthGroup{{Weight: 0.1 + 0.02*float64(i%5), Inv: 0.5}, {Weight: 0.9 - 0.02*float64(i%5), Inv: 0.002}}
		d := TPOTDist{C: 2.5, PerLength: law}
		mean := 0.011 + 0.0002*float64(i%10)
		st := TPOTDistStep{Mean: mean, Edges: edges, Counts: make([]float64, len(edges)), Law: law}
		prev := 0.0
		for j, e := range edges {
			f := d.CDF(mean, 0, e)
			st.Counts[j] = 5000 * (f - prev)
			prev = f
		}
		steps = append(steps, st)
	}
	c, atBound, err := FitTPOTDistPerLength(steps, 0)
	require.NoError(t, err)
	assert.False(t, atBound)
	assert.InDelta(t, 2.5, c, 0.01)
}
