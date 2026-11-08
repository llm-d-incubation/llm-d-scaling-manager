package learnreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle"
)

// Learner PR 5d on the start-seed goldens' real rows. The three recordings
// were made before 5d, so the documents they restore hold no startCounted
// and no settle: replayed as committed, the Pods are folded again and every
// replica waits, whatever the switches say. This chain shows what a 5d
// build does on those rows. Each restart restores its own recorded document,
// plus what the replay of the run before it, on this build, would have stored
// (startCounted and the sightings).
//
// A recording's export names Pods per recording, so a Pod is carried across
// by its variant and its Ready time. Two Pods that share both are the same
// to everything read here.

// goldenRecording is one golden's exported recording, as it is committed.
func goldenRecording(t *testing.T, fixture string) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	rec, err := gunzip(gz)
	require.NoError(t, err)
	return rec
}

// chainRun is one replay of a chain link.
type chainRun struct {
	rep       *Report
	sightings map[string][]settle.Sighting
	state     saturation.LearnedState
	lines     []string
}

// replayChain replays recording with switches, logging at --v=3, and keeps
// what it would have stored.
func replayChain(t *testing.T, recording []byte, switches map[string]map[string]bool) chainRun {
	t.Helper()
	var run chainRun
	var last *replayer
	ctx := logr.NewContext(context.Background(), funcr.New(func(_, args string) {
		run.lines = append(run.lines, args)
	}, funcr.Options{Verbosity: 3}))
	rep, err := Replay(ctx, recording, Options{Mode: Normal, Switches: switches,
		stop: func(r *replayer, _ learnrec.RawEvent) bool {
			last = r
			return false
		}})
	require.NoError(t, err)
	require.NotNil(t, last)
	run.rep = rep
	run.sightings = last.settle.tracker.Sightings()
	run.state = last.sat.ExportLearnedState()
	return run
}

// chainRow is one replica on a recording's first analyzed cycle.
type chainRow struct {
	pod, variant string
	ready        time.Time
}

// firstRows is the replicas of recording's first Analyze, Ready time known.
func firstRows(t *testing.T, recording []byte) []chainRow {
	t.Helper()
	read, err := learnrec.Read(recording)
	require.NoError(t, err)
	for _, ev := range read.Events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d struct {
			Input struct {
				ReplicaMetrics []struct {
					PodName, VariantName string
					ReadySince           time.Time
				}
			}
		}
		require.NoError(t, json.Unmarshal(ev.Data, &d))
		out := make([]chainRow, 0, len(d.Input.ReplicaMetrics))
		for _, r := range d.Input.ReplicaMetrics {
			if !r.ReadySince.IsZero() {
				out = append(out, chainRow{r.PodName, r.VariantName, r.ReadySince})
			}
		}
		require.NotEmpty(t, out, "the first cycle reads no Ready replica")
		return out
	}
	t.Fatal("the recording has no Analyze")
	return nil
}

// spliced is recording with what prev would have stored in its restore: the
// counted Pods added to the recorded document, and a settle.Restore event
// after it, each Pod under the name this recording gives it.
func spliced(t *testing.T, recording []byte, prev chainRun) []byte {
	t.Helper()
	rows := firstRows(t, recording)
	read, err := learnrec.Read(recording)
	require.NoError(t, err)

	var out bytes.Buffer
	seq := 0
	write := func(now time.Time, kind string, data []byte) {
		at, err := json.Marshal(now)
		require.NoError(t, err)
		seq++
		fmt.Fprintf(&out, `{"Seq":%d,"Now":%s,"Kind":%q,"Data":%s}`+"\n", seq, at, kind, data)
	}
	restored := 0
	for _, ev := range read.Events {
		if ev.Kind != saturation.KindRestoreLearnedState {
			write(ev.Now, ev.Kind, ev.Data)
			continue
		}
		restored++
		var d saturation.RestoreEvent
		require.NoError(t, ev.DecodeData(&d))
		var s saturation.LearnedState
		require.NoError(t, d.State.Decode(&s))
		s.StartCounted = translatedCounts(t, s, prev.state, rows)
		d.State, err = learnrec.Marshal(s)
		require.NoError(t, err)
		data, err := learnrec.Marshal(d)
		require.NoError(t, err)
		write(ev.Now, ev.Kind, data)

		restore := settle.RestoreEvent{Interval: 15 * time.Second}
		for _, model := range slices.Sorted(maps.Keys(prev.sightings)) {
			namespace, modelID, _ := strings.Cut(model, "/")
			restore.Models = append(restore.Models, settle.RestoredModel{Namespace: namespace, ModelID: modelID,
				Sightings: translatedSightings(prev.sightings[model], rows)})
		}
		data, err = learnrec.Marshal(restore)
		require.NoError(t, err)
		write(ev.Now, settle.KindRestore, data)
	}
	require.Equal(t, 1, restored, "one restore per controller start")
	return out.Bytes()
}

