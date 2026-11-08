package steadystate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// Rules mutation testing found unpinned: each test fails with the line it
// names reverted.

// equalClockState is n k2 windows, every one last used at the same instant:
// only the key tie-break orders them.
func equalClockState(n int, at time.Time) saturation.LearnedState {
	s := saturation.LearnedState{K2: map[string]capacity.RollingAverageSnapshot{}}
	for i := range n {
		s.K2[fmt.Sprintf("ns|v|m|H100|1|both|b%03d|q5", i)] = capacity.RollingAverageSnapshot{
			Values: []float64{4000}, LastUsed: at, LastWritten: at,
		}
	}
	return s
}

// sizeAfterDropping is the encoded size with the first k entries left out,
// built as encodeLearnedState builds each try.
func sizeAfterDropping(t *testing.T, s saturation.LearnedState, now time.Time, k int) int {
	t.Helper()
	c := cloneLearnedState(s)
	for _, e := range learnedStateEntries(c)[:k] {
		e.drop()
	}
	raw, err := json.Marshal(learnedStateDoc{
		Version: learnedStateVersion, WrittenAt: now,
		Writer: learnedStateWriter{Namespace: config.SystemNamespace(), Identity: learnedStateIdentity()},
		State:  c,
	})
	require.NoError(t, err)
	return len(raw)
}

// The trim leaves out the fewest entries that fit, at every budget, and the
// same ones every time: with equal clocks only the key orders them.
func TestTheTrimIsMinimalAndDeterministic(t *testing.T) {
	now := time.Now()
	s := equalClockState(40, now)
	full := sizeAfterDropping(t, s, now, 0)
	for cut := 1; cut < full/2; cut += 37 {
		budget := full - cut
		raw, dropped, err := encodeLearnedState(s, nil, now, budget)
		require.NoError(t, err)
		require.LessOrEqual(t, len(raw), budget)
		if dropped > 0 {
			assert.Greater(t, sizeAfterDropping(t, s, now, dropped-1), budget,
				"budget %d: %d entries left out where %d would fit", budget, dropped, dropped-1)
		}
	}
	first, _, err := encodeLearnedState(s, nil, now, full*2/3)
	require.NoError(t, err)
	for range 10 {
		again, _, err := encodeLearnedState(s, nil, now, full*2/3)
		require.NoError(t, err)
		require.Equal(t, first, again, "the trim left out different entries on another call")
	}
}

// A restore whose read fails and then works reports a read failure and its
// recovery as reads: counted as read errors, recovered with "can be read
// again", and no write line.
func TestARestoreReadFailureIsReportedAsARead(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))
	var logged []string
	ctx := ctrl.LoggerInto(context.Background(),
		funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}))
	count := func(msg string) int {
		n := 0
		for _, l := range logged {
			if strings.Contains(l, msg) {
				n++
			}
		}
		return n
	}
	denied := true
	c := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if denied && key.Name == learnedStateConfigMap {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
	now := time.Now()
	e := snapshotEngine(t, c, &stateSpy{state: sampleState(4000)})
	e.metricsEmitter = metrics.NewMetricsEmitter()
	e.restoreLearnedState(ctx, now)
	denied = false
	e.restoreLearnedState(ctx, now.Add(2*learnedStateInterval))
	assert.Equal(t, 1, count("the snapshot can be read again"), "the restore that read the snapshot did not log its recovery")
	e.writeLearnedState(ctx, now.Add(2*learnedStateInterval))
	assert.Equal(t, 1, count("could not read the snapshot"))
	assert.Equal(t, 1, count("the snapshot can be read again"), "the read's recovery was not logged once")
	assert.Zero(t, count("the snapshot can be written again"), "a read failure was recovered as a write")
	assert.Equal(t, 1.0, counterValue(t, registry, constants.LLMSMLearnedStateSnapshotErrorsTotal, constants.LabelOperation, "read"))
	assert.Zero(t, counterValue(t, registry, constants.LLMSMLearnedStateSnapshotErrorsTotal, constants.LabelOperation, "write"))
}
