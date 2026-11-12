package learnreplay

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/prefill"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
)

// The joint TTFT model's row of the bar (docs/proposals/slo-capacity-planner-ttft.md,
// "Validation against the staircase", and "As built (J1)"): the joint mean at the
// fleet that ran, against what the fleet measured, step by step.
//
// The recordings hold no router mean (RouterLatency.TTFTSumRate came later), so
// what is compared is the engines' part: the prefill pods' mean TTFT plus the
// decode pods', which on these phases is the router's mean less 9 to 11 ms
// ("Measured: where TTFT goes"). The model's own R is the assumed hop in every
// one of these replays, and is left out on both sides.

// jointStep is one step of a staircase phase, as the model row reads it.
type jointStep struct {
	rate                float64
	predicted, measured float64
	// frozen is the mean priced from the inputs as they stood at the end of
	// the second step, NaN where it refused; unseen counts its cycles past
	// the pull table.
	frozen    float64
	unseen    int
	cycles    int
	pastKnee  bool
	bucketErr float64
}

// jointSettleCycles is the cycles at a step's start left out of it: 180 s at
// the staircase's 15 s cycle, as the measured split was read.
const jointSettleCycles = 12

// stepMinCycles and stepRise cut a phase into steps (staircaseStepBounds).
const (
	stepMinCycles = 38
	stepRise      = 0.04
)

// jointBand is the model row's bar on 2+2 Poisson below the knee.
const jointBandLow, jointBandHigh = -0.05, 0.10

// jointKnownMisses is each 2+2 Poisson step below the knee the model row
// misses, by fixture, with the step (from 1) and its error then. Each must
// still miss: a change that brings one into the band moves it out of here.
//
// b-on's last step, 39.6 r/s, reads 9.5 % under: the decode batch stands at
// 0.86 of its own knee there (n_out·λ_d·β), where the closed loop's k is
// steepest, and the measured ITL rises faster than the line fitted below it.
// The SLO side: a count priced there would be too small. It is reported, not
// tuned away (the proposal's "As built (J1)").
var jointKnownMisses = map[string]map[int]float64{
	"ttft-staircase-b-on": {7: -0.095},
}

// jointCycleLine is what one cycle's slo-ttft-joint line says.
type jointCycleLine struct {
	rate, engine, measured float64
	priced                 bool
}

// readJointLines replays a recording and reads its slo-ttft-joint lines, one
// a cycle.
func readJointLines(t *testing.T, raw []byte) []jointCycleLine {
	t.Helper()
	var lines []jointCycleLine
	logger := funcr.NewJSON(func(obj string) {
		if !strings.Contains(obj, `"msg":"slo-ttft-joint"`) {
			return
		}
		num := func(k string) float64 {
			v, err := strconv.ParseFloat(logField(t, obj, k), 64)
			require.NoError(t, err)
			return v
		}
		lines = append(lines, jointCycleLine{rate: num("rate"), engine: num("engine"), measured: num("engineMeasured"),
			priced: logField(t, obj, "reason") == constants.SLOTTFTJointReasonPriced})
	}, funcr.Options{Verbosity: 10})
	_, err := Replay(logr.NewContext(context.Background(), logger), raw, Options{Mode: Normal})
	require.NoError(t, err)
	return lines
}

// staircaseStepBounds cuts a phase's cycles into its steps by the arrival
// rate: a step of at least stepMinCycles starts where the next 4 cycles'
// mean rate stands stepRise above the step's own mean, and the phase ends at
// its first idle cycle after load. The staircase's steps are 10.5 minutes,
// 42 cycles, and 6 to 40 % apart. Each bound is [start, end) over cycle
// indexes.
func staircaseStepBounds(rates []float64) [][2]int {
	var out [][2]int
	i := 0
	for i < len(rates) && rates[i] < 1 {
		i++
	}
	start, sum := i, 0.0
	for ; i < len(rates); i++ {
		if rates[i] < 1 {
			break
		}
		n := i - start
		if n >= stepMinCycles && i+4 <= len(rates) {
			next := (rates[i] + rates[i+1] + rates[i+2] + rates[i+3]) / 4
			if next > (1+stepRise)*sum/float64(n) {
				out = append(out, [2]int{start, i})
				start, sum = i, 0
			}
		}
		sum += rates[i]
	}
	return append(out, [2]int{start, i})
}

// analyzedInput is one saturation Analyze input and its cycle's clock.
type analyzedInput struct {
	in domain.AnalyzerInput
	at time.Time
}

// analyzedInputs is the recording's saturation Analyze inputs, in order.
func analyzedInputs(t *testing.T, raw []byte) []analyzedInput {
	t.Helper()
	rec, err := learnrec.Read(raw)
	require.NoError(t, err)
	var out []analyzedInput
	for _, ev := range rec.Events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		in, err := decodeInput(d.Input)
		require.NoError(t, err)
		out = append(out, analyzedInput{in: in, at: ev.Now})
	}
	return out
}

