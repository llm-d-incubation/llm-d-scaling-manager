package saturation

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/floor"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
)

// Learner PR 5c, round 2: the claim the PR rests on, that sizing never reads
// a start shorter than the parent's (5b, both 5c switches off) and never
// shorter than measured, swept over seeds, sample sequences, a refused
// outlier, a forget in the middle of a count, and a restore.

// safetyStep is one step of a sequence, applied to both analyzers.
type safetyStep struct {
	// sample brings up a new Pod with this start (0: none this step).
	sample float64
	// repeat reports the Pods already up again, no new one.
	repeat bool
	// forget forgets the variant's learned state (the operator's forget).
	forget bool
	// restart exports the state and restores it into fresh analyzers.
	restart bool
}

// safetyPair is the analyzer with both 5c switches on and the parent, fed
// the same rows.
type safetyPair struct {
	on, off *SaturationAnalyzer
	rows    []domain.ReplicaMetrics
	seed    float64
	clock   time.Time
}

func newSafetyPair(t *testing.T, seed float64) *safetyPair {
	t.Helper()
	p := &safetyPair{seed: seed, clock: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	p.on, p.off = blendAnalyzer(t, true, true), blendAnalyzer(t, false, false)
	p.wire()
	return p
}

func (p *safetyPair) wire() {
	for _, a := range []*SaturationAnalyzer{p.on, p.off} {
		a.now = func() time.Time { return p.clock }
	}
}

func (p *safetyPair) note() {
	for _, a := range []*SaturationAnalyzer{p.on, p.off} {
		a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", modelID: "model",
			variant: confirmVariant, role: domain.RoleDecode, seed: p.seed}, p.rows, logr.Discard())
	}
}

func (p *safetyPair) apply(t *testing.T, s safetyStep) {
	t.Helper()
	p.clock = p.clock.Add(30 * time.Second)
	switch {
	case s.forget:
		for _, a := range []*SaturationAnalyzer{p.on, p.off} {
			a.forgetVariant("ns", "model", confirmVariant, fmt.Sprintf("tok-%d", p.clock.Unix()))
		}
		// The Pods still up are measured afresh, as after a real forget.
		p.note()
	case s.restart:
		on, off := p.on.exportLearnedState(), p.off.exportLearnedState()
		p.on, p.off = blendAnalyzer(t, true, true), blendAnalyzer(t, false, false)
		p.wire()
		for _, x := range []struct {
			a *SaturationAnalyzer
			s LearnedState
		}{{p.on, on}, {p.off, off}} {
			x.a.mu.Lock()
			x.a.installLocked(x.s, "", true)
			x.a.mu.Unlock()
		}
	case s.repeat:
		p.note()
	default:
		p.rows = append(p.rows, confirmReplica(fmt.Sprintf("p%d", len(p.rows)), s.sample))
		p.note()
	}
}

func (p *safetyPair) figures() (on, off startFigures) {
	for i, a := range []*SaturationAnalyzer{p.on, p.off} {
		a.mu.Lock()
		f := a.startFiguresLocked(confirmKey, p.seed)
		a.mu.Unlock()
		if i == 0 {
			on = f
		} else {
			off = f
		}
	}
	return on, off
}

