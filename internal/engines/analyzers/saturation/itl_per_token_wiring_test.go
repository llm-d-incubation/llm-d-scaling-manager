package saturation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// The engine paths of the ITL unit (#274): what Analyze and a restore do,
// not the helpers they call.

// A speculating variant restored from a document in the other unit, and
// exported before its first cycle installs it, does not write its pending
// step-time entries under the per-token mark its live configuration earns:
// read back, they would be taken as per token. From a marked document they
// go out, marked.
func TestITLPerToken_APendingSpeculatingVariantInTheOtherUnitIsNotExported(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("marked=%v", marked), func(t *testing.T) {
			params := specParams()
			a, _, _ := waProcess(params)
			key := a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1)
			stash := stashKey(key, "older-args")
			now := time.Now()
			a.mu.Lock()
			a.variantSeenAt[key] = now
			a.mu.Unlock()
			doc := LearnedState{
				Config:           map[string]string{"test-ns/variant-a": params.Fingerprint()},
				Args:             map[string]string{"test-ns/variant-a": params.ArgsFingerprint()},
				VariantSeenAt:    map[string]time.Time{key: now},
				ITLBaseline:      map[string]float64{key: 0.012},
				ITLSlopes:        map[string]SlopeSnapshot{key: {A: 0.05, FittedAt: now.Add(-time.Hour), Args: params.ArgsFingerprint()}},
				ITLBaselineStash: map[string]float64{stash: 0.02},
			}
			if marked {
				doc.ITLPerToken = map[string]bool{"test-ns/variant-a": true}
			}
			a.RestoreLearnedState(doc)
			s := a.ExportLearnedState()
			assert.True(t, s.ITLPerToken["test-ns/variant-a"], "the live speculating variant is marked")
			if marked {
				assert.Contains(t, s.ITLBaseline, key)
				assert.Contains(t, s.ITLSlopes, key)
				assert.Contains(t, s.ITLBaselineStash, stash)
				return
			}
			assert.NotContains(t, s.ITLBaseline, key, "a step-time baseline exported under the per-token mark")
			assert.NotContains(t, s.ITLSlopes, key, "a step-time slope exported under the per-token mark")
			assert.NotContains(t, s.ITLBaselineStash, stash, "a step-time put-aside baseline exported under the per-token mark")
		})
	}
}

// fitLines logs itl-withheld through Analyze: once over cycles of a
// speculating variant with no counter, with idle cycles between.
func TestITLPerToken_AnalyzeLogsAWithheldReadingOnce(t *testing.T) {
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "spec", capacity.Record{AcceleratorName: "H100", GpuCount: 1,
		EngineParams: specParams()}, propStart)
	a := NewSaturationAnalyzer(store)
	clock := propStart
	a.now = func() time.Time { return clock }
	var lines []string
	ctx := ctrl.LoggerInto(context.Background(),
		funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{Verbosity: 2}))
	states := []domain.VariantReplicaState{
		{VariantName: "spec", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	for i := range 6 {
		rm := makeReplicaMetrics("spec-0", "spec", 5000, 16000, 0, 6000, 1000)
		rm.KvUsageInstant, rm.KvUsageInstantKnown = 0.4, true
		if i%2 == 0 {
			rm.AvgITL, rm.AvgITLKnown = 0.03, true
		} else {
			rm.AvgITLEmpty = true
		}
		_, err := a.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states))
		require.NoError(t, err)
		clock = clock.Add(propCycle)
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, `"itl-withheld"`) {
			n++
			assert.Contains(t, l, "no vllm:spec_decode_num_accepted_tokens_total")
		}
		assert.NotContains(t, l, `"itl-withheld-ended"`, "an idle cycle is no evidence the withholding ended")
	}
	assert.Equal(t, 1, n, "one itl-withheld line over six cycles")
}

// fitLines publishes a speculating variant's own line marked per token.
func TestITLPerToken_AnalyzePublishesAMarkedLine(t *testing.T) {
	line, found, err := specFleetLine(1, nil)
	require.NoError(t, err)
	require.True(t, found, "the variant published a line")
	assert.True(t, line.perToken, "a speculating variant's line is marked per token")
}

