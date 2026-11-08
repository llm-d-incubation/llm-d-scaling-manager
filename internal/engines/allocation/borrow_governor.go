package allocation

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// Giving borrowed capacity back
// (the Kueue borrowing proposal, PR ev-shindin/llm-scaling-manager#161, B5), and stopping new borrowing
// while a lender waits or after a preemption.
//
// The steady-state engine owns one BorrowGovernor. Each cycle it Observes the
// Kueue reads the constraints carry and the variants' placement, before the
// optimizer runs, and asks it what borrowing is blocked; after the optimizer it
// asks it for Releases -- borrowed replicas that never placed -- which lower
// targets and stay in force until the release has landed. Everything here is
// a pure function of what it is told plus its own state, so the rules can be
// tested without a cluster.
//
// By default a lender that waits is never handed capacity back here: only
// capacity Kueue reclaims for its lender is counted (reclaimablePools), so
// Kueue's preemption gives it back. The governor stops new borrowing in the
// lender's cohort while it waits. With kueue.releaseToNonReclaimingLenders
// (PR-C) a lender Kueue does not reclaim for is counted too, and handed back
// what it waits for (borrow_lender_release.go).

// ReleaseCause says why borrowed capacity was released.
type ReleaseCause string

// ReleasePlacementTimeout is the release cause of a borrowed unit that stayed
// unplaced past the placement timeout (B5). It takes a P/D partner that did
// place with it.
const ReleasePlacementTimeout ReleaseCause = "placement-timeout"

// ReleaseKueueGate is the release cause of a replica Kueue held at its
// admission gate, waiting for quota, past the placement timeout in a
// namespace that counts no borrowing (gatedRelease). Nothing was borrowed:
// it starts no cooldown, and the borrow-thrashing alert does not count it.
const ReleaseKueueGate ReleaseCause = "kueue-gate"

// BorrowSettings are the governor's timings.
type BorrowSettings struct {
	PlacementTimeout time.Duration
	Cooldown         time.Duration
	LenderDebounce   time.Duration
	// ReleaseActuation is how long a release may take to land: twice the
	// termination grace plus the KEDA scale-down window. Cycles that count no
	// borrowing for the cooldown plus this drop the release state
	// (HoldReleases).
	ReleaseActuation time.Duration
	// ReleaseLatencies is, per cohort whose pool opts in to
	// releaseToNonReclaimingLenders, how long a lowered target of the models
	// a release there may lower takes to free its GPUs: their KEDA scale-down
	// window, the HPA sync, the KEDA poll and the termination grace
	// (ReleaseBound), capped (MaxReleaseLatency). Absent when not known. A
	// release to a waiting lender of that cohort stays in flight up to twice
	// this, or ReleaseActuation if longer (lenderTimeout).
	ReleaseLatencies map[string]time.Duration
}

// ReleaseBound is how long a lowered target takes to free its GPUs: the
// scale-down window, the HPA sync, the KEDA poll and the termination grace,
// each at the cluster's default when unset (nil). The utilization share's
// release bound, for a single model.
func ReleaseBound(window, polling, grace *time.Duration) time.Duration {
	pick := func(v *time.Duration, def time.Duration) time.Duration {
		if v != nil && *v >= 0 {
			return *v
		}
		return def
	}
	return pick(window, hpaDefaultScaleDownWindow) + hpaSyncPeriod + pick(polling, kedaDefaultPolling) +
		pick(grace, k8sDefaultTerminationGrace)
}

// DefaultBorrowSettings are the proposal's defaults.
func DefaultBorrowSettings() BorrowSettings {
	return BorrowSettings{
		PlacementTimeout: config.DefaultPlacementTimeout,
		Cooldown:         config.DefaultBorrowCooldown,
		LenderDebounce:   config.DefaultLenderDebounce,
		ReleaseActuation: 2*k8sDefaultTerminationGrace + hpaDefaultScaleDownWindow,
	}
}

// LenderKey is one lender queue for one flavor and GPU resource of a cohort,
// as its waiting is debounced.
type LenderKey struct {
	Cohort   string `json:"cohort"`
	Lender   string `json:"lender"`
	Flavor   string `json:"flavor"`
	Resource string `json:"resource"`
}

func (k LenderKey) flavorKey() string { return k.Cohort + "/" + k.Flavor + "/" + k.Resource }

// BorrowLedger is the persisted state: the cooldowns in force, per cohort and
// per "namespace/model", until when (rounded up to the minute, so a cooldown
// extended every cycle rewrites the ledger once a minute); and the
// placement-timeout releases still in force, per "namespace/variant", their
// target. All of it is bounded by the cohorts, models and variants the
// controller governs, and pruned as it expires. IdleSince is when cycles
// stopped counting borrowing, so a restart does not restart the window after
// which the release state is dropped.
type BorrowLedger struct {
	// Version is the ledger format (BorrowLedgerVersion). Written only beside
	// LenderReleases, so a controller that never releases to a lender writes
	// what it always did; a ledger without one is of version 1.
	Version        int                  `json:"version,omitempty"`
	CohortCooldown map[string]time.Time `json:"cohortCooldown,omitempty"`
	ModelCooldown  map[string]time.Time `json:"modelCooldown,omitempty"`
	TimeoutHeld    map[string]int       `json:"timeoutHeld,omitempty"`
	IdleSince      *time.Time           `json:"idleSince,omitempty"`
	// Backoff is the per (model, cohort) cooldowns, keyed
	// "namespace/model@cohort" (B12).
	Backoff map[string]BackoffEntry `json:"backoff,omitempty"`
	// NoHole is, per "namespace/variant", the replicas ordered without a
	// node hole the safety net still watches (M3): the shared record that
	// marked them does not survive a restart, and without them a replica
	// Kueue never placed within guaranteed waited Pending for good (AR2).
	NoHole map[string]NoHoleReplicas `json:"noHole,omitempty"`
	// LenderReleases are the releases to waiting lenders still in flight
	// (releaseToNonReclaimingLenders): a restart, or a new leader, must not
	// release a second time to a lender the first has not reached.
	LenderReleases []LenderRelease `json:"lenderReleases,omitempty"`
}

// BorrowRelease is one release, for logs now and for events and metrics later.
type BorrowRelease struct {
	Namespace string
	ModelID   string
	Variant   string
	From, To  int
	Cause     ReleaseCause
	// GuaranteedGPUs is, on the first record of a placement-timeout release,
	// the GPUs it takes below the namespace's guaranteed figure: the smallest
	// serviceable unit is larger than what the namespace borrows. 0 otherwise.
	GuaranteedGPUs int
	// AtGate marks a release of replicas held at Kueue's admission gate in a
	// namespace that counts no borrowing (gatedRelease): nothing was
	// borrowed, the queue has no room for them.
	AtGate bool
	// Lender is the waiting lender a release of cause ReleaseLenderWaiting
	// hands capacity back to; zero otherwise.
	Lender LenderKey
}

// BorrowCycle is what the governor is told about one cycle.
type BorrowCycle struct {
	Now         time.Time
	Constraints []*ResourceConstraints
	Requests    []ModelScalingRequest
	// ScaleUpPending is, keyed "namespace/variant", whether an unplaced pod
	// has a cluster-autoscaler TriggeredScaleUp not followed by a
	// NotTriggerScaleUp: the placement timeout then waits, up to three times.
	ScaleUpPending map[string]bool
	// PreemptionUnknown is the namespaces whose Kueue preemptions could not
	// be read this cycle: a model there may have been preempted unseen, so
	// none of them grows on borrowed capacity.
	PreemptionUnknown map[string]bool
	// NoHole is, per "namespace/variant", the replicas ordered without a
	// node hole that the shared record holds in flight or lagging (NoHoleOf):
	// kept under the placement timeout like borrowed ones (M3).
	NoHole map[string]NoHoleReplicas
	// Gated is, per namespace and accelerator, the GPUs of managed pods held
	// at Kueue's admission gate: they hold no reservation, so they are no
	// borrowed capacity a release could hand back.
	Gated map[string]map[string]int
	// LedgerUnread is set while the persisted ledger has not been read back:
	// a release to a lender from before the restart may be in flight, so no
	// new one is decided.
	LedgerUnread bool
}

