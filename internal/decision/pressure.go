package decision

import (
	"sync"
	"time"
)

// Pressure is how close a variant is to needing more capacity.
//
// It exists for the RETAINED pool's switching decision. A retained pool holds
// several models on one set of GPUs with only one awake, and choosing which
// means comparing how hard each is being pushed -- a comparison the pool cannot
// make, because it sees Pods and not load. The optimizer already computes both
// numbers per variant on every pass; this carries them to the component that has
// to choose between variants.
type Pressure struct {
	// SpareFraction is how much of this variant's capacity is NOT in use, as a
	// fraction of its supply: 1.0 is idle, 0.0 is exactly saturated, and
	// negative means demand already exceeds what its own replicas can serve.
	//
	// A fraction rather than an absolute, because the pool is comparing variants
	// of different sizes. Ten thousand spare tokens means one thing on a variant
	// with a hundred thousand and another on a variant with eleven.
	SpareFraction float64

	// NeedsScaleUp is the optimizer's own answer to "does this variant want more
	// replicas", not a second threshold invented here. Reusing it keeps the pool
	// from switching for a variant the optimizer considers comfortable, or
	// sitting still for one it is already trying to grow.
	NeedsScaleUp bool

	// At is when the reading was taken.
	At time.Time
}

// PressureStore holds the current reading per variant.
type PressureStore struct {
	mu sync.RWMutex
	// by is namespace → variant → pressure, where a variant is named by its
	// ScaledObject. That is the identity the analyzer carries, having got it
	// from the collector's walk up a Pod's ownerReferences to the managed
	// scaler; the Deployment beneath it is a different name and reading under
	// it finds nothing.
	by map[string]map[string]Pressure
}

// NewPressureStore returns an empty store.
func NewPressureStore() *PressureStore {
	return &PressureStore{by: map[string]map[string]Pressure{}}
}

// DefaultPressure is the store the optimizer writes and the warm pool reads.
var DefaultPressure = NewPressureStore()

// Publish records one variant's reading.
func (s *PressureStore) Publish(namespace, variant string, p Pressure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]map[string]Pressure{}
	}
	if s.by[namespace] == nil {
		s.by[namespace] = map[string]Pressure{}
	}
	s.by[namespace][variant] = p
}

// Get returns a variant's reading and whether there is a fresh one.
//
// False means "not measured", which is not the same as "comfortable". A variant
// the optimizer has not looked at yet must not be switched TO on the strength of
// a number nobody produced, and must not be switched AWAY from either -- both
// would be acting on an absence.
func (s *PressureStore) Get(namespace, variant string, maxAge time.Duration, now time.Time) (Pressure, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.by[namespace][variant]
	if !ok {
		return Pressure{}, false
	}
	if maxAge > 0 && now.Sub(p.At) > maxAge {
		return Pressure{}, false
	}
	return p, true
}

// Reset drops every reading. For tests.
func (s *PressureStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]map[string]Pressure{}
}

// PublishPressure records a reading in the default store.
func PublishPressure(namespace, variant string, p Pressure) {
	DefaultPressure.Publish(namespace, variant, p)
}

// PressureFor reads a variant's pressure from the default store.
func PressureFor(namespace, variant string, maxAge time.Duration, now time.Time) (Pressure, bool) {
	return DefaultPressure.Get(namespace, variant, maxAge, now)
}
