package learnreplay

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The throughput-sweep goldens are one cluster run of the recipe in
// docs/developer-guide/testing.md (learner goldens) with the throughput
// analyzer listed beside saturation: a P/D decode fleet under the closed-loop
// sweep, the leader controller Pod deleted 20 min in, so the run gives two
// recordings, throughput-sweep and throughput-sweep-restart
// (analyzer-learner.md, PR 4b, "Recordings"). They are the only goldens whose
// throughput line sees a real decode engine: the wait after a controller
// start, readings above the band, full fits, and a replica at its sequence
// cap. No joiner is recorded: the one decode pod never scales. The run's
// policy band is 0.12-0.70, not the default.
const (
	throughputSweepFixture        = "throughput-sweep"
	throughputSweepRestartFixture = "throughput-sweep-restart"
)

// throughputSweepFloor is which of the throughput line's paths a recording's
// own replay must reach. Each must be above zero (2.5's per-path counters):
// full and pinned fits and settling readings in both, and readings admitted
// at the sequence cap in the restart recording, whose load reaches the cap.
// A floor is a count, not a rate. At the commit that added them the replay
// counted 18 full, 9 pinned and 5 settling in throughput-sweep, and 10 full,
// 14 pinned, 8 settling and 20 at the cap in throughput-sweep-restart; a PR
// that lowers a floor says why.
//
// Every settling reading in both is the wait after a controller start: a
// replica already serving when the controller started
// (throughput.SwitchITLWaitsAfterRestart). With that switch off the path counts
// 0 in both, though no output file moves, so a settling floor is all that
// holds it here.
//
// Every pinned fit in the restart recording comes from the cap's readings:
// with throughput.SwitchITLAdmitsAtSequenceCap off the path counts 0 pinned
// there, and 7 above-band fits take their place.
//
// Since the tier dwell (issue #495) throughput-sweep's 9 pinned fits are gone:
// the spread collapses inside the dwell, and the line holds its slope, 8 times.
// Its floor is the held path in their place; the restart recording still
// floors the pinned one. The restart recording's light load holds nothing: it
// ends with 35 minutes of it, and the line stayed silent through them with cause
// shape-change, since a shape change drops the held slope with the window.
type throughputSweepFloor struct {
	fixture string
	// paths is the fewest runs of each path, by throughput.PathNames.
	paths map[string]int64
	// restsOn is, for a switch a floor rests on, the paths its replay with
	// that switch off must fail, and only those, sorted.
	restsOn map[string][]string
}

var throughputSweepFloors = []throughputSweepFloor{
	{fixture: throughputSweepFixture, paths: map[string]int64{
		throughput.PathITLOLS: 1, throughput.PathITLHeld: 1, throughput.PathITLSettling: 1},
		restsOn: map[string][]string{throughput.SwitchITLWaitsAfterRestart: {throughput.PathITLSettling}}},
	{fixture: throughputSweepRestartFixture, paths: map[string]int64{
		throughput.PathITLOLS: 1, throughput.PathITLPinned: 1, throughput.PathITLSettling: 1, throughput.PathITLAtCap: 1},
		restsOn: map[string][]string{
			throughput.SwitchITLWaitsAfterRestart:   {throughput.PathITLSettling},
			throughput.SwitchITLAdmitsAtSequenceCap: {throughput.PathITLAtCap, throughput.PathITLPinned},
		}},
}

// throughputFloorProblems names each path of floor a replay's counts fall
// short of, sorted by path.
func throughputFloorProblems(f throughputSweepFloor, paths map[string]int64) []string {
	var out []string
	for _, p := range slices.Sorted(maps.Keys(f.paths)) {
		if paths[p] < f.paths[p] {
			out = append(out, fmt.Sprintf("%s: the throughput line's path %s ran %d times, under its floor of %d",
				f.fixture, p, paths[p], f.paths[p]))
		}
	}
	return out
}