type lenderWatch struct {
	since    time.Time
	reads    int
	lastRead time.Time
}

// stickyRelease keeps a lowered target in force until the pods are gone.
type stickyRelease struct {
	to int
}

// BorrowGovernor holds the release and cooldown state across cycles.
type BorrowGovernor struct {
	mu       sync.Mutex
	settings BorrowSettings

	watches        map[LenderKey]*lenderWatch
	cohortCooldown map[string]time.Time
	modelCooldown  map[string]time.Time
	timeoutHeld    map[string]stickyRelease
	held           map[string]bool
	preemptUnknown map[string]bool
	// pastSince is, per "namespace/type", since when the namespace has held
	// GPUs past its guaranteed figure, as Observe saw it: the earliest a pod
	// of it can have been unplaced as a borrowed one (observePast).
	pastSince map[string]time.Time
	// uncountedSince is pastSince for the Kueue-backed namespaces that count
	// no borrowing: since when each has held GPUs past its Kueue figure
	// (uncountedHeldPast), for the release of a replica gated there
	// (gatedRelease).
	uncountedSince map[string]time.Time
	// gatedPins is the timeoutHeld keys a release of replicas held at Kueue's
	// gate pinned (gatedRelease), kept through HoldReleases' idle window.
	gatedPins map[string]bool
	// rawWaiting is the cohorts where, in the last fresh read, some lender
	// waits: new borrowing stops there at the first sighting (B2, B6). The
	// debounce (waiting) applies only to the alert and the cohort cooldown.
	rawWaiting map[string]bool
	// backoff is, per "namespace/model@cohort", the model's cooldown in that
	// cohort after a preemption or a placement-timeout release, and how many
	// times it doubled (B12).
	backoff map[string]BackoffEntry
	// noHole is, per "namespace/variant", the replicas ordered without a node
	// hole the governor still watches (observeNoHole, noHoleRelease).
	noHole map[string]NoHoleReplicas
	// noHoleRestored is the noHole entries Restore loaded that no cycle has
	// had the variant's state for yet (observeNoHole).
	noHoleRestored map[string]bool
	// lenderRel is the releases to waiting lenders in flight, one per lender
	// (borrow_lender_release.go).
	lenderRel map[LenderKey]*LenderRelease
	// lenderRestored is the lenderRel entries Restore loaded: a unit of one
	// whose variant is gone clears (observeLenderReleases).
	lenderRestored map[LenderKey]bool
	// report is what the last Releases saw of the waiting lenders it did not
	// serve (LenderReport).
	report LenderReport
	// halfLowered is the models whose lender release this cycle's Observe
	// dropped half landed, past twice its timeout (LenderReport.HalfLowered).
	halfLowered []string
	started     bool
	// idleSince is when cycles stopped counting borrowing (HoldReleases
	// without Observe); zero while they count it.
	idleSince time.Time
}

// NewBorrowGovernor returns an empty governor.
func NewBorrowGovernor(s BorrowSettings) *BorrowGovernor {
	return &BorrowGovernor{
		settings: s, watches: map[LenderKey]*lenderWatch{},
		cohortCooldown: map[string]time.Time{}, modelCooldown: map[string]time.Time{},
		timeoutHeld: map[string]stickyRelease{}, held: map[string]bool{},
		preemptUnknown: map[string]bool{}, pastSince: map[string]time.Time{}, uncountedSince: map[string]time.Time{}, gatedPins: map[string]bool{},
		rawWaiting: map[string]bool{}, backoff: map[string]BackoffEntry{}, noHole: map[string]NoHoleReplicas{},
		noHoleRestored: map[string]bool{}, lenderRel: map[LenderKey]*LenderRelease{}, lenderRestored: map[LenderKey]bool{},
	}
}

// SetSettings replaces the timings, for a live config change.
func (g *BorrowGovernor) SetSettings(s BorrowSettings) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settings = s
}

// Bounds on what a persisted ledger may restore. The ledger is read back from
// a ConfigMap anyone who can write it may have edited: an entry count past
// maxLedgerItems per map, or a replica target past maxLedgerReplicas, is not
// something this controller wrote, and is dropped rather than pinning
// variants with it.
const (
	maxLedgerItems    = 4096
	maxLedgerReplicas = 100000
)

// Restore loads a persisted ledger, normally before the first Observe. What it
// cannot use is dropped: an empty key or clock, a negative target, and
// anything past the ledger bounds (maxLedgerItems and maxLedgerReplicas). A
// Restore that comes late -- after cycles have run -- adds only what the
// governor does not already hold, never overwriting a newer figure. Clocks
// past what the settings allow are clamped by the next Observe. It returns how
// many items it dropped as out of bounds, for the caller to log once.
func (g *BorrowGovernor) Restore(l BorrowLedger) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	dropped := 0
	restoreClocks(g.cohortCooldown, l.CohortCooldown, &dropped)
	restoreClocks(g.modelCooldown, l.ModelCooldown, &dropped)
	for _, k := range boundedKeys(l.TimeoutHeld, &dropped) {
		to := l.TimeoutHeld[k]
		if to > maxLedgerReplicas {
			dropped++
			continue
		}
		if _, held := g.timeoutHeld[k]; !held && to >= 0 && strings.Contains(k, "/") {
			g.timeoutHeld[k] = stickyRelease{to: to}
		}
	}
	for _, k := range boundedKeys(l.Backoff, &dropped) {
		e := l.Backoff[k]
		if _, held := g.backoff[k]; !held && strings.Contains(k, "@") && !e.Until.IsZero() &&
			e.Level >= 0 && e.Level <= maxBackoffLevel {
			g.backoff[k] = e
		}
	}
	for _, k := range boundedKeys(l.NoHole, &dropped) {
		h := l.NoHole[k]
		if h.Replicas > maxLedgerReplicas {
			dropped++
			continue
		}
		if _, held := g.noHole[k]; !held && strings.Contains(k, "/") && h.Replicas > 0 && h.GPUs >= 0 && !h.Since.IsZero() {
			g.noHole[k] = h
			g.noHoleRestored[k] = true
		}
	}
	g.restoreLenderReleases(l.LenderReleases, &dropped)
	if l.IdleSince != nil && g.idleSince.IsZero() && !g.started {
		g.idleSince = *l.IdleSince
	}
	return dropped
}

// boundedKeys is m's keys in order, at most maxLedgerItems of them; the rest
// are counted in dropped.
func boundedKeys[V any](m map[string]V, dropped *int) []string {
	keys := slices.Sorted(maps.Keys(m))
	if len(keys) > maxLedgerItems {
		*dropped += len(keys) - maxLedgerItems
		keys = keys[:maxLedgerItems]
	}
	return keys
}

// restoreClocks adds the persisted untils dst does not hold.
func restoreClocks(dst, src map[string]time.Time, dropped *int) {
	for _, k := range boundedKeys(src, dropped) {
		until := src[k]
		if _, held := dst[k]; !held && k != "" && !until.IsZero() {
			dst[k] = until
		}
	}
}

// Ledger returns the state to persist, in a stable order.
func (g *BorrowGovernor) Ledger() BorrowLedger {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ledgerLocked()
}

