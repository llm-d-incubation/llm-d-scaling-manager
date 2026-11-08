package steadystate

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// Back-compat fixtures: a learned-state document as v0.10.0 wrote it and one
// as v0.10.1 (and v0.10.2) wrote it, each restored by the current code.
//
// The two files in testdata/learned-state are FROZEN ARTIFACTS: they stand
// for what those releases wrote, so nothing in this package rewrites or
// checks them against today's code (see the README beside them). They are
// HAND-BUILT, not captured from a cluster: no capture of either release's
// ConfigMap exists. TestLearnedStateFixtures_Generate built them, once, and
// runs only with LEARNED_STATE_WRITE_FIXTURES=1: from the last learned state
// the mu-paths golden recording exported (a real analyzer's state),
// projected onto the frozen v0.10.1 types (learned_state_v0101_test.go),
// with an entry added by hand, in those types and in the key formats
// v0.10.1 wrote, to every map the export left empty (withEveryMap), and
// encoded with encoding/json as v0.10.1's encoder does (json.Marshal of
// learnedStateDoc). The v0.10.0 document is the same less itlSlopes, which
// v0.10.0 did not have (v0.10.0, 823ddcd74, differs from v0.10.1 in these
// types by that field alone).
const (
	fixtureDir       = "testdata/learned-state"
	fixtureV0100     = "v0.10.0-v1.json"
	fixtureV0101     = "v0.10.1-v1.json"
	fixtureNamespace = "wva-system"
	fixtureWriter    = "wva-controller-6d4f9c7b8-x2k4p"
	muPathsGolden    = "../analyzers/learnreplay/testdata/goldens/mu-paths/recording.jsonl.gz"
)

// The fixtures' keys, in the formats v0.10.1 wrote; the saturation
// package's TestLearnedStateKeyFormats_AreV0101s pins the key builders to
// the same samples.
const (
	fixtureArgs     = "aa99efb7bc3fa4d9" // every variant's arguments in the export
	fixturePrevArgs = "5e1d0c3b9a7f2468" // arguments variant-01 ran before
)

func fixtureK2Key(variant, bucket string) string {
	return "ns-01|" + variant + "|model-01|NVIDIA-H100-80GB-HBM3|1|decode|" + bucket + "|q5"
}

func fixtureThroughputKey(variant string) string {
	return "ns-01|" + variant + "|model-01|NVIDIA-H100-80GB-HBM3|1|decode|ilong|medium|q5"
}

func fixtureITLKey(variant string) string {
	return "ns-01|model-01|" + variant + "|NVIDIA-H100-80GB-HBM3|1"
}

