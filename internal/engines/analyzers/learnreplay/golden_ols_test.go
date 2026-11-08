package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
)

// olsFixture is the cluster golden whose load steps k across the ITL band,
// so its window holds the spread a full fit needs.
const olsFixture = "ols-sweep"

// olsSweepMinFullFits is the fewest full ITL fits ols-sweep's replay must
// take. At the commit the golden was added, the replay took 101 full and 94
// pinned fits, the same counts as the controller's own itl-fit lines of the
// run (tier=ols and tier=pinned-B). The floor is not that count: a learner
// change may move fits and rewrite lines.csv with make learner-goldens, which
// does not touch this number. What it must not do is lose the full fits this
// recording exists for. A PR that drops the replay below the floor says why
// in its description and lowers it here, keeping the 101/94 above as the
// recording-time reference.
const olsSweepMinFullFits = 80

// The ols-sweep golden's own replay takes full ITL fits, not only pinned
// ones. The summed path check is met by the in-process mu-paths run alone,
// and make learner-goldens rewrites lines.csv to whatever the replay gives,
// so without this a cluster recording that lost its full fits would pass.
func TestGoldens_OLSSweepReplayFitsOLS(t *testing.T) {
	gz, err := os.ReadFile(filepath.Join(goldenDir, olsFixture, RecordingName))
	require.NoError(t, err)
	rec, err := gunzip(gz)
	require.NoError(t, err)

	rep, err := Replay(context.Background(), rec, Options{Mode: Normal})
	require.NoError(t, err)
	require.Empty(t, rep.Failed())
	assert.GreaterOrEqual(t, rep.Paths[saturation.PathITLOLS], int64(olsSweepMinFullFits),
		"the sweep must still reach full ITL fits, not only pinned ones")
	assert.Positive(t, rep.Paths[saturation.PathITLPinned],
		"pinned fits are there too: the load holds still between its steps")

	// A head with no fit of any kind yet. What keeps it fit-free is the
	// steadiness and sample gate (itl.DefaultSteadyFor, DefaultMinSamples),
	// not the spread: the head already holds readings more than 0.30 of k
	// apart. The first line of either tier came at 22:02:17Z; cut at the
	// first cycle that begins after 22:01:00Z. Both are wall-clock times of
	// this recording: a re-take moves them (testing.md, ols-sweep).
	head := replayHeadBefore(t, rec, time.Date(2026, 11, 1, 22, 1, 0, 0, time.UTC))
	assert.Zero(t, head.Paths[saturation.PathITLOLS], "no full fit yet")
	assert.Zero(t, head.Paths[saturation.PathITLPinned], "no pinned fit yet")
	assert.Positive(t, head.Paths[saturation.PathRowsInstantKV], "precondition: the head offers readings to the window")
}