func (g *BorrowGovernor) ledgerLocked() BorrowLedger {
	var out BorrowLedger
	out.CohortCooldown = persistClocks(g.cohortCooldown)
	out.ModelCooldown = persistClocks(g.modelCooldown)
	if len(g.timeoutHeld) > 0 {
		out.TimeoutHeld = map[string]int{}
		for k, r := range g.timeoutHeld {
			out.TimeoutHeld[k] = r.to
		}
	}
	if !g.idleSince.IsZero() {
		at := g.idleSince.UTC()
		out.IdleSince = &at
	}
	if len(g.backoff) > 0 {
		out.Backoff = make(map[string]BackoffEntry, len(g.backoff))
		for k, e := range g.backoff {
			until := e.Until.Truncate(time.Minute)
			if until.Before(e.Until) {
				until = until.Add(time.Minute)
			}
			out.Backoff[k] = BackoffEntry{Until: until.UTC(), Level: e.Level, LastAt: e.LastAt.Truncate(time.Second).UTC(),
				Preempted: e.Preempted}
		}
	}
	for k, h := range g.noHole {
		if h.Replicas <= 0 || h.Since.IsZero() {
			continue
		}
		if out.NoHole == nil {
			out.NoHole = map[string]NoHoleReplicas{}
		}
		h.Since = h.Since.UTC()
		out.NoHole[k] = h
	}
	if out.LenderReleases = g.persistLenderReleases(); out.LenderReleases != nil {
		out.Version = BorrowLedgerVersion
	}
	return out
}

// persistClocks is the untils rounded up to the minute, nil when empty.
func persistClocks(m map[string]time.Time) map[string]time.Time {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(m))
	for k, until := range m {
		r := until.Truncate(time.Minute)
		if r.Before(until) {
			r = r.Add(time.Minute)
		}
		out[k] = r.UTC()
	}
	return out
}

// MarshalLedger is Ledger as JSON, for the controller-status ConfigMap.
func (g *BorrowGovernor) MarshalLedger() (string, error) {
	b, err := json.Marshal(g.Ledger())
	if err != nil {
		return "", fmt.Errorf("borrow release ledger: %w", err)
	}
	return string(b), nil
}

// UnmarshalBorrowLedger parses a persisted ledger. Fields it does not know are
// ignored, so a ledger another build wrote loads what this one keeps; one of
// a newer format version (or a negative one) returns ErrBorrowLedgerVersion
// and nothing.
func UnmarshalBorrowLedger(s string) (BorrowLedger, error) {
	var l BorrowLedger
	if s == "" {
		return l, nil
	}
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return BorrowLedger{}, fmt.Errorf("borrow release ledger: %w", err)
	}
	if l.Version < 0 || l.Version > BorrowLedgerVersion {
		return BorrowLedger{}, fmt.Errorf("%w %d (this build reads up to %d)", ErrBorrowLedgerVersion, l.Version, BorrowLedgerVersion)
	}
	return l, nil
}

// MarkPreempted starts the model's cooldown from at, when Kueue preempted one
// of its borrowed pods: a preemption found again after a restart cools down
// only for what is left of its cooldown, and never shortens a later one.
// modelKey is "namespace/model". It holds in every cohort; MarkPreemptedIn
// backs off per cohort.
func (g *BorrowGovernor) MarkPreempted(modelKey string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.markModelCooldown(modelKey, at)
}

func (g *BorrowGovernor) markModelCooldown(modelKey string, at time.Time) {
	if until := at.Add(g.settings.Cooldown); until.After(g.modelCooldown[modelKey]) {
		g.modelCooldown[modelKey] = until
	}
}

// MarkPreemptedIn is MarkPreempted for a model whose namespace borrows in
// cohorts: the cooldown backs off per (model, cohort) (B12). Without a cohort
// it is MarkPreempted.
func (g *BorrowGovernor) MarkPreemptedIn(modelKey string, cohorts []string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.markBackoff(modelKey, cohorts, at, true)
}

// BackoffEntry is one (model, cohort)'s borrowing cooldown: until when, how
// many times it doubled, and the event that last started it -- a preemption
// (Preempted) or a placement-timeout release.
type BackoffEntry struct {
	Until     time.Time `json:"until"`
	Level     int       `json:"level,omitempty"`
	LastAt    time.Time `json:"lastAt"`
	Preempted bool      `json:"preempted,omitempty"`
}

// Backoff bounds (B12): the cooldown doubles on a repeat up to four times its
// setting, never past maxBackoff -- unless the setting itself is longer.
const (
	maxBackoffLevel = 2
	maxBackoff      = 40 * time.Minute
	// sameEvent is how close two marks must be to be one event: the pods of
	// one preempted group arrive one by one, and must not double it per pod.
	sameEvent = time.Minute
)

// backoffFor is the cooldown at a backoff level.
func (g *BorrowGovernor) backoffFor(level int) time.Duration {
	return min(g.settings.Cooldown<<level, max(maxBackoff, g.settings.Cooldown))
}

// markBackoff starts the cooldown of modelKey in each cohort from at: a repeat
// -- an event during the cooldown, or within one cooldown after it ended --
// doubles it, up to maxBackoffLevel; one clean cooldown resets it (pruneClocks
// forgets the entry). An event at the same time as the last (sameEvent), or
// older, starts nothing new, and nothing shortens a cooldown in force.
//
// preempted says the event is a preemption; else it is a placement-timeout
// release. A release while a preemption's cooldown is in force is the same
// event (W4): Kueue preempted the borrowed pod, its replacement waited gated
// for the placement timeout, and the release that follows is the preemption's
// consequence, not a second reclaim. Doubling for it held the model off its
// cohort for 20 minutes after one reclaim, about 25 in all.
func (g *BorrowGovernor) markBackoff(modelKey string, cohorts []string, at time.Time, preempted bool) {
	g.markBackoffAfter(modelKey, cohorts, at, preempted, g.settings.PlacementTimeout)
}

// markBackoffAfter is markBackoff for a placement-timeout release whose
// replicas waited timeout -- tripled while a scale-up is pending -- before it
// fired. Such a release is the preemption's consequence while it comes within
// the preemption's cooldown, or within that timeout of the preemption plus
// one base placement timeout for the replacement pod to appear (N-m4): with
// a 15m tripled timeout the release lands past a 10m cooldown, and compared
// against the cooldown alone it doubled again.
func (g *BorrowGovernor) markBackoffAfter(modelKey string, cohorts []string, at time.Time, preempted bool, timeout time.Duration) {
	if len(cohorts) == 0 {
		g.markModelCooldown(modelKey, at)
		return
	}
	for _, c := range cohorts {
		k := modelKey + "@" + c
		e, ok := g.backoff[k]
		if ok && at.Sub(e.LastAt) < sameEvent {
			continue
		}
		if ok && !preempted && e.Preempted && at.Before(e.Until) {
			continue
		}
		if ok && !preempted && e.Preempted && at.Before(e.LastAt.Add(timeout+g.settings.PlacementTimeout)) {
			// The same event, landing after its cooldown ended: the release
			// starts the cooldown it always does, at the level the
			// preemption set, not one up.
			g.backoff[k] = BackoffEntry{Until: at.Add(g.backoffFor(e.Level)), Level: e.Level, LastAt: at}
			continue
		}
		level := 0
		if ok && at.Before(e.Until.Add(g.settings.Cooldown)) {
			level = min(e.Level+1, maxBackoffLevel)
		}
		until := at.Add(g.backoffFor(level))
		if ok && e.Until.After(until) {
			until = e.Until
		}
		g.backoff[k] = BackoffEntry{Until: until, Level: level, LastAt: at, Preempted: preempted}
	}
}

// BorrowBlockedIn reports whether the model may not draw on cohort's pools
// now: its cooldown there, after a preemption or a placement-timeout release,
// has not ended.
func (g *BorrowGovernor) BorrowBlockedIn(namespace, modelID, cohort string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.backoff[namespace+"/"+modelID+"@"+cohort]
	return ok && now.Before(e.Until)
}

