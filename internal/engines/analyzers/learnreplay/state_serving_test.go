package learnreplay

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Each variant's serving replicas line up with the arrival rates, one a
// cycle: current less pending, never below 0, -1 where the cycle did not
// name the variant; a snapshot is cut and padded to its own cycle (#297).
func TestServingReplicas_OneACycleByVariant(t *testing.T) {
	serving := map[string][]int{}
	noteServing(serving, []domain.VariantReplicaState{{VariantName: "a", CurrentReplicas: 2}}, 1)
	noteServing(serving, []domain.VariantReplicaState{{VariantName: "a", CurrentReplicas: 3, PendingReplicas: 1},
		{VariantName: "b", CurrentReplicas: 1, PendingReplicas: 2}}, 2)
	noteServing(serving, []domain.VariantReplicaState{{VariantName: "b", CurrentReplicas: 4}}, 3)
	assert.Equal(t, map[string][]int{"a": {2, 2}, "b": {-1, 0, 4}}, serving)
	assert.Equal(t, map[string][]int{"a": {2, 2, -1}, "b": {-1, 0, 4}}, servingUpTo(serving, 3))
	cut := servingUpTo(serving, 2)
	assert.Equal(t, map[string][]int{"a": {2, 2}, "b": {-1, 0}}, cut)
	cut["a"][0] = 9
	assert.Equal(t, 2, serving["a"][0], "a snapshot is its own copy")
	assert.Nil(t, servingUpTo(map[string][]int{}, 3))
}
