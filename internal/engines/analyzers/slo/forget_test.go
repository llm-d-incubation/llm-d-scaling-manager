package slo

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
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/settle/settlerow"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// The forget-learned-state annotation reaches the slo line (issue ev-shindin/llm-scaling-manager#431),
// behind SwitchITLForgetReachesTheLine. Off is the line before the fix: the
// negative control the switch must fail.

// forgetSibling is a second variant whose name agg is a prefix of, so a
// forget of agg that matched by name alone would take its line too.
var forgetSibling = domain.VariantReplicaState{VariantName: "agg2", Role: domain.RoleBoth,
	AcceleratorName: "H100", GPUsPerReplica: 1, CurrentReplicas: 3}

// forgetCycle observes agg's and agg2's rows as the engine does, prices
// both, moves the clock a cycle on, and returns what each line read.
func forgetCycle(r *settleRig) (agg, sibling itl.Reading) {
	r.t.Helper()
	rows := append(settleFleet(),
		row("agg2", "agg2-a", 0.30), row("agg2", "agg2-b", 0.45), row("agg2", "agg2-c", 0.60))
	in := domain.AnalyzerInput{Namespace: "ns", ModelID: "m", Config: policy(44*time.Millisecond, 0),
		VariantStates: []domain.VariantReplicaState{settleAgg, forgetSibling}, ReplicaMetrics: rows}
	r.tr.Observe(utils.GetNamespacedKey("ns", "m"),
		settlerow.Of(rows, map[string]string{"agg": domain.RoleBoth, "agg2": domain.RoleBoth}), r.now)
	fleet := readFleet(in)
	_, _, a := r.a.priceVariant(in, settleAgg, windowKey("ns", "m", settleAgg), fleet, 0.044, 0.85, r.now)
	_, _, s := r.a.priceVariant(in, forgetSibling, windowKey("ns", "m", forgetSibling), fleet, 0.044, 0.85, r.now)
	r.now = r.now.Add(settleCycle)
	return a.line, s.line
}

// A forget drops the variant's line -- its window and the intercept its
// full fit learned -- and nothing of a sibling's. Off, as before, the line
// keeps pricing from the readings taken before the forget.
func TestForget_TheAnnotationForgetsTheLine(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newSettleRig(t, map[string]bool{SwitchITLForgetReachesTheLine: on})
		var agg, sib itl.Reading
		for range settleCycles + 10 {
			agg, sib = forgetCycle(r)
		}
		replicas := len(settleFleet())
		require.Equal(t, itl.TierOLS, agg.Tier, "premise: agg's line is a full fit")
		require.True(t, agg.BaselineLearned, "premise: agg's line learned its intercept")
		require.Greater(t, agg.Held, replicas, "premise: agg's window holds more than one cycle")
		require.Greater(t, sib.Held, replicas, "premise: the sibling's window holds more than one cycle")

		dropped := r.a.ForgetVariant(context.Background(), "ns", "m", "agg")
		after, sibAfter := forgetCycle(r)
		if !on {
			assert.Zero(t, dropped, "switch off: nothing is forgotten")
			assert.Greater(t, after.Held, replicas, "switch off: the window kept the readings from before the forget")
			assert.True(t, after.BaselineLearned, "switch off: the intercept stays")
			assert.Equal(t, itl.TierOLS, after.Tier, "switch off: the line still prices from the old readings")
			continue
		}
		assert.Equal(t, 2, dropped, "the window and the intercept")
		assert.Equal(t, replicas, after.Held, "the window holds this cycle's readings alone")
		assert.False(t, after.BaselineStored, "the intercept is gone")
		assert.NotEqual(t, itl.TierOLS, after.Tier, "no full fit from one cycle")
		assert.Greater(t, sibAfter.Held, replicas, "the sibling agg2 keeps its window")
		assert.True(t, sibAfter.BaselineLearned, "and its intercept")
	}
}

