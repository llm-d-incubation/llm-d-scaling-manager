package saturation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/shape"
)

// The saturation analyzer's calls into internal/signals/whatif, pinned where
// they pass a replica's terms: its engine ranks and the deployment's
// parameters. whatif's own tests price the arithmetic; these hold that the
// analyzer hands it the replica in front of it.

// A replica with no live KV reading is sized from the deployment's
// --max-total-tokens flag, which sizes one engine, times its ranks. At
// priceReplicas that capacity reaches the derived mu: four ranks of 100,000
// flag tokens hold four times the sequences one would.
func TestWhatifWiring_TheDerivedMuIsPricedOnEveryRanksFlagCapacity(t *testing.T) {
	const ranks, flagTokens = 4, 100_000
	ctx, logs := observedCtx(t)
	params := deploymentParams()
	// A cap that does not bind, so the sequence count is the cache's.
	params.MaxNumSeqs = 4096
	params.TotalKvTokensOverride = flagTokens
	store := capacity.NewStore()
	store.Update("test-ns", "test-model", "variant-d", capacity.Record{
		AcceleratorName: "H200", GpuCount: 1, EngineParams: params,
	}, time.Now())
	a := NewSaturationAnalyzer(store)
	line := itl.Model{A: 0.08, B: 0.01}
	a.publishLine(a.itlPhysicsKey("test-ns", "test-model", "variant-d", "H200", 1, ""),
		"a fitted line", line, time.Now())
	// No live capacity reading: the flag is all there is.
	rm := makeReplicaMetrics("pod-1", "variant-d", 0, 0, 0, 100, 50)
	rm.EngineRanks = ranks
	require.Equal(t, ranks, rm.Ranks())
	in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
		{VariantName: "variant-d", Role: domain.RoleDecode, AcceleratorName: "H200",
			CurrentReplicas: 1, GPUsPerReplica: 1},
	})
	_, err := a.Analyze(ctx, in)
	require.NoError(t, err)

	fields := requireLogged(t, logs, "derived-mu")
	require.Equal(t, true, fields["ok"], "the line prices mu: %v", fields)
	kPrice := fields["kPrice"].(float64)
	kvReq := fields["kvReqPerSeq"].(float64)
	divisor := fields["muDivisor"].(float64)
	require.Greater(t, kvReq, 0.0)
	require.Greater(t, divisor, 0.0)

	wantSeqs := kPrice * ranks * flagTokens / kvReq
	require.Less(t, wantSeqs, float64(params.MaxNumSeqs*ranks), "precondition: the cap does not bind")
	assert.InDelta(t, wantSeqs, fields["seqs"], 1e-9*wantSeqs,
		"the sequences of four ranks' flag capacity, not one rank's")
	assert.InDelta(t, wantSeqs/line.ITLAt(kPrice)/divisor, fields["rate"], 1e-9*wantSeqs,
		"the rate those sequences complete at")
}

// aggregatedShapeNow sizes an aggregated variant's shape g on the replica's
// whole capacity: with no live reading, the flag times the ranks.
func TestWhatifWiring_TheAggregatedShapeIsSizedOnEveryRanksFlagCapacity(t *testing.T) {
	const ranks, flagTokens = 8, 160_000
	params := keyParams(256)
	params.TotalKvTokensOverride = flagTokens
	rm := makeReplicaMetrics("agg-0", "agg", 0, 0, 0, 1000, 500)
	rm.EngineRanks = ranks
	require.Equal(t, ranks, rm.Ranks())
	c := &cycle{a: NewSaturationAnalyzer(capacity.NewStore()), muDivisor: 500, fleetInput: 1000, fleetOutput: 500,
		input: domain.AnalyzerInput{ReplicaMetrics: []domain.ReplicaMetrics{rm}}}

	s := shape.New(1000, 500, 0)
	want := shapeG(ranks*flagTokens, s.ILeff, s.KVreq, 500)
	require.Greater(t, want, 0.0)
	assert.InDelta(t, want, c.aggregatedShapeNow("agg", params), 1e-9*want,
		"g = C*IL_eff/(KVreq*OL) on eight ranks of the flag")
}

// The real caller of whatif.PricingK. A policy whose band is malformed never
// reaches it: ITLBand's validation resets such a band to the default, so
// pricingK prices on the default band whatever the band says, and the repair
// inside PricingK is a second line behind it.
func TestWhatifWiring_PricingKOnAMalformedBand(t *testing.T) {
	for _, b := range [][2]float64{{0.5, 0.3}, {0.15, 1.5}, {-0.1, 0.8}, {0.4, 0.4}} {
		policy := func(kv, scaleUp float64) *config.ScalingPolicy {
			cfg := &config.ScalingPolicy{KvCacheThreshold: kv, ScaleUpThreshold: scaleUp}
			cfg.AnalyzerTuning.ITLBand.MinObservableK, cfg.AnalyzerTuning.ITLBand.MaxObservableK = b[0], b[1]
			return cfg
		}
		lo, hi := policy(0.99, 0.99).ITLBand()
		assert.Equal(t, [2]float64{config.DefaultITLMinObservableK, config.DefaultITLMaxObservableK}, [2]float64{lo, hi},
			"validation resets the band %v to the default", b)
		assert.Equal(t, config.DefaultITLMaxObservableK, pricingK(policy(0.99, 0.99)),
			"a product above the band %v prices at the default top", b)
		assert.Equal(t, config.DefaultITLMinObservableK, pricingK(policy(0.1, 0.5)),
			"a product below the band %v prices at the default bottom", b)
	}
}