// jointModelRow reads one phase's model row: the analyzer's joint mean (its
// engines' part) against the engines' measured mean, per step; and, beside
// it, the mean priced from the inputs frozen at the end of the second step,
// fed from the recording's rows with no settle tracker, as the what-if CLI
// reads them (State.Joint).
func jointModelRow(t *testing.T, fixture string) []jointStep {
	t.Helper()
	raw := staircaseRecording(t, fixture)
	lines := readJointLines(t, raw)
	inputs := analyzedInputs(t, raw)
	require.Len(t, lines, len(inputs), "%s: one slo-ttft-joint line a cycle", fixture)
	rates := make([]float64, len(lines))
	for i, l := range lines {
		rates[i] = l.rate
	}
	bounds := staircaseStepBounds(rates)

	samples := newTTFTSamples()
	var frozen *prefill.JointEstimate
	var steps []jointStep
	next := 0
	for k, b := range bounds {
		for ; next < b[1]; next++ {
			samples.noteJoint(inputs[next].in, inputs[next].at, func(string) string { return "" },
				func(string) bool { return false })
		}
		if k == 1 {
			est := samples.joint.Estimate()
			frozen = &est
		}
		s := jointStep{frozen: math.NaN()}
		var pred, meas, fz, rate float64
		nf := 0
		for i := b[0] + jointSettleCycles; i < b[1]; i++ {
			l := lines[i]
			if !l.priced || !whatif.Finite(l.measured) || !whatif.Finite(l.engine) {
				continue
			}
			if l.measured > 1 {
				s.pastKnee = true
			}
			pred += l.engine
			meas += l.measured
			rate += l.rate
			s.cycles++
			if frozen != nil && k > 1 {
				q := whatif.JointQuestion{PrefillReplicas: 2, DecodeReplicas: 2, Rate: l.rate}
				if p, refused := whatif.JointTTFTMean(*frozen, q); refused == "" {
					fz += p.Engine
					nf++
					if p.HasFlag(whatif.PullUnseen) {
						s.unseen++
					}
				}
			}
		}
		if s.cycles == 0 {
			continue
		}
		n := float64(s.cycles)
		s.rate, s.predicted, s.measured = rate/n, pred/n, meas/n
		if nf > 0 {
			s.frozen = fz / float64(nf)
		}
		s.bucketErr = s.predicted/s.measured - 1
		if len(steps) > 0 && s.measured > 2.5*steps[0].measured {
			s.pastKnee = true
		}
		steps = append(steps, s)
	}
	return steps
}

// The joint model's row of the bar, on the four staircase phases committed as
// goldens. Every one of them ran 2 prefill and 2 decode pods. On 2+2 Poisson
// (a-off at 1000-token prompts; b-off and b-on at 8000), the mean at the fleet
// that ran is within -5 % to +10 % of the engines' measured mean at every step
// below the knee. C (Ca² 4) is logged and not held to the band: the bar
// expects it to fail until a burst term on the decode batch exists.
//
// The frozen column is the extrapolation the count search will lean on: the
// inputs as they stood after the second step, priced at each later step. Its
// cycles past the highest pull bucket seen are counted (PullUnseen).
func TestGoldens_TTFTJointModelRow(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	for _, c := range []struct {
		fixture string
		band    bool
	}{
		{"ttft-staircase-a-off", true},
		{"ttft-staircase-b-off", true},
		{"ttft-staircase-b-on", true},
		{"ttft-staircase-c-on", false},
	} {
		steps := jointModelRow(t, c.fixture)
		require.NotEmpty(t, steps, c.fixture)
		below := 0
		for i, s := range steps {
			frozenErr := s.frozen/s.measured - 1
			t.Logf("%s step %d: %.1f r/s, %d cycles, model %.1f ms, measured %.1f ms, %+.1f %%; frozen after step 2 %+.1f %% (%d past the pull table); past the knee %v",
				c.fixture, i+1, s.rate, s.cycles, 1000*s.predicted, 1000*s.measured, 100*s.bucketErr,
				100*frozenErr, s.unseen, s.pastKnee)
			if s.pastKnee {
				continue
			}
			below++
			if !c.band {
				continue
			}
			if miss, ok := jointKnownMisses[c.fixture][i+1]; ok {
				assert.InDelta(t, miss, s.bucketErr, 0.005, "%s step %d: a known miss moved", c.fixture, i+1)
				assert.False(t, s.bucketErr >= jointBandLow && s.bucketErr <= jointBandHigh,
					"%s step %d: a known miss is in the band now; take it off jointKnownMisses", c.fixture, i+1)
				continue
			}
			assert.GreaterOrEqual(t, s.bucketErr, jointBandLow, "%s step %d", c.fixture, i+1)
			assert.LessOrEqual(t, s.bucketErr, jointBandHigh, "%s step %d", c.fixture, i+1)
		}
		// Two at least: a-off ran three steps, and its first is not priced. Its
		// load held flat there, ITL within prefill.MinJointSpread, so neither
		// decode line is fitted until the second.
		assert.GreaterOrEqual(t, below, 2, "%s: too few steps below the knee to read the row", c.fixture)
	}
}

// staircaseStepBounds finds a staircase's steps and its end.
func TestStaircaseStepBounds(t *testing.T) {
	rates := make([]float64, 0, stepMinCycles+20)
	rates = append(rates, 0, 0)
	for range stepMinCycles + 6 {
		rates = append(rates, 10)
	}
	for range 10 {
		rates = append(rates, 11)
	}
	rates = append(rates, 0, 0)
	// The split comes where two of the next four cycles read 11: their mean,
	// 10.5, stands past stepRise over 10.
	m := stepMinCycles
	assert.Equal(t, [][2]int{{2, m + 6}, {m + 6, m + 18}}, staircaseStepBounds(rates))
	rates[m+8] = 10.2 // one cycle of 11 in the next four reads 10.3: no split yet
	assert.Equal(t, [][2]int{{2, m + 7}, {m + 7, m + 18}}, staircaseStepBounds(rates))
	assert.Equal(t, [][2]int{{2, 2}}, staircaseStepBounds([]float64{0, 0}), "no load is one empty step")
}
