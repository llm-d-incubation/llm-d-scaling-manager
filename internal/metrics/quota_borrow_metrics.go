package metrics

import (
	"fmt"
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// Kueue cohort borrowing series
// (the Kueue borrowing proposal, PR ev-shindin/llm-scaling-manager#161, Part C). Every
// other borrowing condition is a reason on llmsm_model_scaling_blocked.
var (
	quotaLimitGPUs      *prometheus.GaugeVec
	quotaBorrowWithheld *prometheus.GaugeVec
	kueueUnplacedPods   *prometheus.GaugeVec
	kueuePreemptedPods  *prometheus.CounterVec
	kueueLenderWaiting  *prometheus.GaugeVec
	borrowReleases      *prometheus.CounterVec
	kueueBorrowDrift    *prometheus.GaugeVec
	kueueNeverRead      *prometheus.GaugeVec
	kueueVersionKnown   *prometheus.GaugeVec
	kueueCooldown       *prometheus.GaugeVec
	kueueReleaseLatency *prometheus.GaugeVec
	kueueBorrowedGPUs   *prometheus.GaugeVec
	lenderBlocked       *prometheus.GaugeVec
	ledgerUnread        *prometheus.GaugeVec
)

// registerQuotaBorrowMetrics creates and registers the borrowing series.
// Called from InitMetrics, after controllerInstance is set.
func registerQuotaBorrowMetrics(registry prometheus.Registerer) error {
	withInstance := func(labels ...string) []string {
		if controllerInstance != "" {
			labels = append(labels, constants.LabelControllerInstance)
		}
		return labels
	}
	quotaLimitGPUs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMQuotaLimitGPUs,
		Help: "A namespace-scope quota entry's GPU cap for one namespace and accelerator, by tier. " +
			"guaranteed: the cap without borrowing. borrowable: what the namespace may draw past it now, " +
			"were no other namespace to draw first -- min(its borrow cap, the free GPUs of its Kueue " +
			"cohorts' shared pools serving the accelerator); 0 where borrowing is not counted. " +
			"An unlimited guaranteed figure publishes no series.",
	}, withInstance(constants.LabelLimiterName, constants.LabelNamespace, constants.LabelAccelerator, constants.LabelTier))
	quotaBorrowWithheld = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMQuotaBorrowWithheld,
		Help: "1 for the reason a namespace's borrowable GPUs are 0, for a quota entry that reads Kueue " +
			"borrowing. Present only while the reason holds.",
	}, withInstance(constants.LabelLimiterName, constants.LabelNamespace, constants.LabelAccelerator, constants.LabelReason))
	kueueUnplacedPods = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueUnplacedPods,
		Help: "Pods of a variant, in a namespace that counts Kueue borrowing, that have not placed for longer " +
			"than the placement timeout (Unschedulable, or held at Kueue's admission gate). Absent otherwise.",
	}, withInstance(constants.LabelNamespace, constants.LabelVariantName))
	kueuePreemptedPods = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: constants.LLMSMKueuePreemptedPodsTotal,
		Help: "Kueue preemptions of a model (a Workload evicted, reason Preempted) in a namespace that counts " +
			"borrowing, one per event: Workloads of the model preempted within a minute of each other count " +
			"once, so a P/D pair, a LeaderWorkerSet or a pod group counts once. 0 from the first cycle that " +
			"counts borrowing for the model.",
	}, withInstance(constants.LabelNamespace, constants.LabelModelName))
	kueueLenderWaiting = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueLenderWaitingSeconds,
		Help: "Seconds a Kueue lender ClusterQueue has waited for capacity it lent, published only while a " +
			"namespace this controller governs still borrows in its cohort. Above the borrow cooldown, " +
			"Kueue has not taken back what it lent.",
	}, withInstance(constants.LabelCohort, constants.LabelClusterQueue))
	borrowReleases = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: constants.LLMSMBorrowReleasesTotal,
		Help: "Releases of borrowed Kueue capacity by a model, one per model and cycle whatever the number of " +
			"variants lowered, so a P/D pair counts once; by cause: placement-timeout (a borrowed replica did " +
			"not place in time), kueue-gate (a replica held at Kueue's admission gate in a namespace that counts " +
			"no borrowing), lender-waiting (handed back to a waiting lender Kueue does not reclaim for, with " +
			"kueue.releaseToNonReclaimingLenders). 0 from the first cycle that counts borrowing for the model.",
	}, withInstance(constants.LabelNamespace, constants.LabelModelName, constants.LabelCause))
	kueueBorrowDrift = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueBorrowDriftGPUs,
		Help: "Per Kueue ClusterQueue in a cohort, the GPUs by which the borrowed figure the scaling manager " +
			"computes (usage past nominal) differs from Kueue's status.flavorsReservation borrowed figure. " +
			"0 while they agree; above 0, Kueue's accounting is not what borrowing assumes.",
	}, withInstance(constants.LabelClusterQueue))
	kueueNeverRead = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueNeverRead,
		Help: "1 for a quota entry that reads Kueue, leaves some accelerator type unlimited (-1) in its static " +
			"caps, and has never read Kueue successfully: nothing bounds that type. Absent otherwise.",
	}, withInstance(constants.LabelLimiterName))
	kueueVersionKnown = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueControllerVersionKnown,
		Help: "Per controller, while some quota entry counts Kueue borrowing: 1 when every such entry knows " +
			"the Kueue controller's version -- its image names one, or kueue.assumeTestedVersion vouches for it -- " +
			"and 0 when one does not. 0 has two causes, without assumeTestedVersion: an image that names no " +
			"version (digest-only), which withholds borrowing as kueue-version-unknown; or a Deployment that " +
			"cannot be read, the controller down, which freezes borrowing as kueue-controller-down. Absent while " +
			"no entry counts borrowing.",
	}, withInstance())
	kueueCooldown = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueBorrowCooldownSeconds,
		Help: "The borrowCooldown in force, in seconds, while a quota entry counts Kueue borrowing. " +
			"The lender-waiting alert compares llmsm_kueue_lender_waiting_seconds against it.",
	}, withInstance())
	kueueReleaseLatency = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueBorrowReleaseLatencySeconds,
		Help: "How long a release of borrowed capacity to a waiting lender of a cohort takes to land, in " +
			"seconds: the slowest KEDA scale-down window of the models a release there may lower, plus the HPA " +
			"sync, the KEDA poll and the termination grace, capped at 1800. Published per cohort whose pool " +
			"opts in to kueue.releaseToNonReclaimingLenders; the lender-waiting alert adds it to the cooldown " +
			"for that cohort alone.",
	}, withInstance(constants.LabelCohort))
	lenderBlocked = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueLenderReleaseBlocked,
		Help: "1 while a waiting lender Kueue does not reclaim for is handed nothing, with " +
			"kueue.releaseToNonReclaimingLenders: reason unservable (what this controller could give back " +
			"would not admit its next Workload), taken-by-another-borrower (a release landed and another " +
			"borrower took it) or kueue-preempting (Workloads Kueue preempted still hold their quota while " +
			"their pods stop, at most 30 minutes after each eviction).",
	}, withInstance(constants.LabelCohort, constants.LabelClusterQueue, constants.LabelReason))
	ledgerUnread = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueBorrowLedgerUnread,
		Help: "1 while the persisted borrowing release ledger cannot be read and a quota entry sets " +
			"kueue.releaseToNonReclaimingLenders: no release to a lender is decided until it is. Reason " +
			"forbidden, read-error or newer-version.",
	}, withInstance(constants.LabelReason))
	kueueBorrowedGPUs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: constants.LLMSMKueueBorrowedGPUs,
		Help: "GPUs the namespaces this controller governs hold past their guaranteed figures, per Kueue " +
			"cohort and flavor, as the last cycle counted them. Absent where nothing is borrowed.",
	}, withInstance(constants.LabelCohort, constants.LabelFlavor))
	for name, c := range map[string]prometheus.Collector{
		constants.LLMSMKueueControllerVersionKnown:      kueueVersionKnown,
		constants.LLMSMKueueBorrowCooldownSeconds:       kueueCooldown,
		constants.LLMSMKueueBorrowReleaseLatencySeconds: kueueReleaseLatency,
		constants.LLMSMKueueLenderReleaseBlocked:        lenderBlocked,
		constants.LLMSMKueueBorrowLedgerUnread:          ledgerUnread,
		constants.LLMSMKueueBorrowedGPUs:                kueueBorrowedGPUs,
		constants.LLMSMKueueBorrowDriftGPUs:             kueueBorrowDrift,
		constants.LLMSMKueueNeverRead:                   kueueNeverRead,
		constants.LLMSMQuotaLimitGPUs:                   quotaLimitGPUs,
		constants.LLMSMQuotaBorrowWithheld:              quotaBorrowWithheld,
		constants.LLMSMKueueUnplacedPods:                kueueUnplacedPods,
		constants.LLMSMKueuePreemptedPodsTotal:          kueuePreemptedPods,
		constants.LLMSMKueueLenderWaitingSeconds:        kueueLenderWaiting,
		constants.LLMSMBorrowReleasesTotal:              borrowReleases,
	} {
		if err := registry.Register(c); err != nil {
			return fmt.Errorf("failed to register %s: %w", name, err)
		}
	}
	return nil
}

