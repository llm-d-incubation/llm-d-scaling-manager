package slo

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/internal/learnertest"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// Issue ev-shindin/llm-scaling-manager#467: the slo line priced one-sided at the sequence cap while B is
// unlearned (SwitchITLOneSidedAtCap), through the line's pricing
// (priceVariant), on the engine's rig. The slo line is priced at the fill
// where it meets the TPOT target: a lower fill is more replicas, so a line
// priced under the true one meets the target at a higher fill.

// sloCapTruth is a true line, a fill k_cap a replica at its cap sits at, and
// the fill the true line meets the TPOT target at.
type sloCapTruth struct{ a, b, kCap float64 }

func (l sloCapTruth) itl(k float64) float64 { return l.a*k + l.b }

func (l sloCapTruth) String() string {
	return fmt.Sprintf("A %.2f B %.0f ms k_cap %.2f", l.a, 1000*l.b, l.kCap)
}

// sloTruthFill is the fill each truth meets its target at: inside the band.
const sloTruthFill = 0.5

// target is the TPOT target the true line meets at sloTruthFill.
func (l sloCapTruth) target() float64 { return l.itl(sloTruthFill) }

// sloCapTruths is issue ev-shindin/llm-scaling-manager#467's bias grid.
var sloCapTruths = []sloCapTruth{{0.05, 0.010, 0.97}, {0.05, 0.030, 0.97}, {0.01, 0.020, 0.97}, {0.01, 0.030, 0.97}}

// capRig is the settle rig over a store stating a cap of 64 for agg.
func capRig(t *testing.T, switches map[string]bool) *settleRig {
	t.Helper()
	r := newSettleRig(t, switches)
	s := capacity.NewStore()
	s.Update("ns", "m", "agg", capacity.Record{EngineParams: capParams()}, r.now)
	r.a.store = s
	return r
}

// capParams is agg's engine: the flags state no cap, its scale target
// declares 64.
func capParams() *capacity.EngineParams {
	return &capacity.EngineParams{MaxNumSeqs: 256, DeclaredBatchCap: 64}
}

// sibAgg is a second variant of the model, in agg's physics (the same
// accelerator, GPUs and role), with no rows: its key holds whatever a test
// files under it.
var sibAgg = domain.VariantReplicaState{VariantName: "sib", Role: domain.RoleBoth,
	AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 1}

// sibKey is sib's window key.
var sibKey = windowKey("ns", "m", sibAgg)

// learnedOn files a learned B b on key, its line fitted under args.
func learnedOn(key, args string, b float64) func(*settleRig) {
	return func(r *settleRig) {
		r.a.lines.SetBaseline(key, b)
		r.a.mu.Lock()
		r.a.args[key] = args
		r.a.mu.Unlock()
	}
}

// scoped is a learned B b on sib, in agg's physics.
func scoped(b float64) func(*settleRig) { return learnedOn(sibKey, capParams().ArgsTag(), b) }

// outOfScope is, per dimension, a key that differs from agg's physics in
// that one dimension alone, and the engine arguments it was fitted under:
// another accelerator, another GPU count, another namespace, other engine
// arguments, and another role (pre, a prefill variant of the model).
var outOfScope = []struct{ name, key, args string }{
	{"accelerator", "ns|m|sib|H200|1", ""},
	{"GPU count", "ns|m|sib|H100|8", ""},
	{"namespace", "ns2|m|sib|H100|1", ""},
	{"engine args", sibKey, "other-args"},
	{"role", windowKey("ns", "m", preAgg), ""},
}

// preAgg is a prefill variant of the model, in agg's hardware.
var preAgg = domain.VariantReplicaState{VariantName: "pre", Role: domain.RolePrefill,
	AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 1}

// outOfScopePrior is a learned B b on one out-of-scope key, fitted under
// args (agg's arguments when "").
func outOfScopePrior(key, args string, b float64) func(*settleRig) {
	if args == "" {
		args = capParams().ArgsTag()
	}
	return learnedOn(key, args, b)
}