// A forget of a variant the analyzer holds nothing of, or of another model's
// variant of the same name, drops nothing.
func TestForget_OnlyTheNamedModelsVariant(t *testing.T) {
	r := newSettleRig(t, nil)
	for range settleCycles + 10 {
		forgetCycle(r)
	}
	assert.Zero(t, r.a.ForgetVariant(context.Background(), "ns", "other-model", "agg"))
	assert.Zero(t, r.a.ForgetVariant(context.Background(), "other-ns", "m", "agg"))
	assert.Zero(t, r.a.ForgetVariant(context.Background(), "ns", "m", "ag"), "a name agg starts with")
	agg, _ := forgetCycle(r)
	assert.Equal(t, itl.TierOLS, agg.Tier, "agg's line is untouched")
	// Control: the same analyzer forgets agg when asked by its own name.
	assert.Equal(t, 2, r.a.ForgetVariant(context.Background(), "ns", "m", "agg"))
}

// The forget also drops the engine arguments the line was learned under: a
// line made anew records them again, rather than reading the next
// arguments as a change and sweeping the new line.
func TestForget_DropsTheArgumentsTheLineWasLearnedUnder(t *testing.T) {
	a := New(nil, nil)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	key := windowKey("ns", "m", settleAgg)
	sib := windowKey("ns", "m", forgetSibling)
	a.linesUnderArgs(key, "args-1", "ns", "m", "agg", now)
	a.linesUnderArgs(sib, "args-1", "ns", "m", "agg", now)
	a.mu.Lock()
	a.seen[key], a.seen[sib] = now, now
	a.mu.Unlock()

	a.ForgetVariant(context.Background(), "ns", "m", "agg")
	a.mu.Lock()
	defer a.mu.Unlock()
	assert.NotContains(t, a.args, key)
	assert.NotContains(t, a.seen, key)
	assert.Contains(t, a.args, sib, "control: the sibling keeps its arguments")
	assert.Contains(t, a.seen, sib)
}

// Forget reaches every learner on learn's list, by name: each is either
// the line this analyzer keeps or a learner it holds nothing of. A name it
// does not know is not handled.
func TestForget_EveryLearnerOnTheListIsHandled(t *testing.T) {
	a := New(nil, nil)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range learn.Learners() {
		_, _, handled := a.forgetLearnerLocked(name, "ns", "m", "v")
		assert.True(t, handled, "the learner %q is on learn's list and the slo analyzer has no forget decision for it", name)
	}
	dropped, keys, handled := a.forgetLearnerLocked("no-such-learner", "ns", "m", "v")
	assert.False(t, handled, "negative control: a name the analyzer does not know is not handled")
	assert.Zero(t, dropped)
	assert.Empty(t, keys)
}

// A forget drops the variant's accelerator memo, as the saturation
// analyzer's drops its lastAccelerator: a cycle whose pods do not resolve
// the accelerator then keys the line under the reading, not under the card
// the variant ran on before the forget. Another variant keeps its memo.
func TestForget_DropsTheAcceleratorMemo(t *testing.T) {
	unresolved := func(vs domain.VariantReplicaState) domain.VariantReplicaState {
		vs.AcceleratorName = ""
		return vs
	}
	for _, on := range []bool{true, false} {
		a := New(nil, nil)
		require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchITLForgetReachesTheLine: on}))
		require.Equal(t, windowKey("ns", "m", settleAgg), a.variantKey("ns", "m", settleAgg), "premise: a resolved card keys the line")
		a.variantKey("ns", "m", forgetSibling)
		held := windowKey("ns", "m", settleAgg)
		require.Equal(t, held, a.variantKey("ns", "m", unresolved(settleAgg)), "premise: the memo keys an unresolved cycle")

		dropped := a.ForgetVariant(context.Background(), "ns", "m", "agg")
		got := a.variantKey("ns", "m", unresolved(settleAgg))
		if !on {
			assert.Zero(t, dropped)
			assert.Equal(t, held, got, "switch off: the memo stays")
			continue
		}
		assert.Equal(t, 1, dropped, "the memo, and no line was held")
		assert.Equal(t, windowKey("ns", "m", unresolved(settleAgg)), got, "the memo is gone: the reading keys the line")
		assert.Equal(t, windowKey("ns", "m", forgetSibling), a.variantKey("ns", "m", unresolved(forgetSibling)),
			"control: agg2 keeps its memo")
	}
}

