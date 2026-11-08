package itl

import "github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"

// A key's line across a rollout (issue #481). A line is one engine's, and a
// rollout puts two under one key: the readings of each stay out of the other's
// fit. So the key remembers the revision its readings are of, and the rows
// decide (domain.NewestRevision): not the scale target's status, which a plain
// scale trips too.
//
// While the target rolls out, a line is offered the newest revision's rows
// alone. With no pod of a new revision serving it is offered none, and holds
// what it has: the old pods are what serves, and the old line prices them. The
// first rows of a new revision clear the window (clearWindowLocked), which
// keeps B and the held slope: a full fit never spans two engines, and the line
// holds its slope until 10 new readings arrive.

// revisionState is the revision a key's readings are of.
type revisionState struct {
	revision string
	// moved is set when the key moved to a new revision during a rollout: the
	// revision is then the new one, and its pods are offered.
	moved bool
}

// RolloutSight is what a key's rows say of a rollout this cycle.
type RolloutSight struct {
	// Rows is what the line is offered.
	Rows []domain.ReplicaMetrics
	// Held is a rollout with no pod of a new revision serving, or none that
	// can be told: the line is offered nothing and holds what it has.
	Held bool
	// Moved is this cycle's move to a new revision: the window was cleared.
	Moved bool
}

// RolloutRows is what variant's line under key takes of rows this cycle, and
// clears the window on a real change of revision. rolling is the scale target
// saying it rolls out, or the rows reporting two revisions. A plain scale that
// trips the status changes no revision and clears nothing.
func (t *Tracker) RolloutRows(key, variant string, rows []domain.ReplicaMetrics, rolling bool) RolloutSight {
	newest, distinct := domain.NewestRevision(rows, variant)
	t.mu.Lock()
	defer t.mu.Unlock()
	st, known := t.revisions[key]
	if !rolling {
		sight := RolloutSight{Rows: rows}
		if newest == "" {
			return sight
		}
		t.revisions[key] = revisionState{revision: newest}
		if known && newest != st.revision {
			t.clearWindowLocked(key)
			sight.Moved = true
		}
		return sight
	}
	// Which pods are new cannot be told: no revision, two with no order, or
	// a key with no memory and one revision, which may be either.
	if newest == "" || (!known && distinct < 2) || (known && newest == st.revision && !st.moved) {
		return RolloutSight{Held: true}
	}
	// A key already on the new revision whose only Ready revision is now
	// another: the new pods flapped, or the rollout turned back. Moving would
	// clear the window and teach the line from the pods it left, and moving
	// again when the new pod returns would clear it again, so a crash-looping
	// rollout never reached 10 readings. It holds; a rollback that finishes
	// moves it when the status clears.
	if known && st.moved && newest != st.revision && distinct < 2 {
		return RolloutSight{Held: true}
	}
	var sight RolloutSight
	if !known || newest != st.revision {
		t.revisions[key] = revisionState{revision: newest, moved: true}
		t.clearWindowLocked(key)
		sight.Moved = true
	}
	for _, rm := range rows {
		if rm.VariantName == variant && rm.Revision == newest {
			sight.Rows = append(sight.Rows, rm)
		}
	}
	return sight
}

// dropRevisions forgets the revision of every key stale reports true for: the
// variant is gone. An operator's forget and a change of arguments keep it,
// since it says what the replicas run now and fits nothing.
func (t *Tracker) dropRevisions(stale func(key string) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.revisions {
		if stale(key) {
			delete(t.revisions, key)
		}
	}
}
