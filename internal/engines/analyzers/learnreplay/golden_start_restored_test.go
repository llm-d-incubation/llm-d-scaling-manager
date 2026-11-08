package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// startRestoredFixture is the kind recording made by a 5d build, from the
// controller restart under load: unlike the start-seed goldens, its restore
// holds what the build before it stored.
const startRestoredFixture = "start-restored"

// startRestoredEvents is the golden's restore: the document as restored, the
// analyzer's report of it, and how many sightings the tracker was handed.
func startRestoredEvents(t *testing.T, recording []byte) (saturation.LearnedState, saturation.RestoreReport, int) {
	t.Helper()
	read, err := learnrec.Read(recording)
	require.NoError(t, err)
	var state saturation.LearnedState
	var report saturation.RestoreReport
	restores, settles, sightings := 0, 0, 0
	for _, ev := range read.Events {
		switch ev.Kind {
		case saturation.KindRestoreLearnedState:
			restores++
			var d saturation.RestoreEvent
			require.NoError(t, ev.DecodeData(&d))
			require.NoError(t, d.State.Decode(&state))
			report = d.Report
		case settle.KindRestore:
			settles++
			var d settle.RestoreEvent
			require.NoError(t, ev.DecodeData(&d))
			for _, m := range d.Models {
				sightings += len(m.Sightings)
			}
		}
	}
	require.Equal(t, 1, restores, "one restore per controller start")
	require.Equal(t, 1, settles, "the sightings came back in a real restore")
	return state, report, sightings
}

// A real restart of a 5d build: the document holds the counted Pods and the
// sightings its controller wrote, nothing here is spliced, and the fold the
// start-seed goldens show is gone. Without D the same recording folds the
// running Pods again; without E no sighting comes back.
func TestGoldens_StartRestoredRestoresForReal(t *testing.T) {
	recording := goldenRecording(t, startRestoredFixture)
	state, report, sightings := startRestoredEvents(t, recording)
	require.NotEmpty(t, state.StartCounted, "the document holds the counted Pods")
	require.Positive(t, sightings, "the document holds the sightings")
	require.Positive(t, report.StartKeys, "the restore read the start keys")

	on := replayChain(t, recording, nil)
	assert.Positive(t, countOn(t, on, "startKeys"))
	assert.Positive(t, countOn(t, on, "sightings"))
	assert.Positive(t, on.rep.Paths[settlePathRestored])
	assert.Positive(t, restoredWrites(on), "a restored estimate says so until it admits a start")

	first := exportedStarts(t, on.rep.Recording)[0]
	require.NotEmpty(t, first)
	for key, restored := range state.StartSeconds {
		assert.Equal(t, restored, first[key], "%s: the running Pods were folded again", key)
	}

	t.Logf("startKeys %d, sightings %d, restored writes %d, k2 refused as joined %d",
		countOn(t, on, "startKeys"), countOn(t, on, "sightings"), restoredWrites(on), on.rep.Paths[saturation.PathK2UnsettledJoined])

	dOff := replayChain(t, recording, map[string]map[string]bool{
		PackageSaturation: {saturation.SwitchStartCountedPodsRestored: false}})
	folded := exportedStarts(t, dOff.rep.Recording)[0]
	moved := 0
	for key, restored := range state.StartSeconds {
		if folded[key] != restored {
			moved++
		}
	}
	assert.Positive(t, moved, "D off: the same recording folds the running Pods")
	assert.Zero(t, restoredWrites(dOff))

	eOff := replayChain(t, recording, map[string]map[string]bool{
		PackageSettle: {settle.SwitchFirstSightRestored: false}})
	t.Logf("E off: k2 refused as joined %d", eOff.rep.Paths[saturation.PathK2UnsettledJoined])
	assert.Zero(t, countOn(t, eOff, "sightings"))
	assert.Zero(t, eOff.rep.Paths[settlePathRestored])
}
