package saturation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Issue ev-shindin/llm-scaling-manager#316 against the frozen documents: the restore-then-run 5a left to
// 5d. A v0.10.1 document restored under light load keeps the ITL line on its
// held slope; a v0.10.0 one, which has no slope, restores and has no line to
// hold, as that release did. The back-compat row of "Shipped behaviour each
// PR must keep".
//
// The fixtures name a configuration by its fingerprints, which no parameters
// here hash to. The test rewrites those two strings, and the fixture's names
// for the rig's, so the variant running mtParams is the variant the document
// describes. Every figure is the document's.
func TestFrozenDocuments_ALightLoadRestoreRunsOnTheHeldSlope(t *testing.T) {
	const (
		fixtureFingerprint = "c4158ae264689362"
		fixtureArgs        = "aa99efb7bc3fa4d9"
		variant            = "variant-01"
		accelerator        = "NVIDIA-H100-80GB-HBM3"
	)
	states := []domain.VariantReplicaState{{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: accelerator,
		CurrentReplicas: 1, GPUsPerReplica: 1}}
	run := func(file string) (ok, held bool, a float64) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "steadystate", "testdata", "learned-state", file))
		require.NoError(t, err)
		params := mtParams(64, "vllm:v1")
		text := strings.NewReplacer(fixtureFingerprint, params.Fingerprint(), fixtureArgs, params.ArgsFingerprint(),
			"ns-01", "test-ns", "model-01", "test-model").Replace(string(raw))
		var doc struct {
			WrittenAt time.Time    `json:"writtenAt"`
			State     LearnedState `json:"state"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &doc))

		store := capacity.NewStore()
		store.Update("test-ns", "test-model", variant, capacity.Record{EngineParams: params}, time.Now())
		sat := NewSaturationAnalyzer(store)
		clock := doc.WrittenAt.Add(2 * time.Minute)
		sat.now = func() time.Time { return clock }
		rep := sat.RestoreLearnedState(doc.State)
		require.Positive(t, rep.PendingVariants, "%s: nothing to install", file)

		rm := makeReplicaMetrics("pod-0", variant, int64(0.06*16000), 16000, 0, 1000, 500)
		rm.KvUsageInstant = 0.06
		rm.AvgITL, rm.AvgITLKnown = 0.01, true
		ctx, logs := observedCtx(t)
		for range 2 {
			_, err = sat.Analyze(context.WithoutCancel(ctx), makeAnalyzerInput([]domain.ReplicaMetrics{rm}, states))
			require.NoError(t, err)
			clock = clock.Add(15 * time.Second)
		}
		e := logs.FilterMessage("derived-mu").All()
		require.NotEmpty(t, e, "%s: a decode replica logs its derived mu", file)
		m := e[len(e)-1].ContextMap()
		ok, _ = m["ok"].(bool)
		held, _ = m["lineHeld"].(bool)
		a, _ = m["itlA"].(float64)
		return ok, held, a
	}

	ok, held, a := run("v0.10.1-v1.json")
	assert.True(t, ok, "v0.10.1: the restarted controller derives mu")
	assert.True(t, held, "v0.10.1: from the held slope")
	assert.InDelta(t, 0.02, a, 1e-9, "the document's slope")

	ok, held, _ = run("v0.10.0-v1.json")
	assert.False(t, ok, "v0.10.0: no slope stored, so no line under light load")
	assert.False(t, held)
}