// capRows is three replicas of agg at their cap on truth l, at k_cap - 0.01,
// k_cap and k_cap + 0.01, each ITL scaled by (1 + noise[i]).
func capRows(l sloCapTruth, noise [3]float64) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, 3)
	for i, k := range []float64{l.kCap - 0.01, l.kCap, l.kCap + 0.01} {
		r := row("agg", fmt.Sprintf("agg-%d", i), k)
		r.AvgITL = l.itl(k) * (1 + noise[i])
		r.RunningRequests, r.QueueLength, r.PreemptionRateKnown = 64, 5, true
		out = append(out, r)
	}
	return out
}

// pricedAtTarget runs n cycles of rows on r at l's target and returns the
// last cycle's line and the fill it meets the target at (whatif.PriceTPOT,
// the band's default), NaN with no line.
func pricedAtTarget(r *settleRig, l sloCapTruth, n int, rows func(c int) []domain.ReplicaMetrics) (itl.Reading, float64) {
	r.t.Helper()
	var read itl.Reading
	for c := range n {
		read = r.cycleAt(rows(c), l.target())
	}
	if read.Tier == "" {
		return read, math.NaN()
	}
	p := whatif.PriceTPOT(whatif.DefaultBand(), read.Model, l.target())
	if !p.OK || !p.Feasible {
		return read, math.NaN()
	}
	return read, p.Fill
}

// cycleAt is one cycle through the analyzer's Analyze at a TPOT target in
// seconds, and what the line read, as the slo-variant log line states it:
// the line, its tier, whether its slope was held, how many readings it kept
// at the cap, whether it pinned a learned B (its source), and whether it was
// priced at the prior (linePinnedAtBPrior, which the test requires present).
// A cycle that logs no slo-variant line for agg reads as no line.
func (r *settleRig) cycleAt(rows []domain.ReplicaMetrics, target float64) itl.Reading {
	r.t.Helper()
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: policy(time.Duration(target*float64(time.Second)), 0),
		ArrivalRate: 1, VariantStates: []domain.VariantReplicaState{settleAgg, sibAgg, preAgg}, ReplicaMetrics: rows}
	r.tr.Observe(utils.GetNamespacedKey("ns", "m"), settlerow.Of(rows, map[string]string{"agg": domain.RoleBoth}), r.now)
	core, logs := observer.New(zapcore.Level(-logging.VERBOSE))
	_, err := r.a.Analyze(logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core))), in)
	require.NoError(r.t, err)
	r.now = r.now.Add(settleCycle)
	lines := logs.FilterMessage("slo-variant").FilterField(zap.String("variant", "agg")).All()
	if len(lines) == 0 {
		return itl.Reading{}
	}
	f := lines[len(lines)-1].ContextMap()
	pinned, ok := f["linePinnedAtBPrior"].(bool)
	require.True(r.t, ok, "slo-variant carries no linePinnedAtBPrior")
	read := itl.Reading{Model: itl.Model{A: f["lineA"].(float64), B: f["lineB"].(float64)},
		PinnedAtBPrior: pinned}
	read.Tier, _ = f["lineTier"].(string)
	read.SlopeHeld, _ = f["lineSlopeHeld"].(bool)
	if kept, ok := f["kept"].(int64); ok {
		read.Capped = int(kept)
	}
	src, _ := f["source"].(string)
	read.BaselineLearned = src == constants.SLOSourceOwn || src == constants.SLOSourcePinned
	return read
}

// priorFill is the fill, in the default band, where the prior's line meets
// l's target, on readings without noise: the line with intercept bPrior
// capped at the window's least ITL (at k_cap - 0.01), through the mean
// at-cap point (k_cap, the truth there), its slope floored at 0; NaN where it
// meets it nowhere in the band (the analyzer abstains).
func priorFill(l sloCapTruth, bPrior float64) float64 {
	b := min(bPrior, l.itl(l.kCap-0.01))
	m := itl.Model{A: max(0, (l.itl(l.kCap)-b)/l.kCap), B: b}
	p := whatif.PriceTPOT(whatif.DefaultBand(), m, l.target())
	if !p.OK || !p.Feasible {
		return math.NaN()
	}
	return p.Fill
}

