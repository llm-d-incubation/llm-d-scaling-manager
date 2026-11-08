package throughput

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// The forget-learned-state annotation reaches the throughput line, behind
// SwitchITLForgetReachesTheLine. Off is the line before the change: the
// negative control the switch must fail.

// forgetRun is a line run with the forget switch set.
func forgetRun(t *testing.T, a *ThroughputAnalyzer, on bool) *lineRun {
	t.Helper()
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLForgetReachesTheLine: on}))
	return newLineRun(t, a)
}

// fitLine runs cycles of rows until v1's line is a full fit with a learned
// intercept, and holds that premise.
func fitLine(r *lineRun, rows func() []domain.ReplicaMetrics) {
	r.t.Helper()
	for range 40 {
		r.step(rows())
		if read, _, _ := lineRead(r.a); read.Tier == itl.TierOLS && read.BaselineLearned {
			return
		}
	}
	require.Fail(r.t, "premise: v1's line never reached a full fit")
}

// forgetV1 forgets v1 in the run's namespace and model.
func forgetV1(a *ThroughputAnalyzer) int {
	return a.ForgetVariant(context.Background(), "ns", "m", "v1")
}

// A forget drops the line: its windows and intercept, its held slope, and
// the figures last priced from it. The next cycle has one cycle's readings
// and no line, and the line comes back from new readings, on the default
// intercept. Off, as before, the line keeps pricing from the old readings.
func TestForget_TheAnnotationForgetsTheLine(t *testing.T) {
	key := variantKey("ns", "m", "v1")
	replicas := len(settleFleet())
	for _, on := range []bool{true, false} {
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, on)
		fitLine(r, settleFleet)
		require.Contains(t, a.lines.Slopes(), key, "premise: the key holds a slope")
		require.Greater(t, a.lines.WindowLen(key), replicas, "premise: the window holds more than one cycle")

		dropped := forgetV1(a)
		st, _ := a.VariantState("m", "ns", "v1")
		_, priced := r.step(settleFleet())
		read, _, _ := lineRead(a)
		_, hasB := a.lines.Baseline(key)
		if !on {
			assert.Zero(t, dropped, "switch off: nothing is forgotten")
			assert.False(t, st.ITLModel.IsZero(), "switch off: the snapshot keeps the line")
			assert.True(t, priced, "switch off: the line still prices from the old readings")
			assert.Equal(t, itl.TierOLS, read.Tier)
			assert.True(t, hasB, "switch off: the intercept stays")
			continue
		}
		assert.Equal(t, 2, dropped, "the window and the intercept")
		assert.True(t, st.ITLModel.IsZero(), "the snapshot shows no line")
		assert.False(t, st.HasFittedB, "the snapshot shows no intercept")
		assert.Zero(t, st.SampleCount)
		assert.Zero(t, st.PerReplicaSupply, "nothing priced from the old line is shown")
		assert.Zero(t, st.TotalSupply)
		assert.Zero(t, st.Demand)
		assert.False(t, st.Shape.KVreq == 0, "the shape is the traffic, and stays")
		assert.False(t, priced, "no line from one cycle")
		assert.Equal(t, replicas, read.Held, "the window holds this cycle's readings alone")
		assert.False(t, hasB, "the intercept is gone")
		assert.False(t, read.SlopeHeld, "no slope is held")

		// Control: the line was emptied, not stopped.
		var back domain.VariantCapacity
		var ok bool
		for range 4 {
			back, ok = r.step(settleFleet())
		}
		require.True(t, ok, "the line comes back from new readings")
		assert.NotEqual(t, itlReasonT2Held, back.Reason, "from the new readings, not a held slope")
	}
}

// A line with no readings left prices from its held slope, for up to the
// hold. A forget takes that slope, so nothing fitted before it prices after
// it. ClearWindow empties the windows and keeps B, so the key's readings are
// gone here and its slope is what would have priced.
func TestForget_TakesTheHeldSlope(t *testing.T) {
	key := variantKey("ns", "m", "v1")
	for _, on := range []bool{true, false} {
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, on)
		fitLine(r, settleFleet)
		require.True(t, a.lines.ClearWindow(key), "premise: the window held readings")

		_, forgotten := a.forgetVariant("ns", "m", "v1")
		vc, priced := r.step(settleFleet())
		if !on {
			require.True(t, priced, "switch off: the held slope prices")
			assert.Equal(t, itlReasonT2Held, vc.Reason)
			continue
		}
		assert.False(t, priced, "the held slope is gone")
		assert.NotContains(t, a.lines.Slopes(), key)
		assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, forgotten)
	}
}

