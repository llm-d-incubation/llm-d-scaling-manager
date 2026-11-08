package learnreplay

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The ols-sweep-slo goldens are one cluster run of the ols-sweep recipe with
// the slo analyzer listed beside saturation (a policy tier with TPOT and TTFT
// targets) and a streamed load, so the router measured the latency the slo
// analyzer judges. The leader controller Pod was deleted under load, which
// split the run in two recordings: ols-sweep-slo up to the restart and
// ols-sweep-slo-restart after it (docs/developer-guide/testing.md).
const (
	olsSLOFixture        = "ols-sweep-slo"
	olsSLORestartFixture = "ols-sweep-slo-restart"
)

// olsSLOFloor is what one of the two recordings' own replays must still
// reach. At the commit they were added, each replay took the counts in the
// comment beside its floor, the same as the controller's own log of the run
// (itl-fit tier=ols and tier=pinned-B; slo-variant lineTier=ols and
// pinned-B; slo-model lines with a finite observed figure). The two analyzers
// fit from the same rows with the same rules, each with its own tracker, so
// their counts agree. A floor sits about a fifth under its count: a learner
// change may move fits and judgements (make learner-goldens rewrites
// lines.csv and slo-published.csv and does not touch these), but must not
// lose the fits and the judged windows these recordings exist for. A PR that
// drops a replay below a floor says why and lowers it here, keeping the
// counts as the recording-time reference. A re-take moves every number and
// time below to the new run's (testing.md).
type olsSLOFloor struct {
	fixture string
	// satOLS and sloOLS are the fewest full fits of each analyzer's line,
	// and satPinned and sloPinned the fewest pinned ones.
	satOLS, sloOLS, satPinned, sloPinned int64
	// routerServed is the fewest slo Analyze calls of the recording the
	// router served latency to: TPOT requests and a TTFT histogram both
	// present. An input: only a new recording moves it.
	routerServed int
	// judgedTPOT and judgedTTFT are the fewest objectives of each kind the
	// replay's own slo Analyze calls judged: a finite observed figure, and
	// not excluded. An output: an analyzer change moves it.
	judgedTPOT, judgedTTFT int
	// cut is the last moment before either analyzer's first fit of any
	// tier: the head replay ends with the cycle just before that fit, so a
	// change that admits a fit even one cycle earlier (the sample gate, the
	// steadiness gate, the pinned fit's decline) leaves a fit in the head.
	// The replay is deterministic, so the head is the same on every run.
	// The time is this recording's wall clock.
	cut time.Time
}

var olsSLOFloors = []olsSLOFloor{
	// Recorded 13:40:07Z to 14:00:43Z: 15 full and 33 pinned fits for each
	// analyzer, the first a full one on the cycle at 13:48:55.69Z, after a
	// cycle at 13:48:40.62Z; declined from 13:43:38Z. 79 of 83 slo Analyze
	// calls served router latency, and 55 of them judged TPOT and TTFT (the
	// 24 before 13:47:10Z were excluded as a transient).
	{fixture: olsSLOFixture, satOLS: 12, sloOLS: 12, satPinned: 26, sloPinned: 26,
		routerServed: 63, judgedTPOT: 44, judgedTTFT: 44,
		cut: time.Date(2026, 11, 2, 13, 48, 50, 0, time.UTC)},
	// Recorded 14:01:02Z to 14:36:35Z, after the restart: 57 full and 66
	// pinned fits for each analyzer, the first a full one on the cycle at
	// 14:06:02.55Z, after a cycle at 14:05:47.53Z; declined from 14:03:17Z.
	// All 143 slo Analyze calls served router latency, and 115 judged TPOT
	// and 113 TTFT.
	// The floor fell from 45 full fits to 35 with the tier dwell (issue #495):
	// it refuses a full fit no higher than the pinned fit, and each line takes
	// 42 of the recording's 52.
	{fixture: olsSLORestartFixture, satOLS: 35, sloOLS: 35, satPinned: 52, sloPinned: 52,
		routerServed: 114, judgedTPOT: 92, judgedTTFT: 90,
		cut: time.Date(2026, 11, 2, 14, 6, 0, 0, time.UTC)},
}