// sloResult is one row of the slo bias table.
type sloResult struct {
	// off and on are the fills without noise; want the prior's line's
	// (priorFill); lo and hi the least and most fill over the noisy seeds,
	// and infeasible how many of them abstained.
	off, on, want, lo, hi float64
	infeasible            int
}

// sloRow measures one truth on and off, with and without noise, prints its
// row and holds it: off above the truth's fill (the negative control); on
// at or under the prior's line's fill (priorFill), to the mix of the three
// fills the fit reads, and infeasible exactly where the prior's line is.
func sloRow(t *testing.T, label string, l sloCapTruth, prime func(*settleRig), bPrior float64) sloResult {
	t.Helper()
	const seeds = 20
	rig := func(switches map[string]bool) *settleRig {
		r := capRig(t, switches)
		if prime != nil {
			prime(r)
		}
		return r
	}
	rowsOf := func(n [3][]float64) func(c int) []domain.ReplicaMetrics {
		return func(c int) []domain.ReplicaMetrics {
			if n[0] == nil {
				return capRows(l, [3]float64{})
			}
			return capRows(l, [3]float64{n[0][c], n[1][c], n[2][c]})
		}
	}
	var res sloResult
	_, res.off = pricedAtTarget(rig(map[string]bool{SwitchITLOneSidedAtCap: false}), l, 24, rowsOf([3][]float64{}))
	read, on := pricedAtTarget(rig(nil), l, 24, rowsOf([3][]float64{}))
	res.on = on
	res.lo, res.hi = math.Inf(1), math.Inf(-1)
	for seed := uint64(1); seed <= seeds; seed++ {
		rnd := learnertest.Rand(seed)
		n := [3][]float64{learnertest.ARNoise(rnd, 24, 0.02, 0.5), learnertest.ARNoise(rnd, 24, 0.02, 0.5),
			learnertest.ARNoise(rnd, 24, 0.02, 0.5)}
		_, f := pricedAtTarget(rig(nil), l, 24, rowsOf(n))
		if math.IsNaN(f) {
			res.infeasible++
			continue
		}
		res.lo, res.hi = min(res.lo, f), max(res.hi, f)
	}
	res.want = priorFill(l, bPrior)
	t.Logf("| %s | %.2f | %.0f ms | %.2f | %.4f | %.4f | %.4f | [%.4f, %.4f], infeasible %d of %d | %.2f |",
		label, l.a, 1000*l.b, l.kCap, res.off, res.on, res.want, res.lo, res.hi, res.infeasible, seeds, sloTruthFill)
	require.Greater(t, res.off, sloTruthFill, "negative control, %s: off did not price above the truth's fill", l)
	require.True(t, read.PinnedAtBPrior, "%s: on, not priced at the prior", l)
	if math.IsNaN(res.want) {
		require.True(t, math.IsNaN(res.on), "%s: on priced %.4f where the prior's line meets the target nowhere", l, res.on)
		return res
	}
	require.False(t, math.IsNaN(res.on), "%s: on abstained where the prior's line meets the target at %.4f", l, res.want)
	require.LessOrEqual(t, res.on, res.want+0.005, "%s: on over the prior's line's fill", l)
	return res
}