// aboveBand is rows of v1 steadily above the default band, under no stated
// cap, at a k past itl.DefaultKSat: what the above-band fallback keeps.
func aboveBand() []domain.ReplicaMetrics {
	early := lineStart.Add(-time.Hour)
	return []domain.ReplicaMetrics{
		lineRow("pod-a", 0.88, lineAt(0.88), early),
		lineRow("pod-b", 0.90, lineAt(0.90), early),
		lineRow("pod-c", 0.92, lineAt(0.92), early),
	}
}

// The above-band fallback's windows go too: its line is priced from them,
// and a key that fitted from them holds their slope.
func TestForget_TakesTheAboveBandFallback(t *testing.T) {
	replicas := len(aboveBand())
	for _, on := range []bool{true, false} {
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, on)
		for range 8 {
			r.step(aboveBand())
		}
		read, _, _ := lineRead(a)
		require.True(t, read.AboveBandFit, "premise: the fallback prices the line")
		require.Greater(t, read.AboveBandHeld, replicas, "premise: its window holds more than one cycle")

		forgetV1(a)
		vc, priced := r.step(aboveBand())
		read, _, _ = lineRead(a)
		if !on {
			require.True(t, priced, "switch off: the fallback keeps pricing")
			assert.Equal(t, itlReasonT2AboveBand, vc.Reason)
			assert.Greater(t, read.AboveBandHeld, replicas)
			continue
		}
		assert.False(t, priced, "one cycle above the band is no line")
		assert.Equal(t, replicas, read.AboveBandHeld, "its window holds this cycle's readings alone")
		assert.Empty(t, a.lines.Slopes(), "and the slope it fitted is gone")
	}
}

// A forget ends a learned B, so a line at the cap prices one-sided again,
// as an unlearned one does; the readings it took at the cap go with the
// window. The cap itself is read from the store each cycle: nothing here
// keeps it.
func TestForget_AtTheCapALineIsPricedOneSidedAgain(t *testing.T) {
	early := lineStart.Add(-time.Hour)
	capOnly := func() []domain.ReplicaMetrics {
		return []domain.ReplicaMetrics{capLineRow("pod-cap", 0.92),
			lineRow("pod-a", 0.05, lineAt(0.05), early), lineRow("pod-b", 0.05, lineAt(0.05), early)}
	}
	for _, on := range []bool{true, false} {
		a := NewThroughputAnalyzer().WithCapacityStore(paramsStore(capacity.EngineParams{Engine: "vllm", DeclaredBatchCap: 64}))
		r := forgetRun(t, a, on)
		fitLine(r, func() []domain.ReplicaMetrics { return append(capOnly(), settleFleet()...) })
		read, _, _ := lineRead(a)
		require.Positive(t, read.Capped, "premise: readings at the cap")
		held := r.a.lines.WindowLen(variantKey("ns", "m", "v1"))
		require.Greater(t, held, len(capOnly())+len(settleFleet()), "premise: the window holds more than one cycle")

		forgetV1(a)
		for range 12 {
			r.step(capOnly())
		}
		read, batchCap, _ := lineRead(a)
		require.Positive(t, read.Capped)
		require.EqualValues(t, 64, batchCap, "the cap still reaches the admission")
		if !on {
			assert.True(t, read.BaselineLearned, "switch off: B stays")
			assert.False(t, read.PinnedAtLeastITL, "switch off: a learned B ends the one-sided price")
			continue
		}
		assert.False(t, read.BaselineLearned, "B is gone")
		assert.True(t, read.PinnedAtLeastITL, "no B: priced one-sided again")
		assert.LessOrEqual(t, read.Held, 12*len(capOnly()), "no reading from before the forget")
	}
}

// A forget takes the readings stamped settling, and not the settle: a
// replica inside its settle at a forget stays inside it, because the settle
// tracker, not the line, keeps when it joined.
func TestForget_AReplicaSettlingAtAForgetStillSettles(t *testing.T) {
	r := newSettleRig(t, nil)
	fleet := steadyFleet()
	for range 12 {
		r.cycle(fleet)
	}
	join := append(steadyFleet(), lineRow("pod-join", 0.46, 0.45, r.now))
	read := r.cycle(join)
	require.Positive(t, read.Settling, "premise: the joiner's reading is stamped")

	forgetV1(r.a)
	read = r.cycle(join)
	assert.Equal(t, len(join), read.Held, "the stamped readings went with the window")
	assert.Positive(t, read.Settling, "the joiner still settles")
}