// Each ols-sweep-slo recording's own replay takes full and pinned fits of
// both lines, the saturation analyzer's and the slo analyzer's, and judges
// TPOT and TTFT from router latency: a cluster golden of the slo analyzer
// that lost them would pass the summed path check and the row comparison,
// which make learner-goldens rewrites to whatever the replay gives. The head
// before the first fit is the negative control: the same recording, cut,
// reaches none.
func TestGoldens_OLSSweepSLOReplayFitsBothLines(t *testing.T) {
	for _, f := range olsSLOFloors {
		t.Run(f.fixture, func(t *testing.T) {
			rec := readGoldenRecording(t, f.fixture)
			rep, err := Replay(context.Background(), rec, Options{Mode: Normal})
			require.NoError(t, err)
			require.Empty(t, rep.Failed())
			judged := sloJudged(t, rep.Recording)
			served := sloAnalyzesWithRouterLatency(t, rec)
			t.Logf("%s: saturation ols %d pinned %d; slo ols %d pinned %d; slo Analyze with router latency %d; judged tpot %d ttft %d",
				f.fixture, rep.Paths[saturation.PathITLOLS], rep.Paths[saturation.PathITLPinned],
				rep.Paths[slo.PathITLOLS], rep.Paths[slo.PathITLPinned], served,
				judged[constants.SLOObjectiveTPOT], judged[constants.SLOObjectiveTTFT])

			assert.GreaterOrEqual(t, rep.Paths[saturation.PathITLOLS], f.satOLS, "saturation's line still comes from full fits")
			assert.GreaterOrEqual(t, rep.Paths[slo.PathITLOLS], f.sloOLS, "the slo analyzer's line still comes from full fits")
			assert.GreaterOrEqual(t, rep.Paths[saturation.PathITLPinned], f.satPinned,
				"pinned saturation fits too: the load holds still between steps")
			assert.GreaterOrEqual(t, rep.Paths[slo.PathITLPinned], f.sloPinned, "pinned slo fits too")
			assert.GreaterOrEqual(t, served, f.routerServed,
				"the router served TPOT and TTFT to the slo analyzer: the load streamed")
			assert.GreaterOrEqual(t, judged[constants.SLOObjectiveTPOT], f.judgedTPOT, "the replay still judges TPOT from the router's latency")
			assert.GreaterOrEqual(t, judged[constants.SLOObjectiveTTFT], f.judgedTTFT, "the replay still judges TTFT from the router's latency")

			head := replayHeadBefore(t, rec, f.cut)
			assert.Zero(t, head.Paths[saturation.PathITLOLS], "no saturation full fit before the cut")
			assert.Zero(t, head.Paths[saturation.PathITLPinned], "no saturation pinned fit before the cut")
			assert.Zero(t, head.Paths[slo.PathITLOLS], "no slo full fit before the cut")
			assert.Zero(t, head.Paths[slo.PathITLPinned], "no slo pinned fit before the cut")
			assert.Positive(t, head.Paths[saturation.PathRowsInstantKV], "precondition: the head offers readings to the window")
			assert.Positive(t, sloDeclined(t, head.Recording),
				"precondition: the head reaches the pinned fit's decline, the verdict just before the first fit")
		})
	}
}

// The restart golden starts from nothing: the controller came back with the
// learned-state snapshot off, so its recording starts with an empty store and
// restores no learned state. A replay refuses a non-empty start; a restore
// would replay, and could pass every floor above. The acceptance golden,
// whose run restores a snapshot, is the negative control: the same reading
// finds its restore.
func TestGoldens_OLSSweepSLORestartRestoresNothing(t *testing.T) {
	start, restores := startAndRestores(t, readGoldenRecording(t, olsSLORestartFixture))
	assert.Zero(t, start.StoreRecords, "the restart recording starts with an empty store")
	assert.Empty(t, restores, "the restart recording restores no learned state: the snapshot was off")

	_, restores = startAndRestores(t, readGoldenRecording(t, "acceptance"))
	assert.Positive(t, restores[saturation.KindRestoreLearnedState],
		"negative control: the acceptance run's restore is read as one")
}

// startAndRestores is rec's start line, and its count of each learned-state
// restore event.
func startAndRestores(t *testing.T, rec []byte) (learnrec.Start, map[string]int) {
	t.Helper()
	read, err := learnrec.Read(rec)
	require.NoError(t, err)
	require.NotEmpty(t, read.Events)
	require.Equal(t, learnrec.KindStart, read.Events[0].Kind, "precondition: the recording opens with its start")
	var start learnrec.Start
	require.NoError(t, read.Events[0].DecodeData(&start))
	restores := map[string]int{}
	for _, ev := range read.Events {
		if ev.Kind == saturation.KindRestoreLearnedState || ev.Kind == saturation.KindTakeRestoreOutcomes {
			restores[ev.Kind]++
		}
	}
	return start, restores
}

// readGoldenRecording is the named golden's recording, unzipped.
func readGoldenRecording(t *testing.T, fixture string) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	rec, err := gunzip(gz)
	require.NoError(t, err)
	return rec
}

// sloAnalyzeEvents is every slo Analyze event of rec, decoded.
func sloAnalyzeEvents(t *testing.T, rec []byte) []slo.AnalyzeEvent {
	t.Helper()
	read, err := learnrec.Read(rec)
	require.NoError(t, err)
	var out []slo.AnalyzeEvent
	for _, ev := range read.Events {
		if ev.Kind != slo.KindAnalyze {
			continue
		}
		var a slo.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&a))
		out = append(out, a)
	}
	return out
}

// sloAnalyzesWithRouterLatency counts the slo Analyze events of rec that the
// router served latency to: a TPOT request rate and a TTFT histogram.
func sloAnalyzesWithRouterLatency(t *testing.T, rec []byte) int {
	t.Helper()
	n := 0
	for _, a := range sloAnalyzeEvents(t, rec) {
		if a.RouterLatency != nil && a.RouterLatency.TPOTCountRate > 0 && len(a.RouterLatency.TTFTBuckets) > 0 {
			n++
		}
	}
	return n
}

// sloJudged counts, per objective, what the slo Analyze events of rec
// published as judged: a finite observed figure, not excluded. Read from a
// replay's own recording, it is what the replay judged.
func sloJudged(t *testing.T, rec []byte) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, a := range sloAnalyzeEvents(t, rec) {
		if a.Published == nil {
			continue
		}
		for _, o := range a.Published.Objectives {
			if o.Excluded == "" && !math.IsNaN(o.Observed) && !math.IsInf(o.Observed, 0) {
				out[o.Objective]++
			}
		}
	}
	return out
}

// sloDeclined counts the variants the slo Analyze events of rec priced from
// no line because the pinned fit declined.
func sloDeclined(t *testing.T, rec []byte) int {
	t.Helper()
	n := 0
	for _, a := range sloAnalyzeEvents(t, rec) {
		if a.Published == nil {
			continue
		}
		for _, v := range a.Published.Variants {
			if v.Source == constants.SLOSourceDeclined {
				n++
			}
		}
	}
	return n
}