// The bias table at the slo line's price, the fill where it meets the TPOT
// target, against the truth's (0.5), through Analyze. Off (the negative
// control) the line through the default B meets the target at a higher
// fill than the truth, every point of the grid: too few replicas. On, at or
// under the prior's line's fill, so not higher than the truth's while B is
// at most the prior, without noise. The analyzer abstains (infeasible) only
// where even the prior's line, capped at the window's least ITL, is past
// the target across the band, and the table says on which points it does,
// so a NaN never passes an assertion vacuously. Cases, one subtest each, as
// on the saturation line: the 30 ms floor, with the noise gate (the least
// noisy fill at B = prior at most 0.03 over the truth's); a learned 45 ms in
// agg's physics; a learned 45 ms on a key out of scope in one dimension,
// which must leave the floor's fill; an 80 ms outlier, capped by the
// window; and a true B above the prior (40 ms), at the prior's line's fill.
func TestOneSided_TheBiasTableAtTheTarget(t *testing.T) {
	t.Log("| prior | A | B | k_cap | fill off | fill on | prior's line | on, 2 % noise: [least, most] | truth |")
	// The points each case abstains on, by design: the target under every
	// ITL the window reads (A 0.01 B 20 ms: 25 ms against at least 29.6
	// ms), or a raised prior whose line is past the target across the band.
	infeasible := map[string]map[sloCapTruth]bool{
		"30 ms":         {{0.01, 0.020, 0.97}: true},
		"45 ms scoped":  {{0.05, 0.010, 0.97}: true, {0.01, 0.020, 0.97}: true, {0.01, 0.030, 0.97}: true},
		"80 ms outlier": {{0.05, 0.010, 0.97}: true, {0.05, 0.030, 0.97}: true, {0.01, 0.020, 0.97}: true, {0.01, 0.030, 0.97}: true},
	}
	check := func(t *testing.T, label string, l sloCapTruth, r sloResult) {
		t.Helper()
		require.Equal(t, infeasible[label][l], math.IsNaN(r.on), "%s, %s: infeasible %v, want %v", label, l,
			math.IsNaN(r.on), infeasible[label][l])
		if !math.IsNaN(r.on) {
			require.LessOrEqual(t, r.on, sloTruthFill+1e-9, "%s, %s: on priced a higher fill than the truth", label, l)
		}
	}
	floor := map[sloCapTruth]sloResult{}
	for _, l := range sloCapTruths {
		t.Run("floor/"+l.String(), func(t *testing.T) {
			r := sloRow(t, "30 ms", l, nil, itl.DefaultBPrior)
			floor[l] = r
			check(t, "30 ms", l, r)
			if l.b == itl.DefaultBPrior {
				require.LessOrEqual(t, r.hi, sloTruthFill+0.03, "%s: the noisiest fill past its gate", l)
			}
		})
	}
	for _, l := range sloCapTruths {
		t.Run("scoped 45 ms/"+l.String(), func(t *testing.T) {
			check(t, "45 ms scoped", l, sloRow(t, "45 ms scoped", l, scoped(0.045), 0.045))
		})
	}
	for _, d := range outOfScope {
		for _, l := range sloCapTruths {
			t.Run("out of scope by "+d.name+"/"+l.String(), func(t *testing.T) {
				r := sloRow(t, "45 ms, other "+d.name, l, outOfScopePrior(d.key, d.args, 0.045), itl.DefaultBPrior)
				f := floor[l]
				require.True(t, r.on == f.on || (math.IsNaN(r.on) && math.IsNaN(f.on)),
					"%s: a 45 ms B on another %s moved the fill %.4f to %.4f", l, d.name, f.on, r.on)
			})
		}
	}
	for _, l := range sloCapTruths {
		t.Run("outlier 80 ms/"+l.String(), func(t *testing.T) {
			check(t, "80 ms outlier", l, sloRow(t, "80 ms outlier", l, scoped(0.080), 0.080))
		})
	}
	t.Run("B above the prior", func(t *testing.T) {
		l := sloCapTruth{0.01, 0.040, 0.97}
		r := sloRow(t, "30 ms", l, nil, itl.DefaultBPrior)
		require.False(t, math.IsNaN(r.on), "B above the prior: abstained")
		require.Greater(t, r.on, sloTruthFill, "B above the prior: not above the truth's fill")
		require.InDelta(t, priorFill(l, itl.DefaultBPrior), r.on, 0.005, "B above the prior: not the prior's line's fill")
	})
}

