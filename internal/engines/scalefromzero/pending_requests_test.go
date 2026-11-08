/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalefromzero

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

const pendingTestModelID = "e2e-scaling-manager/dummy-model"

// queueSample builds one EPP flow-control queue-size sample as the pod scraping
// source hands it over: the metric family in __name__, the rest as labels.
func queueSample(metricName, targetModel string, value float64) source.MetricValue {
	return source.MetricValue{
		Value: value,
		Labels: map[string]string{
			metricNameLabel:      metricName,
			targetEPPMetricLabel: targetModel,
			"model_name":         targetModel,
			"fairness_id":        "default-flow",
			"priority":           "0",
			"inference_pool":     "optimized-baseline",
		},
	}
}

func TestPendingRequestsForModel(t *testing.T) {
	tests := []struct {
		name      string
		values    []source.MetricValue
		wantFound bool
		// wantMetricName is the family the returned sample must come from, so a
		// regression that reads the wrong (or the deprecated) family is caught
		// rather than passing on the alias.
		wantMetricName string
		wantValue      float64
		// wantQueueFamily is whether EPP exported the flow-control queue family at
		// all — the distinction between "nothing is queued" and "nothing can ever
		// wake this model". Every wantFound: false case below is one or the other,
		// and they demand opposite responses from an operator, so the table states
		// it explicitly rather than leaving it to the zero value.
		wantQueueFamily bool
	}{
		{
			name:            "current metric name with queued requests",
			values:          []source.MetricValue{queueSample(constants.EPPFlowControlQueueSize, pendingTestModelID, 3)},
			wantFound:       true,
			wantMetricName:  constants.EPPFlowControlQueueSize,
			wantValue:       3,
			wantQueueFamily: true,
		},
		{
			name:            "deprecated metric name alone is still honoured",
			values:          []source.MetricValue{queueSample(constants.SchedulerFlowControlQueueSize, pendingTestModelID, 2)},
			wantFound:       true,
			wantMetricName:  constants.SchedulerFlowControlQueueSize,
			wantValue:       2,
			wantQueueFamily: true,
		},
		{
			name: "both families exported: the current one is read",
			values: []source.MetricValue{
				queueSample(constants.SchedulerFlowControlQueueSize, pendingTestModelID, 5),
				queueSample(constants.EPPFlowControlQueueSize, pendingTestModelID, 5),
			},
			wantFound:       true,
			wantMetricName:  constants.EPPFlowControlQueueSize,
			wantValue:       5,
			wantQueueFamily: true,
		},
		{
			name: "current family exported and empty: the deprecated alias must not override it",
			values: []source.MetricValue{
				queueSample(constants.EPPFlowControlQueueSize, pendingTestModelID, 0),
				// A stale alias sample would wake the workload with nothing queued.
				queueSample(constants.SchedulerFlowControlQueueSize, pendingTestModelID, 4),
			},
			wantFound:       false,
			wantQueueFamily: true,
		},
		{
			name:            "queue empty",
			values:          []source.MetricValue{queueSample(constants.EPPFlowControlQueueSize, pendingTestModelID, 0)},
			wantFound:       false,
			wantQueueFamily: true,
		},
		{
			name:            "queued requests belong to another model",
			values:          []source.MetricValue{queueSample(constants.EPPFlowControlQueueSize, "other/model", 7)},
			wantFound:       false,
			wantQueueFamily: true,
		},
		{
			name: "this model's queue is picked out of a mixed set",
			values: []source.MetricValue{
				queueSample(constants.EPPFlowControlQueueSize, "other/model", 7),
				queueSample(constants.EPPFlowControlQueueSize, pendingTestModelID, 1),
			},
			wantFound:       true,
			wantMetricName:  constants.EPPFlowControlQueueSize,
			wantValue:       1,
			wantQueueFamily: true,
		},
		{
			name: "unrelated EPP metrics are ignored",
			values: []source.MetricValue{
				{
					Value:  9,
					Labels: map[string]string{metricNameLabel: "llm_d_epp_flow_control_queue_bytes", targetEPPMetricLabel: pendingTestModelID},
				},
				{
					Value:  9,
					Labels: map[string]string{metricNameLabel: "inference_pool_ready_pods", "name": "optimized-baseline"},
				},
			},
			wantFound: false,
		},
		{
			name:      "no metrics at all",
			values:    nil,
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, familyExported := pendingRequestsForModel(tt.values, pendingTestModelID)
			require.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantQueueFamily, familyExported,
				"an absent queue family means the model cannot be woken; an empty queue means it has nothing to serve")
			if !tt.wantFound {
				return
			}
			assert.Equal(t, tt.wantMetricName, got.Labels[metricNameLabel])
			assert.Equal(t, tt.wantValue, got.Value)
		})
	}
}

// TestPendingRequestsForModelMatchesDeployedEPPLabels pins the label set the
// llm-d EPP actually exports on this gauge. Upstream
// gateway-api-inference-extension labels it {fairness_id, priority} only; the
// model scoping the engine relies on comes from llm-d's added
// target_model_name. If that label disappears the engine must stop matching —
// silently waking every variant on the pool would be worse than not waking.
func TestPendingRequestsForModelMatchesDeployedEPPLabels(t *testing.T) {
	upstreamShape := []source.MetricValue{
		{
			Value: 4,
			Labels: map[string]string{
				metricNameLabel: constants.SchedulerFlowControlQueueSize,
				"fairness_id":   "default-flow",
				"priority":      "0",
			},
		},
	}

	_, found, familyExported := pendingRequestsForModel(upstreamShape, pendingTestModelID)
	assert.False(t, found, "a queue sample with no target_model_name must not match a specific model")
	// The family IS exported here — flow control is running, it just labels the
	// sample in a way this engine cannot attribute. That is a very different
	// problem from EPP not queueing at all, and must not be reported as one.
	assert.True(t, familyExported)
}