// Each key a forget takes is reported, sorted and by the operator, for the
// log: the keys the saturation analyzer files in its learner report.
func TestForget_ReportsEachForgottenKey(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newSettleRig(t, map[string]bool{SwitchITLForgetReachesTheLine: on})
		for range settleCycles + 2 {
			forgetCycle(r)
		}
		other := domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth, AcceleratorName: "H200", GPUsPerReplica: 2}
		r.a.lines.Observe(windowKey("ns", "m", other), "agg", settleFleet(), itl.Admission{}, r.now)
		_, forgotten := r.a.forgetVariant("ns", "m", "agg")
		if !on {
			assert.Empty(t, forgotten, "switch off: nothing is forgotten")
			continue
		}
		assert.Equal(t, []learn.Forgotten{
			{Key: windowKey("ns", "m", settleAgg), By: learn.ByOperator},
			{Key: windowKey("ns", "m", other), By: learn.ByOperator},
		}, forgotten, "every key of agg, under each card, and none of agg2")
	}
}

// Every field of the Analyzer that is not a plain value (a bool, a number,
// a string) is either forgotten by a forget, kept on purpose, or
// configuration and plumbing (forget.go names each): a map, slice, pointer,
// struct, interface, func or sync type added later fails here until someone
// decides. The forgotten ones are held by the tests above.
func TestForget_EveryFieldIsClassified(t *testing.T) {
	forgotten := []string{"lines", "seen", "args", "lineLogged", "accelerators", "imbalance", "decodeTTFT", "decodeTerms", "prompts", "joint"}
	kept := []string{"transients", "ownSettle", "ownSettled", "paths", "learnerReports", "forgottenReports"}
	plumbing := []string{"router", "store", "now", "mu", "rec", "settle", "prefillEstimates"}
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
	typ := reflect.TypeFor[Analyzer]()
	for i := range typ.NumField() {
		if f := typ.Field(i); !plain(f.Type.Kind()) {
			state = append(state, f.Name)
		}
	}
	require.Contains(t, state, "lines", "premise: the reflection found the analyzer's state")
	assert.ElementsMatch(t, slices.Concat(forgotten, kept, plumbing), state,
		"each field of the Analyzer that is not a plain value must be forgotten, kept or plumbing; say which in forget.go")
}

// The forget logs one line per key it took, at the default verbosity, on
// the caller's logger (its fields carried), with recording off and on.
func TestForget_LogsEachForgottenKeyOnTheCallersLogger(t *testing.T) {
	other := domain.VariantReplicaState{VariantName: "agg", Role: domain.RoleBoth, AcceleratorName: "H200", GPUsPerReplica: 2}
	for _, recording := range []bool{false, true} {
		r := newSettleRig(t, nil)
		var buf bytes.Buffer
		if recording {
			r.a.WithRecorder(learnrec.New(&buf, 0))
		}
		for range settleCycles + 2 {
			forgetCycle(r)
		}
		r.a.lines.Observe(windowKey("ns", "m", other), "agg", settleFleet(), itl.Admission{}, r.now)

		// The shipped verbosity: Info and nothing more verbose.
		core, logs := observer.New(zapcore.InfoLevel)
		ctx := logr.NewContext(context.Background(), zapr.NewLogger(zap.New(core)).WithValues("cycle", "c-1"))
		r.a.ForgetVariant(ctx, "ns", "m", "agg")
		lines := logs.FilterMessage(logging.ForgottenKeyMessage).All()
		require.Len(t, lines, 2, "recording %v: one line per key", recording)
		for i, want := range []string{windowKey("ns", "m", settleAgg), windowKey("ns", "m", other)} {
			fields := lines[i].ContextMap()
			assert.Equal(t, zapcore.InfoLevel, lines[i].Level)
			assert.Equal(t, want, fields["key"])
			assert.Equal(t, "agg", fields["variant"])
			assert.Equal(t, learn.ITL, fields["learner"])
			assert.Equal(t, "c-1", fields["cycle"], "the caller's logger, with its fields")
			assert.NotContains(t, fields, "by", "every key here is the operator's: no cause field")
		}
		assert.Equal(t, recording, strings.Contains(buf.String(), KindForgetVariant), "the event is recorded only with recording on")

		// Control: a second forget takes nothing and logs nothing.
		r.a.ForgetVariant(ctx, "ns", "m", "agg")
		assert.Len(t, logs.FilterMessage(logging.ForgottenKeyMessage).All(), 2)
	}
}