// CoolingDown reports whether the model borrows nothing new now in some
// cohort, or in all of them: a preemption or a placement-timeout release
// started its cooldown, which has not ended.
func (g *BorrowGovernor) CoolingDown(namespace, modelID string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := namespace + "/" + modelID
	if until, ok := g.modelCooldown[key]; ok && now.Before(until) {
		return true
	}
	for k, e := range g.backoff {
		if strings.HasPrefix(k, key+"@") && now.Before(e.Until) {
			return true
		}
	}
	return false
}

// namespaceCohorts is the cohorts constraints map ns to, sorted, once each.
func namespaceCohorts(constraints []*ResourceConstraints, ns string) []string {
	var out []string
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for _, co := range c.NamespaceCohorts[ns] {
			if !slices.Contains(out, co) {
				out = append(out, co)
			}
		}
	}
	slices.Sort(out)
	return out
}

// BlockedCohorts is the cohorts where nothing new may be borrowed now: a
// lender waits in the last fresh read -- the raw signal, at its first
// sighting, not debounced (B2) -- or a lender's debounced wait put the cohort
// in its cooldown. Kueue is reclaiming for it what it lent, and new borrowing
// would only be preempted in turn.
func (g *BorrowGovernor) BlockedCohorts(now time.Time) map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]bool{}
	for c, until := range g.cohortCooldown {
		if now.Before(until) {
			out[c] = true
		}
	}
	for c := range g.rawWaiting {
		out[c] = true
	}
	return out
}

// BorrowBlocked reports whether a variant may not grow on borrowed capacity
// now: its model is cooling down after a preemption, its namespace's
// preemptions could not be read this cycle, or the variant has unplaced
// replicas (the placement hold; for a LeaderWorkerSet until the whole group
// placed). Guaranteed growth is never blocked here.
func (g *BorrowGovernor) BorrowBlocked(namespace, modelID, variant string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.preemptUnknown[namespace] {
		return true
	}
	if until, ok := g.modelCooldown[namespace+"/"+modelID]; ok && now.Before(until) {
		return true
	}
	return g.held[namespace+"/"+variant]
}

// Observe takes one cycle's Kueue reads and variant states: it debounces the
// lenders, advances the cooldowns, and records the placement hold. A stale
// read changes nothing about its lenders.
func (g *BorrowGovernor) Observe(c BorrowCycle) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.idleSince = time.Time{}
	g.preemptUnknown = maps.Clone(c.PreemptionUnknown)
	states := indexStates(c.Requests)
	g.pruneClocks(c.Now)
	g.held = map[string]bool{}
	for k, s := range states {
		if s.state.UnplacedReplicas > 0 {
			g.held[k] = true
		}
	}
	for k, r := range g.timeoutHeld {
		if s, ok := states[k]; !ok || heldOf(s.state) <= r.to {
			delete(g.timeoutHeld, k)
			delete(g.gatedPins, k)
		}
	}
	g.observePast(c)
	g.observeNoHole(c, states)
	chunks, _ := mergeSharedPools(c.Constraints)
	seen := map[LenderKey]bool{}
	// listed is every pool of the cycle, fresh holds the value false for a
	// stale one.
	listed := map[string]bool{}
	anyStale := false
	raw := map[string]bool{}
	for _, ch := range chunks {
		listed[ch.key()] = !ch.Stale
		anyStale = anyStale || ch.Stale
		if ch.Stale && g.rawWaiting[ch.Cohort] {
			raw[ch.Cohort] = true // frozen: the last fresh sighting stands
		}
		for _, l := range ch.Lenders {
			key := LenderKey{Cohort: ch.Cohort, Lender: l.Queue, Flavor: ch.Flavor, Resource: ch.Resource}
			seen[key] = true
			if ch.Stale {
				continue // frozen: nothing about this lender changes
			}
			if l.Waiting {
				raw[ch.Cohort] = true
			}
			g.observeLender(key, l.Waiting, ch.ReadAt, c.Now)
			if g.waiting(key, c.Now) {
				g.cohortCooldown[ch.Cohort] = c.Now.Add(g.settings.Cooldown)
			}
		}
	}
	g.rawWaiting = raw
	// known is a pool read fresh this cycle, or one no longer listed in a cycle
	// that read nothing stale (its cohort or flavor went).
	known := func(fk string) bool {
		fresh, ok := listed[fk]
		return fresh || (!ok && !anyStale)
	}
	// A lender missing from a pool read fresh -- its ClusterQueue deleted,
	// stopped, or moved to another cohort -- waits for nothing: its watch goes.
	for k := range g.watches {
		if !seen[k] && known(k.flavorKey()) {
			delete(g.watches, k)
		}
	}
	// A release to a lender whose pool is gone from a cycle that read nothing
	// stale reaches no one: it goes, or it would pin its variants until it
	// timed out.
	for k := range g.lenderRel {
		if _, ok := listed[k.flavorKey()]; !ok && !anyStale {
			g.dropLenderRelease(k)
		}
	}
	g.observeLenderReleases(chunks, c.Requests, states, c.Now)
	g.started = true
}

// observePast records when each borrowing namespace went past its guaranteed
// figure, per type: the first cycle that sees it past guaranteed. That is
// also the first cycle after a start, or after cycles that counted no
// borrowing, for a namespace already past guaranteed then: pastSince is not
// persisted, and a zero clock would let a pod unplaced for an hour, say
// within guaranteed before the restart, time out at once. Starting it at the
// restart costs at most one placement timeout more.
//
// The Kueue-backed namespaces that count no borrowing are tracked the same
// way, apart (uncountedSince): since when each has held GPUs past its Kueue
// figure (uncountedHeldPast).
func (g *BorrowGovernor) observePast(c BorrowCycle) {
	trackPast(g.pastSince, namespaceHeldPast(c.Constraints), c.Now)
	trackPast(g.uncountedSince, uncountedHeldPast(c.Constraints), c.Now)
}

// trackPast keeps since, per "namespace/type", the first time held showed
// GPUs past the figure, and forgets a key once it no longer does.
func trackPast(since map[string]time.Time, held map[string]map[string]int, now time.Time) {
	past := map[string]bool{}
	for ns, perType := range held {
		for t, n := range perType {
			if n <= 0 {
				continue
			}
			k := ns + "/" + t
			past[k] = true
			if _, ok := since[k]; !ok {
				since[k] = now
			}
		}
	}
	for k := range since {
		if !past[k] {
			delete(since, k)
		}
	}
}

// pruneClocks drops expired cooldowns and clamps one further away than the
// cooldown (a restored, or changed, setting).
func (g *BorrowGovernor) pruneClocks(now time.Time) {
	for k, e := range g.backoff {
		switch {
		case !now.Before(e.Until.Add(g.settings.Cooldown)):
			delete(g.backoff, k) // a clean cooldown since it ended: reset
		case e.Until.Sub(now) > g.backoffFor(maxBackoffLevel):
			e.Until = now.Add(g.backoffFor(maxBackoffLevel))
			g.backoff[k] = e
		}
	}
	for _, m := range []map[string]time.Time{g.cohortCooldown, g.modelCooldown} {
		for k, until := range m {
			switch {
			case !now.Before(until):
				delete(m, k)
			case until.Sub(now) > g.settings.Cooldown:
				m[k] = now.Add(g.settings.Cooldown)
			}
		}
	}
}

// observeLender advances one lender's debounce on a new read, and resets it
// when a fresh read says it no longer waits.
func (g *BorrowGovernor) observeLender(key LenderKey, waiting bool, readAt, now time.Time) {
	w := g.watches[key]
	newRead := w == nil || readAt.After(w.lastRead)
	if !newRead {
		return
	}
	if !waiting {
		delete(g.watches, key)
		return
	}
	if w == nil {
		w = &lenderWatch{since: now}
		g.watches[key] = w
	}
	w.reads++
	w.lastRead = readAt
}

