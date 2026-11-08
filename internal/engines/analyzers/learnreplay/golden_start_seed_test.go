package learnreplay

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// startSeedEvents is one start-seed golden's exported recording, in order.
func startSeedEvents(t *testing.T, fixture string) []learnrec.RawEvent {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	rec, err := gunzip(gz)
	require.NoError(t, err)
	events, err := readEvents(rec)
	require.NoError(t, err)
	return events
}

// The first start-seed golden exercises the seed path. Exactly one variant
// carries the seed, 120 s. Share pacing reads the seed for more than one
// cycle while that variant has no measured Pod (12 cycles on the run), and
// then that variant's first Pod, 241 s by the Pods' own timestamps. Nothing
// is restored: the controller started with no snapshot. A golden that held
// none of this would pass every other test.
func TestGoldens_StartSeedHoldsTheSeedAndTheMeasuredStart(t *testing.T) {
	const seedSeconds, firstStart = 120.0, 241.0
	seeded := ""
	cycle, atSeed, lastSeed, firstMeasured, restored := 0, 0, -1, -1, 0
	for _, ev := range startSeedEvents(t, startSeedFixture) {
		if ev.Kind == saturation.KindRestoreLearnedState {
			restored++
		}
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		require.NotNil(t, d.Result)
		in, err := decodeInput(d.Input)
		require.NoError(t, err)
		for _, st := range in.VariantStates {
			if !st.StartSeedSet {
				continue
			}
			require.InDelta(t, seedSeconds, st.StartSeedSeconds, 0, "the seed the ScaledObject set")
			if seeded == "" {
				seeded = st.VariantName
			}
			require.Equal(t, seeded, st.VariantName, "one variant carries the seed")
		}
		measured := 0.0
		for _, row := range in.ReplicaMetrics {
			if row.VariantName == seeded && row.StartSeconds > 0 {
				measured = row.StartSeconds
			}
		}
		switch d.Result.RoleStartSeconds[domain.RoleBoth] {
		case seedSeconds:
			require.Zero(t, measured, "cycle %d: the seed is read with the seeded variant measured", cycle)
			atSeed++
			lastSeed = cycle
		case firstStart:
			if firstMeasured < 0 {
				require.InDelta(t, firstStart, measured, 1, "cycle %d: the first measured start is the seeded variant's own", cycle)
				firstMeasured = cycle
			}
		}
		cycle++
	}
	require.NotEmpty(t, seeded, "no variant carries the seed")
	assert.Greater(t, atSeed, 1, "share pacing read the seed for one cycle or none")
	require.GreaterOrEqual(t, lastSeed, 0, "share pacing never read the seed")
	require.GreaterOrEqual(t, firstMeasured, 0, "share pacing never read the first measured start")
	assert.Less(t, lastSeed, firstMeasured, "the seed is read after the seeded variant's measured start")
	assert.Zero(t, restored, "the first controller start restored something")
}

// Each restart golden restores both start keys, at figures the Pods' own
// timestamps agree with: 20 to 22 s for the fast variant, 241 to 242 s for
// the slow one.
func TestGoldens_StartSeedRestartsRestoreBothStartKeys(t *testing.T) {
	for _, fixture := range []string{startSeedRestartFixture, startSeedRestart2Fixture} {
		t.Run(fixture, func(t *testing.T) {
			restored := 0
			for _, ev := range startSeedEvents(t, fixture) {
				if ev.Kind != saturation.KindRestoreLearnedState {
					continue
				}
				restored++
				var d saturation.RestoreEvent
				require.NoError(t, ev.DecodeData(&d))
				var state saturation.LearnedState
				require.NoError(t, d.State.Decode(&state))
				figures := slices.Sorted(maps.Values(state.StartSeconds))
				require.Len(t, figures, 2, "both start keys")
				assert.InDelta(t, 21, figures[0], 1, "the fast variant")
				assert.InDelta(t, 241.5, figures[1], 0.5, "the slow variant")
			}
			assert.Equal(t, 1, restored, "one restore per controller start")
		})
	}
}