// QuotaBudget is one namespace-scope quota entry's figures for one namespace
// and accelerator, as the cycle used them.
type QuotaBudget struct {
	Limiter, Namespace, Accelerator string
	// Guaranteed below 0 is unlimited and publishes no series.
	Guaranteed int
	Borrowable int
	// Reason is why Borrowable is 0, for an entry that reads borrowing.
	Reason string
}

// seriesSet is the series a Publish call set on some gauges, so the next call
// can delete only those that are gone. publish sets every current series
// before it deletes any, so a scrape never lands on an empty set.
type seriesSet struct {
	mu        sync.Mutex
	published map[*prometheus.GaugeVec]map[string]prometheus.Labels
}

// publish makes the series of the gauges in vecs exactly those fill sets.
func (s *seriesSet) publish(vecs []*prometheus.GaugeVec, fill func(set func(*prometheus.GaugeVec, prometheus.Labels, float64))) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := map[*prometheus.GaugeVec]map[string]prometheus.Labels{}
	fill(func(g *prometheus.GaugeVec, l prometheus.Labels, v float64) {
		if controllerInstance != "" {
			l[constants.LabelControllerInstance] = controllerInstance
		}
		g.With(l).Set(v)
		if current[g] == nil {
			current[g] = map[string]prometheus.Labels{}
		}
		current[g][seriesKey(l)] = l
	})
	for _, g := range vecs {
		for k, l := range s.published[g] {
			if _, ok := current[g][k]; !ok {
				g.Delete(l)
			}
		}
	}
	s.published = current
}

