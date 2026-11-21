package learnreplay

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// heldTable is what saturation.SwitchITLFloorBaselineHeld moves on one
// fixture, the floor below the band on both sides.
type heldTable struct {
	fixture string
	// cycles is the decode-pricing cycles with a demand on both sides; held
	// those whose line's B differs, the held floor's B on and the default
	// (or no line) off.
	cycles, held int
	// moved, down and up are the cycles whose wanted replicas differ, ask
	// fewer and ask more with the switch on; sumOff and sumOn the
	// replica-cycles asked.
	moved, down, up int
	sumOff, sumOn   int
	// muOff and muOn are the median mu over the cycles with a line on both
	// sides, in requests per second.
	muOff, muOn float64
}

// heldReplicaTables is each fixture's table as measured, frozen.
//
// e1, e1-tuned and pd-restart do not move: every cycle that pins through
// the floor reads one. The staircases do: on the cycles that read no floor B
// (the pinned fit too close to the floor's fill for its intercept, or the
// floor's readings aged out) the line went back to the 6 ms default; held,
// it keeps the last floor's B, and no replica ask moves. Three-pods does not
// move: the cycles it would hold are in a shape change, which ends the hold
// (heldSlope.floorBGone). No fixture asks for fewer replicas on any cycle.
var heldReplicaTables = []heldTable{
	{"e1", 762, 0, 0, 0, 0, 1439, 1439, 5.270, 5.270},
	{"e1-tuned", 748, 0, 0, 0, 0, 1387, 1387, 4.783, 4.783},
	{"pd-restart", 233, 0, 0, 0, 0, 830, 830, 6.294, 6.294},
	{"ttft-staircase-b-off", 289, 64, 0, 0, 0, 283, 283, 21.785, 21.785},
	{"ttft-staircase-b-on", 289, 23, 0, 0, 0, 284, 284, 22.473, 22.473},
	{"ttft-staircase-c-on", 292, 49, 0, 0, 0, 286, 286, 22.135, 22.049},
	{"ttft-three-pods", 203, 0, 0, 0, 0, 482, 482, 4.146, 4.146},
}

// The replicas the decode role asks for with the B last read from the floor
// held on the cycles that read none (saturation.SwitchITLFloorBaselineHeld)
// and without, the floor itself on both sides.
func TestGoldens_ITLFloorBaselineHeldReplicas(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	for _, want := range heldReplicaTables {
		gz, err := os.ReadFile(filepath.Join(goldenDir, want.fixture, RecordingName))
		require.NoError(t, err)
		raw, err := gunzip(gz)
		require.NoError(t, err)
		replay := func(on bool) floorRun {
			rep, err := Replay(context.Background(), raw, Options{Mode: Normal,
				Switches: map[string]map[string]bool{PackageSaturation: {
					saturation.SwitchITLBaselineFromFloor: true, saturation.SwitchITLFloorBaselineHeld: on}}})
			require.NoError(t, err)
			require.Empty(t, rep.Failed())
			outs, err := Outputs(rep.Recording)
			require.NoError(t, err)
			return readFloorRun(t, outs)
		}
		off, on := replay(false), replay(true)
		got := heldTable{fixture: want.fixture}
		var muOff, muOn []float64
		for key, o := range off {
			n, ok := on[key]
			if !ok {
				continue
			}
			got.cycles++
			if n.hasLine && (!o.hasLine || math.Abs(n.intercept-o.intercept) > 1e-12) {
				got.held++
			}
			got.sumOff += o.wanted
			got.sumOn += n.wanted
			switch {
			case n.wanted < o.wanted:
				got.moved++
				got.down++
				t.Logf("%s: %s asks %d, %d off (B %.4f, %.4f off; mu %.3f, %.3f off)", want.fixture, key, n.wanted, o.wanted,
					n.intercept, o.intercept, n.mu, o.mu)
			case n.wanted > o.wanted:
				got.moved++
				got.up++
				t.Logf("%s: %s asks %d, %d off (B %.4f, %.4f off; mu %.3f, %.3f off)", want.fixture, key, n.wanted, o.wanted,
					n.intercept, o.intercept, n.mu, o.mu)
			}
			if o.hasLine && n.hasLine && o.mu > 0 && n.mu > 0 {
				muOff, muOn = append(muOff, o.mu), append(muOn, n.mu)
			}
		}
		got.muOff, got.muOn = median(muOff), median(muOn)
		t.Logf("%s: %d cycles, %d on a held B; wanted replicas moved on %d (%d down, %d up), replica-cycles %d -> %d; median mu %.3f -> %.3f",
			got.fixture, got.cycles, got.held, got.moved, got.down, got.up, got.sumOff, got.sumOn, got.muOff, got.muOn)
		assert.Equal(t, want.cycles, got.cycles, want.fixture)
		assert.Equal(t, want.held, got.held, want.fixture)
		assert.Equal(t, want.moved, got.moved, want.fixture)
		assert.Equal(t, want.down, got.down, want.fixture)
		assert.Equal(t, want.up, got.up, want.fixture)
		assert.Equal(t, want.sumOff, got.sumOff, want.fixture)
		assert.Equal(t, want.sumOn, got.sumOn, want.fixture)
		assert.InDelta(t, want.muOff, got.muOff, 5e-4, want.fixture)
		assert.InDelta(t, want.muOn, got.muOn, 5e-4, want.fixture)
	}
}