// The window's cap brings an outlier prior down to a line that meets the
// target. An 80 ms prior learned in agg's physics, on A 0.01 B 20 ms with a
// replica in the band reading 22 ms: uncapped, the prior's line is past the
// 25 ms target across the band and the variant would abstain; capped at the
// 22 ms reading, it meets the target in the band, at or under the truth's
// fill. On every cycle priced at the prior, before a full fit learns B.
func TestOneSided_TheCapBringsAnOutlierPriorToAFeasibleFill(t *testing.T) {
	l := sloCapTruth{0.01, 0.020, 0.97}
	band := row("agg", "agg-band", 0.20)
	band.AvgITL = l.itl(0.20)
	uncapped := itl.Model{A: max(0, (l.itl(l.kCap)-0.080)/l.kCap), B: 0.080}
	require.False(t, whatif.PriceTPOT(whatif.DefaultBand(), uncapped, l.target()).Feasible,
		"premise: the uncapped 80 ms line meets the target")
	r := capRig(t, nil)
	scoped(0.080)(r)
	atPrior := 0
	for c := range 40 {
		read := r.cycleAt(append(capRows(l, [3]float64{}), band), l.target())
		if read.Tier == "" || !read.PinnedAtBPrior {
			continue
		}
		atPrior++
		p := whatif.PriceTPOT(whatif.DefaultBand(), read.Model, l.target())
		require.True(t, p.OK && p.Feasible, "cycle %d: the capped outlier line abstained (%+v)", c, read.Model)
		require.LessOrEqual(t, p.Fill, sloTruthFill+1e-9, "cycle %d: priced a higher fill than the truth", c)
	}
	t.Logf("%d cycles priced at the capped 80 ms prior", atPrior)
	require.Positive(t, atPrior, "premise: no cycle was priced at the prior")
}

// The window's cap makes a point feasible once a reading shows B is under
// the target. On A 0.01 B 20 ms, with only the capped replicas, the least
// ITL is 29.6 ms, above the 25 ms target, and the variant abstains. A fourth
// replica reading 22 ms (in the band, at k 0.20) caps the prior at 22 ms.
// On every cycle that is priced at the prior, before the window holds the
// spread a full fit needs, the line meets the target inside the band, at
// most at the capped prior's line's fill and at or under the truth's. Once
// the full fit learns B, the rule ends, and the line is the truth's. On no
// cycle with a line does the variant abstain.
func TestOneSided_AReadingUnderTheTargetMakesItFeasible(t *testing.T) {
	l := sloCapTruth{0.01, 0.020, 0.97}
	_, alone := pricedAtTarget(capRig(t, nil), l, 24, func(int) []domain.ReplicaMetrics { return capRows(l, [3]float64{}) })
	require.True(t, math.IsNaN(alone), "premise: the capped replicas alone abstain, got %.4f", alone)
	band := row("agg", "agg-band", 0.20)
	band.AvgITL = l.itl(0.20)
	capped := itl.Model{A: (l.itl(l.kCap) - band.AvgITL) / l.kCap, B: band.AvgITL}
	cappedFill := whatif.PriceTPOT(whatif.DefaultBand(), capped, l.target()).Fill
	r := capRig(t, nil)
	atPrior := 0
	for c := range 40 {
		read := r.cycleAt(append(capRows(l, [3]float64{}), band), l.target())
		if read.Tier == "" {
			continue
		}
		p := whatif.PriceTPOT(whatif.DefaultBand(), read.Model, l.target())
		require.True(t, p.OK && p.Feasible, "cycle %d: abstained with a reading under the target (%+v)", c, read.Model)
		require.LessOrEqual(t, p.Fill, sloTruthFill+1e-9, "cycle %d: priced a higher fill than the truth", c)
		if read.PinnedAtBPrior {
			atPrior++
			require.LessOrEqual(t, p.Fill, cappedFill+1e-9, "cycle %d: priced a higher fill than the capped prior's line", c)
		}
	}
	t.Logf("%d cycles priced at the prior, capped at 22 ms (fill at most %.4f)", atPrior, cappedFill)
	require.Positive(t, atPrior, "premise: no cycle was priced at the prior")
}