// A forget takes the variant it names, and nothing of another: a name that
// begins with it, the same name under another model or namespace.
func TestForget_OnlyTheNamedVariant(t *testing.T) {
	sibling := func() []domain.ReplicaMetrics {
		rows := settleFleet()
		for i := range rows {
			rows[i].VariantName = "v10"
			rows[i].PodName += "-sibling"
		}
		return rows
	}
	a := NewThroughputAnalyzer()
	r := forgetRun(t, a, true)
	fitLine(r, func() []domain.ReplicaMetrics { return append(settleFleet(), sibling()...) })
	v1, v10 := variantKey("ns", "m", "v1"), variantKey("ns", "m", "v10")
	require.Positive(t, a.lines.WindowLen(v10), "premise: the sibling holds a line")

	ctx := context.Background()
	assert.Zero(t, a.ForgetVariant(ctx, "ns", "other-model", "v1"))
	assert.Zero(t, a.ForgetVariant(ctx, "other-ns", "m", "v1"))
	assert.Zero(t, a.ForgetVariant(ctx, "ns", "m", "v"), "a name v1 starts with")
	require.Positive(t, a.lines.WindowLen(v1), "v1 is untouched")

	assert.Equal(t, 2, forgetV1(a))
	assert.Zero(t, a.lines.WindowLen(v1))
	assert.Positive(t, a.lines.WindowLen(v10), "v10 keeps its window")
	_, hasB := a.lines.Baseline(v10)
	assert.True(t, hasB, "and its intercept")
	assert.Contains(t, a.lines.Slopes(), v10, "and its slope")
}

// Nothing refills a forgotten line but the variant's own replicas: not the
// readings of a sibling that keeps its line, and not those of a warm pool
// Pod lent to the variant, which report under its name. Observe is what
// discriminates: it hands the tracker the lent row, and the tracker refuses
// it. Through Analyze, ownReplicasOnly drops the row first, which is defence
// in depth: with it removed this test still passes, on the tracker's refusal.
func TestForget_NoSiblingOrLentReplicaRefillsTheLine(t *testing.T) {
	key := variantKey("ns", "m", "v1")
	sibling := func() []domain.ReplicaMetrics {
		rows := settleFleet()
		for i := range rows {
			rows[i].VariantName = "v2"
			rows[i].PodName += "-sibling"
		}
		return rows
	}
	lent := func() []domain.ReplicaMetrics {
		rows := settleFleet()
		for i := range rows {
			rows[i].FromWarmPool = true
			rows[i].PodName += "-lent"
		}
		return rows
	}
	a := NewThroughputAnalyzer()
	r := forgetRun(t, a, true)
	fitLine(r, func() []domain.ReplicaMetrics { return append(settleFleet(), sibling()...) })
	require.Equal(t, 2, forgetV1(a))

	for range 16 {
		_, priced := r.step(append(sibling(), lent()...))
		require.False(t, priced, "v1 was priced from readings that are not its own")
	}
	a.Observe(context.Background(), r.now(), "m", "ns", lent())
	assert.Zero(t, a.lines.WindowLen(key))
	_, hasB := a.lines.Baseline(key)
	assert.False(t, hasB)
	assert.NotContains(t, a.lines.Slopes(), key)

	// Control: the variant's own replicas do refill it.
	for range 4 {
		r.step(settleFleet())
	}
	assert.Positive(t, a.lines.WindowLen(key))
}

// No exported method of the analyzer is named as a restore. That is all this
// checks: a name, not what a method does. It is a tripwire for a restore added
// later, which would have to say how a forget stops it; that nothing restores
// a line today is read from the code (forget.go).
func TestForget_NothingHereRestoresALine(t *testing.T) {
	typ := reflect.TypeFor[*ThroughputAnalyzer]()
	require.Positive(t, typ.NumMethod(), "premise: the reflection found the analyzer's methods")
	_, ok := reflect.TypeFor[*itl.Tracker]().MethodByName("RestoreBaseline")
	require.True(t, ok, "control: this is how a restore is named")
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		for _, word := range []string{"Restore", "Snapshot", "LearnedState", "Import"} {
			assert.NotContains(t, name, word)
		}
	}
}