func TestStartSafety_SizingIsNeverShorterThanTheParentOrTheMeasurement(t *testing.T) {
	samples := []float64{15, 70, 460}
	sequences := map[string][]safetyStep{}
	// Every sequence of one to three samples from {15, 70, 460}.
	var gen func(prefix []safetyStep)
	gen = func(prefix []safetyStep) {
		if len(prefix) > 0 {
			var name strings.Builder
			for _, s := range prefix {
				name.WriteString(strconv.FormatFloat(s.sample, 'g', -1, 64) + ",")
			}
			sequences[name.String()] = append([]safetyStep(nil), prefix...)
		}
		if len(prefix) == 3 {
			return
		}
		for _, s := range samples {
			gen(append(prefix, safetyStep{sample: s}))
		}
	}
	gen(nil)
	// An outlier 5b refuses (over 3 x the estimate), repeated, then more.
	sequences["outlier"] = []safetyStep{{sample: 60}, {sample: 2700}, {repeat: true}, {sample: 65}}
	sequences["outlier-confirms"] = []safetyStep{{sample: 60}, {sample: 900}, {sample: 950}, {sample: 1000}}
	// A forget in the middle of a count.
	sequences["forget-mid-count"] = []safetyStep{{sample: 460}, {forget: true}, {sample: 15}, {sample: 15}}
	sequences["forget-fast"] = []safetyStep{{sample: 15}, {sample: 15}, {forget: true}, {sample: 460}}
	// A restore, then new samples.
	sequences["restore-then-samples"] = []safetyStep{{sample: 460}, {restart: true}, {sample: 15}, {sample: 70}}
	sequences["restore-fast"] = []safetyStep{{sample: 15}, {restart: true}, {sample: 460}, {sample: 460}, {sample: 15}}

	// Seeds: none, under every mean the samples make, over every one, and
	// one between.
	seeds := map[string]float64{"none": 0, "below": 10, "between": 120, "above": 3000}

	steps, ran, blended := 0, 0, 0
	for seedName, seed := range seeds {
		for name, seq := range sequences {
			t.Run(seedName+"/"+name, func(t *testing.T) {
				ran++
				p := newSafetyPair(t, seed)
				for i, s := range seq {
					p.apply(t, s)
					on, off := p.figures()
					steps++
					if on.blended {
						blended++
					}
					require.GreaterOrEqual(t, on.sized, off.sized,
						"step %d (%+v): on sized %v under the parent's %v", i, s, on.sized, off.sized)
					require.GreaterOrEqual(t, on.sized, on.measured,
						"step %d (%+v): on sized %v under its measurement %v", i, s, on.sized, on.measured)
				}
			})
		}
	}
	// Not vacuous: every sequence ran under every seed, and most steps
	// compared a blend, where on and off differ.
	require.Equal(t, len(seeds)*len(sequences), ran)
	require.Equal(t, 45, len(sequences), "39 sample sequences and 6 others")
	require.Greater(t, blended, steps/3, "the steps compared blends")
	t.Logf("%d sequences x %d seeds, %d steps, %d blended", len(sequences), len(seeds), steps, blended)
}

// The P/D floor: each role keeps the larger of its two pricings, and names
// the start that priced it. Decode, priced over a longer (5c) start, orders
// less than at 5b's start and so keeps 5b's term and measured start; prefill
// orders more at 5c's and keeps it, with its seed.
func TestStartFloor_EachRoleKeepsTheLargerOfItsTwoPricings(t *testing.T) {
	sized := floor.Floor{Lambda: 5, ByRole: map[string]float64{domain.RoleDecode: 1000, domain.RolePrefill: 900},
		Terms: map[string]floor.Term{domain.RoleDecode: {Mu: 1}, domain.RolePrefill: {Mu: 2}}}
	parent := floor.Floor{Lambda: 5, ByRole: map[string]float64{domain.RoleDecode: 1200, domain.RolePrefill: 700},
		Terms: map[string]floor.Term{domain.RoleDecode: {Mu: 11}, domain.RolePrefill: {Mu: 22}}}
	starts := roleStarts{
		sized:  map[string]float64{domain.RoleDecode: 200, domain.RolePrefill: 120},
		source: map[string]string{domain.RoleDecode: startSourceMeasured, domain.RolePrefill: startSourceSeed},
		parent: map[string]float64{domain.RoleDecode: 15},
	}
	got, seconds, sources := pickLargerFloor(sized, parent, starts)
	assert.Equal(t, 1200.0, got.ByRole[domain.RoleDecode], "decode: 5b's larger order")
	assert.Equal(t, 11.0, got.Terms[domain.RoleDecode].Mu, "with 5b's term")
	assert.Equal(t, 15.0, seconds[domain.RoleDecode])
	assert.Equal(t, startSourceMeasured, sources[domain.RoleDecode])
	assert.Equal(t, 900.0, got.ByRole[domain.RolePrefill], "prefill: 5c's larger order")
	assert.Equal(t, 2.0, got.Terms[domain.RolePrefill].Mu)
	assert.Equal(t, 120.0, seconds[domain.RolePrefill])
	assert.Equal(t, startSourceSeed, sources[domain.RolePrefill], "prefill names its seed")
	for role := range got.ByRole {
		assert.Equal(t, max(sized.ByRole[role], parent.ByRole[role]), got.ByRole[role], "%s is the larger", role)
	}
	assert.Equal(t, 1000.0, sized.ByRole[domain.RoleDecode], "the input is not changed")

	// A role 5b projected over no start, which orders more unprojected: it
	// takes 5b's figure and names no start.
	parent.ByRole[domain.RolePrefill] = 950
	got, seconds, sources = pickLargerFloor(sized, parent, starts)
	assert.Equal(t, 950.0, got.ByRole[domain.RolePrefill])
	assert.NotContains(t, seconds, domain.RolePrefill)
	assert.NotContains(t, sources, domain.RolePrefill)
	// A role only 5b prices.
	parent.ByRole["both"], parent.Terms["both"] = 50, floor.Term{Mu: 3}
	got, _, _ = pickLargerFloor(sized, parent, starts)
	assert.Equal(t, 50.0, got.ByRole["both"])
}