// claim takes one row of variant at ready from rows, and says which Pod it
// names here.
func claim(rows []chainRow, variant string, ready time.Time) ([]chainRow, string, bool) {
	i := slices.IndexFunc(rows, func(r chainRow) bool { return r.variant == variant && r.ready.Equal(ready) })
	if i < 0 {
		return rows, "", false
	}
	pod := rows[i].pod
	return slices.Delete(slices.Clone(rows), i, i+1), pod, true
}

// translatedCounts is prev's counted Pods under this recording's names, for
// the start keys its document holds.
func translatedCounts(t *testing.T, doc, prev saturation.LearnedState, rows []chainRow) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	for _, key := range slices.Sorted(maps.Keys(prev.StartCounted)) {
		ready := prev.StartCounted[key]
		f := strings.Split(key, "|")
		require.Len(t, f, 7, key)
		base := strings.Join(f[:5], "|")
		if _, ok := doc.StartSeconds[base]; !ok {
			continue
		}
		var pod string
		var ok bool
		if rows, pod, ok = claim(rows, f[2], ready); ok {
			out[base+"|"+f[5]+"|"+pod] = ready
		}
	}
	require.NotEmpty(t, out, "no counted Pod carried across")
	return out
}

// translatedSightings is sightings under this recording's names.
func translatedSightings(sightings []settle.Sighting, rows []chainRow) []settle.Sighting {
	out := make([]settle.Sighting, 0, len(sightings))
	for _, s := range sightings {
		var pod string
		var ok bool
		if rows, pod, ok = claim(rows, s.VariantName, s.ReadySince); ok {
			s.PodName = pod
			out = append(out, s)
		}
	}
	return out
}

// exportedStarts is each exported state's start figures, in order.
func exportedStarts(t *testing.T, recording []byte) []map[string]float64 {
	t.Helper()
	read, err := learnrec.Read(recording)
	require.NoError(t, err)
	var out []map[string]float64
	for _, ev := range read.Events {
		if ev.Kind != saturation.KindExportLearnedState {
			continue
		}
		var s saturation.LearnedState
		require.NoError(t, ev.DecodeData(&s))
		out = append(out, s.StartSeconds)
	}
	require.NotEmpty(t, out, "no export")
	return out
}

// countOn is the figure field carries on the run's learner-start line.
func countOn(t *testing.T, run chainRun, field string) int {
	t.Helper()
	re := regexp.MustCompile(`"` + field + `"=(\d+)`)
	for _, l := range run.lines {
		if strings.Contains(l, `"msg"="learner-start"`) {
			m := re.FindStringSubmatch(l)
			require.Len(t, m, 2, "no %s on %s", field, l)
			n, err := strconv.Atoi(m[1])
			require.NoError(t, err)
			return n
		}
	}
	t.Fatal("no learner-start line")
	return 0
}

// restoredWrites is how many learner-write lines of the start learner say
// restored: true.
func restoredWrites(run chainRun) int {
	n := 0
	for _, l := range run.lines {
		if strings.Contains(l, `"msg"="learner-write"`) && strings.Contains(l, `"learner"="start"`) &&
			strings.Contains(l, `"restored"=true`) {
			n++
		}
	}
	return n
}