// With the steady gate off the line is the parent's: its window and B, in
// the variant's state. They go too, and the key is reported.
func TestForget_TheParentsWindowAndB(t *testing.T) {
	key := variantKey("ns", "m", "v1")
	a := NewThroughputAnalyzer()
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLSteadyGate: false}))
	r := newLineRun(t, a)
	for range 12 {
		r.step(settleFleet())
	}
	st, _ := a.VariantState("m", "ns", "v1")
	require.Positive(t, st.SampleCount, "premise: the parent's window holds readings")
	require.True(t, st.HasFittedB, "premise: the parent fitted B")

	dropped, forgotten := a.forgetVariant("ns", "m", "v1")
	assert.Equal(t, 2, dropped, "the window and B")
	assert.Equal(t, []learn.Forgotten{{Key: key, By: learn.ByOperator}}, forgotten)
	st, _ = a.VariantState("m", "ns", "v1")
	assert.Zero(t, st.SampleCount)
	assert.False(t, st.HasFittedB)
	assert.Zero(t, st.LastFittedB)
}

// A forget drops what learner-write last logged of the line: the next line
// is a first write, compared with nothing forgotten.
func TestForget_TheNextLearnerWriteIsAFirstWrite(t *testing.T) {
	key := variantKey("ns", "m", "v1")
	a := NewThroughputAnalyzer()
	r := forgetRun(t, a, true)
	fitLine(r, settleFleet)
	require.Contains(t, a.itlLogged, key, "premise: the line logged")
	forgetV1(a)
	assert.NotContains(t, a.itlLogged, key)
}

// Forget reaches every learner on learn's list, by name: each is either the
// line this analyzer keeps or a learner it holds nothing of. A name it does
// not know is not handled.
func TestForget_EveryLearnerOnTheListIsHandled(t *testing.T) {
	a := NewThroughputAnalyzer()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range learn.Learners() {
		_, _, handled := a.forgetLearnerLocked(name, "ns", "m", "v")
		assert.True(t, handled, "the learner %q is on learn's list and the throughput analyzer has no forget decision for it", name)
	}
	dropped, keys, handled := a.forgetLearnerLocked("no-such-learner", "ns", "m", "v")
	assert.False(t, handled, "negative control: a name the analyzer does not know is not handled")
	assert.Zero(t, dropped)
	assert.Empty(t, keys)
}

// Each key a forget takes is reported by the operator: the one key of the
// line, and none for a variant it holds nothing of.
func TestForget_ReportsEachForgottenKey(t *testing.T) {
	for _, on := range []bool{true, false} {
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, on)
		for range 4 {
			r.step(settleFleet())
		}
		_, forgotten := a.forgetVariant("ns", "m", "v1")
		if !on {
			assert.Empty(t, forgotten, "switch off: nothing is forgotten")
			continue
		}
		assert.Equal(t, []learn.Forgotten{{Key: variantKey("ns", "m", "v1"), By: learn.ByOperator}}, forgotten)
		_, forgotten = a.forgetVariant("ns", "m", "v1")
		assert.Empty(t, forgotten, "a second forget has nothing to report")
	}
}

// The forget logs one line per key it took, at the default verbosity, on the
// caller's logger (its fields carried), with recording off and on; and the
// key reads as text, without the NUL that separates it in memory.
func TestForget_LogsEachForgottenKeyOnTheCallersLogger(t *testing.T) {
	for _, recording := range []bool{false, true} {
		a := NewThroughputAnalyzer()
		r := forgetRun(t, a, true)
		var buf bytes.Buffer
		if recording {
			a.WithRecorder(learnrec.New(&buf, 0))
		}
		for range 4 {
			r.step(settleFleet())
		}

		// The shipped verbosity: Info and nothing more verbose.
		core, logs := observer.New(zapcore.InfoLevel)
		ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)).WithValues("cycle", "c-1"))
		a.ForgetVariant(ctx, "ns", "m", "v1")
		lines := logs.FilterMessage(logging.ForgottenKeyMessage).All()
		require.Len(t, lines, 1, "recording %v: one line per key", recording)
		fields := lines[0].ContextMap()
		assert.Equal(t, zapcore.InfoLevel, lines[0].Level)
		assert.Equal(t, "ns|m|v1", fields["key"])
		assert.Equal(t, "v1", fields["variant"])
		assert.Equal(t, learn.ITL, fields["learner"])
		assert.Equal(t, AnalyzerName, fields["analyzer"])
		assert.Equal(t, "c-1", fields["cycle"], "the caller's logger, with its fields")
		assert.NotContains(t, fields, "by", "every key here is the operator's: no cause field")
		assert.Equal(t, recording, strings.Contains(buf.String(), KindForgetVariant), "the event is recorded only with recording on")

		// Control: a second forget takes nothing and logs nothing.
		a.ForgetVariant(ctx, "ns", "m", "v1")
		assert.Len(t, logs.FilterMessage(logging.ForgottenKeyMessage).All(), 1)
	}
}

