package learnreplay

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// floorScaleUpTarget is the utilization a role is sized to: the recorded
// policies' scale-up threshold, 0.85 on every fixture below.
const floorScaleUpTarget = 0.85

// floorCycle is one decode-pricing role at one cycle: the replicas its demand
// asks for at floorScaleUpTarget, its per-replica capacity, its variant's mu
// and the line's intercept B.
type floorCycle struct {
	wanted       int
	capacity, mu float64
	intercept    float64
	hasLine      bool
}

// floorRun is a replay's decode-pricing roles, cycle by cycle, keyed
// now|model|role.
type floorRun map[string]floorCycle

// readFloorRun reads a replay's results.csv and variant-capacities.csv.
func readFloorRun(t *testing.T, outs map[string][]byte) floorRun {
	t.Helper()
	rows := func(b []byte) [][]string {
		r, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
		require.NoError(t, err)
		return r[1:]
	}
	type variantRow struct {
		Role               string
		PerReplicaCapacity float64
		ServiceRate        *struct {
			HasLine       bool
			LineIntercept float64
			Rate          float64
		}
	}
	run := floorRun{}
	caps := map[string]variantRow{}
	for _, r := range rows(outs["variant-capacities.csv"]) {
		if r[0] != saturation.KindAnalyze {
			continue
		}
		var v variantRow
		require.NoError(t, json.Unmarshal([]byte(r[4]), &v))
		if v.ServiceRate == nil || v.Role == "prefill" {
			continue
		}
		caps[r[1]+"|"+r[2]+"|"+v.Role] = v
	}
	for _, r := range rows(outs["results.csv"]) {
		var res struct {
			Result *struct{ RoleDemand map[string]float64 }
		}
		require.NoError(t, json.Unmarshal([]byte(r[4]), &res))
		if res.Result == nil {
			continue
		}
		for role, demand := range res.Result.RoleDemand {
			key := r[1] + "|" + r[2] + "|" + role
			v, ok := caps[key]
			if !ok || !(v.PerReplicaCapacity > 0) {
				continue
			}
			run[key] = floorCycle{wanted: int(math.Ceil(demand / (v.PerReplicaCapacity * floorScaleUpTarget))),
				capacity: v.PerReplicaCapacity, mu: v.ServiceRate.Rate, intercept: v.ServiceRate.LineIntercept,
				hasLine: v.ServiceRate.HasLine}
		}
	}
	return run
}

// floorTable is what the switch moves on one fixture.
type floorTable struct {
	fixture string
	// cycles is the decode-pricing cycles with a demand; floorB those whose
	// line, with the switch on, has a B off the 6 ms default (the floor, or
	// a full fit's).
	cycles, floorB int
	// moved is the cycles whose wanted replicas differ; sumOff and sumOn
	// the replica-cycles asked over every cycle; down and up the cycles
	// asking fewer and more.
	moved, down, up int
	sumOff, sumOn   int
	// muOff and muOn are the median mu over the cycles with a line on both
	// sides, in requests per second.
	muOff, muOn float64
}

// floorReplicaTables is each fixture's table as measured, frozen.
//
// The floor moves B toward what the engines read, either way. On e1 it pins
// B at 9.3-12.3 ms, above the 6 ms default: mu rises and the decode role
// asks for one replica fewer on 18 cycles (e1-tuned 2). On the staircases
// and three-pods it pins 0.4-3.8 ms, under the default and where the b-on
// line's own full fits learned 1.4-2.4 ms: mu falls (b-off 31.2 -> 21.8
// r/s), which the staircases' demand absorbs without a replica more, and
// three-pods asks for one more on 31 cycles. a-off never reads a line.
//
// Round 1 of the review aged the floor out at DefaultObservationMaxAge and
// cleared it with the window: the staircases pin it on fewer cycles (b-off
// 194 -> 130, b-on 190 -> 167, c-on 196 -> 147), where the floor was older
// than the readings it was paired with. No replica count moved.
//
// Round 2 dropped a decode variant's floor while the model's shape changes:
// b-off's mu reads 21.8 where it read 21.1, and three-pods pins the floor
// on 102 cycles where it pinned 145, asking for one replica more on 31
// cycles where it asked on 36 (482 replica-cycles where 487).
//
// llm-d-incubation/llm-d-scaling-manager#188's switch on from the
// constructor prices a resident reading above the context as read, in both
// runs: e1 asks for 5 replica-cycles more with the floor off (1454 where
// 1449), and the floor moves 15 cycles down where it moved 18; e1-tuned 1
// where 2. The median mu moves by under 1% on e1-tuned and three-pods and
// by under 0.2% on the staircases; no staircase replica count moves.
var floorReplicaTables = []floorTable{
	{"e1", 762, 552, 15, 15, 0, 1454, 1439, 5.277, 5.270},
	{"e1-tuned", 748, 545, 1, 1, 0, 1388, 1387, 4.755, 4.783},
	{"ttft-staircase-a-off", 128, 0, 0, 0, 0, 125, 125, 0, 0},
	{"ttft-staircase-b-off", 289, 130, 0, 0, 0, 283, 283, 31.239, 21.785},
	{"ttft-staircase-b-on", 289, 167, 0, 0, 0, 284, 284, 32.646, 22.473},
	{"ttft-staircase-c-on", 292, 147, 0, 0, 0, 286, 286, 28.938, 22.135},
	{"ttft-three-pods", 203, 102, 31, 0, 31, 451, 482, 4.676, 4.146},
}

