package scalefromzero

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// referenceRecorder does what the real recorder does first: it builds a
// reference to the object, and on a failure logs "Could not construct reference"
// and drops the event. record.FakeRecorder skips that step, so an event on an
// object in no scheme reads as recorded. Here it fails the test.
type referenceRecorder struct {
	t      *testing.T
	scheme *runtime.Scheme

	mu   sync.Mutex
	refs map[string][]*corev1.ObjectReference
}

var _ record.EventRecorder = (*referenceRecorder)(nil)

func (r *referenceRecorder) record(object runtime.Object, reason string) {
	got, err := reference.GetReference(r.scheme, object)
	if err != nil {
		r.t.Errorf("the recorder would drop the %s event on %T: %v", reason, object, err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs[reason] = append(r.refs[reason], got)
}

func (r *referenceRecorder) Event(object runtime.Object, _, reason, _ string) {
	r.record(object, reason)
}

func (r *referenceRecorder) Eventf(object runtime.Object, _, reason, _ string, _ ...any) {
	r.record(object, reason)
}

func (r *referenceRecorder) AnnotatedEventf(object runtime.Object, _ map[string]string, _, reason, _ string, _ ...any) {
	r.record(object, reason)
}

// TestWakeRecordsScaledUpOnTheScaledObject: the variant is synthesized and in no
// scheme, so a ScaledUp event recorded on it never reaches the API server. It
// hangs on the ScaledObject, where kubectl describe shows it.
func TestWakeRecordsScaledUpOnTheScaledObject(t *testing.T) {
	f := newWakeFixture(t, "target")
	f.src.values = []source.MetricValue{queueDepth(constants.EPPFlowControlQueueSize, f.model, 3)}
	rec := &referenceRecorder{t: t, scheme: f.engine.client.Scheme(), refs: map[string][]*corev1.ObjectReference{}}
	f.engine.recorder = rec

	require.NoError(t, f.engine.optimize(context.Background()))
	_, published, _ := f.woken()
	require.True(t, published, "the fixture must wake, or nothing here was recorded")

	refs := rec.refs[constants.K8SEventScaledUp]
	require.Len(t, refs, 1, "one ScaledUp event for the wake")
	assert.Equal(t, "ScaledObject", refs[0].Kind)
	assert.Equal(t, "keda.sh/v1alpha1", refs[0].APIVersion)
	assert.Equal(t, f.variant, refs[0].Name)
	assert.Equal(t, f.ns, refs[0].Namespace)
}