var (
	quotaBudgetSeries      seriesSet
	kueueUnplacedSeries    seriesSet
	kueueLenderWaitSeries  seriesSet
	kueueBorrowDriftSeries seriesSet
	kueueNeverReadSeries   seriesSet
)

// PublishKueueBorrowDrift makes llmsm_kueue_borrow_drift_gpus exactly these
// ClusterQueues, keyed by name. Call it after each successful Kueue read.
func PublishKueueBorrowDrift(byQueue map[string]int) {
	if kueueBorrowDrift == nil {
		return
	}
	kueueBorrowDriftSeries.publish([]*prometheus.GaugeVec{kueueBorrowDrift},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for queue, gpus := range byQueue {
				set(kueueBorrowDrift, prometheus.Labels{constants.LabelClusterQueue: queue}, float64(gpus))
			}
		})
}

var (
	kueueCooldownSeries       seriesSet
	kueueReleaseLatencySeries seriesSet
	kueueBorrowedGPUsSeries   seriesSet
	lenderBlockedSeries       seriesSet
	ledgerUnreadSeries        seriesSet
)

// PublishKueueReleaseLatency sets llmsm_kueue_borrow_release_latency_seconds
// to exactly the cohorts given, in seconds: those whose pool opts in to
// releaseToNonReclaimingLenders. Every other cohort's series is removed.
func PublishKueueReleaseLatency(byCohort map[string]float64) {
	if kueueReleaseLatency == nil {
		return
	}
	kueueReleaseLatencySeries.publish([]*prometheus.GaugeVec{kueueReleaseLatency},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for cohort, s := range byCohort {
				set(kueueReleaseLatency, prometheus.Labels{constants.LabelCohort: cohort}, s)
			}
		})
}