// The P/D floor through Analyze: decode seeded at 120 s and blended, prefill
// measured at 400 s. On this rig only decode's floor binds (prefill learns no
// mu, so the floor has no prefill term); the per-role choice between the
// two pricings is TestStartFloor_EachRoleKeepsTheLargerOfItsTwoPricings.
// Here: decode's line names decode's own start, not prefill's, and no role
// orders less than with the 5c switches off.
func TestStartFloor_ThroughAnalyzeAPDFleetNamesEachRolesStart(t *testing.T) {
	run := func(on bool) (*domain.AnalyzerResult, map[string]map[string]any) {
		rig, err := floorRig()
		require.NoError(t, err)
		require.NoError(t, rig.a.SetSwitchesForReplay(map[string]bool{
			SwitchStartReadsSeed: on, SwitchStartBlendsFirstSamples: on}))
		ctx, logs := observedCtx(t)
		rig.ctx = ctx
		rows := []domain.ReplicaMetrics{
			imbRow("r0", "v", 0.95, 300, 1000, 1000, 1.0),
			imbRow("r1", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r2", "v", 0.10, 0, 1000, 1000, 0.33),
			imbRow("r3", "v", 0.10, 0, 1000, 1000, 0.34),
			imbRow("q0", "pre", 0.95, 300, 1000, 1, 1.0),
			imbRow("q1", "pre", 0.10, 0, 1000, 1, 0.5),
		}
		rows[0].StartSeconds = 15
		rows[4].StartSeconds = 400
		states := aggStates(4)
		states[0].Role = domain.RoleDecode
		states[0].StartSeedSeconds, states[0].StartSeedSet = 120, true
		states = append(states, domain.VariantReplicaState{VariantName: "pre", Role: domain.RolePrefill,
			AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1})
		var res *domain.AnalyzerResult
		for range 8 {
			res, err = rig.analyze(rows, states, 10, propCycle)
			require.NoError(t, err)
		}
		byRole := map[string]map[string]any{}
		for _, e := range logs.FilterMessage("throughput-demand-floor").All() {
			m := e.ContextMap()
			byRole[m["role"].(string)] = m
		}
		return res, byRole
	}
	onRes, on := run(true)
	offRes, off := run(false)
	t.Logf("on: decode %v from %v, prefill %v from %v; demand %v / %v", on[domain.RoleDecode]["startEstimateSeconds"],
		on[domain.RoleDecode]["startSource"], on[domain.RolePrefill]["startEstimateSeconds"],
		on[domain.RolePrefill]["startSource"], onRes.RoleDemand, offRes.RoleDemand)
	require.Contains(t, on, domain.RoleDecode, "premise: decode's floor binds")
	require.NotContains(t, on, domain.RolePrefill, "premise: prefill's does not, on this rig")
	for _, role := range []string{domain.RoleDecode, domain.RolePrefill} {
		assert.GreaterOrEqual(t, onRes.RoleDemand[role], offRes.RoleDemand[role], "%s orders no less", role)
	}
	assert.Equal(t, 15.0, off[domain.RoleDecode]["startEstimateSeconds"], "off: decode's one measured start")
	// On: the blend toward decode's own 120 s seed, (2 x 120 + 15) / 3 = 85,
	// never prefill's 400 s.
	assert.Equal(t, 85.0, on[domain.RoleDecode]["startEstimateSeconds"], "decode's own start")
	assert.Equal(t, startSourceMeasured, on[domain.RoleDecode]["startSource"])
}

// warnRig counts the contradiction warnings a sequence of notes logs.
func warnRig(t *testing.T) (*SaturationAnalyzer, logr.Logger, *[]map[string]any) {
	t.Helper()
	var out []map[string]any
	logger := funcr.NewJSON(func(obj string) {
		var m map[string]any
		if json.Unmarshal([]byte(obj), &m) == nil && m["msg"] == StartSeedContradictedMessage {
			out = append(out, m)
		}
	}, funcr.Options{Verbosity: 10})
	return blendAnalyzer(t, true, true), logger, &out
}

func noteSeeded(a *SaturationAnalyzer, logger logr.Logger, seed float64, rows ...domain.ReplicaMetrics) {
	at := a.now().Add(30 * time.Second)
	a.now = func() time.Time { return at }
	a.noteReplicaStartFor(startNote{key: confirmKey, namespace: "ns", modelID: "model",
		variant: confirmVariant, role: domain.RoleDecode, seed: seed}, rows, logger)
}

// The warning's bounds, each side: a mean just under S/2 and just over 2S
// warns, one exactly at 2S does not, and the default prior never warns
// however far off.
func TestStartSeed_TheWarningsBounds(t *testing.T) {
	for _, c := range []struct {
		name         string
		seed, sample float64
		warns        bool
	}{
		{"just under S/2", 120, 50, true},
		{"at S/2", 120, 60, false},
		{"just over 2S", 120, 250, true},
		{"at 2S", 60, 120, false},
		{"over S/3, under S/2", 120, 45, true},
		{"the default prior, far off", 0, 3000, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, logger, got := warnRig(t)
			noteSeeded(a, logger, c.seed, confirmReplica("p0", c.sample))
			if c.warns {
				assert.Len(t, *got, 1)
			} else {
				assert.Empty(t, *got)
			}
		})
	}
}