// median is the median of xs, 0 with none.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

// The replicas the decode role asks for, with the ITL line's B pinned at the
// floor below the band (saturation.SwitchITLBaselineFromFloor) and without,
// on the recordings whose lines sit on the default B.
func TestGoldens_ITLBaselineFromFloorReplicas(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	for _, want := range floorReplicaTables {
		gz, err := os.ReadFile(filepath.Join(goldenDir, want.fixture, RecordingName))
		require.NoError(t, err)
		raw, err := gunzip(gz)
		require.NoError(t, err)
		replay := func(on bool) floorRun {
			rep, err := Replay(context.Background(), raw, Options{Mode: Normal,
				Switches: map[string]map[string]bool{PackageSaturation: {saturation.SwitchITLBaselineFromFloor: on}}})
			require.NoError(t, err)
			require.Empty(t, rep.Failed())
			outs, err := Outputs(rep.Recording)
			require.NoError(t, err)
			return readFloorRun(t, outs)
		}
		off, on := replay(false), replay(true)
		got := floorTable{fixture: want.fixture}
		var muOff, muOn []float64
		for key, o := range off {
			n, ok := on[key]
			if !ok {
				continue
			}
			got.cycles++
			if n.hasLine && n.intercept > 0 && math.Abs(n.intercept-0.006) > 1e-9 {
				got.floorB++
			}
			got.sumOff += o.wanted
			got.sumOn += n.wanted
			switch {
			case n.wanted < o.wanted:
				got.moved++
				got.down++
			case n.wanted > o.wanted:
				got.moved++
				got.up++
			}
			if o.hasLine && n.hasLine && o.mu > 0 && n.mu > 0 {
				muOff, muOn = append(muOff, o.mu), append(muOn, n.mu)
			}
		}
		got.muOff, got.muOn = median(muOff), median(muOn)
		var bOn []float64
		for _, n := range on {
			if n.hasLine && n.intercept > 0 && math.Abs(n.intercept-0.006) > 1e-9 {
				bOn = append(bOn, n.intercept)
			}
		}
		slices.Sort(bOn)
		if len(bOn) > 0 {
			t.Logf("%s: B off the default, with the switch on: min %.4f median %.4f max %.4f", want.fixture, bOn[0], median(bOn), bOn[len(bOn)-1])
		}
		t.Logf("%s: %d cycles, %d on a floor B; wanted replicas moved on %d (%d down, %d up), replica-cycles %d -> %d; median mu %.3f -> %.3f",
			got.fixture, got.cycles, got.floorB, got.moved, got.down, got.up, got.sumOff, got.sumOn, got.muOff, got.muOn)
		assert.Equal(t, want.cycles, got.cycles, want.fixture)
		assert.Equal(t, want.floorB, got.floorB, want.fixture)
		assert.Equal(t, want.moved, got.moved, want.fixture)
		assert.Equal(t, want.down, got.down, want.fixture)
		assert.Equal(t, want.up, got.up, want.fixture)
		assert.Equal(t, want.sumOff, got.sumOff, want.fixture)
		assert.Equal(t, want.sumOn, got.sumOn, want.fixture)
		assert.InDelta(t, want.muOff, got.muOff, 5e-4, want.fixture)
		assert.InDelta(t, want.muOn, got.muOn, 5e-4, want.fixture)
	}
}