// BlockedLender is one series of llmsm_kueue_lender_release_blocked.
type BlockedLender struct {
	Cohort, ClusterQueue, Reason string
}

// PublishLenderReleaseBlocked sets llmsm_kueue_lender_release_blocked to exactly
// the lenders given.
func PublishLenderReleaseBlocked(lenders []BlockedLender) {
	if lenderBlocked == nil {
		return
	}
	lenderBlockedSeries.publish([]*prometheus.GaugeVec{lenderBlocked},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for _, l := range lenders {
				set(lenderBlocked, prometheus.Labels{constants.LabelCohort: l.Cohort,
					constants.LabelClusterQueue: l.ClusterQueue, constants.LabelReason: l.Reason}, 1)
			}
		})
}

// PublishBorrowLedgerUnread sets llmsm_kueue_borrow_ledger_unread to 1 with
// reason, or removes it when reason is empty.
func PublishBorrowLedgerUnread(reason string) {
	if ledgerUnread == nil {
		return
	}
	ledgerUnreadSeries.publish([]*prometheus.GaugeVec{ledgerUnread},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			if reason != "" {
				set(ledgerUnread, prometheus.Labels{constants.LabelReason: reason}, 1)
			}
		})
}

// versionKnownBy holds, per Kueue reader (by its quota entry), whether the
// controller's version counts as known to it, for the readers whose entry
// counts borrowing and whose read checked the controller (R2).
var versionKnownBy = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// PublishKueueVersionKnown records reader source's answer and sets
// llmsm_kueue_controller_version_known: 1 while every reader that counts
// borrowing knows the version -- read from the image, or vouched for by
// kueue.assumeTestedVersion, the operator's documented fix -- and 0 while one
// does not. counting false drops the reader's answer: its entry counts no
// borrowing, or its read found no cohort and did not check the controller,
// so it has nothing to page about. With no reader left the series is
// deleted, rather than left at a stale value.
func PublishKueueVersionKnown(source string, known, counting bool) {
	versionKnownBy.Lock()
	defer versionKnownBy.Unlock()
	if counting {
		versionKnownBy.m[source] = known
	} else {
		delete(versionKnownBy.m, source)
	}
	publishVersionKnownLocked()
}

// PruneKueueVersionKnown keeps, of the readers' answers, only those of the
// quota entries in live -- the entries that count Kueue borrowing in the
// limiter just built (R2-2). An entry removed from the config, or no longer
// counting borrowing, publishes nothing after the rebuild: its last answer
// would otherwise hold llmsm_kueue_controller_version_known at 0, paging,
// until the process restarts.
func PruneKueueVersionKnown(live []string) {
	versionKnownBy.Lock()
	defer versionKnownBy.Unlock()
	for source := range versionKnownBy.m {
		if !slices.Contains(live, source) {
			delete(versionKnownBy.m, source)
		}
	}
	publishVersionKnownLocked()
}