// waiting reports whether a lender counts as waiting: two fresh reads in a
// row and at least the debounce since the first.
func (g *BorrowGovernor) waiting(key LenderKey, now time.Time) bool {
	w := g.watches[key]
	return w != nil && w.reads >= 2 && now.Sub(w.since) >= g.settings.LenderDebounce
}

// Releases decides this cycle's releases after the optimizer -- the
// placement-timeout ones, then, where the entry opts in, those to waiting
// lenders Kueue does not reclaim for -- lowers the decisions' targets
// accordingly, and keeps every release still in flight in force -- over the
// analyzer's demand floor and hold, which would otherwise raise the target
// again next cycle.
func (g *BorrowGovernor) Releases(c BorrowCycle, decisions []domain.VariantDecision) []BorrowRelease {
	g.mu.Lock()
	defer g.mu.Unlock()
	states := indexStates(c.Requests)
	byVariant := map[string]int{}
	for i := range decisions {
		byVariant[decisions[i].Namespace+"/"+decisions[i].VariantName] = i
	}
	out := g.timeoutReleases(c, states, namespaceHeldPast(c.Constraints), uncountedHeldPast(c.Constraints))
	// The placement-timeout pins first: what they lower is already lowered
	// when the lender release counts what is left to give (borrowedLeft).
	g.applyPins(decisions, byVariant, states)
	out = append(out, g.lenderReleases(c, states, decisions)...)
	return out
}

// applyPins keeps every release in flight in force on decisions.
func (g *BorrowGovernor) applyPins(decisions []domain.VariantDecision, byVariant map[string]int, states map[string]variantRef) {
	for k, r := range g.timeoutHeld {
		lowerTo(decisions, byVariant, k, r.to, "borrow-unplaced")
	}
	g.applyLenderPins(decisions, byVariant, states)
}

// HoldReleases keeps the releases in flight in force in a cycle that counts
// no borrowing, so neither the demand floor nor the hold raises a released
// variant again while, say, a Kueue read failed; nothing new is decided.
// Once cycles have counted no borrowing for the cooldown plus the actuation
// window, borrowing is taken to be off: the release state is dropped, and the
// next cycle that counts borrowing starts as after a restart. The start of
// that window is persisted, so a restart does not restart it.
//
// A pin holds only until it has landed: a placement-timeout hold goes once
// the variant holds no more than its target, as Observe drops it. A variant
// missing from this cycle's requests -- dropped out of discovery for a cycle
// -- has not landed: its pin stays until it is seen again, or until the
// window above drops the release state. Without
// this, growth the cycle allows within nominal -- borrowing is not counted,
// so every target is -- stayed capped at a released figure for the whole
// window after the replica it released was long gone.
func (g *BorrowGovernor) HoldReleases(now time.Time, requests []ModelScalingRequest, decisions []domain.VariantDecision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idleSince.IsZero() || g.idleSince.After(now) {
		g.idleSince = now // unset, or a restored stamp from the future
	}
	states := indexStates(requests)
	if now.Sub(g.idleSince) > g.settings.Cooldown+g.settings.ReleaseActuation {
		clear(g.watches)
		clear(g.rawWaiting)
		// A pin of a replica held at Kueue's gate (GatedReleases) is no
		// borrowing state: it stays while its variant is read and has not
		// landed.
		for k, r := range g.timeoutHeld {
			if s, ok := states[k]; g.gatedPins[k] && ok && heldOf(s.state) > r.to {
				continue
			}
			delete(g.timeoutHeld, k)
			delete(g.gatedPins, k)
		}
		clear(g.pastSince)
		clear(g.noHole)
		clear(g.noHoleRestored)
		clear(g.lenderRel)
		clear(g.lenderRestored)
		g.started = false
		if len(g.timeoutHeld) == 0 {
			return
		}
	}
	for k, r := range g.timeoutHeld {
		if s, ok := states[k]; ok && heldOf(s.state) <= r.to {
			delete(g.timeoutHeld, k)
			delete(g.gatedPins, k)
		}
	}
	// A release to a lender that has landed pins nothing more; with no fresh
	// read to clear it on, it goes now. One not landed keeps its pins until
	// it lands or times out.
	for k, e := range g.lenderRel {
		if unitsLanded(e.Units, states) || now.Sub(e.ReleasedAt) > g.lenderTimeout(k.Cohort) {
			g.dropLenderRelease(k)
		}
	}
	byVariant := map[string]int{}
	for i := range decisions {
		byVariant[decisions[i].Namespace+"/"+decisions[i].VariantName] = i
	}
	g.applyPins(decisions, byVariant, states)
}

// timeoutReleases releases borrowed units unplaced past the placement timeout
// (B5): the unplaced replicas a borrowing namespace holds past its guaranteed
// figure, for a P/D model with the partner replicas that keep the pair's ratio
// (releaseUnit). Growth within guaranteed -- a reclaiming queue's growth into
// nominal it lent out -- is exempt, since Kueue reclaims for it, except the
// replicas ordered without a node hole, which noHoleRelease watches the same
// way (M3): Kueue's preemption may free no node for them. "Past
// guaranteed" counts the replicas the namespace holds, gated ones included
// (heldPast): a replica Kueue gates while its queue already reserves its whole
// guaranteed figure -- the replacement of a preempted borrowed pod -- has
// nothing to reclaim and would wait forever. Each release is taken off
// heldPast, so two models of one namespace never both release the same
// borrowed GPUs; one release per model per cycle. The model's cooldown starts.
//
// The clock is per replica (expiredUnplaced): each unplaced replica counts
// from when it became unplaced, or from when its namespace went past
// guaranteed if that is later. A replica stuck since before the namespace
// borrowed therefore does not time out one borrowed a minute ago, and a
// variant that keeps adding unplaced replicas does not reset the clock of its
// oldest one. A LeaderWorkerSet replica is a group: its clock is when its
// newest unplaced pod became so, and the group counts once however many of
// its pods are unplaced. The release takes as many replicas as are past the
// timeout.
//
// A Kueue-backed namespace that counts no borrowing has its replicas held at
// Kueue's admission gate released the same way (gatedRelease), past its Kueue
// figure (uncounted) rather than its guaranteed one.
func (g *BorrowGovernor) timeoutReleases(c BorrowCycle, states map[string]variantRef, heldPast, uncounted map[string]map[string]int) []BorrowRelease {
	return g.eachUnplaced(c, func(req ModelScalingRequest, st domain.VariantReplicaState, timeout time.Duration) []BorrowRelease {
		rel := g.borrowedRelease(c, req, st, states, heldPast, g.pastSince, timeout)
		if rel == nil {
			rel = g.gatedRelease(c, req, st, states, uncounted)
		}
		if rel == nil {
			rel = g.noHoleRelease(c, req, st, states, timeout)
		}
		return rel
	})
}

// eachUnplaced runs release over every variant with unplaced replicas, at
// most one release per model per cycle, skipping a model with a release in
// flight. timeout is the placement timeout, tripled while a cluster-autoscaler
// scale-up is pending on the variant's pods. A release past the placement
// timeout starts the model's backoff; one of replicas held at Kueue's gate
// (ReleaseKueueGate) does not: nothing was borrowed.
func (g *BorrowGovernor) eachUnplaced(c BorrowCycle,
	release func(req ModelScalingRequest, st domain.VariantReplicaState, timeout time.Duration) []BorrowRelease) []BorrowRelease {
	var out []BorrowRelease
	for _, req := range c.Requests {
		// A release in flight for any variant of the model makes its counts
		// stale until it lands: a partner released for an unplaced replica
		// (partnerUnit) must not be released again, nor the pair, while it
		// terminates. A release to a lender is one too.
		if g.modelPinned(req) {
			continue
		}
		for _, st := range req.VariantStates {
			if st.UnplacedReplicas <= 0 {
				continue
			}
			timeout := g.settings.PlacementTimeout
			if c.ScaleUpPending[req.Namespace+"/"+st.VariantName] {
				timeout *= 3
			}
			rel := release(req, st, timeout)
			if rel == nil {
				continue
			}
			out = append(out, rel...)
			if rel[0].Cause != ReleaseKueueGate {
				g.markBackoffAfter(req.Namespace+"/"+req.ModelID, namespaceCohorts(c.Constraints, req.Namespace), c.Now, false, timeout)
			}
			break // one release per model per cycle: the counts above are now stale
		}
	}
	return out
}

