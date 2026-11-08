// Package decision holds the latest per-target scaling decision in memory so it
// can be shared between the optimize pipeline (writer) and the KEDA external
// scaler (reader) without a round-trip through the Kubernetes API or Prometheus.
//
// The old CRD has been removed (variants are synthesized in-memory
// each cycle), so the desired-replica decision is not persisted anywhere the
// external scaler could read. This store mirrors the value the actuator emits to
// the llmsm_desired_replicas gauge, keyed by the scale target's namespace/name.
//
// A process-wide Default store is used, mirroring the package-level metrics
// registries already in this codebase.
package decision

import (
	"sync"
	"time"
)

// Decision is the latest scaling decision the scaling manager computed for one scale target.
type Decision struct {
	// DesiredReplicas is the optimizer's target replica count.
	DesiredReplicas int32
	// UpdatedAt is when the decision was last written.
	UpdatedAt time.Time
}

// Store keeps the latest Decision per scale target, keyed by namespace/name
// (the Deployment/LWS name, equal to the former Variant name). It is
// safe for concurrent use.
//
// Writers may also be observed: Subscribe returns a channel that is signalled
// whenever a given target's decision is written, which the external scaler's
// StreamIsActive uses to push activation to KEDA the moment a decision lands
// rather than waiting for KEDA's next poll.
type Store struct {
	mu sync.RWMutex
	m  map[string]Decision
	// subs holds the notification channels per target key. The inner map is
	// keyed by a monotonic id so a subscriber can remove exactly its own
	// channel without identity comparison.
	subs      map[string]map[uint64]chan struct{}
	nextSubID uint64
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		m:    make(map[string]Decision),
		subs: make(map[string]map[uint64]chan struct{}),
	}
}

func storeKey(namespace, name string) string {
	return namespace + "/" + name
}

// Set records the latest desired replica count for a scale target and wakes any
// subscribers watching it.
func (s *Store) Set(namespace, name string, desiredReplicas int32) {
	key := storeKey(namespace, name)

	s.mu.Lock()
	s.m[key] = Decision{
		DesiredReplicas: desiredReplicas,
		UpdatedAt:       time.Now(),
	}
	// Collect under the same lock that published the write, so a subscriber
	// registered before this Set is guaranteed to be woken for it.
	waiters := make([]chan struct{}, 0, len(s.subs[key]))
	for _, ch := range s.subs[key] {
		waiters = append(waiters, ch)
	}
	s.mu.Unlock()

	// Signal outside the lock, and never block: the channels are buffered to 1
	// and carry no payload, so a pending wake-up already covers this write.
	// Subscribers re-read the current decision with Get when they wake.
	for _, ch := range waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe registers interest in a scale target and returns a channel that
// receives a value whenever that target's decision is written, plus a cancel
// func that unregisters it. Cancel must be called to avoid leaking the
// registration; calling it more than once is safe.
//
// Wake-ups coalesce: the channel is buffered to 1 and signals carry no payload,
// so a subscriber that is slow to read sees one wake-up covering every write it
// missed, and reads the latest state with Get.
func (s *Store) Subscribe(namespace, name string) (<-chan struct{}, func()) {
	key := storeKey(namespace, name)
	ch := make(chan struct{}, 1)

	s.mu.Lock()
	id := s.nextSubID
	s.nextSubID++
	if s.subs[key] == nil {
		s.subs[key] = make(map[uint64]chan struct{})
	}
	s.subs[key][id] = ch
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.subs[key], id)
			if len(s.subs[key]) == 0 {
				delete(s.subs, key)
			}
		})
	}
}

// Get returns the latest Decision for a scale target and whether one exists.
func (s *Store) Get(namespace, name string) (Decision, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.m[storeKey(namespace, name)]
	return d, ok
}

// Default is the process-wide decision store shared between the actuator (writer)
// and the external scaler (reader).
var Default = NewStore()

// Set records a decision in the Default store.
func Set(namespace, name string, desiredReplicas int32) {
	Default.Set(namespace, name, desiredReplicas)
}

// Get reads a decision from the Default store.
func Get(namespace, name string) (Decision, bool) {
	return Default.Get(namespace, name)
}