// publishVersionKnownLocked sets llmsm_kueue_controller_version_known from
// versionKnownBy, deleting it when no reader is left. Callers hold
// versionKnownBy.
func publishVersionKnownLocked() {
	if kueueVersionKnown == nil {
		return
	}
	l := prometheus.Labels{}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	if len(versionKnownBy.m) == 0 {
		kueueVersionKnown.Delete(l)
		return
	}
	v := 1.0
	for _, k := range versionKnownBy.m {
		if !k {
			v = 0
		}
	}
	kueueVersionKnown.With(l).Set(v)
}

// PublishKueueBorrowCooldown sets llmsm_kueue_borrow_cooldown_seconds to the
// cooldown in force while borrowing is counted, and removes it when on is
// false.
func PublishKueueBorrowCooldown(seconds float64, on bool) {
	if kueueCooldown == nil {
		return
	}
	kueueCooldownSeries.publish([]*prometheus.GaugeVec{kueueCooldown},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			if on {
				set(kueueCooldown, prometheus.Labels{}, seconds)
			}
		})
}

// BorrowedGPUs is what this controller's namespaces hold past their guaranteed
// figures in one cohort and flavor.
type BorrowedGPUs struct {
	Cohort, Flavor string
	GPUs           int
}

// PublishKueueBorrowedGPUs makes llmsm_kueue_borrowed_gpus exactly these cohorts
// and flavors. Call it every cycle, with nil when nothing is borrowed.
func PublishKueueBorrowedGPUs(held []BorrowedGPUs) {
	if kueueBorrowedGPUs == nil {
		return
	}
	kueueBorrowedGPUsSeries.publish([]*prometheus.GaugeVec{kueueBorrowedGPUs},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for _, b := range held {
				if b.GPUs <= 0 {
					continue
				}
				set(kueueBorrowedGPUs, prometheus.Labels{constants.LabelCohort: b.Cohort,
					constants.LabelFlavor: b.Flavor}, float64(b.GPUs))
			}
		})
}

// PublishKueueNeverRead makes llmsm_kueue_never_read exactly these limiter
// entries. Call it every cycle, with nil when none is never read.
func PublishKueueNeverRead(limiters []string) {
	if kueueNeverRead == nil {
		return
	}
	kueueNeverReadSeries.publish([]*prometheus.GaugeVec{kueueNeverRead},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for _, name := range limiters {
				set(kueueNeverRead, prometheus.Labels{constants.LabelLimiterName: name}, 1)
			}
		})
}

// PublishQuotaBudgets makes llmsm_quota_limit_gpus and llmsm_quota_borrow_withheld
// exactly this cycle's budgets: a namespace that left, a borrowable figure
// that is no longer 0 and a reason that changed all lose their old series.
// Call it every cycle, with nil when no limiter is in force.
func PublishQuotaBudgets(budgets []QuotaBudget) {
	if quotaLimitGPUs == nil {
		return
	}
	quotaBudgetSeries.publish([]*prometheus.GaugeVec{quotaLimitGPUs, quotaBorrowWithheld},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for _, b := range budgets {
				if b.Guaranteed < 0 {
					continue
				}
				tier := func(t string) prometheus.Labels {
					return prometheus.Labels{constants.LabelLimiterName: b.Limiter, constants.LabelNamespace: b.Namespace,
						constants.LabelAccelerator: b.Accelerator, constants.LabelTier: t}
				}
				set(quotaLimitGPUs, tier(constants.QuotaTierGuaranteed), float64(b.Guaranteed))
				set(quotaLimitGPUs, tier(constants.QuotaTierBorrowable), float64(b.Borrowable))
				if b.Reason != "" && b.Borrowable == 0 {
					set(quotaBorrowWithheld, prometheus.Labels{constants.LabelLimiterName: b.Limiter,
						constants.LabelNamespace: b.Namespace, constants.LabelAccelerator: b.Accelerator,
						constants.LabelReason: b.Reason}, 1)
				}
			}
		})
}