// borrowedRelease is the release of st's unplaced replicas the namespace holds
// past its guaranteed figure, past timeout (timeoutReleases); nil when none.
// What it releases is taken off heldPast. since is, per "namespace/type",
// since when the namespace has held past the figure (pastSince).
func (g *BorrowGovernor) borrowedRelease(c BorrowCycle, req ModelScalingRequest, st domain.VariantReplicaState,
	states map[string]variantRef, heldPast map[string]map[string]int, since map[string]time.Time, timeout time.Duration) []BorrowRelease {
	typeKey, ok := accelerator.FindKey(heldPast[req.Namespace], accOf(req, st.VariantName))
	if !ok {
		return nil
	}
	expired := expiredUnplaced(st, since[req.Namespace+"/"+typeKey], timeout, c.Now)
	if expired == 0 {
		return nil
	}
	borrowed := heldPast[req.Namespace][typeKey]
	gpr := max(st.GPUsPerReplica, 1)
	if borrowed < gpr {
		return nil // within guaranteed: Kueue reclaims for it (the no-hole net aside)
	}
	k := min(expired, st.UnplacedReplicas, borrowed/gpr, st.CurrentReplicas-minOf(st))
	unit, gpus := releaseUnit(req, st.VariantName, k, borrowed, states)
	if unit == nil || !unitFits(unit, req.Namespace, states) {
		return nil
	}
	heldPast[req.Namespace][typeKey] -= gpus
	return g.pinUnit(req, unit, states, max(0, gpus-borrowed))
}

// gatedRelease is the release of st's replicas held at Kueue's admission gate
// waiting for quota (VariantReplicaState.GatedAt) in a namespace of
// UncountedKueueNamespaces, past the placement timeout, when the namespace
// holds GPUs past its Kueue figure (uncounted, from uncountedHeldPast): Kueue
// admits nothing past that figure from the cohort, so short of preempting
// lower-priority work in the queue itself the replica waits gated for good.
// borrowedRelease's rules over the gated replicas alone: an Unschedulable
// replica Kueue admitted is not the queue's to give back. The timeout is
// never stretched: the cluster autoscaler does not act on a gated pod. Cause
// ReleaseKueueGate. Nil when none expired, or the namespace is within its
// figure.
func (g *BorrowGovernor) gatedRelease(c BorrowCycle, req ModelScalingRequest, st domain.VariantReplicaState,
	states map[string]variantRef, uncounted map[string]map[string]int) []BorrowRelease {
	if len(st.GatedAt) == 0 || len(uncounted[req.Namespace]) == 0 {
		return nil
	}
	gated := st
	gated.UnplacedAt, gated.UnplacedReplicas = st.GatedAt, len(st.GatedAt)
	out := g.borrowedRelease(c, req, gated, states, uncounted, g.uncountedSince, g.settings.PlacementTimeout)
	for i := range out {
		out[i].AtGate, out[i].Cause, out[i].GuaranteedGPUs = true, ReleaseKueueGate, 0
		g.gatedPins[req.Namespace+"/"+out[i].Variant] = true
	}
	return out
}

// GatedReleases is the release half of a cycle that counts no borrowing and
// has namespaces of UncountedKueueNamespaces (c.Constraints): their replicas
// held at Kueue's gate past the placement timeout (gatedRelease). Unlike
// Observe and Releases it touches no borrowing state -- lender watches,
// cooldowns, the placement hold, the no-hole net, idleSince -- so an entry
// that counts borrowing and is withheld for a cycle keeps it; call it after
// HoldReleases. Its pins are kept in force on decisions, and kept through
// HoldReleases' idle window while their variant is still read.
func (g *BorrowGovernor) GatedReleases(c BorrowCycle, decisions []domain.VariantDecision) []BorrowRelease {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.gatedPins {
		if _, held := g.timeoutHeld[k]; !held {
			delete(g.gatedPins, k) // landed
		}
	}
	uncounted := uncountedHeldPast(c.Constraints)
	trackPast(g.uncountedSince, uncounted, c.Now)
	states := indexStates(c.Requests)
	out := g.eachUnplaced(c, func(req ModelScalingRequest, st domain.VariantReplicaState, _ time.Duration) []BorrowRelease {
		return g.gatedRelease(c, req, st, states, uncounted)
	})
	byVariant := map[string]int{}
	for i := range decisions {
		byVariant[decisions[i].Namespace+"/"+decisions[i].VariantName] = i
	}
	g.applyPins(decisions, byVariant, states)
	return out
}

// noHoleRelease is the safety net under the node-hole exemption (M3): st's
// unplaced replicas that were ordered without a node hole -- growth Kueue was
// to admit by preempting borrowers (BorrowDraw.NoHole), the steady-state
// engine's or a wake's -- released like borrowed ones once unplaced past
// timeout. Kueue preempts by priority, not by node: the borrower it takes may
// be a pod that was itself Pending, and then no node frees and the replica
// would wait for good. Nil when none of st's replicas is tracked so, or none
// expired; the release takes no more replicas than are tracked.
func (g *BorrowGovernor) noHoleRelease(c BorrowCycle, req ModelScalingRequest, st domain.VariantReplicaState,
	states map[string]variantRef, timeout time.Duration) []BorrowRelease {
	key := req.Namespace + "/" + st.VariantName
	h := g.noHole[key]
	if h.Replicas <= 0 {
		return nil
	}
	// Only unplaced time after the order counts: a replica unplaced since
	// before it -- one admitted within guaranteed, Unschedulable for a node
	// that is down -- must not release the new growth at once.
	expired := expiredUnplaced(st, h.Since, timeout, c.Now)
	k := min(expired, st.UnplacedReplicas, h.Replicas, st.CurrentReplicas-minOf(st))
	if k <= 0 {
		return nil
	}
	unit, gpus := releaseUnit(req, st.VariantName, k, -1, states)
	if unit == nil || !unitFits(unit, req.Namespace, states) {
		return nil
	}
	h.Replicas -= unit[st.VariantName]
	g.noHole[key] = h
	return g.pinUnit(req, unit, states, gpus)
}

// pinUnit keeps a released unit's lowered targets in force until it lands
// (timeoutHeld) and returns its release records; guaranteed is the GPUs it
// takes below the namespace's guaranteed figure, on the first record.
func (g *BorrowGovernor) pinUnit(req ModelScalingRequest, unit map[string]int, states map[string]variantRef, guaranteed int) []BorrowRelease {
	out := make([]BorrowRelease, 0, len(unit))
	for _, v := range slices.Sorted(maps.Keys(unit)) {
		k := req.Namespace + "/" + v
		s := states[k].state
		to := s.CurrentReplicas - unit[v]
		g.timeoutHeld[k] = stickyRelease{to: to}
		out = append(out, BorrowRelease{Namespace: req.Namespace, ModelID: req.ModelID, Variant: v,
			From: s.CurrentReplicas, To: to, Cause: ReleasePlacementTimeout, GuaranteedGPUs: guaranteed})
		guaranteed = 0
	}
	return out
}

// NoHoleReplicas is what one variant's replicas ordered without a node hole
// hold (BorrowDraw.NoHole): how many, their GPUs, and since when: the
// earliest order (BorrowDraw.OrderedAt), or when the governor first saw them.
type NoHoleReplicas struct {
	Replicas int       `json:"replicas"`
	GPUs     int       `json:"gpus"`
	Since    time.Time `json:"since"`
}