// lastRecordedExport is the last ExportLearnedState event in a recording:
// the state and the clock it was exported at.
func lastRecordedExport(t *testing.T, path string) (saturation.LearnedState, time.Time) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	var last struct {
		Seq  uint64
		Now  time.Time
		Kind string
		Data learnrec.Raw
	}
	found := false
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"Kind":"saturation.ExportLearnedState"`)) {
			continue
		}
		require.NoError(t, learnrec.Unmarshal(sc.Bytes(), &last))
		found = true
	}
	require.NoError(t, sc.Err())
	require.True(t, found, "no ExportLearnedState event in %s", path)
	var s saturation.LearnedState
	require.NoError(t, last.Data.Decode(&s))
	return s, last.Now
}

// withEveryMap adds, to a state written at at, an entry to each map the
// recorded export left empty, as v0.10.1's export writes them:
//
//   - variant-05 runs a configuration not fully read, so v0.10.1 recorded
//     no arguments for it and kept each window's own (k2Args,
//     throughputArgs; a window under its variant's arguments carries no tag);
//   - variant-01 ran fixturePrevArgs before: its k2 and throughput windows
//     and its ITL baseline under them are put aside (k2Stash,
//     throughputStash, itlBaselineStash), and its k2 lower bound holds the
//     stashed figure as a previous one, dual-written to priorK2 as v0.10.1
//     does for a rollback (its export's setAsideByArgs);
//   - variant-04 is borrowing variant-03's lower figure: an armed bound,
//     dual-written to k2BorrowedSince;
//   - variant-02 carries a previous figure with no bound and no window of
//     its own, as a document from before lowerBounds (#151) left it and
//     v0.10.1's trim handles it (an orphan priorK2 entry);
//   - variant-03 was forgotten on request two hours before.
func withEveryMap(s *v0101LearnedState, at time.Time) {
	ago := func(n int) time.Time { return at.Add(-time.Duration(n) * time.Minute) }
	window := func(v float64) v0101RollingAverageSnapshot {
		return v0101RollingAverageSnapshot{Values: []float64{v, v, v, v, v, v}, LastUsed: ago(20), LastWritten: ago(40)}
	}
	// variant-05: a configuration not fully read, so no arguments recorded
	// for the variant, and its windows keep their own tags.
	v5k2, v5tp := fixtureK2Key("variant-05", "medium"), fixtureThroughputKey("variant-05")
	s.Config["ns-01/variant-05"] = "c4158ae264689362"
	s.K2[v5k2] = v0101RollingAverageSnapshot{Values: []float64{92000, 92000, 92000}, LastUsed: at, LastWritten: at}
	s.Throughput[v5tp] = v0101ThroughputSnapshot{
		v0101RollingAverageSnapshot: v0101RollingAverageSnapshot{Values: []float64{6.12, 6.12, 6.12}, LastUsed: at, LastWritten: at},
		SampledAt:                   at, LastRead: 6.12,
	}
	s.VariantSeenAt[fixtureITLKey("variant-05")] = at
	s.Accelerators["ns-01/variant-05"] = v0101AcceleratorSnapshot{Name: "NVIDIA-H100-80GB-HBM3", LastUsed: at}
	s.K2Args = map[string]string{v5k2: fixtureArgs}
	s.ThroughputArgs = map[string]string{v5tp: fixtureArgs}
	s.K2Stash = map[string]v0101RollingAverageSnapshot{
		fixtureK2Key("variant-01", "medium") + "#" + fixturePrevArgs: window(88000),
	}
	s.ThroughputStash = map[string]v0101ThroughputSnapshot{
		fixtureThroughputKey("variant-01") + "#" + fixturePrevArgs: {
			v0101RollingAverageSnapshot: window(6.5), SampledAt: ago(40), LastRead: 6.5,
		},
	}
	s.ITLBaselineStash = map[string]float64{fixtureITLKey("variant-01") + "#" + fixturePrevArgs: 0.0058}
	s.LowerBounds = map[string]v0101LowerBoundSnapshot{
		fixtureK2Key("variant-01", "medium"): {
			Seen: at, Args: fixtureArgs, ArgsVersion: 3,
			Prior: 88000, PriorSince: ago(40), PriorUsed: ago(20), FigureWritten: ago(40),
		},
		fixtureK2Key("variant-04", "medium"): {
			Armed: true, Since: ago(3), Tick: at, Seen: at, Donor: "variant-03", DonorK2: 88000,
			Args: fixtureArgs, ArgsVersion: 3, Offered: 88000,
		},
	}
	s.PriorK2 = map[string]v0101PriorK2Snapshot{
		fixtureK2Key("variant-01", "medium"): {K2: 88000, InstalledAt: ago(20)},
		fixtureK2Key("variant-02", "long"):   {K2: 90000, InstalledAt: ago(30)},
	}
	s.K2BorrowedSince = map[string]time.Time{fixtureK2Key("variant-04", "medium"): ago(3)}
	s.Forgotten = map[string]v0101ForgetMarkSnapshot{"ns-01/variant-03": {Token: "2026-03-02-reprofile", Seen: ago(120)}}
}

// Rewrites the frozen fixtures. It runs only when asked: the committed files
// are artifacts of the releases they stand for, and today's golden projected
// through today's types is not what those releases wrote.
func TestLearnedStateFixtures_Generate(t *testing.T) {
	if os.Getenv("LEARNED_STATE_WRITE_FIXTURES") != "1" {
		t.Skip("the fixtures are frozen; set LEARNED_STATE_WRITE_FIXTURES=1 to build them again")
	}
	state, at := lastRecordedExport(t, muPathsGolden)
	require.NotEmpty(t, state.ITLSlopes, "precondition: the export has slopes, the field v0.10.0 lacks")
	require.NotEmpty(t, state.K2, "precondition: the export has k2 windows")

	cur, err := json.Marshal(state)
	require.NoError(t, err)
	var old v0101LearnedState
	require.NoError(t, json.Unmarshal(cur, &old))
	withEveryMap(&old, at)
	doc := v0101LearnedStateDoc{
		Version:   1,
		WrittenAt: at,
		Writer:    v0101LearnedStateWriter{Namespace: fixtureNamespace, Identity: fixtureWriter},
		State:     old,
	}
	v0101, err := json.Marshal(doc)
	require.NoError(t, err)
	doc.State.ITLSlopes = nil
	v0100, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(fixtureDir, 0o755))
	for name, b := range map[string][]byte{fixtureV0101: v0101, fixtureV0100: v0100} {
		require.NoError(t, os.WriteFile(filepath.Join(fixtureDir, name), append(b, '\n'), 0o644))
		t.Logf("wrote %s, %d bytes", name, len(b))
	}
}

// restoreFixture restores the document in file through the engine's own path
// (ConfigMap, decode, analyzer) into a real saturation analyzer whose clock
// reads one minute after the document was written, and returns the document
// as the current code decodes it, the analyzer and the restore's log line.
func restoreFixture(t *testing.T, file string) (learnedStateDoc, *saturation.SaturationAnalyzer, string) {
	t.Helper()
	t.Setenv("POD_NAMESPACE", fixtureNamespace)
	raw, err := os.ReadFile(filepath.Join(fixtureDir, file))
	require.NoError(t, err)
	var doc learnedStateDoc
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Empty(t, doc.rejected(), "the current code rejects the %s document", file)

	now := doc.WrittenAt.Add(time.Minute)
	sat := saturation.NewSaturationAnalyzer(capacity.NewStore()).WithClock(clocktesting.NewFakePassiveClock(now))
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: learnedStateConfigMap, Namespace: config.SystemNamespace()},
		Data:       map[string]string{learnedStateKey(): string(raw)},
	}
	cfg := config.NewTestConfig()
	config.SetLearnedStateSnapshotForTest(cfg, true)
	e := &Engine{Config: cfg, client: fake.NewClientBuilder().WithObjects(cm).Build(), analyzers: testAnalyzers(sat)}

	var lines []string
	ctx := logr.NewContext(context.Background(),
		funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{}))
	e.restoreLearnedState(ctx, now)
	var restored string
	for _, l := range lines {
		if strings.Contains(l, "Learned state: snapshot restored") {
			restored = l
		}
	}
	require.NotEmpty(t, restored, "no restore line; logged: %v", lines)
	return doc, sat, restored
}

// assertRestoredWhole checks that a restore took every entry of the
// document, in every map: nothing aged, unusable or unattributable, every
// shared line installed, every variant's entries waiting for its first
// cycle, both put-aside windows and three lower bounds installed, and the
// analyzer's export holding the document's entries unchanged.
//
// The lower bounds: variant-04's comes back armed on its borrow's clock;
// variant-02's orphan priorK2 entry is installed as a bound holding that
// previous figure (exported as a bound beside it, and as the same priorK2
// entry); variant-01's previous figure comes back as it was.
func assertRestoredWhole(t *testing.T, doc learnedStateDoc, sat *saturation.SaturationAnalyzer, line string) {
	t.Helper()
	t.Logf("restore: %s", line)
	for _, want := range []string{`"aged"=0`, `"unusable"=0`, `"unattributable"=0`, `"forgotten"=0`,
		`"lowerBounds"=3`, `"stashed"=2`} {
		assert.Contains(t, line, want)
	}
	assert.Contains(t, line, `"itlLines"=`+strconv.Itoa(len(doc.State.ITLLines)))
	assert.Contains(t, line, `"pendingVariants"=`+strconv.Itoa(len(doc.State.Config)))

	exp := sat.ExportLearnedState()
	assert.Equal(t, doc.State.K2, exp.K2, "k2")
	assert.Equal(t, doc.State.Throughput, exp.Throughput, "throughput")
	assert.Equal(t, doc.State.ITLBaseline, exp.ITLBaseline, "itlBaseline")
	assert.Equal(t, doc.State.ITLBaselineStash, exp.ITLBaselineStash, "itlBaselineStash")
	assert.Equal(t, doc.State.StartSeconds, exp.StartSeconds, "startSeconds")
	assert.Equal(t, doc.State.VariantSeenAt, exp.VariantSeenAt, "variantSeenAt")
	assert.Equal(t, doc.State.Accelerators, exp.Accelerators, "accelerators")
	assert.Equal(t, doc.State.ITLLines, exp.ITLLines, "itlLines")
	assert.Equal(t, doc.State.K2Args, exp.K2Args, "k2Args")
	assert.Equal(t, doc.State.ThroughputArgs, exp.ThroughputArgs, "throughputArgs")
	assert.Equal(t, doc.State.K2Stash, exp.K2Stash, "k2Stash")
	assert.Equal(t, doc.State.ThroughputStash, exp.ThroughputStash, "throughputStash")
	assert.Equal(t, doc.State.Forgotten, exp.Forgotten, "forgotten")
	assert.Equal(t, doc.State.Config, exp.Config, "config")
	assert.Equal(t, doc.State.Args, exp.Args, "args")
	assert.Equal(t, doc.State.K2BorrowedSince, exp.K2BorrowedSince, "k2BorrowedSince")
	assert.Equal(t, doc.State.PriorK2, exp.PriorK2, "priorK2")

	borrow := fixtureK2Key("variant-04", "medium")
	assert.True(t, exp.LowerBounds[borrow].Armed, "variant-04's borrow comes back armed")
	assert.Equal(t, doc.State.LowerBounds[borrow], exp.LowerBounds[borrow], "variant-04's bound")
	own := fixtureK2Key("variant-01", "medium")
	assert.Equal(t, doc.State.LowerBounds[own], exp.LowerBounds[own], "variant-01's previous figure")
	orphan := fixtureK2Key("variant-02", "long")
	require.Contains(t, exp.LowerBounds, orphan, "variant-02's previous figure was not installed as a bound")
	assert.Equal(t, doc.State.PriorK2[orphan].K2, exp.LowerBounds[orphan].Prior, "the bound holds the previous figure")
	assert.False(t, exp.LowerBounds[orphan].Armed, "a previous figure alone is not a running borrow")
	assert.Len(t, exp.LowerBounds, len(doc.State.LowerBounds)+1, "the document's bounds and the installed prior, no more")
}

// A v0.10.0 document, which has no itlSlopes, restores whole; the ITL lines
// and baselines come back, and no slope is invented.
func TestLearnedStateFixture_V0100Restores(t *testing.T) {
	doc, sat, line := restoreFixture(t, fixtureV0100)
	require.NotEmpty(t, doc.State.K2, "precondition: the fixture holds windows")
	require.Empty(t, doc.State.ITLSlopes, "precondition: a v0.10.0 document has no slopes")
	assertRestoredWhole(t, doc, sat, line)
	assert.Empty(t, sat.ExportLearnedState().ITLSlopes, "no slope came from a document that had none")
}

// A v0.10.1 document restores whole, its fitted slopes included.
func TestLearnedStateFixture_V0101Restores(t *testing.T) {
	doc, sat, line := restoreFixture(t, fixtureV0101)
	require.NotEmpty(t, doc.State.ITLSlopes, "precondition: a v0.10.1 document has slopes")
	assertRestoredWhole(t, doc, sat, line)
	assert.Equal(t, doc.State.ITLSlopes, sat.ExportLearnedState().ITLSlopes, "itlSlopes")
}

// Each fixture is a document v0.10.1's decoder reads with no field to spare:
// decoded strictly into the frozen types, nothing is unknown, and every map
// the frozen type knows holds an entry (v0.10.0's has no itlSlopes). So
// neither fixture carries a field the release it stands for could not have
// written, and none it could have written is left out.
func TestLearnedStateFixtures_AreV0101Shaped(t *testing.T) {
	for _, name := range []string{fixtureV0100, fixtureV0101} {
		raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
		require.NoError(t, err)
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var doc v0101LearnedStateDoc
		require.NoError(t, dec.Decode(&doc), name)
		assert.Equal(t, 1, doc.Version, name)
		st := reflect.ValueOf(doc.State)
		for i := range st.NumField() {
			field := st.Type().Field(i).Name
			if name == fixtureV0100 && field == "ITLSlopes" {
				assert.Zero(t, st.Field(i).Len(), "%s: a v0.10.0 document has no itlSlopes", name)
				continue
			}
			assert.NotZero(t, st.Field(i).Len(), "%s: %s is empty", name, field)
		}
	}
}