// The rule lasts while the key has no learned B: one-sided while unlearned;
// off once a full fit in the band learns B; back once the line is forgotten
// (ForgetVariant).
//
// The truth's B (20 ms) is under the prior, a sibling's learned 30 ms, so
// the prior's line is above the truth and a raise after B is learned shows.
func TestOneSided_ALearnedBEndsItAndAForgetBringsItBack(t *testing.T) {
	l := sloCapTruth{0.05, 0.020, 0.97}
	r := capRig(t, nil)
	scoped(0.030)(r)
	capOnly := func(int) []domain.ReplicaMetrics { return capRows(l, [3]float64{}) }
	read, _ := pricedAtTarget(r, l, 12, capOnly)
	require.False(t, read.BaselineLearned)
	require.True(t, read.PinnedAtBPrior, "unlearned: not priced one-sided")

	pricedAtTarget(r, l, 64, func(c int) []domain.ReplicaMetrics {
		k := 0.25
		if (c/16)%2 == 1 {
			k = 0.65
		}
		b0, b1 := row("agg", "band-0", k), row("agg", "band-1", 0.45)
		b0.AvgITL, b1.AvgITL = l.itl(k), l.itl(0.45)
		return append(capOnly(c), b0, b1)
	})
	read, _ = pricedAtTarget(r, l, 140, capOnly)
	require.True(t, read.BaselineLearned, "premise: the band's full fit learned B")
	require.Positive(t, read.Capped, "premise: the capped replicas are still read")
	require.False(t, read.PinnedAtBPrior, "learned: still priced one-sided")
	require.InDelta(t, l.itl(0.5), read.Model.ITLAt(0.5), 1e-9, "learned: not the true line")

	require.Positive(t, r.a.ForgetVariant(context.Background(), "ns", "m", "agg"))
	read, _ = pricedAtTarget(r, l, 24, capOnly)
	require.False(t, read.BaselineLearned)
	require.True(t, read.PinnedAtBPrior, "forgotten: the one-sided price did not come back")
}

// A held slope keeps the one-sided price, and ends with the hold: once the
// capped replicas leave, the line holds the last line fitted, which met the
// target at the prior's line's fill (here B is the prior, so the truth's,
// 0.5), until the slope is older than slopeHoldFor (1 h here); then it is
// silent. Off (the negative control), the held line meets the target above
// the truth's fill.
func TestOneSided_TheHeldSlopeKeepsThePriceAndEndsWithTheHold(t *testing.T) {
	l := sloCapTruth{0.01, 0.030, 0.97}
	feasible := func(m itl.Model) (float64, bool) {
		p := whatif.PriceTPOT(whatif.DefaultBand(), m, l.target())
		return p.Fill, p.OK && p.Feasible
	}
	type outcome struct {
		heldFill                       float64
		heldFeasible                   bool
		lastFit, lastHeld, firstSilent time.Duration
	}
	run := func(switches map[string]bool) outcome {
		r := capRig(t, switches)
		r.a.WithSlopeHoldFor(time.Hour)
		t0 := r.now
		for range 20 {
			r.cycleAt(capRows(l, [3]float64{}), l.target())
		}
		idle := []domain.ReplicaMetrics{row("agg", "agg-0", 0.05), row("agg", "agg-1", 0.05), row("agg", "agg-2", 0.05)}
		var o outcome
		for r.now.Before(t0.Add(2 * time.Hour)) {
			at := r.now.Sub(t0)
			read := r.cycleAt(idle, l.target())
			switch {
			case read.Tier != "" && !read.SlopeHeld:
				o.lastFit = at
			case read.Tier != "":
				o.heldFill, o.heldFeasible = feasible(read.Model)
				o.lastHeld = at
			case o.firstSilent == 0 && o.lastHeld > 0:
				o.firstSilent = at
			}
		}
		return o
	}
	o := run(nil)
	t.Logf("last fit %s, held to %s (feasible %v), silent from %s", o.lastFit, o.lastHeld, o.heldFeasible, o.firstSilent)
	require.NotZero(t, o.lastHeld, "premise: the slope was held")
	require.True(t, o.heldFeasible, "premise: the held line meets the target in the band")
	require.InDelta(t, priorFill(l, itl.DefaultBPrior), o.heldFill, 1e-9, "the held line does not keep the prior's fill")
	require.NotZero(t, o.firstSilent, "the hold never ended")
	require.Equal(t, settleCycle, o.firstSilent-o.lastHeld)
	require.True(t, o.lastHeld-o.lastFit <= time.Hour && o.firstSilent-o.lastFit > time.Hour,
		"the hold ended %s after the last fit, want just past 1h", o.firstSilent-o.lastFit)

	off := run(map[string]bool{SwitchITLOneSidedAtCap: false})
	t.Logf("off: held fill %.4f (feasible %v)", off.heldFill, off.heldFeasible)
	require.True(t, off.heldFeasible && off.heldFill > sloTruthFill,
		"negative control: off, the held line did not meet the target above the truth's fill")
}

