package settle

import "time"

// The settle tracker's recorder kinds live here rather than in enginerec,
// which holds the composite's: the replay applies them to its own settle
// tracker beside the analyzers, not through the engine's replay.

// KindObserve is the learner recording's event for the engine's Observe of
// one model's cycle (docs/proposals/analyzer-learner.md, 2.4). It records
// the model and, as the event's clock, the instant the engine observed at;
// the rows are the ones the model's next Analyze event records, so they are
// not recorded twice. A replay observes its own tracker with them at that
// instant.
const KindObserve = "settle.Observe"

// KindHold is the learner recording's event for the engine's Hold of one
// model: a cycle that read no rows of it. Its data is an ObserveEvent.
const KindHold = "settle.Hold"

// KindRestore is the learner recording's event for the engine handing the
// tracker the sightings a learned-state snapshot held
// (Tracker.Restore). Its data is a RestoreEvent.
const KindRestore = "settle.Restore"

// RestoreEvent is KindRestore's data: the snapshot's sightings, model by
// model, and the interval the engine observes at, which sets how long an
// unclaimed one is kept. The model is named by its parts, not the engine's
// namespaced key: a recording's export renames a namespace and a model ID
// each whole.
type RestoreEvent struct {
	Models   []RestoredModel
	Interval time.Duration
}

// RestoredModel is one model's sightings in a RestoreEvent.
type RestoredModel struct {
	Namespace string
	ModelID   string
	Sightings []Sighting
}

// ObserveEvent is KindObserve's and KindHold's data: the model, and the
// interval the engine observes at, which sets the tracker's tolerance
// (Tracker.SetInterval).
type ObserveEvent struct {
	Namespace string
	ModelID   string
	Interval  time.Duration
}
