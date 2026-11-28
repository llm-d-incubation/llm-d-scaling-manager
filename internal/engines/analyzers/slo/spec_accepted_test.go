package slo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/inferenceengine"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// The switch is off from the constructor: the controller's v2 TPOT model
// reads a row's accepted drafts as it is, as before.
func TestSpecAcceptedAtMostK_OffFromTheConstructor(t *testing.T) {
	a := New(nil, nil)
	fields := a.switchFields()
	require.Contains(t, fields, SwitchSpecAcceptedAtMostK)
	assert.False(t, *fields[SwitchSpecAcceptedAtMostK])
	assert.Contains(t, ReplaySwitchNames(), SwitchSpecAcceptedAtMostK)
	assert.Nil(t, a.maxAcceptedFor("ns", "m"), "off: nothing is held")
}

// On, each variant's k is its engine's num_speculative_tokens, read from
// the store; a variant with no record, or flags that do not speculate,
// reads 0, which holds nothing.
func TestSpecAcceptedAtMostK_ReadsKFromTheStore(t *testing.T) {
	s := capacity.NewStore()
	spec := capacity.EngineParams{Engine: inferenceengine.EngineVLLM,
		SpeculativeConfig: `{"method":"mtp","num_speculative_tokens":3}`}
	plain := capacity.EngineParams{Engine: inferenceengine.EngineVLLM}
	s.Update("ns", "m", "spec", capacity.Record{EngineParams: &spec}, time.Now())
	s.Update("ns", "m", "plain", capacity.Record{EngineParams: &plain}, time.Now())
	a := New(nil, s)
	require.NoError(t, a.SetSwitchesForReplay(map[string]bool{SwitchSpecAcceptedAtMostK: true}))
	k := a.maxAcceptedFor("ns", "m")
	require.NotNil(t, k)
	assert.Equal(t, 3, k("spec"))
	assert.Equal(t, 0, k("plain"))
	assert.Equal(t, 0, k("absent"))
}