// NoHoleOf is, per "namespace/variant", the replicas the entries ordered
// without a node hole (BorrowDraw.NoHole), in flight or lagging: what the
// governor keeps under the placement timeout (BorrowCycle.NoHole).
func NoHoleOf(entries []BorrowDraw) map[string]NoHoleReplicas {
	var out map[string]NoHoleReplicas
	for _, d := range entries {
		if !d.NoHole || d.GPUs <= 0 || d.Replicas <= 0 {
			continue
		}
		if out == nil {
			out = map[string]NoHoleReplicas{}
		}
		k := d.Namespace + "/" + d.Variant
		h := out[k]
		since := h.Since
		if !d.OrderedAt.IsZero() && (since.IsZero() || d.OrderedAt.Before(since)) {
			since = d.OrderedAt
		}
		out[k] = NoHoleReplicas{Replicas: h.Replicas + d.Replicas, GPUs: h.GPUs + d.GPUs, Since: since}
	}
	return out
}

// observeNoHole keeps track of the replicas ordered without a node hole: what
// the record holds this cycle, and what it held before for a variant that
// still has a replica unplaced since the order -- the record forgets an entry
// once a read shows it, while its pod may still wait for a node no preemption
// frees. An entry is stamped when first seen (Since: the order, or now) and
// keeps that stamp; replicas unplaced since before it are not its own, so
// they neither keep it nor run its clock (noHoleRelease).
//
// An entry restored from the ledger is kept through cycles without its
// variant's state -- the first cycles after a restart may not have read
// the variant yet -- until a cycle has it, for at most three placement
// timeouts from its stamp: the longest the net waits (AR2).
func (g *BorrowGovernor) observeNoHole(c BorrowCycle, states map[string]variantRef) {
	for k, h := range g.noHole {
		if _, recorded := c.NoHole[k]; recorded {
			delete(g.noHoleRestored, k)
			continue
		}
		s, ok := states[k]
		if !ok && g.noHoleRestored[k] && c.Now.Sub(h.Since) <= 3*g.settings.PlacementTimeout {
			continue
		}
		delete(g.noHoleRestored, k)
		if !ok || h.Replicas <= 0 || !unplacedSince(s.state, h.Since) {
			delete(g.noHole, k)
		}
	}
	for k, h := range c.NoHole {
		prev, watched := g.noHole[k]
		since := prev.Since
		if !watched {
			since = c.Now
			if !h.Since.IsZero() && h.Since.Before(since) {
				since = h.Since
			}
		}
		g.noHole[k] = NoHoleReplicas{Replicas: max(prev.Replicas, h.Replicas), GPUs: max(prev.GPUs, h.GPUs), Since: since}
	}
}

// unplacedSince reports whether any of st's unplaced replicas became so at or
// after since. A state without per-replica times reports its newest one
// (UnplacedSince).
func unplacedSince(st domain.VariantReplicaState, since time.Time) bool {
	if st.UnplacedReplicas <= 0 {
		return false
	}
	if len(st.UnplacedAt) == 0 {
		return !st.UnplacedSince.IsZero() && !st.UnplacedSince.Before(since)
	}
	for _, at := range st.UnplacedAt {
		if !at.IsZero() && !at.Before(since) {
			return true
		}
	}
	return false
}

// expiredUnplaced is how many of a variant's unplaced replicas (UnplacedAt:
// pods, or LeaderWorkerSet groups) have been unplaced for at least timeout,
// each counted from when it became unplaced or from pastSince (when its
// namespace went past guaranteed), whichever is later. A state without
// per-replica times counts its UnplacedReplicas at UnplacedSince.
func expiredUnplaced(st domain.VariantReplicaState, pastSince time.Time, timeout time.Duration, now time.Time) int {
	expired := func(at time.Time) bool {
		if at.IsZero() {
			return false
		}
		if pastSince.After(at) {
			at = pastSince
		}
		return now.Sub(at) >= timeout
	}
	if len(st.UnplacedAt) == 0 {
		if expired(st.UnplacedSince) {
			return st.UnplacedReplicas
		}
		return 0
	}
	n := 0
	for _, at := range st.UnplacedAt {
		if expired(at) {
			n++
		}
	}
	return n
}

// releaseUnit is the unit that releases k replicas of variant, and its GPUs.
// For a single-role model that is the k replicas. For a disaggregated model it
// adds, per partner role, the replicas that keep the pair's ratio -- the
// role's total times k over the variant's role total, rounded down -- so a
// role reaches 0 only when every role does: 1P:2D releasing one decode keeps
// its prefill (1P:1D), releasing its prefill takes both decodes with it. A
// role split over several variants counts all of them, and its part is taken
// from the variants with the most replicas above minReplicas first. k shrinks
// until the unit's GPUs fit capGPUs (negative: no cap), but never below one
// replica. At one replica whose pair is larger than capGPUs, the partners are
// cut to what capGPUs covers when the variant's role keeps a replica (the pair
// stays serviceable: 2P:4D releasing a prefill with 8 GPUs borrowed releases
// that prefill alone, not 24 GPUs). When the variant's role would reach 0, no
// role may be left without its partner: partner replicas that free the
// variant's GPUs go instead, the variant's role keeping its replica
// (partnerUnit), and only when the partners cannot spare one does the whole
// pair go, a unit larger than what is borrowed. Nil when no unit releases a
// replica within minReplicas.
func releaseUnit(req ModelScalingRequest, variant string, k, capGPUs int, states map[string]variantRef) (map[string]int, int) {
	if k <= 0 {
		return nil, 0
	}
	gprOf := func(v string) int { return max(states[req.Namespace+"/"+v].state.GPUsPerReplica, 1) }
	if !req.Disaggregated {
		return map[string]int{variant: k}, k * gprOf(variant)
	}
	role := ""
	totals := map[string]int{}
	byRole := map[string][]domain.VariantReplicaState{}
	for _, st := range req.VariantStates {
		if st.VariantName == variant {
			role = st.Role
		}
		totals[st.Role] += st.CurrentReplicas
		byRole[st.Role] = append(byRole[st.Role], st)
	}
	if totals[role] <= 0 {
		return nil, 0
	}
	for ; k >= 1; k-- {
		unit := map[string]int{variant: k}
		gpus := k * gprOf(variant)
		fits := true
		for r, total := range totals {
			if r == role {
				continue
			}
			n := k * total / totals[role]
			sts := slices.Clone(byRole[r])
			slices.SortStableFunc(sts, func(a, b domain.VariantReplicaState) int {
				return cmp.Or(cmp.Compare(b.CurrentReplicas-minOf(b), a.CurrentReplicas-minOf(a)), strings.Compare(a.VariantName, b.VariantName))
			})
			for _, st := range sts {
				take := min(n, max(0, st.CurrentReplicas-minOf(st)))
				if take > 0 {
					unit[st.VariantName] += take
					gpus += take * gprOf(st.VariantName)
					n -= take
				}
			}
			fits = fits && n == 0
		}
		if fits && (capGPUs < 0 || gpus <= capGPUs) {
			return unit, gpus
		}
		if fits && k == 1 {
			if totals[role]-1 > 0 {
				return trimmedUnit(req.Namespace, variant, role, capGPUs, byRole, states)
			}
			if spare, n := partnerUnit(req.Namespace, variant, role, capGPUs, byRole, states); spare != nil {
				return spare, n
			}
			return unit, gpus // no partner can spare a replica: the whole pair goes
		}
	}
	return nil, 0
}

