package decision

import (
	"sync"
	"time"
)

// Activations records when a model was last woken from zero, so the scale-to-zero
// path can leave it alone for its retention period.
//
// A model woken from zero has served nothing yet — the request that triggered the
// wake is still queued in the EPP while the pod pulls and loads. The scale-to-zero
// idle signal is a request counter over the retention window
// (sum(increase(vllm:request_success_total[retentionPeriod]))), which reads zero
// for exactly that model, so without this the enforcer sees a brand-new replica as
// idle and zeroes it again — the wake never survives long enough to serve the
// request that asked for it.
//
// Keyed by (namespace, modelID) rather than by scale target: retention is a
// per-model policy (config.ScaleToZeroRetentionPeriod), and a wake may bring up
// several targets at once (a P/D serving set), all of which must be held.
type Activations struct {
	mu sync.RWMutex
	m  map[string]time.Time
	// firstAt records when the CURRENT wake episode began, which m cannot: Mark is
	// re-called every poll while requests are still queued, so m holds the most
	// recent publish rather than the start. Measuring how long a wake took needs
	// the start, and the difference between the two is the whole measurement — a
	// warm FMA bind and a cold model load differ by roughly 2s versus 50s.
	firstAt map[string]time.Time
	// now is the clock, overridden in tests.
	now func() time.Time
}

// NewActivations returns an empty registry.
func NewActivations() *Activations {
	return &Activations{
		m:       make(map[string]time.Time),
		firstAt: make(map[string]time.Time),
		now:     time.Now,
	}
}

func activationKey(namespace, modelID string) string {
	return namespace + "/" + modelID
}

// Mark records that the model was woken from zero now. Repeated calls extend the
// hold, which is what a still-pending queue should do: the engine re-publishes
// every poll while requests are waiting.
func (a *Activations) Mark(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := activationKey(namespace, modelID)
	a.m[key] = a.now()
	// Only the first Mark of an episode opens the timing window. Re-publishing
	// while the queue drains must not restart the clock, or every wake would
	// measure as one poll interval.
	if _, open := a.firstAt[key]; !open {
		a.firstAt[key] = a.now()
	}
}

// CompleteWake closes the current wake episode and reports how long it took,
// or false when the model has no episode open.
//
// Called when the model is next observed serving. Idempotent by construction:
// the episode is deleted here, so the many polls that follow report nothing and
// a single wake contributes one observation rather than one per poll.
func (a *Activations) CompleteWake(namespace, modelID string) (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := activationKey(namespace, modelID)
	at, open := a.firstAt[key]
	if !open {
		return 0, false
	}
	delete(a.firstAt, key)
	return a.now().Sub(at), true
}

// Clear forgets the model's activation, so normal idle accounting takes over
// immediately rather than at the end of the hold. Lapsed holds drop out on their
// own (see WithinRetention), so this is only needed to end one early.
func (a *Activations) Clear(namespace, modelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := activationKey(namespace, modelID)
	delete(a.m, key)
	// An episode ended without the model ever being seen serving is not a wake
	// that took a long time — it is a wake that did not complete. Dropping it
	// keeps the histogram a record of real wakes rather than of abandoned ones.
	delete(a.firstAt, key)
}

// WithinRetention reports whether the model was woken from zero less than
// retention ago, and so must not be scaled back to zero yet. A model that was
// never woken (or whose hold has lapsed) reports false, leaving normal
// scale-to-zero accounting in charge.
//
// A lapsed entry is dropped as it is read: the enforcer asks about every model
// it gates on every cycle, so this is where a hold that has served its purpose
// gets collected. Nothing else prunes the map, and without this an entry is
// written once per wake and kept for the life of the process.
//
// A non-positive retention disables the hold. It deliberately does NOT prune:
// retention comes from config and can be turned back on, and a model whose hold
// is merely disabled this cycle should not silently lose the wake it recorded.
func (a *Activations) WithinRetention(namespace, modelID string, retention time.Duration) bool {
	if retention <= 0 {
		return false
	}
	key := activationKey(namespace, modelID)

	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.m[key]
	if !ok {
		return false
	}
	if a.now().Sub(at) < retention {
		return true
	}
	delete(a.m, key)
	return false
}

// DefaultActivations is the process-wide registry, written by the
// scale-from-zero engine and read by the scale-to-zero enforcement gate.
var DefaultActivations = NewActivations()

// CompleteWake closes the default registry's wake episode for the model and
// reports its duration. See Activations.CompleteWake.
func CompleteWake(namespace, modelID string) (time.Duration, bool) {
	return DefaultActivations.CompleteWake(namespace, modelID)
}

// MarkActivated records a wake-from-zero in the default registry.
func MarkActivated(namespace, modelID string) {
	DefaultActivations.Mark(namespace, modelID)
}

// WithinActivationRetention reports whether the default registry is still
// holding the model up after a wake-from-zero.
func WithinActivationRetention(namespace, modelID string, retention time.Duration) bool {
	return DefaultActivations.WithinRetention(namespace, modelID, retention)
}