// After a restart the warning is logged again, once, from the restored
// count; a forget resets it; and seeds A, B, A warn three times: each change
// of seed is a new configuration an operator made, and is judged afresh.
func TestStartSeed_TheWarningAfterARestartAForgetAndASeedChange(t *testing.T) {
	a, logger, got := warnRig(t)
	noteSeeded(a, logger, 3600, confirmReplica("p0", 70))
	require.Len(t, *got, 1)
	s := a.exportLearnedState()
	s.VariantSeenAt = map[string]time.Time{confirmKey: a.now()}

	b, logger2, got2 := warnRig(t)
	b.now = a.now
	b.mu.Lock()
	b.installLocked(s, "", true)
	b.mu.Unlock()
	noteSeeded(b, logger2, 3600, confirmReplica("p0", 70))
	noteSeeded(b, logger2, 3600, confirmReplica("p0", 70))
	assert.Len(t, *got2, 1, "the restored count warns once in the new process")
	// Two: the restored Pod, and p0 counted again, since the Pods already
	// counted are not in the snapshot until 5d.
	assert.Equal(t, float64(2), (*got2)[0]["pods"])

	b.mu.Lock()
	b.forgetStartLocked("ns", "model", confirmVariant, learn.ByOperator)
	_, kept := b.startSeedWarned[confirmKey]
	b.mu.Unlock()
	assert.False(t, kept, "a forget drops the warning's memory")
	noteSeeded(b, logger2, 3600, confirmReplica("p1", 70))
	assert.Len(t, *got2, 2, "and the seed is judged afresh")

	c, logger3, got3 := warnRig(t)
	for _, seed := range []float64{3600, 2000, 3600} {
		noteSeeded(c, logger3, seed, confirmReplica("p0", 70))
	}
	assert.Len(t, *got3, 3, "A, B, A: three warnings, one per change of seed")
}

// The warning text is built from the rule's constants.
func TestStartSeed_TheWarningTextFollowsTheRule(t *testing.T) {
	assert.Contains(t, StartSeedContradictedMessage, fmt.Sprintf("between %g and %g times", startSeedLowFactor, startSeedHighFactor))
	assert.Contains(t, StartSeedContradictedMessage, fmt.Sprintf("until %d of its pods", startConfirmPods))
}
