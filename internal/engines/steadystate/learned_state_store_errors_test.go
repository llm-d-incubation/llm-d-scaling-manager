package steadystate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A store that can be read and not written -- the learned-state Role missing,
// the manager Role still reading ConfigMaps -- reports the write failure once
// and its recovery once. With one error slot for both operations, each
// minute's successful read cleared the write's error: "works again" and the
// same Forbidden were logged every minute, a store that looked like it kept
// recovering.
func TestAStoreThatCannotBeWrittenSaysSoOnce(t *testing.T) {
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
	forbidden := func(name string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, name, nil)
	}
	c := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if denied {
				return forbidden(obj.GetName())
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if denied {
				return forbidden(obj.GetName())
			}
			return cl.Update(ctx, obj, opts...)
		},
	})
	now := time.Now()
	e := snapshotEngine(t, c, &stateSpy{state: sampleState(4000)})
	e.restoreLearnedState(ctx, now)
	for i := range 4 {
		e.writeLearnedState(ctx, now.Add(time.Duration(i)*learnedStateInterval))
	}
	assert.Equal(t, 1, count("could not write the snapshot"), "the write failure is logged once, not every interval")
	assert.Zero(t, count("again"), "a read that worked reported the store as recovered while it still could not be written")

	denied = false
	e.writeLearnedState(ctx, now.Add(4*learnedStateInterval))
	assert.Equal(t, 1, count("the snapshot can be written again"), "the recovery of the write is logged once")
	assert.Zero(t, count("the snapshot can be read again"), "the read never failed")
}

// The loop stopping -- an upgrade, a leader hand-off -- writes what was
// learned since the last write, inside the interval that would hold an
// ordinary write back; a controller that never read the snapshot writes
// nothing over it.
func TestTheLoopStoppingWritesTheLatestState(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, c, spy)
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	spy.state = sampleState(5000)
	e.writeLearnedState(ctx, now.Add(10*time.Second))
	before, _ := storedDoc(t, c)
	assert.Equal(t, []float64{4000}, before.State.K2["ns|v|m|H100|1|both|short|q5"].Values, "precondition: the interval holds an ordinary write back")

	e.flushLearnedState()
	after, _ := storedDoc(t, c)
	assert.Equal(t, []float64{5000}, after.State.K2["ns|v|m|H100|1|both|short|q5"].Values, "the loop stopped without writing what it learned")

	unread := snapshotEngine(t, c, &stateSpy{state: sampleState(9000)})
	unread.flushLearnedState()
	kept, _ := storedDoc(t, c)
	assert.Equal(t, []float64{5000}, kept.State.K2["ns|v|m|H100|1|both|short|q5"].Values, "a controller that never read the snapshot overwrote it")
}

// The loop stopping writes clock progress the digest leaves out -- a
// window's last use, a bound's tick -- which an ordinary write only carries
// on the refresh: the next leader otherwise read the time since as a pause.
func TestTheLoopStoppingWritesClockOnlyProgress(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, c, spy)
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	const key = "ns|v|m|H100|1|both|short|q5"
	used := now.Add(5 * time.Minute).UTC().Truncate(time.Second)
	w := spy.state.K2[key]
	w.LastUsed = used
	spy.state.K2[key] = w

	e.flushLearnedState()
	got, _ := storedDoc(t, c)
	assert.True(t, got.State.K2[key].LastUsed.Equal(used),
		"the loop stopped without writing clock progress the digest leaves out: last used %v, want %v", got.State.K2[key].LastUsed, used)
}

// A snapshot the operator deleted, and the controller has since found gone
// and forgotten, is not created again by the loop stopping: the restart that
// follows a delete-and-restart reset starts cold.
func TestTheLoopStoppingDoesNotRecreateADeletedSnapshot(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	now := time.Now()
	spy := &stateSpy{state: sampleState(4000)}
	e := snapshotEngine(t, c, spy)
	e.restoreLearnedState(ctx, now)
	e.writeLearnedState(ctx, now)
	_, cm := storedDoc(t, c)
	require.NoError(t, c.Delete(ctx, cm))
	e.writeLearnedState(ctx, now.Add(learnedStateInterval))
	require.False(t, e.learnedState.seen, "precondition: the deletion was found")

	e.flushLearnedState()
	got := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKey{Namespace: cm.Namespace, Name: cm.Name}, got)
	assert.True(t, apierrors.IsNotFound(err), "the loop stopping created the deleted snapshot again: %v", err)
}