// trimmedUnit is one replica of variant plus as many partner replicas, from
// the variants with the most above minReplicas first, as fit capGPUs beside
// it and never more than the pair's ratio: a release past what is borrowed
// takes no more guaranteed capacity than the replica itself needs.
func trimmedUnit(ns, variant, role string, capGPUs int, byRole map[string][]domain.VariantReplicaState,
	states map[string]variantRef) (map[string]int, int) {
	gprOf := func(v string) int { return max(states[ns+"/"+v].state.GPUsPerReplica, 1) }
	total := func(r string) int {
		n := 0
		for _, st := range byRole[r] {
			n += st.CurrentReplicas
		}
		return n
	}
	unit := map[string]int{variant: 1}
	gpus := gprOf(variant)
	for _, r := range slices.Sorted(maps.Keys(byRole)) {
		if r == role {
			continue
		}
		ratio := total(r) / total(role)
		sts := slices.Clone(byRole[r])
		slices.SortStableFunc(sts, func(a, b domain.VariantReplicaState) int {
			return cmp.Or(cmp.Compare(b.CurrentReplicas-minOf(b), a.CurrentReplicas-minOf(a)), strings.Compare(a.VariantName, b.VariantName))
		})
		for _, st := range sts {
			for n := max(0, st.CurrentReplicas-minOf(st)); n > 0 && ratio > 0 && gpus+gprOf(st.VariantName) <= capGPUs; n-- {
				unit[st.VariantName]++
				gpus += gprOf(st.VariantName)
				ratio--
			}
		}
	}
	return unit, gpus
}

// partnerUnit is the release for an unplaced replica that is the last of its
// role, when the pair it would take is larger than what is borrowed: partner
// replicas of the other roles instead of the replica itself, the fewest whose
// GPUs cover the unplaced replica's, within capGPUs, from the variants with
// the most above minReplicas first, never a role's last replica. Kueue's
// reservation within a queue is fungible, so the GPUs they hand back admit the
// pending replica, and the model keeps serving: 1P:2D with its only prefill
// unplaced releases one decode (1P:1D), not both decodes and the prefill. Nil
// when the partners cannot spare that much (1P:1D): the whole pair goes then.
func partnerUnit(ns, variant, role string, capGPUs int, byRole map[string][]domain.VariantReplicaState,
	states map[string]variantRef) (map[string]int, int) {
	gprOf := func(v string) int { return max(states[ns+"/"+v].state.GPUsPerReplica, 1) }
	need := gprOf(variant)
	unit := map[string]int{}
	gpus := 0
	for _, r := range slices.Sorted(maps.Keys(byRole)) {
		if r == role || gpus >= need {
			continue
		}
		spare := -1 // the role keeps one replica
		for _, st := range byRole[r] {
			spare += st.CurrentReplicas
		}
		sts := slices.Clone(byRole[r])
		slices.SortStableFunc(sts, func(a, b domain.VariantReplicaState) int {
			return cmp.Or(cmp.Compare(b.CurrentReplicas-minOf(b), a.CurrentReplicas-minOf(a)), strings.Compare(a.VariantName, b.VariantName))
		})
		for _, st := range sts {
			g := gprOf(st.VariantName)
			for n := max(0, st.CurrentReplicas-minOf(st)); n > 0 && spare > 0 && gpus < need && gpus+g <= capGPUs; n-- {
				unit[st.VariantName]++
				gpus += g
				spare--
			}
		}
	}
	if gpus < need {
		return nil, 0
	}
	return unit, gpus
}

// unitFits reports whether every variant of a unit releases at least one
// replica and stays at or above its minReplicas.
func unitFits(unit map[string]int, ns string, states map[string]variantRef) bool {
	for v, n := range unit {
		s, ok := states[ns+"/"+v]
		if !ok || n <= 0 || s.state.CurrentReplicas-n < minOf(s.state) {
			return false
		}
	}
	return true
}

func minOf(s domain.VariantReplicaState) int {
	if s.MinReplicas != nil {
		return *s.MinReplicas
	}
	return 0
}

// heldOf is the replicas a variant's pods still hold: HeldReplicas when its
// pods were listed (terminating pods hold their GPUs), else CurrentReplicas.
func heldOf(s domain.VariantReplicaState) int {
	if s.HeldKnown {
		return max(s.HeldReplicas, s.CurrentReplicas)
	}
	return s.CurrentReplicas
}

// lowerTo caps a decision's target at to, as a scale-down the release owns.
func lowerTo(decisions []domain.VariantDecision, byVariant map[string]int, key string, to int, reason string) {
	i, ok := byVariant[key]
	if !ok {
		return
	}
	d := &decisions[i]
	if d.TargetReplicas <= to {
		return
	}
	d.TargetReplicas = to
	d.GPUsAllocated = to * max(d.GPUsPerReplica, 1)
	action := domain.ActionScaleDown
	if to >= d.CurrentReplicas {
		action = domain.ActionNoChange
	}
	d.SetDecisionReason(action, domain.DecisionReasonV2, string(domain.DecisionReasonV2)+" ("+reason+")")
}

type variantRef struct {
	state domain.VariantReplicaState
	cost  float64
}

func indexStates(requests []ModelScalingRequest) map[string]variantRef {
	out := map[string]variantRef{}
	for _, req := range requests {
		costs := map[string]float64{}
		for _, v := range req.Variants {
			costs[v.VariantName] = v.Cost
		}
		for _, st := range req.VariantStates {
			out[req.Namespace+"/"+st.VariantName] = variantRef{state: st, cost: costs[st.VariantName]}
		}
	}
	return out
}

// accOf is a variant's accelerator as discovery resolved it.
func accOf(req ModelScalingRequest, variant string) string {
	for _, v := range req.Variants {
		if v.VariantName == variant {
			return v.AcceleratorName
		}
	}
	for _, st := range req.VariantStates {
		if st.VariantName == variant {
			return st.AcceleratorName
		}
	}
	return ""
}

// namespaceHeldPast is, per borrowing namespace and type, the GPUs its
// variants hold past its guaranteed figure, gated and unplaced replicas
// included: the placement timeout asks whether an unplaced replica depends on
// borrowing, and one past the guaranteed figure does whether or not Kueue has
// reserved for it. Within the figure Kueue admits, or reclaims lent-out
// nominal, for it.
func namespaceHeldPast(constraints []*ResourceConstraints) map[string]map[string]int {
	return namespaceBorrowed(constraints, nil)
}

// namespaceBorrowed is, per borrowing namespace and type, the GPUs it holds
// past its guaranteed figure, not counting gated pods: they hold no
// reservation, so there is nothing of theirs to hand back to a lender. Gated
// GPUs come off the type their node selector names (accelerator identity,
// family-aware); gated GPUs that name no type are shared over the types, in
// a stable order, up to what each holds past guaranteed, as DeductUnreserved
// takes them from any pool.
func namespaceBorrowed(constraints []*ResourceConstraints, gated map[string]map[string]int) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil {
			continue
		}
		for ns := range c.NamespaceBorrow {
			past := map[string]int{}
			for t, p := range c.NamespacePools[ns] {
				if p.Limit >= 0 {
					past[t] = p.Used - p.Limit
				}
			}
			if len(past) == 0 {
				continue
			}
			untyped := 0
			for acc, n := range gated[ns] {
				if acc == "" {
					untyped += n
					continue
				}
				if t, ok := accelerator.FindBudgetKey(past, acc); ok {
					past[t] -= n
				}
			}
			if out[ns] == nil {
				out[ns] = map[string]int{}
			}
			for _, t := range slices.Sorted(maps.Keys(past)) {
				take := min(untyped, max(0, past[t]))
				untyped -= take
				out[ns][t] = max(0, past[t]-take)
			}
		}
	}
	return out
}

func compareLenderKeys(a, b LenderKey) int {
	return cmp.Or(strings.Compare(a.Cohort, b.Cohort), strings.Compare(a.Flavor, b.Flavor),
		strings.Compare(a.Resource, b.Resource), strings.Compare(a.Lender, b.Lender))
}