// UnplacedVariant is one variant's pods unplaced past the placement timeout.
type UnplacedVariant struct {
	Namespace, Variant string
	Pods               int
}

// PublishKueueUnplacedPods makes llmsm_kueue_unplaced_pods exactly these
// variants. Call it every cycle, with nil when no borrowing is counted.
func PublishKueueUnplacedPods(variants []UnplacedVariant) {
	if kueueUnplacedPods == nil {
		return
	}
	kueueUnplacedSeries.publish([]*prometheus.GaugeVec{kueueUnplacedPods},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for _, v := range variants {
				if v.Pods <= 0 {
					continue
				}
				set(kueueUnplacedPods, prometheus.Labels{constants.LabelNamespace: v.Namespace,
					constants.LabelVariantName: v.Variant}, float64(v.Pods))
			}
		})
}

// WaitingLender is one lender ClusterQueue waiting while its cohort is
// borrowed from.
type WaitingLender struct {
	Cohort, ClusterQueue string
	Seconds              float64
}

// PublishKueueLenderWaiting makes llmsm_kueue_lender_waiting_seconds exactly
// these lenders. Call it every cycle, with nil when no borrowing is counted.
func PublishKueueLenderWaiting(lenders []WaitingLender) {
	if kueueLenderWaiting == nil {
		return
	}
	kueueLenderWaitSeries.publish([]*prometheus.GaugeVec{kueueLenderWaiting},
		func(set func(*prometheus.GaugeVec, prometheus.Labels, float64)) {
			for _, l := range lenders {
				set(kueueLenderWaiting, prometheus.Labels{constants.LabelCohort: l.Cohort,
					constants.LabelClusterQueue: l.ClusterQueue}, l.Seconds)
			}
		})
}

// InitBorrowLossSeries creates a model's loss counters at 0: its releases, by
// each of causes, and its Kueue preemptions. A counter created by its first
// increment starts at 1, and increase() over a range holding its first sample
// sees nothing, so the thrashing alert would miss the first loss of each kind.
func InitBorrowLossSeries(namespace, modelName string, causes ...string) {
	if borrowReleases == nil || kueuePreemptedPods == nil {
		return
	}
	l := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelName}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	kueuePreemptedPods.With(l).Add(0)
	for _, cause := range causes {
		withCause := prometheus.Labels{constants.LabelCause: cause}
		for k, v := range l {
			withCause[k] = v
		}
		borrowReleases.With(withCause).Add(0)
	}
}

// InitBorrowReleaseSeries creates, at 0, a model's release counter of one
// cause alone, for a model whose releases are not losses of borrowed capacity
// (cause kueue-gate): no preemption counter is created for it.
func InitBorrowReleaseSeries(namespace, modelName, cause string) {
	if borrowReleases == nil {
		return
	}
	l := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelName, constants.LabelCause: cause}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	borrowReleases.With(l).Add(0)
}

// CountBorrowRelease counts one release of borrowed capacity by a model.
func CountBorrowRelease(namespace, modelID, cause string) {
	if borrowReleases == nil {
		return
	}
	l := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelID, constants.LabelCause: cause}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	borrowReleases.With(l).Inc()
}

// CountKueuePreemptedPods counts n Kueue preemption events of a model.
func CountKueuePreemptedPods(namespace, modelName string, n int) {
	if kueuePreemptedPods == nil || n <= 0 {
		return
	}
	l := prometheus.Labels{constants.LabelNamespace: namespace, constants.LabelModelName: modelName}
	if controllerInstance != "" {
		l[constants.LabelControllerInstance] = controllerInstance
	}
	kueuePreemptedPods.With(l).Add(float64(n))
}