// The switch is a replay switch the analyzer accepts, and off is the line
// before ev-shindin/llm-scaling-manager#467.
func TestOneSided_TheSwitchPicksTheLine(t *testing.T) {
	require.Contains(t, ReplaySwitchNames(), SwitchITLOneSidedAtCap)
	l := sloCapTruth{0.05, 0.030, 0.97}
	capOnly := func(int) []domain.ReplicaMetrics { return capRows(l, [3]float64{}) }
	on, _ := pricedAtTarget(capRig(t, nil), l, 12, capOnly)
	off, _ := pricedAtTarget(capRig(t, map[string]bool{SwitchITLOneSidedAtCap: false}), l, 12, capOnly)
	require.True(t, on.PinnedAtBPrior)
	require.False(t, off.PinnedAtBPrior)
	require.Equal(t, itl.DefaultBaselineSec, off.Model.B)
}

// The slo noise gate holds for the analyzer's line and fails for the line
// through the least at-cap reading (4b-3's rule and the first prior): at B
// equal to the prior, the least of the noisy readings sits under the true
// line, so its line meets the target at a higher fill. The least-reading
// line is worked by hand from the readings the analyzer kept.
func TestOneSided_TheLeastReadingLineFailsTheNoiseGate(t *testing.T) {
	const seeds = 20
	for _, l := range []sloCapTruth{{0.05, 0.030, 0.97}, {0.01, 0.030, 0.97}} {
		t.Run(l.String(), func(t *testing.T) {
			hiOn, hiLeast := math.Inf(-1), math.Inf(-1)
			for seed := uint64(1); seed <= seeds; seed++ {
				rnd := learnertest.Rand(seed)
				n := [3][]float64{learnertest.ARNoise(rnd, 24, 0.02, 0.5), learnertest.ARNoise(rnd, 24, 0.02, 0.5),
					learnertest.ARNoise(rnd, 24, 0.02, 0.5)}
				r := capRig(t, nil)
				_, on := pricedAtTarget(r, l, 24, func(c int) []domain.ReplicaMetrics {
					return capRows(l, [3]float64{n[0][c], n[1][c], n[2][c]})
				})
				rs := r.a.lines.Readings(windowKey("ns", "m", settleAgg))
				least := itl.Observation{ITLSec: math.Inf(1)}
				for _, o := range rs.All {
					if o.K >= itl.DefaultKSat && o.ITLSec < least.ITLSec {
						least = o
					}
				}
				m := itl.Model{A: max(0, (least.ITLSec-itl.DefaultBPrior)/least.K), B: itl.DefaultBPrior}
				p := whatif.PriceTPOT(whatif.DefaultBand(), m, l.target())
				if !math.IsNaN(on) {
					hiOn = max(hiOn, on)
				}
				if p.OK && p.Feasible {
					hiLeast = max(hiLeast, p.Fill)
				}
			}
			t.Logf("%s: the noisiest fill %.4f, through the least reading %.4f (truth %.2f, gate %.2f)",
				l, hiOn, hiLeast, sloTruthFill, sloTruthFill+0.03)
			require.LessOrEqual(t, hiOn, sloTruthFill+0.03, "the analyzer's line past the gate")
			require.Greater(t, hiLeast, sloTruthFill+0.03, "the least-reading line passes the gate: it tests nothing")
		})
	}
}