// Every field of ThroughputAnalyzer that is not a plain value is forgotten
// by a forget, kept on purpose, or configuration and plumbing (forget.go
// names each): one added later fails here until someone decides.
func TestForget_EveryFieldIsClassified(t *testing.T) {
	forgotten := []string{"lines", "itlLogged", "variantStates"}
	kept := []string{"ownSettle", "ownSettled", "paths", "learnerReports", "forgottenReports"}
	plumbing := []string{"mu", "clock", "rec", "settle", "store"}
	plain := func(k reflect.Kind) bool {
		switch k {
		case reflect.Bool, reflect.String,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			return true
		}
		return false
	}
	var state []string
	typ := reflect.TypeFor[ThroughputAnalyzer]()
	for i := range typ.NumField() {
		if f := typ.Field(i); !plain(f.Type.Kind()) {
			state = append(state, f.Name)
		}
	}
	require.Contains(t, state, "lines", "premise: the reflection found the analyzer's state")
	assert.ElementsMatch(t, slices.Concat(forgotten, kept, plumbing), state,
		"each field of ThroughputAnalyzer that is not a plain value must be forgotten, kept or plumbing; say which in forget.go")
}

// Every field of a variant's state is forgotten or kept on purpose, plain
// values too: a bool here can be what prices the line (hasFittedB). The
// forgotten ones are held by a state with every field set, below.
func TestForget_EveryVariantStateFieldIsClassified(t *testing.T) {
	forgotten := []string{"observationWindow", "lastFittedB", "hasFittedB", "lineArgs", "consecutiveGPSMismatches",
		"lastITLModel", "lastPerReplicaSupply", "lastTotalSupply", "lastDemand", "reading", "hasReading"}
	// The traffic now, or what a log line last said: no line is priced from
	// them. batchCap and its source are read again each cycle.
	kept := []string{"shapeTracker", "role", "lastSanityReport", "lastObservedAt", "rollout", "rolloutHeld", "loggedRolloutHeld", "shapeChanged", "everLive",
		"shapeChangedAt", "gpsMismatched", "lineSilent", "batchCap", "batchCapSource", "summaryLogged", "loggedAtCap", "loggedCapSource"}
	typ := reflect.TypeFor[variantState]()
	fields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Name)
	}
	assert.ElementsMatch(t, slices.Concat(forgotten, kept), fields,
		"each field of variantState must be forgotten or kept; say which in forget.go")

	// forgetLine leaves every forgotten field at its zero value: a field on
	// the list it does not reset fails here.
	a := NewThroughputAnalyzer()
	a.mu.Lock()
	s := a.getOrCreateVariantState("k")
	a.mu.Unlock()
	s.observationWindow.Add(0.5, 0.03, lineStart)
	s.lastFittedB, s.hasFittedB, s.consecutiveGPSMismatches = 0.01, true, 2
	s.lastITLModel = itl.Model{A: 0.05, B: 0.01}
	s.lastPerReplicaSupply, s.lastTotalSupply, s.lastDemand = 1, 2, 3
	s.reading, s.hasReading = itl.Reading{Tier: itl.TierOLS}, true
	v := reflect.ValueOf(s).Elem()
	assert.Equal(t, 2, s.forgetLine(), "the parent's window and B are counted")
	assert.Zero(t, s.observationWindow.Len())
	for _, name := range forgotten {
		if name == "observationWindow" {
			continue
		}
		assert.True(t, v.FieldByName(name).IsZero(), "%s survived forgetLine", name)
	}
}