// fitLines lends a speculating borrower only a line in its own unit, and a
// line restored from a document that marks nothing is in step time: the
// borrower takes it only with the correction off, and a borrower that does
// not speculate takes it either way.
func TestITLPerToken_AnalyzeBorrowsAnOldRestoredLineOnlyInItsUnit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params *capacity.EngineParams
		lineIn bool // the line restored is marked per token
		want   bool
	}{
		{"speculating borrower, old line", specParams(), false, false},
		{"speculating borrower, per-token line", specParams(), true, true},
		{"borrower not speculating, old line", keyParams(256), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := capacity.NewStore()
			store.Update("test-ns", "test-model", "starved", capacity.Record{AcceleratorName: "H100", GpuCount: 1,
				EngineParams: tc.params}, propStart)
			a := NewSaturationAnalyzer(store)
			a.now = func() time.Time { return propStart }
			physics := a.itlPhysicsKey("test-ns", "test-model", "starved", "H100", 1,
				a.engineFingerprint("test-ns", "test-model", "starved"))
			require.NotEmpty(t, physics)
			doc := LearnedState{ITLLines: map[string]LineSnapshot{physics: {A: 0.05, B: 0.01,
				FromVariant: "test-ns|test-model|lender|H100|1", LearnedAt: propStart.Add(-time.Minute)}}}
			if tc.lineIn {
				doc.ITLLinesPerToken = map[string]bool{physics: true}
			}
			a.RestoreLearnedState(doc)

			rm := makeReplicaMetrics("starved-0", "starved", 1000, 16000, 0, 6000, 1000)
			rm.KvUsageInstant, rm.KvUsageInstantKnown = 0.05, true
			in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
				{VariantName: "starved", Role: domain.RoleDecode, AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			})
			in.Config.(*config.ScalingPolicy).EnableLearnedStateReuse = true
			c := a.newCycle(context.Background(), in, in.Config.(*config.ScalingPolicy))
			c.observeFleetShape()
			c.resolvePricing()
			c.fitLines()
			borrowed := c.itlBorrowedFrom["starved"] != ""
			assert.Equal(t, tc.want, borrowed, "borrowed %v, line %+v", borrowed, c.itlModels["starved"])
			if tc.want {
				assert.Equal(t, itl.Model{A: 0.05, B: 0.01}, c.itlModels["starved"])
			}
		})
	}
}

// A variant restored from a marked document and not yet seen by this process
// has only pending entries: they go out with their per-token mark, or the
// next restart would read them as step time and drop them.
func TestITLPerToken_APendingOnlyVariantKeepsItsMark(t *testing.T) {
	params := specParams()
	a, _, _ := waProcess(params)
	key := a.itlWindowKey("test-ns", "test-model", "variant-a", "H100", 1)
	now := time.Now()
	a.RestoreLearnedState(LearnedState{
		Config:        map[string]string{"test-ns/variant-a": params.Fingerprint()},
		Args:          map[string]string{"test-ns/variant-a": params.ArgsFingerprint()},
		VariantSeenAt: map[string]time.Time{key: now},
		ITLBaseline:   map[string]float64{key: 0.012},
		ITLPerToken:   map[string]bool{"test-ns/variant-a": true},
	})
	s := a.ExportLearnedState()
	assert.Contains(t, s.ITLBaseline, key, "the pending baseline is exported")
	assert.True(t, s.ITLPerToken["test-ns/variant-a"], "the pending per-token baseline lost its mark")
}

// The log state of a variant that is gone ages with it, as its ITL arguments
// do: a variant deleted while its readings were withheld leaves nothing.
func TestITLPerToken_TheWithheldLogStateAgesWithItsVariant(t *testing.T) {
	a := NewSaturationAnalyzer(capacity.NewStore())
	live, gone := "ns|m|live|H100|1", "ns|m|gone|H100|1"
	a.mu.Lock()
	a.itlWithheldLogged[live] = itlWithheldReasons{noCounter: 1}
	a.itlWithheldLogged[gone] = itlWithheldReasons{noCounter: 1}
	a.variantSeenAt[live] = a.now()
	a.mu.Unlock()
	a.EvictStaleHistory()
	a.mu.Lock()
	defer a.mu.Unlock()
	assert.Contains(t, a.itlWithheldLogged, live)
	assert.NotContains(t, a.itlWithheldLogged, gone, "a variant no longer seen kept its withheld-log state")
}