// The acceptance for 5d on the recorded fleet. Measured on the cluster, the
// slow variant's estimate moved 241.46 to 241.66 at the first restart and
// 241.66 to 241.77 at the second, because the running Pods were folded
// again, so no learner-write said restored: true.
//
// D on stops that fold, and D off reproduces those two figures, on the same
// spliced recording. With both on, start keys and restored sightings count
// above zero at both restarts.
func TestGoldens_StartSeedChainRestoresCountsAndFirstSight(t *testing.T) {
	prev := replayChain(t, goldenRecording(t, startSeedFixture), nil)
	require.NotEmpty(t, prev.state.StartCounted, "premise: a 5d build stores the counted Pods")
	require.NotEmpty(t, prev.sightings, "premise: a 5d build stores the sightings")

	for _, link := range []struct {
		fixture    string
		slow, fold float64
		// joined is the k2 readings the wait refuses with E off: none at
		// the second restart, whose first minute saturated no replica.
		joined int64
	}{
		{startSeedRestartFixture, 241.46, 241.66, 12},
		{startSeedRestart2Fixture, 241.66, 241.77, 0},
	} {
		t.Run(link.fixture, func(t *testing.T) {
			recording := spliced(t, goldenRecording(t, link.fixture), prev)

			on := replayChain(t, recording, nil)
			dOff := replayChain(t, recording, map[string]map[string]bool{
				PackageSaturation: {saturation.SwitchStartCountedPodsRestored: false}})
			eOff := replayChain(t, recording, map[string]map[string]bool{
				PackageSettle: {settle.SwitchFirstSightRestored: false}})

			slowest := func(starts map[string]float64) float64 { return slices.Max(slices.Collect(maps.Values(starts))) }
			gotOn, gotOff := slowest(exportedStarts(t, on.rep.Recording)[0]), slowest(exportedStarts(t, dOff.rep.Recording)[0])
			t.Logf("slow variant after the restart: %.4f with D, %.4f without; startKeys %d, sightings %d, restored writes %d, settle-restored %d; k2 refused as joined %d with E, %d without",
				gotOn, gotOff, countOn(t, on, "startKeys"), countOn(t, on, "sightings"), restoredWrites(on), on.rep.Paths[settlePathRestored],
				on.rep.Paths[saturation.PathK2UnsettledJoined], eOff.rep.Paths[saturation.PathK2UnsettledJoined])

			assert.InDelta(t, link.slow, gotOn, 0.01, "D on: the running Pods are not folded again")
			assert.InDelta(t, link.fold, gotOff, 0.01, "D off: the fold the cluster run measured")

			assert.Equal(t, 2, countOn(t, on, "startKeys"), "both start keys")
			assert.Positive(t, countOn(t, on, "sightings"))
			assert.Positive(t, on.rep.Paths[settlePathRestored])
			assert.Positive(t, restoredWrites(on), "a restored estimate says so until it admits a start")
			assert.Zero(t, restoredWrites(dOff), "D off: the fold ends the restored mark on the first cycle")

			assert.Zero(t, on.rep.Paths[saturation.PathK2UnsettledJoined], "E on: no replica here waits")
			assert.Equal(t, link.joined, eOff.rep.Paths[saturation.PathK2UnsettledJoined], "E off: every replica waits")
			assert.Zero(t, countOn(t, eOff, "sightings"), "E off: no sighting comes back")
			assert.Zero(t, eOff.rep.Paths[settlePathRestored])
			assert.Equal(t, 2, countOn(t, eOff, "startKeys"), "E off leaves the start keys")
			prev = on
		})
	}
}

// Replayed as committed, neither switch has anything to act on: the recorded
// documents hold no startCounted, and no recording holds a settle.Restore
// event. So the goldens themselves still show the fold, and only the chain
// above shows it stop.
func TestGoldens_StartSeedRecordingsPredate5d(t *testing.T) {
	for _, fixture := range startSeedFixtures {
		for _, ev := range startSeedEvents(t, fixture) {
			assert.NotEqual(t, settle.KindRestore, ev.Kind, fixture)
			if ev.Kind != saturation.KindRestoreLearnedState {
				continue
			}
			var d saturation.RestoreEvent
			require.NoError(t, ev.DecodeData(&d))
			var s saturation.LearnedState
			require.NoError(t, d.State.Decode(&s))
			assert.Empty(t, s.StartCounted, fixture)
		}
	}
}
