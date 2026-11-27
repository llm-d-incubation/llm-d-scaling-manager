package learnreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// restoredFloorBFixture is the golden whose controller restart restores ITL
// slopes: two decode keys with no learned B, each holding the A it fitted
// before the restart.
const restoredFloorBFixture = "start-seed-restart-2"

// withRestoredFloorB is recording with every ITL slope its restore carries
// pinned through floorB, as a controller with saturation.SwitchITLBaselineFromFloor
// on exports one, stamped when it was fitted. No golden holds such a restore
// (issue llm-d-incubation/llm-d-scaling-manager#373).
func withRestoredFloorB(t *testing.T, recording []byte, floorB float64) []byte {
	t.Helper()
	read, err := learnrec.Read(recording)
	require.NoError(t, err)
	var out bytes.Buffer
	seq, slopes := 0, 0
	write := func(now time.Time, kind string, data []byte) {
		at, err := json.Marshal(now)
		require.NoError(t, err)
		seq++
		fmt.Fprintf(&out, `{"Seq":%d,"Now":%s,"Kind":%q,"Data":%s}`+"\n", seq, at, kind, data)
	}
	for _, ev := range read.Events {
		if ev.Kind != saturation.KindRestoreLearnedState {
			write(ev.Now, ev.Kind, ev.Data)
			continue
		}
		var d saturation.RestoreEvent
		require.NoError(t, ev.DecodeData(&d))
		var s saturation.LearnedState
		require.NoError(t, d.State.Decode(&s))
		for k, sl := range s.ITLSlopes {
			sl.FloorB, sl.FloorBAt = floorB, sl.FittedAt
			s.ITLSlopes[k] = sl
			slopes++
		}
		d.State, err = learnrec.Marshal(s)
		require.NoError(t, err)
		data, err := learnrec.Marshal(d)
		require.NoError(t, err)
		write(ev.Now, ev.Kind, data)
	}
	require.Positive(t, slopes, "the restore carries slopes to pin through the floor's B")
	return out.Bytes()
}

// restoredFloorLines counts, over every cycle of recording replayed for the
// planner with sw over PlannerSwitches, the decode lines and the ones priced
// through floorB.
func restoredFloorLines(t *testing.T, recording []byte, floorB float64, sw map[string]map[string]bool) (lines, atFloor int) {
	t.Helper()
	every := func(last int) []int {
		cs := make([]int, 0, last)
		for c := 1; c < last; c++ {
			cs = append(cs, c)
		}
		return cs
	}
	st, early, err := StatesFrom(context.Background(), BytesSource(recording), Options{Switches: sw},
		StateQuery{Namespace: "ns-01", ModelID: "model-01"}, every)
	require.NoError(t, err)
	states := make([]*State, 0, len(early)+1)
	states = append(states, st)
	for _, s := range early {
		states = append(states, s)
	}
	for _, s := range states {
		for _, vs := range s.Variants {
			if vs.Tier == "" {
				continue
			}
			lines++
			if math.Abs(vs.Line.B-floorB) < 1e-12 {
				atFloor++
			}
		}
	}
	return lines, atFloor
}

// The planner's replay turns the floor's B off (PlannerSwitches), and a floor
// B restored from a snapshot stays off with it: no line of the replay is
// priced through it. Before issue llm-d-incubation/llm-d-scaling-manager#373
// the restored slope was held with the B it was fitted through whatever the
// switch said, and every line of this recording was priced through the
// restored floor B. The slope is the floor line's, so it is not held on the
// default B either: this recording's decode keys never fit a line of their
// own after the restart, and the planner reads none. The same restore without
// a floor's B is held on the default, as before.
//
// With the switch set on, as the controller runs, the restored B still prices
// the held lines, which is how the test knows the restore reaches the
// planner's state at all.
func TestPlannerSwitches_ARestoredFloorBIsOffInThePlanner(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	// Under the 6 ms default, so a line through it reads TPOT low.
	const floorB = 0.0042
	golden := goldenRecording(t, restoredFloorBFixture)
	rec := withRestoredFloorB(t, golden, floorB)

	held, onFloor := restoredFloorLines(t, golden, floorB, nil)
	require.Positive(t, held, "as committed, the planner holds the restored slopes")
	assert.Zero(t, onFloor)

	lines, atFloor := restoredFloorLines(t, rec, floorB, nil)
	assert.Zero(t, atFloor, "the planner priced %d of %d lines through the restored floor B", atFloor, lines)
	assert.Zero(t, lines, "the floor line's slope is held on another B")

	linesOn, atFloorOn := restoredFloorLines(t, rec, floorB,
		map[string]map[string]bool{PackageSaturation: {saturation.SwitchITLBaselineFromFloor: true}})
	require.Positive(t, linesOn)
	assert.Equal(t, linesOn, atFloorOn, "with the switch set on, the restored floor B prices the held lines")
	t.Logf("as committed: %d lines held; restored floor B: %d lines, %d through it; switch on: %d lines, %d through it",
		held, lines, atFloor, linesOn, atFloorOn)
}