// Each throughput-sweep recording's own replay reaches every path of its
// floor: a cluster golden of the throughput line that lost its full fits,
// its settling stamp or its cap admission would still pass the summed path
// check and the row comparison, which make learner-goldens rewrites. The
// same replay with the line's steady gate off (the parent's line, which
// counts nothing) shows only that a silent counter fails. So a floor that
// rests on one switch (restsOn) is replayed with that switch off too, the rest
// of the line on: the paths it names must fail, and no others.
func TestGoldens_ThroughputSweepFloors(t *testing.T) {
	for _, f := range throughputSweepFloors {
		t.Run(f.fixture, func(t *testing.T) {
			rec := readGoldenRecording(t, f.fixture)
			read, err := learnrec.Read(rec)
			require.NoError(t, err)
			var start learnrec.Start
			require.NoError(t, read.Events[0].DecodeData(&start))
			require.True(t, start.Throughput, "precondition: the run listed the throughput analyzer")

			rep, err := Replay(context.Background(), rec, Options{Mode: Normal})
			require.NoError(t, err)
			require.Empty(t, rep.Failed())
			t.Logf("%s: throughput line paths %v", f.fixture, throughputPaths(rep.Paths))
			assert.Empty(t, throughputFloorProblems(f, rep.Paths))

			off, err := Replay(context.Background(), rec, Options{Mode: Normal, Switches: map[string]map[string]bool{
				PackageThroughput: {throughput.SwitchITLSteadyGate: false}}})
			require.NoError(t, err)
			assert.Len(t, throughputFloorProblems(f, off.Paths), len(f.paths),
				"negative control: the parent's line reaches none of the floor's paths")

			for name, paths := range f.restsOn {
				sw, err := Replay(context.Background(), rec, Options{Mode: Normal, Switches: map[string]map[string]bool{
					PackageThroughput: {name: false}}})
				require.NoError(t, err)
				t.Logf("%s: %s off, throughput line paths %v", f.fixture, name, throughputPaths(sw.Paths))
				want := make([]string, 0, len(paths))
				for _, p := range paths {
					want = append(want, fmt.Sprintf("%s: the throughput line's path %s ran 0 times, under its floor of %d",
						f.fixture, p, f.paths[p]))
				}
				assert.Equal(t, want, throughputFloorProblems(f, sw.Paths), "%s off", name)
			}
		})
	}
}

// throughputPaths is the throughput line's counts out of a replay's paths.
func throughputPaths(paths map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for _, p := range throughput.PathNames() {
		out[p] = paths[p]
	}
	return out
}

// The floor check itself, on a fixture that exists now: mu-paths runs the
// throughput analyzer in-process. With the steady gate on its replay reaches
// full and pinned fits; with it off, neither, and the check names each. Its
// settling count is zero by construction (no joiner warms up past the
// line's admission there), so a floor on it fails on either: the reason the
// required set waits for the recording.
func TestGoldens_TheThroughputFloorCheckFails(t *testing.T) {
	rec := readGoldenRecording(t, "mu-paths")
	floor := throughputSweepFloor{fixture: "mu-paths", paths: map[string]int64{
		throughput.PathITLOLS: 1, throughput.PathITLPinned: 1}}

	on, err := Replay(context.Background(), rec, Options{Mode: Normal})
	require.NoError(t, err)
	t.Logf("mu-paths, line on the tracker: %v", throughputPaths(on.Paths))
	assert.Empty(t, throughputFloorProblems(floor, on.Paths))

	off, err := Replay(context.Background(), rec, Options{Mode: Normal, Switches: map[string]map[string]bool{
		PackageThroughput: {throughput.SwitchITLSteadyGate: false}}})
	require.NoError(t, err)
	t.Logf("mu-paths, the parent's line: %v", throughputPaths(off.Paths))
	got := throughputFloorProblems(floor, off.Paths)
	require.Len(t, got, 2, "%v", got)
	assert.Contains(t, got[0], "mu-paths: the throughput line's path throughput-itl-ols ran 0 times, under its floor of 1")

	settling := throughputSweepFloor{fixture: "mu-paths", paths: map[string]int64{throughput.PathITLSettling: 1}}
	assert.Len(t, throughputFloorProblems(settling, on.Paths), 1, "the in-process goldens stamp no reading settling")
}
