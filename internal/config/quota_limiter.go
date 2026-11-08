package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// ReservedLimiterNames are the names that what held a decision back is
// reported under -- the limiter_name label of llmsm_decisions_limited_total,
// the ResourceConstrained Event and limitedBy in the scaling-decision line --
// when it is not a quota entry: the physical limiter (whatever its entry is
// named, it is reported as gpu-limiter), node fit, and the two Kueue
// borrowing rules. A quota entry is reported under its own name, so one named
// like these could not be told apart from them. The allocation package holds
// the constants these mirror, and a test keeps the two in step.
//
// A quota entry named one of these fails Validate. Earlier releases accepted
// it with a warning; from v0.10.0 the "default" entry holding it is rejected
// like any other invalid entry, so a controller starting on it does not start.
var ReservedLimiterNames = []string{
	"gpu-limiter",
	"node-fit",
	"pd-pair-borrows-whole",
	"lender-cannot-reclaim",
}

// reservedNameError is the error for a quota entry, at where, named name, one
// of ReservedLimiterNames.
func reservedNameError(where, name string) error {
	return fmt.Errorf("%s: quota entry name %q is reserved: decisions held back by something other than a quota entry "+
		"are reported under it (reserved: %s); rename the entry", where, name, strings.Join(ReservedLimiterNames, ", "))
}

// QuotaLimiterReservedNamespaceKey is the reserved key in the namespace-scoped
// quotas map that matches any namespace not explicitly listed. A real namespace
// named "default" cannot be configured directly via this key — list it in
// `exclude` or use a strict allowlist instead.
const QuotaLimiterReservedNamespaceKey = "default"

// QuotaUnlimited is the per-type quota value that disables the cap for that
// accelerator type in the containing namespace. Follows the Kubernetes
// convention of -1 = no limit (e.g., `terminationGracePeriodSeconds: -1`).
const QuotaUnlimited = -1

// MaxQuotaValue is the largest finite quota a single entry may declare. It is
// far above any realistic GPU count (a million accelerators) and exists only to
// keep the aggregate sum (summed across namespaces in aggregateNamespacePools)
// well clear of int64 overflow — an overflowed sum could wrap negative and be
// misread as the QuotaUnlimited (-1) sentinel.
const MaxQuotaValue = 1 << 20 // 1,048,576

// QuotaScope identifies whether a quota limiter applies cluster-wide or
// per-namespace. Both scopes can coexist; each acts independently.
type QuotaScope string

const (
	// QuotaScopeCluster caps total GPUs of a given accelerator type across
	// all namespaces.
	QuotaScopeCluster QuotaScope = "cluster"

	// QuotaScopeNamespace caps GPUs of a given accelerator type within a
	// specific namespace, with optional fall-through via the reserved
	// `default` key.
	QuotaScopeNamespace QuotaScope = "namespace"
)

// QuotaLimiterConfig is one entry in the quota-limiter ConfigMap. Each
// entry produces an independent limiter — cluster and namespace scopes
// are declared as separate entries so operators can enable them
// independently.
//
// The active scope is chosen by the Scope field: when Scope ==
// QuotaScopeCluster, only ClusterQuotas is consulted; when Scope ==
// QuotaScopeNamespace, only NamespaceQuotas and Exclude are.
type QuotaLimiterConfig struct {
	// Name identifies the limiter in logs, metrics, and DecisionStep traces.
	// Must be non-empty and unique across all entries.
	Name string `yaml:"name" json:"name"`

	// Type must be the literal string "quota" — leaves room for future
	// limiter types in the same config schema (e.g., reservation, priority).
	Type string `yaml:"type" json:"type"`

	// Scope selects which quota map below is consulted.
	Scope QuotaScope `yaml:"scope" json:"scope"`

	// ClusterQuotas applies when Scope == QuotaScopeCluster. Keys are
	// accelerator type names (e.g., "H100"); values are the cluster-wide
	// cap in GPUs (or QuotaUnlimited for no cap).
	ClusterQuotas map[string]int `yaml:"quotas,omitempty" json:"quotas,omitempty"`

	// NamespaceQuotas applies when Scope == QuotaScopeNamespace. Top-level
	// keys are namespace names (with the reserved key `default` matching
	// any namespace not explicitly listed); inner keys are accelerator
	// type names; inner values are caps (or QuotaUnlimited).
	//
	// Looked up via QuotaForNamespace; missing or zero entries mean "no
	// allocation".
	NamespaceQuotas map[string]map[string]int `yaml:"namespaceQuotas,omitempty" json:"namespaceQuotas,omitempty"`

	// Exclude lists namespaces that bypass this limiter entirely (no
	// constraint applied). Only meaningful when Scope == QuotaScopeNamespace.
	// Useful for system namespaces or privileged tenants.
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`

	// Kueue, when enabled, reads GPU quotas from Kueue (ClusterQueue nominal
	// quotas, attributed to namespaces through their LocalQueues) and bounds
	// this entry by them: per namespace (or cluster) and accelerator type the
	// SMALLER of the Kueue cap and the static cap above wins. See BoundBy for
	// the exact rules. Nil or disabled means the static maps are the only source.
	Kueue *KueueQuotaSource `yaml:"kueue,omitempty" json:"kueue,omitempty"`
}

// KueueQuotaSource configures reading quotas from Kueue for one quota entry.
//
// Kueue is the cluster's admission-time quota authority; this makes the scaling manager respect
// the same figures before it asks KEDA for a replica that Kueue would then hold
// pending. The mapping is: a ClusterQueue's nominalQuota for a GPU extended
// resource, per flavor, becomes a per-accelerator-type cap; a namespace is
// granted the caps of every ClusterQueue one of its LocalQueues points at; the
// cluster figure is the sum over all ClusterQueues. With CountBorrowable (the
// default), a namespace-scope entry also counts what its queues can borrow from
// their cohorts, where that is safe: the namespace's cap becomes its guaranteed
// figure and the greedy optimizer may draw past it from the cohort's free pool
// (the Kueue borrowing proposal, PR #161, Part B).
//
// A ClusterQueue shared by several namespaces is a per-namespace CEILING here,
// not a partition: each namespace with a LocalQueue on it is bounded at the
// queue's full nominal quota, so in sum they may still ask for more than the
// queue admits, and Kueue holds the excess pending. The bound removes the
// single-namespace over-ask; where namespaces share a queue, the shared budget
// is Kueue's to arbitrate, as it was before.
type KueueQuotaSource struct {
	// Enabled turns the reader on. A `kueue:` block with enabled false is the
	// same as no block at all.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Resources lists the extended-resource names that count as GPUs in a
	// ClusterQueue (e.g. "nvidia.com/gpu"). Empty means every vendor resource
	// the scaling manager already knows (constants.VendorResources).
	Resources []string `yaml:"resources,omitempty" json:"resources,omitempty"`

	// RefreshInterval caps how often Kueue is re-read; a Go duration string.
	// Empty means DefaultKueueRefreshInterval. Between reads the last snapshot is
	// served, so a quota edit in Kueue takes up to this long to bind here.
	RefreshInterval string `yaml:"refreshInterval,omitempty" json:"refreshInterval,omitempty"`

	// CountBorrowable lets the entry count GPUs its ClusterQueue can borrow from
	// its cohort, on top of the guaranteed figure. Absent means true: borrowing
	// is on by default, and is counted only where it is safe (B1). false keeps
	// exactly the nominal cap, as before borrowing was read.
	CountBorrowable *bool `yaml:"countBorrowable,omitempty" json:"countBorrowable,omitempty"`

	// PlacementTimeout is how long a borrowed replica may stay unplaced --
	// unschedulable, or held at Kueue's admission gate -- before it is released
	// (B5); a Go duration. Empty means DefaultPlacementTimeout. The same setting
	// bounds how long the utilization-share optimizer holds a receiver whose
	// replica could not be placed (HoldFill), so the two cannot drift apart.
	PlacementTimeout string `yaml:"placementTimeout,omitempty" json:"placementTimeout,omitempty"`

	// BorrowCooldown is how long nothing new is borrowed in a cohort after one
	// of its lenders waited, and by a model after Kueue preempted one of its
	// borrowed pods; a Go duration. Empty means DefaultBorrowCooldown.
	BorrowCooldown string `yaml:"borrowCooldown,omitempty" json:"borrowCooldown,omitempty"`

	// LenderDebounce is how long a lender must read as waiting, over at least
	// two fresh reads, before it counts as waiting; a Go duration. Empty means
	// DefaultLenderDebounce.
	LenderDebounce string `yaml:"lenderDebounce,omitempty" json:"lenderDebounce,omitempty"`

	// BorrowNamespaces, when set, limits counting borrowable capacity to these
	// namespaces, so borrowing can be piloted on a few before the whole entry.
	// Every other namespace caps at nominal, withheld with reason
	// not-selected. Empty means every namespace the entry governs.
	BorrowNamespaces []string `yaml:"borrowNamespaces,omitempty" json:"borrowNamespaces,omitempty"`

	// ControllerNamespace is the namespace of the Kueue controller Deployment
	// (kueue-controller-manager), whose image tag tells the Kueue version
	// borrowing is checked against. Empty means kueue-system.
	ControllerNamespace string `yaml:"controllerNamespace,omitempty" json:"controllerNamespace,omitempty"`

	// AssumeTestedVersion counts borrowing under a Kueue controller whose image
	// names no version (a digest-only reference), as if it were a tested one.
	// Default false: such a version withholds borrowing
	// (kueue-version-unknown). It applies only to a version that cannot be
	// read from a readable Deployment, and never lifts kueue-controller-down.
	AssumeTestedVersion bool `yaml:"assumeTestedVersion,omitempty" json:"assumeTestedVersion,omitempty"`

	// ReleaseToNonReclaimingLenders also counts what lenders Kueue does not
	// reclaim for lend -- reclaimWithinCohort Never, or LowerPriority below
	// the borrowing model's priority -- and hands it back when such a lender
	// waits: the scaling manager lowers the borrowing models, lowest priority
	// first, by whole units covering what the lender waits for. It makes the
	// scaling manager Kueue's reclaim agent for those lenders. Off by
	// default (experimental): that lending is then not counted
	// (lender-cannot-reclaim), and a waiting lender only stops new borrowing
	// in its cohort. It needs countBorrowable.
	ReleaseToNonReclaimingLenders bool `yaml:"releaseToNonReclaimingLenders,omitempty" json:"releaseToNonReclaimingLenders,omitempty"`
}

// KueueBorrowSelected reports whether the entry's borrowNamespaces selects ns:
// true when the list is empty or names ns.
func (q QuotaLimiterConfig) KueueBorrowSelected(ns string) bool {
	return q.Kueue == nil || len(q.Kueue.BorrowNamespaces) == 0 || slices.Contains(q.Kueue.BorrowNamespaces, ns)
}

// HasUnlimitedCap reports whether the entry's static caps, in its scope, leave
// some accelerator type unlimited (QuotaUnlimited, -1): only an external
// source then bounds that type.
func (q QuotaLimiterConfig) HasUnlimitedCap() bool {
	if q.Scope == QuotaScopeCluster {
		for _, v := range q.ClusterQuotas {
			if v == QuotaUnlimited {
				return true
			}
		}
		return false
	}
	for _, perType := range q.NamespaceQuotas {
		for _, v := range perType {
			if v == QuotaUnlimited {
				return true
			}
		}
	}
	return false
}

// Defaults of the borrowing timings (the Kueue borrowing proposal, PR #161,
// B5). The placement timeout and the cooldown are first guesses, to be
// replaced by figures the live-Kueue e2e measures.
const (
	DefaultPlacementTimeout = 5 * time.Minute
	DefaultBorrowCooldown   = 10 * time.Minute
	DefaultLenderDebounce   = 60 * time.Second
)

// KueueBorrowTimings are the borrowing timings of one entry.
type KueueBorrowTimings struct {
	PlacementTimeout time.Duration
	// PlacementTimeoutSet is true when the entry sets PlacementTimeout.
	PlacementTimeoutSet bool
	Cooldown            time.Duration
	LenderDebounce      time.Duration
}

// BorrowTimings returns the entry's borrowing timings, each at its default
// when unset or unparseable (Validate rejects the latter).
func (q QuotaLimiterConfig) BorrowTimings() KueueBorrowTimings {
	t := KueueBorrowTimings{PlacementTimeout: DefaultPlacementTimeout, Cooldown: DefaultBorrowCooldown, LenderDebounce: DefaultLenderDebounce}
	if q.Kueue == nil {
		return t
	}
	if d, ok := positiveDuration(q.Kueue.PlacementTimeout); ok {
		t.PlacementTimeout, t.PlacementTimeoutSet = d, true
	}
	if d, ok := positiveDuration(q.Kueue.BorrowCooldown); ok {
		t.Cooldown = d
	}
	if d, ok := positiveDuration(q.Kueue.LenderDebounce); ok {
		t.LenderDebounce = d
	}
	return t
}

// positiveDuration parses s as a positive Go duration.
func positiveDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d > 0
}

// KueueCountsBorrowable reports whether this entry reads Kueue and would count
// borrowable capacity: the Kueue block is enabled and countBorrowable is not
// false.
func (q QuotaLimiterConfig) KueueCountsBorrowable() bool {
	return q.KueueEnabled() && (q.Kueue.CountBorrowable == nil || *q.Kueue.CountBorrowable)
}

// KueueReleasesToNonReclaiming reports whether this entry counts what lenders
// Kueue does not reclaim for lend, and hands it back when they wait
// (releaseToNonReclaimingLenders, off by default). It needs the entry to
// count borrowing.
func (q QuotaLimiterConfig) KueueReleasesToNonReclaiming() bool {
	return q.KueueCountsBorrowable() && q.Kueue.ReleaseToNonReclaimingLenders
}

// DefaultKueueRefreshInterval is how often Kueue quotas are re-read when the
// entry does not say. The saturation cycle is of the same order, so a shorter
// value would only add API calls the next decision cannot use.
const DefaultKueueRefreshInterval = 30 * time.Second

// KueueEnabled reports whether this entry reads quotas from Kueue.
func (q QuotaLimiterConfig) KueueEnabled() bool {
	return q.Kueue != nil && q.Kueue.Enabled
}

// KueueRefreshInterval returns the configured Kueue refresh interval, or the
// default when unset. Callers must have run Validate, which rejects a value that
// does not parse; an unparseable value here falls back to the default rather than
// panicking.
func (q QuotaLimiterConfig) KueueRefreshInterval() time.Duration {
	if q.Kueue == nil || q.Kueue.RefreshInterval == "" {
		return DefaultKueueRefreshInterval
	}
	d, err := time.ParseDuration(q.Kueue.RefreshInterval)
	if err != nil || d <= 0 {
		return DefaultKueueRefreshInterval
	}
	return d
}

// validate checks the Kueue block of one entry. Only the fields an operator can
// get wrong are checked: a resource name must be non-empty and the refresh
// interval must be a positive duration.
func (k *KueueQuotaSource) validate(location string) error {
	if k == nil {
		return nil
	}
	var errs []error
	for i, r := range k.Resources {
		if strings.TrimSpace(r) == "" {
			errs = append(errs, fmt.Errorf("%s.kueue.resources[%d]: resource name must not be empty", location, i))
		}
	}
	seen := make(map[string]bool, len(k.BorrowNamespaces))
	for i, ns := range k.BorrowNamespaces {
		switch {
		case len(validation.IsDNS1123Label(ns)) > 0:
			errs = append(errs, fmt.Errorf("%s.kueue.borrowNamespaces[%d]: %q is not a namespace name", location, i, ns))
		case seen[ns]:
			errs = append(errs, fmt.Errorf("%s.kueue.borrowNamespaces[%d]: %q is listed twice", location, i, ns))
		}
		seen[ns] = true
	}
	if k.ControllerNamespace != "" && len(validation.IsDNS1123Label(k.ControllerNamespace)) > 0 {
		errs = append(errs, fmt.Errorf("%s.kueue.controllerNamespace: %q is not a namespace name", location, k.ControllerNamespace))
	}
	for _, f := range []struct{ name, value string }{
		{"refreshInterval", k.RefreshInterval},
		{"placementTimeout", k.PlacementTimeout},
		{"borrowCooldown", k.BorrowCooldown},
		{"lenderDebounce", k.LenderDebounce},
	} {
		if f.value == "" {
			continue
		}
		d, err := time.ParseDuration(f.value)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s.kueue.%s: %q is not a duration: %w", location, f.name, f.value, err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("%s.kueue.%s: %q must be positive", location, f.name, f.value))
		}
	}
	if k.ReleaseToNonReclaimingLenders && k.CountBorrowable != nil && !*k.CountBorrowable {
		errs = append(errs, fmt.Errorf("%s.kueue.releaseToNonReclaimingLenders: true needs countBorrowable; "+
			"with countBorrowable: false nothing is borrowed, so nothing is handed back", location))
	}
	return errors.Join(errs...)
}

// clone returns a copy of k that shares no mutable state with the original.
func (k *KueueQuotaSource) clone() *KueueQuotaSource {
	if k == nil {
		return nil
	}
	out := *k
	if k.Resources != nil {
		out.Resources = slices.Clone(k.Resources)
	}
	if k.CountBorrowable != nil {
		v := *k.CountBorrowable
		out.CountBorrowable = &v
	}
	if k.BorrowNamespaces != nil {
		out.BorrowNamespaces = slices.Clone(k.BorrowNamespaces)
	}
	return &out
}

// ExternalCaps is what an external source grants one subject — a namespace, or
// the cluster. Values are finite caps; a source never emits QuotaUnlimited.
type ExternalCaps struct {
	// ByType caps per accelerator type, keyed by the short name the source could
	// attribute (a Kueue flavor whose node labels name a product).
	ByType map[string]int
	// Untyped is a grant the source could NOT attribute to an accelerator type —
	// a Kueue flavor without a product label, which Kueue's own quickstart uses.
	// It bounds every type the static entry names for this subject; on its own
	// it names no type and so cannot open one. HasUntyped tells a real grant of
	// 0 apart from no untyped grant.
	Untyped    int
	HasUntyped bool
}

// IsZero reports whether the source granted this subject nothing at all —
// neither a typed nor an untyped cap.
func (c ExternalCaps) IsZero() bool {
	return len(c.ByType) == 0 && !c.HasUntyped
}

// ExternalQuotas is a snapshot of GPU caps read from an external authority
// (today Kueue), per namespace for the namespace scope and for the whole
// cluster for the cluster scope.
//
// Absence carries meaning, and it differs between the levels:
//   - A namespace missing from Namespace is NOT governed by the source (no
//     LocalQueue of its reaches a ClusterQueue that declares a GPU resource), so
//     the static entry alone decides for it.
//   - A type missing from a PRESENT namespace's ByType is a cap of 0 — unless
//     that namespace has an untyped grant, which then applies — because the
//     source knows the namespace and grants it nothing of that type.
//   - A zero Cluster means the source declares no GPU budget at all, so it
//     contributes nothing at cluster scope.
type ExternalQuotas struct {
	Namespace map[string]ExternalCaps
	Cluster   ExternalCaps
	// ObservedAt is when the source was last read successfully; zero when it
	// never was, in which case the maps are empty and mean nothing.
	ObservedAt time.Time
	// Borrow is, per namespace Namespace governs, what borrowing in the
	// source's cohorts would add. BoundBy ignores it; a quota entry that counts
	// borrowing reads it beside NamespaceGuaranteed and NamespaceWithinNominal.
	Borrow map[string][]ExternalBorrow
	// NamespaceGuaranteed is Namespace at each queue's guaranteed figure
	// instead of its nominal one, built by the same rules (typed and untyped
	// grants, inactive queues), so it can stand in for Namespace in BoundBy for
	// a namespace whose borrowing is counted. Where a queue's borrowing is
	// withheld its guaranteed figure is its nominal, so the two agree.
	NamespaceGuaranteed map[string]ExternalCaps
	// NamespaceWithinNominal is Namespace at each queue's within-nominal figure
	// (ExternalBorrow.WithinNominal): what scale-from-zero, the warm pool and
	// rescale may spend (B7).
	NamespaceWithinNominal map[string]ExternalCaps
	// SharedPools are the cohorts' free pools, one per cohort, flavor and GPU
	// resource, each spent once across every namespace in the cohort.
	SharedPools []ExternalSharedPool
	// BorrowStale is set when the read succeeded for the nominal figures but
	// not for borrowing: a transient failure to list the Cohort objects. Every
	// borrowing row then carries the stale reason, and a quota entry that counts
	// borrowing treats the snapshot as stale -- it freezes, never shrinks.
	BorrowStale bool
	// KueueControllerDown is set when no Kueue leader is known to run (its
	// leader Lease not renewed, or without one no ready replica, or neither
	// readable: Forbidden or never found), while Kueue's API still answers: the
	// figures are readable but stop moving. A quota entry that counts
	// borrowing freezes, as on a stale snapshot, and reports every governed
	// namespace withheld with reason kueue-controller-down.
	KueueControllerDown bool
	// ClassPriority is, per priority class ("<group>/<name>", or "none"; see
	// kueue.TemplatePriorityClass), the highest spec.priority of an unfinished
	// GPU Workload of that class. A lender on reclaimWithinCohort LowerPriority
	// gets back what a model holds only when it outranks the model's own
	// Workloads, whose priority is looked up here by the model's class.
	ClassPriority map[string]int32
	// NamespaceHighestPriority is, per namespace, the highest spec.priority of
	// its unfinished GPU Workloads: the priority of a model whose pod template
	// names no class, read cautiously. A namespace none of whose Workloads was
	// read is absent.
	NamespaceHighestPriority map[string]int32
}

// ExternalSharedPool is one cohort's free capacity of one flavor and GPU
// resource, and where pods of that flavor may run.
type ExternalSharedPool struct {
	Cohort   string `json:"cohort"`
	Flavor   string `json:"flavor"`
	Resource string `json:"resource"`
	// Accelerator is the product the flavor's node labels name; empty for a
	// flavor without a product label.
	Accelerator string              `json:"accelerator,omitempty"`
	NodeLabels  map[string]string   `json:"nodeLabels,omitempty"`
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Free is the cohort's free pool; 0 when Reason is set.
	Free   int    `json:"free"`
	Reason string `json:"reason,omitempty"`
	// WaitingLenders are the member queues that, in this read, wait for
	// capacity they lent (kueue.LenderWaiting). Not debounced.
	WaitingLenders []string `json:"waitingLenders,omitempty"`
	// Lenders is every active member declaring this key, for the release of
	// borrowed capacity to a lender that waits (B6).
	Lenders []ExternalLender `json:"lenders,omitempty"`
	// PendingGPUs is what the pending Workloads of every member queue request
	// of this flavor and resource: demand Kueue may admit onto the free pool
	// first, whichever namespace or controller it belongs to.
	PendingGPUs int `json:"pendingGPUs,omitempty"`
	// CoFree is the cohort's free pool of every other resource the flavor's
	// resource group covers (CPU, memory, ...), in milli-units.
	CoFree map[string]int64 `json:"coFree,omitempty"`
	// CoUnbound are the co-resources charged on a flavor not known: the
	// resource group covering them offers several flavors.
	CoUnbound []string `json:"coUnbound,omitempty"`
	// Borrowed is what the cohort's members borrow of this key (Kueue's
	// status borrowed), BorrowerPriority the highest priority it is held at,
	// nil when not every borrower's was read (kueue.SharedPool).
	Borrowed         int    `json:"borrowed,omitempty"`
	BorrowerPriority *int32 `json:"borrowerPriority,omitempty"`
	// BorrowerLowest is the lowest priority a borrowing member holds a GPU
	// Workload at; nil when none borrows or one's was not read
	// (kueue.SharedPool.BorrowerLowest).
	BorrowerLowest *int32 `json:"borrowerLowest,omitempty"`
	// EvictingGPUs is what the members' preempted Workloads still hold
	// reserved while their pods stop; EvictionsRead whether the Workloads
	// were read (kueue.SharedPool).
	EvictingGPUs      int  `json:"evictingGPUs,omitempty"`
	StuckEvictingGPUs int  `json:"stuckEvictingGPUs,omitempty"`
	EvictionsRead     bool `json:"evictionsRead,omitempty"`
}

// ExternalLender is one cohort member as a lender of one flavor and resource.
type ExternalLender struct {
	Queue string `json:"queue"`
	// Waiting is the raw per-read predicate: a pending Workload that asks for
	// this GPU resource and fits within the nominal it does not hold.
	Waiting bool `json:"waiting,omitempty"`
	// Reclaim is its reclaimWithinCohort policy (kueue.Reclaims).
	Reclaim string `json:"reclaim,omitempty"`
	// Unused is what it lends and does not use itself.
	Unused int `json:"unused,omitempty"`
	// Priority is the lowest priority of its unfinished GPU Workloads, pending
	// or reserved; nil when none was read.
	Priority *int32 `json:"priority,omitempty"`
	// Kept is what it keeps of its nominal (lendingLimit) and does not use:
	// Kueue admits that much of its own without anything handed back.
	Kept int `json:"kept,omitempty"`
	// PendingGPUs is the smallest pending Workload Kueue could admit next
	// within its nominal (kueue.LenderState.PendingGPUs): what a release to
	// it (releaseToNonReclaimingLenders) is sized by.
	PendingGPUs int `json:"pendingGPUs,omitempty"`
	// PendingPriority is the highest priority among those Workloads, the
	// one Kueue preempts for it at; nil when none counts.
	PendingPriority *int32 `json:"pendingPriority,omitempty"`
	// PendingSince is when the longest-waiting of them started to wait; nil
	// when none counts.
	PendingSince *time.Time `json:"pendingSince,omitempty"`
}

// ExternalBorrow is one queue's borrowing figures for one flavor and GPU
// resource, as granted to a namespace. Guaranteed plus Borrowable is how far the
// queue could reach in its cohort; Reason, when set, says why Borrowable is 0.
type ExternalBorrow struct {
	Queue       string `json:"queue"`
	Cohort      string `json:"cohort,omitempty"`
	Flavor      string `json:"flavor"`
	Resource    string `json:"resource"`
	Accelerator string `json:"accelerator,omitempty"`
	Nominal     int    `json:"nominal"`
	Guaranteed  int    `json:"guaranteed"`
	Borrowable  int    `json:"borrowable"`
	// BorrowCap is the queue's borrowingLimit, QuotaUnlimited when unset.
	BorrowCap int `json:"borrowCap"`
	// WithinNominal is what the queue could reach without borrowing (B7).
	WithinNominal int `json:"withinNominal"`
	// LentOut is the part of a reclaiming queue's (reclaimWithinCohort Any)
	// unused nominal its peers borrow now and Kueue would preempt them for,
	// past PreemptAfter: growth into it needs no node hole.
	LentOut int `json:"lentOut,omitempty"`
	// PreemptAfter is how far a reclaiming queue grows before Kueue has to
	// preempt for it: what it keeps unused plus what the cohort's free pool
	// covers. Growth into it needs a node hole and debits the pool.
	PreemptAfter int `json:"preemptAfter,omitempty"`
	// PreemptKept is the part of PreemptAfter the queue keeps unused, which
	// no cohort pool holds: growth into it takes nothing from the pool.
	PreemptKept int `json:"preemptKept,omitempty"`
	// SourceBorrowed is the source's own figure of what the queue borrows now,
	// logged as a cross-check.
	SourceBorrowed int    `json:"kueueBorrowed"`
	Reason         string `json:"reason,omitempty"`
	// PinRequired is set when a variant must pin a flavor's nodeLabels for its
	// borrowing to be counted: the queue offers more than one flavor for the
	// GPU.
	PinRequired bool `json:"pinRequired,omitempty"`
	// CoHeadroom is, per co-resource, how far the queue's usage of it may
	// still grow: nominal plus borrowingLimit less usage, in milli-units;
	// absent where borrowingLimit is unset. CoKept is what it keeps unlent and
	// does not use.
	CoHeadroom map[string]int64 `json:"coHeadroom,omitempty"`
	CoKept     map[string]int64 `json:"coKept,omitempty"`
	// Reserved is what the source has charged the queue on this flavor and
	// resource, whoever holds it: every workload it admitted, managed or not.
	// A quota entry charges the queue's figures with the part its own variants
	// do not hold (the non-managed charge).
	Reserved int `json:"reserved,omitempty"`
	// CoUnused is, per co-resource, what the queue's nominal quota of it
	// leaves after everything reserved, in milli-units: the co-resource
	// headroom of growth within nominal.
	CoUnused map[string]int64 `json:"coUnused,omitempty"`
	// Charge is the queue's non-managed charge on this flavor and resource,
	// as the row's namespace sees it: Reserved less what that namespace's
	// managed variants hold in the queue. Negative only for a type several of
	// the namespace's queues grant, where its GPUs are counted in each. Set by
	// the quota entry, not the source; the row's Nominal, Guaranteed and
	// WithinNominal are Kueue's, before it.
	Charge int `json:"charge,omitempty"`
}

// Counted reports whether the row's borrowing is counted: no reason, or only
// the reason that qualifies a guaranteed figure below nominal.
func (b ExternalBorrow) Counted() bool {
	return b.Reason == "" || b.Reason == BorrowReasonNominalLentOut
}

// BorrowReasonNominalLentOut is the one ExternalBorrow reason under which
// borrowing is still counted: it qualifies the guaranteed figure, it does not
// withhold. It mirrors kueue.ReasonNominalLentOut, which config cannot import.
const BorrowReasonNominalLentOut = "nominal-lent-out"

// Withheld returns b with borrowing not counted, for reason: the guaranteed
// and within-nominal figures fall back to nominal, as when borrowing is off.
func (b ExternalBorrow) Withheld(reason string) ExternalBorrow {
	b.Guaranteed, b.WithinNominal, b.Borrowable, b.LentOut, b.PreemptAfter, b.Reason = b.Nominal, b.Nominal, 0, 0, 0, reason
	b.PreemptKept = 0
	return b
}

// BoundBy returns this entry bounded by an external snapshot: the static maps and
// the external ones are combined per key so that the SMALLER cap wins, which is
// what an operator declaring both means — neither source may raise what the other
// granted. Exclude is untouched; excluded namespaces bypass both sources.
//
// Rules for the namespace scope, per namespace N present in ext.Namespace:
//   - N excluded: skipped.
//   - N is the reserved "default" key: skipped. Writing it would turn one
//     namespace's Kueue grant into the fall-through for every unlisted namespace;
//     the K8s "default" namespace is already unconfigurable here (see
//     QuotaLimiterReservedNamespaceKey).
//   - N listed in NamespaceQuotas: its map becomes min(static map, ext map).
//   - N unlisted but a static "default" exists: min(default map, ext map). The
//     namespace becomes explicitly listed, at that per-namespace budget.
//   - N unlisted and NamespaceQuotas is empty: the external map is taken as is
//     — an entry with no static map has asked for the source to be the whole
//     answer.
//   - N unlisted in a non-empty static allowlist without "default": left out.
//     The static list denies N and the source cannot open it.
//
// Namespaces the snapshot does not mention keep their static treatment.
//
// min over a static map and an external grant is taken over the UNION of their
// type keys: a static type the source does not name gets the source's untyped
// grant if there is one, else 0; a source type the static map does not name is
// 0 — both are closed allowlists, so "not listed" already means "denied" on
// each. Type keys are matched by accelerator identity (accelerator.SameName) and
// the static spelling is kept, so a Kueue flavor labelled
// "NVIDIA-H100-80GB-HBM3" bounds a static "H100". QuotaUnlimited (-1) on either
// side yields the other side's value — which is how an entry hands the numbers
// to Kueue while keeping the type names: `default: {H100: -1}` plus a Kueue
// flavor with no product label gives every namespace min(-1, untyped).
//
// An untyped grant standing alone names no type, so where the static entry
// names none either (Kueue-alone paths) it cannot be applied; UnappliedUntyped
// reports those subjects so the limiter can say so.
//
// Cluster scope: a zero ext.Cluster changes nothing; an empty static quotas map
// takes ext.Cluster's typed caps as is; otherwise min over the union, as above.
func (q QuotaLimiterConfig) BoundBy(ext ExternalQuotas) QuotaLimiterConfig {
	out := q.clone()
	switch q.Scope {
	case QuotaScopeCluster:
		switch {
		case ext.Cluster.IsZero():
		case len(q.ClusterQuotas) == 0:
			out.ClusterQuotas = maps.Clone(ext.Cluster.ByType)
		default:
			out.ClusterQuotas = minTypeQuotas(q.ClusterQuotas, ext.Cluster)
		}
	case QuotaScopeNamespace:
		if len(ext.Namespace) == 0 {
			return out
		}
		if out.NamespaceQuotas == nil {
			out.NamespaceQuotas = make(map[string]map[string]int, len(ext.Namespace))
		}
		staticDefault, hasDefault := q.NamespaceQuotas[QuotaLimiterReservedNamespaceKey]
		staticEmpty := len(q.NamespaceQuotas) == 0
		for ns, caps := range ext.Namespace {
			if ns == QuotaLimiterReservedNamespaceKey || q.IsExcluded(ns) || caps.IsZero() {
				continue
			}
			switch base, listed := q.NamespaceQuotas[ns]; {
			case listed:
				out.NamespaceQuotas[ns] = minTypeQuotas(base, caps)
			case hasDefault:
				out.NamespaceQuotas[ns] = minTypeQuotas(staticDefault, caps)
			case staticEmpty && len(caps.ByType) > 0:
				out.NamespaceQuotas[ns] = maps.Clone(caps.ByType)
			}
		}
	}
	return out
}

// UnappliedUntyped lists the subjects whose untyped external grant BoundBy could
// not apply because the static entry names no accelerator type for them: the
// namespaces (by name), and "cluster" for the cluster scope. Such a grant is
// real — Kueue would admit that many GPUs — but the limiter needs a type to
// budget under; the fix is to name the types in the static map, with -1 to let
// the source supply the figure.
func (q QuotaLimiterConfig) UnappliedUntyped(ext ExternalQuotas) []string {
	var out []string
	switch q.Scope {
	case QuotaScopeCluster:
		if ext.Cluster.HasUntyped && len(q.ClusterQuotas) == 0 {
			out = append(out, "cluster")
		}
	case QuotaScopeNamespace:
		_, hasDefault := q.NamespaceQuotas[QuotaLimiterReservedNamespaceKey]
		for ns, caps := range ext.Namespace {
			if !caps.HasUntyped || ns == QuotaLimiterReservedNamespaceKey || q.IsExcluded(ns) {
				continue
			}
			if _, listed := q.NamespaceQuotas[ns]; listed || hasDefault {
				continue
			}
			out = append(out, ns)
		}
	}
	slices.Sort(out)
	return out
}

// minTypeQuotas bounds a static per-type map by an external grant, over the
// union of type keys. A static type the grant does not name takes the untyped
// grant when there is one, else 0; a granted type the static map does not name
// is 0. Keys are matched by accelerator identity and the result keeps the static
// spelling for matched keys, the grant's for the rest.
func minTypeQuotas(base map[string]int, caps ExternalCaps) map[string]int {
	out := make(map[string]int, len(base)+len(caps.ByType))
	matched := make(map[string]bool, len(caps.ByType))
	for accType, baseCap := range base {
		// Every grant naming this accelerator, summed: a static "H100" is
		// bounded by the PCIe flavor's quota PLUS the SXM flavor's, since it
		// means either; a static "NVIDIA-H100-PCIE-80GB" meets only its own.
		otherCap, found := 0, false
		for grantType, n := range caps.ByType {
			if accelerator.SameName(grantType, accType) {
				otherCap += n
				found = true
				matched[grantType] = true
			}
		}
		if !found && caps.HasUntyped {
			otherCap = caps.Untyped
		}
		out[accType] = minQuota(baseCap, otherCap)
	}
	for accType := range caps.ByType {
		if matched[accType] {
			continue // already combined under the static spelling
		}
		out[accType] = 0
	}
	return out
}

// minQuota returns the tighter of two caps, treating QuotaUnlimited as no bound.
func minQuota(a, b int) int {
	switch {
	case a == QuotaUnlimited:
		return b
	case b == QuotaUnlimited:
		return a
	}
	return min(a, b)
}

// IsExcluded reports whether the given namespace bypasses this limiter.
// Always false for cluster-scoped entries.
func (q QuotaLimiterConfig) IsExcluded(namespace string) bool {
	if q.Scope != QuotaScopeNamespace {
		return false
	}
	return slices.Contains(q.Exclude, namespace)
}

// QuotaForNamespace returns the per-type quota map for the given namespace,
// applying the lookup rules from the design:
//
//  1. Excluded namespaces return (nil, true) — caller should skip enforcement.
//  2. Exact match in NamespaceQuotas wins.
//  3. Otherwise, fall through to the reserved `default` key. The returned map
//     represents the cap *per* unlisted namespace (matching the Kubernetes
//     LimitRange default semantic), not a shared pool — the limiter tracks
//     usage per concrete namespace name, so each unlisted namespace consumes
//     its own budget at the default level.
//  4. Otherwise, return an empty map (treated as zero quota by callers).
//
// The boolean second return value indicates whether the namespace is
// excluded (true) — the per-type map is nil in that case and callers
// must skip enforcement rather than treating it as zero.
//
// The returned per-type map is a fresh copy, so callers may read or mutate it
// without aliasing the config (and, critically, without two unlisted namespaces
// sharing one `default` map). Always returns (nil, false) for cluster-scoped
// entries.
func (q QuotaLimiterConfig) QuotaForNamespace(namespace string) (map[string]int, bool) {
	if q.Scope != QuotaScopeNamespace {
		return nil, false
	}
	if q.IsExcluded(namespace) {
		return nil, true
	}
	if quotas, ok := q.NamespaceQuotas[namespace]; ok {
		return maps.Clone(quotas), false
	}
	if quotas, ok := q.NamespaceQuotas[QuotaLimiterReservedNamespaceKey]; ok {
		return maps.Clone(quotas), false
	}
	return map[string]int{}, false
}

// clone returns a deep copy of q: the ClusterQuotas / NamespaceQuotas maps and
// the Exclude slice and the Kueue block are duplicated so the result shares no
// mutable state with the original. Used by Config.EffectiveQuotaEntries to hand out entries that callers
// cannot use to mutate the config-owned snapshot.
func (q QuotaLimiterConfig) clone() QuotaLimiterConfig {
	out := q // copies scalar fields and (to be replaced) map/slice headers
	if q.ClusterQuotas != nil {
		out.ClusterQuotas = maps.Clone(q.ClusterQuotas)
	}
	if q.NamespaceQuotas != nil {
		out.NamespaceQuotas = make(map[string]map[string]int, len(q.NamespaceQuotas))
		for ns, perType := range q.NamespaceQuotas {
			out.NamespaceQuotas[ns] = maps.Clone(perType)
		}
	}
	if q.Exclude != nil {
		out.Exclude = slices.Clone(q.Exclude)
	}
	out.Kueue = q.Kueue.clone()
	return out
}

// QuotaLimiterEntries is the top-level ConfigMap shape: one or more
// QuotaLimiterConfig entries under a single `limiters` key. Allows
// multiple limiters (e.g., cluster + namespace) to coexist in one
// ConfigMap data entry.
type QuotaLimiterEntries struct {
	Limiters []QuotaLimiterConfig `yaml:"limiters" json:"limiters"`
}

// Validate checks an entries block for structural correctness. It
// returns nil only when every entry parses cleanly. When multiple entries
// (or multiple fields within an entry) are broken, all errors are
// accumulated and joined with errors.Join — an operator fixing a malformed
// ConfigMap sees every problem in one pass instead of one-per-rebuild.
// Error messages name the failing entry by index and field so the bad row
// is easy to locate.
//
// Validation rules (all from issue #1002):
//   - Name is non-empty, unique across entries, and not one of
//     ReservedLimiterNames.
//   - Type == "quota" (other limiter types may be added later).
//   - Scope is one of the two QuotaScope values.
//   - Per-type quota values are >= QuotaUnlimited (only -1 is allowed as negative).
//   - Accelerator type names are non-empty.
//   - For namespace scope: namespace names are non-empty; warn (not error)
//     if a namespace appears in both Exclude and NamespaceQuotas — the
//     entry is still valid and Exclude wins.
//
// Returned warnings (non-fatal) are surfaced via a second return value
// so callers can log them without rejecting the config.
func (e *QuotaLimiterEntries) Validate() (warnings []string, err error) {
	if e == nil {
		return nil, errors.New("quota limiter entries are nil")
	}
	var errs []error
	seenNames := make(map[string]int, len(e.Limiters))
	for i, entry := range e.Limiters {
		if entry.Name == "" {
			errs = append(errs, fmt.Errorf("entry[%d]: name must not be empty", i))
			// Without a name we cannot meaningfully validate the rest of this
			// entry's contents (error messages would lack the entry identifier).
			continue
		}
		if prev, ok := seenNames[entry.Name]; ok {
			errs = append(errs, fmt.Errorf("entry[%d]: duplicate limiter name %q (first seen at entry[%d])", i, entry.Name, prev))
			continue
		}
		if slices.Contains(ReservedLimiterNames, entry.Name) {
			errs = append(errs, reservedNameError(fmt.Sprintf("entry[%d]", i), entry.Name))
		}
		seenNames[entry.Name] = i

		if entry.Type != "quota" {
			errs = append(errs, fmt.Errorf("entry[%d] (%q): type must be \"quota\", got %q", i, entry.Name, entry.Type))
			// Keep going: scope/quotas validation is still useful diagnostically.
		}
		if kErr := entry.Kueue.validate(fmt.Sprintf("entry[%d] (%q)", i, entry.Name)); kErr != nil {
			errs = append(errs, kErr)
		}
		if entry.Kueue != nil && entry.Kueue.ReleaseToNonReclaimingLenders && !entry.Kueue.Enabled {
			warnings = append(warnings, fmt.Sprintf("entry[%d] (%q): kueue.releaseToNonReclaimingLenders is set but kueue.enabled "+
				"is false; nothing is read from Kueue, so nothing is counted or handed back", i, entry.Name))
		}

		switch entry.Scope {
		case QuotaScopeCluster:
			if len(entry.NamespaceQuotas) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): namespaceQuotas is invalid for cluster scope; use the quotas map", i, entry.Name))
			}
			if len(entry.Exclude) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): exclude is invalid for cluster scope", i, entry.Name))
			}
			if vErr := validateTypeQuotaMap(entry.ClusterQuotas, fmt.Sprintf("entry[%d] (%q).quotas", i, entry.Name)); vErr != nil {
				errs = append(errs, vErr)
			}

		case QuotaScopeNamespace:
			if len(entry.ClusterQuotas) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): quotas is invalid for namespace scope; use namespaceQuotas", i, entry.Name))
			}
			for _, ns := range entry.Exclude {
				switch {
				case strings.TrimSpace(ns) == "":
					errs = append(errs, fmt.Errorf("entry[%d] (%q): exclude contains an empty namespace name", i, entry.Name))
				case len(validation.IsDNS1123Label(ns)) > 0:
					warnings = append(warnings,
						fmt.Sprintf("entry[%d] (%q): exclude entry %q is not a valid namespace name (check for spaces or capitals); it never matches a namespace", i, entry.Name, ns))
				}
			}
			for ns, perType := range entry.NamespaceQuotas {
				if strings.TrimSpace(ns) == "" {
					errs = append(errs, fmt.Errorf("entry[%d] (%q): namespaceQuotas contains an empty namespace key", i, entry.Name))
					continue
				}
				if entry.IsExcluded(ns) {
					warnings = append(warnings,
						fmt.Sprintf("entry[%d] (%q): namespace %q is in both exclude and namespaceQuotas; the quota entry will be ignored (exclude wins)", i, entry.Name, ns))
				}
				if vErr := validateTypeQuotaMap(perType, fmt.Sprintf("entry[%d] (%q).namespaceQuotas[%q]", i, entry.Name, ns)); vErr != nil {
					errs = append(errs, vErr)
				}
			}

		default:
			errs = append(errs, fmt.Errorf("entry[%d] (%q): scope must be %q or %q, got %q",
				i, entry.Name, QuotaScopeCluster, QuotaScopeNamespace, entry.Scope))
		}
	}
	return warnings, errors.Join(errs...)
}

// unboundedExcludeWarnings warns about the namespaces a namespace-scoped entry
// excludes that have no GPU limit on some accelerator type. physical is
// whether a physical (gpu-inventory) limiter is declared beside these entries,
// which the caller knows (ScalingPolicy.LimiterWarnings): it gives every named
// type a number, but not a workload that names no type ("unknown").
//
// It reads the configuration as the planner reads the limiters built from it,
// through the same lookup (accelerator.AllowlistBound and
// accelerator.ClusterAllowlistBound), so a type it names is one the planner
// leaves unbounded (see excludeBounds.unbounded):
//
//   - A namespace no other namespace-scoped entry holds is bounded only by the
//     cluster-wide bounds: the scope: cluster entries, each a closed
//     allowlist, and the physical limiter.
//   - A namespace another namespace-scoped entry holds (does not exclude) is
//     bounded by that entry's caps for it too (QuotaForNamespace), a closed
//     allowlist: it has no limit on a type only when every such entry sets the
//     type to -1.
//
// The types checked are finite: every accelerator name an entry lists, the
// family of each, one product of each family that no entry lists (they all
// read alike; see accelerator.UnlistedProduct), and "unknown". Every other
// type falls outside the families a closed allowlist lists, so it is denied.
//
// Not reported: anything, when a scope: cluster entry reads Kueue, and a
// namespace an entry that reads Kueue holds. Their numbers come from Kueue at
// runtime, so they cannot be checked here. Nor an exclude entry that is not a
// valid namespace name: it never matches a namespace, and Validate says so.
func (e *QuotaLimiterEntries) unboundedExcludeWarnings(physical bool) []string {
	b := excludeBounds{physical: physical}
	for _, entry := range e.Limiters {
		if entry.Scope != QuotaScopeCluster {
			continue
		}
		if entry.KueueEnabled() {
			return nil
		}
		b.cluster = append(b.cluster, entry.ClusterQuotas)
	}
	checked := e.checkedTypes()
	var warnings []string
	for i, entry := range e.Limiters {
		if entry.Scope != QuotaScopeNamespace {
			continue
		}
		// One warning per reason, naming every namespace it holds for.
		type reason struct{ holders, types string }
		var order []reason
		byReason := map[reason][]string{}
		for _, ns := range entry.Exclude {
			if len(validation.IsDNS1123Label(ns)) > 0 {
				continue
			}
			caps, holders, kueue := e.holders(ns)
			if kueue {
				continue
			}
			r := reason{holders: strings.Join(holders, ", ")}
			if len(caps) > 0 || b.inForce() {
				types := describeTypes(checked, func(name string) bool { return b.unbounded(name, caps) })
				if len(types) == 0 {
					continue
				}
				r.types = strings.Join(types, ", ")
			}
			if _, ok := byReason[r]; !ok {
				order = append(order, r)
			}
			if !slices.Contains(byReason[r], ns) {
				byReason[r] = append(byReason[r], ns)
			}
		}
		for _, r := range order {
			namespaces := byReason[r]
			switch {
			case r.types == "":
				warnings = append(warnings, fmt.Sprintf("entry[%d] (%q): the namespaces in exclude %v have no GPU limit: "+
					"no scope: cluster quota and no gpu-inventory limiter are declared, so the scaling manager does not bound them",
					i, entry.Name, namespaces))
			case r.holders == "":
				warnings = append(warnings, fmt.Sprintf("entry[%d] (%q): the namespaces in exclude %v have no GPU limit on [%s]: "+
					"no gpu-inventory limiter and no scope: cluster quota gives those accelerator types a number (-1 is unlimited)",
					i, entry.Name, namespaces, r.types))
			default:
				warnings = append(warnings, fmt.Sprintf("entry[%d] (%q): the namespaces in exclude %v have no GPU limit on [%s]: "+
					"the namespace quotas that hold them (%s) set those accelerator types to -1 (unlimited), "+
					"and no gpu-inventory limiter and no scope: cluster quota gives them a number",
					i, entry.Name, namespaces, r.types, r.holders))
			}
		}
	}
	return warnings
}

// holders returns the caps each namespace-scoped entry that does not exclude
// ns holds it to (QuotaForNamespace), those entries' quoted names, and whether
// one of them reads Kueue, whose caps come from Kueue at runtime.
func (e *QuotaLimiterEntries) holders(ns string) (caps []map[string]int, names []string, kueue bool) {
	for _, other := range e.Limiters {
		quotas, excluded := other.QuotaForNamespace(ns)
		if other.Scope != QuotaScopeNamespace || excluded {
			continue
		}
		if other.KueueEnabled() {
			kueue = true
		}
		caps = append(caps, quotas)
		names = append(names, fmt.Sprintf("%q", other.Name))
	}
	return caps, names, kueue
}

// checkedTypes is the finite set of accelerator names unboundedExcludeWarnings
// checks: every name an entry lists, the family of each, one product of each
// family that no entry lists, and "unknown". Sorted, and spellings that differ
// only in case are one name.
func (e *QuotaLimiterEntries) checkedTypes() []string {
	var listed []string
	for _, entry := range e.Limiters {
		listed = slices.AppendSeq(listed, maps.Keys(entry.ClusterQuotas))
		for _, perType := range entry.NamespaceQuotas {
			listed = slices.AppendSeq(listed, maps.Keys(perType))
		}
	}
	names := []string{constants.DefaultAcceleratorName}
	for _, name := range listed {
		names = append(names, name)
		if name == constants.DefaultAcceleratorName {
			continue
		}
		family := accelerator.Family(name)
		names = append(names, family)
		if product, ok := accelerator.UnlistedProduct(family); ok {
			names = append(names, product)
		}
	}
	slices.Sort(names)
	var out []string
	for _, n := range names {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
			out = append(out, n)
		}
	}
	return out
}

// describeTypes names the checked types that are unbounded, for a warning. A
// family whose every checked name is unbounded is named once, by its family
// name ("H100"). Otherwise each unbounded name is listed: a product by its
// name, the family name itself as "H100 (bare name)" (a workload whose
// selector names only the family), and the product standing for the ones no
// entry lists as "other H100 products".
func describeTypes(checked []string, unbounded func(string) bool) []string {
	var families []string
	members := map[string][]string{}
	for _, name := range checked {
		f := strings.ToLower(accelerator.Family(name))
		if _, ok := members[f]; !ok {
			families = append(families, f)
		}
		members[f] = append(members[f], name)
	}
	var out []string
	for _, f := range families {
		var hit []string
		for _, name := range members[f] {
			if unbounded(name) {
				hit = append(hit, name)
			}
		}
		if len(hit) == 0 {
			continue
		}
		if len(hit) == len(members[f]) {
			out = append(out, accelerator.Family(hit[0]))
			continue
		}
		for _, name := range hit {
			family := accelerator.Family(name)
			if product, ok := accelerator.UnlistedProduct(family); ok && name == product {
				name = "other " + family + " products"
			} else if accelerator.IsFamily(name) {
				name += " (bare name)"
			}
			out = append(out, name)
		}
	}
	return out
}

// excludeBounds is the cluster-wide bounds a namespace the namespace quotas
// exclude is planned under, read from the configuration.
type excludeBounds struct {
	// cluster is the caps of each scope: cluster entry.
	cluster []map[string]int
	// physical is whether a physical (gpu-inventory) limiter is declared.
	physical bool
}

// inForce reports whether any cluster-wide bound is declared. Without one a
// namespace no namespace quota holds has no limit on any type.
func (b excludeBounds) inForce() bool {
	return b.physical || len(b.cluster) > 0
}

// quotaBound reads a quota value as an allowlist bound: -1 is unlimited.
func quotaBound(v int) accelerator.Bound {
	return accelerator.Bound{Free: v, Unlimited: v == QuotaUnlimited}
}

// finite reports whether a cluster-wide bound gives the accelerator called
// name a number: the scope: cluster quotas together, read by
// accelerator.ClusterAllowlistsBound (a type one does not list is 0, except
// "unknown", and a family is bounded by the products that fit under all of
// them), or the physical limiter, which bounds every type a workload names
// and not "unknown".
func (b excludeBounds) finite(name string) bool {
	if b.physical && name != constants.DefaultAcceleratorName {
		return true
	}
	_, unlimited, bounds := accelerator.ClusterAllowlistsBound(b.cluster, name, quotaBound)
	return bounds && !unlimited
}

// unbounded reports whether a workload naming name, in a namespace the
// namespace quotas excluding it leave to caps (one per entry that holds it; none
// when no entry does), has no GPU limit. The caller has checked that some
// bound is in force.
//
// A namespace no entry holds is unbounded on a type no cluster-wide bound gives
// a number. "unknown" counts on its own only when a cluster quota lists it:
// otherwise a workload naming no type may land on any listed pool, and is
// unbounded exactly when one of those types is, which is named instead.
//
// A namespace an entry holds is a closed allowlist there: it is unbounded on a
// type only when every holder lists it as -1, and no cluster-wide bound gives
// it a number.
func (b excludeBounds) unbounded(name string, caps []map[string]int) bool {
	if b.finite(name) {
		return false
	}
	if len(caps) == 0 {
		if name != constants.DefaultAcceleratorName {
			return true
		}
		return slices.ContainsFunc(b.cluster, func(quotas map[string]int) bool {
			_, _, bounds := accelerator.ClusterAllowlistBound(quotas, name, quotaBound)
			return bounds
		})
	}
	if accelerator.ClosesFamily(caps, name) {
		// A family one holder lists only through products: the products that
		// fit under every holder and every cluster quota, as the planner reads
		// it (mergeNamespaceConstraints).
		_, unlimited, _ := accelerator.ClusterAllowlistsBound(slices.Concat(caps, b.cluster), name, quotaBound)
		return unlimited
	}
	for _, c := range caps {
		if _, unlimited, listed := accelerator.AllowlistBound(c, name, quotaBound); !listed || !unlimited {
			return false
		}
	}
	if accelerator.ClosesFamily(b.cluster, name) && accelerator.ListsProduct(caps, name) {
		// Every holder lists the family as -1, one lists a product of it too,
		// and the cluster quotas list the family only through products: the
		// family's GPUs are those products, each at what every holder and
		// cluster quota allows it, as the planner reads it.
		_, unlimited, _ := accelerator.ClusterAllowlistsBound(slices.Concat(caps, b.cluster), name, quotaBound)
		return unlimited
	}
	return true
}

// validateTypeQuotaMap checks per-accelerator-type entries: type name
// non-empty, value >= QuotaUnlimited. Empty maps are allowed (the
// limiter simply has no caps for that scope). Errors within a single map
// are accumulated with errors.Join so an operator with multiple bad rows
// sees them all at once.
func validateTypeQuotaMap(quotas map[string]int, location string) error {
	var errs []error
	for accType, value := range quotas {
		if strings.TrimSpace(accType) == "" {
			errs = append(errs, fmt.Errorf("%s: accelerator type name must not be empty", location))
			continue
		}
		if value < QuotaUnlimited {
			errs = append(errs, fmt.Errorf("%s[%q]: quota value %d is invalid; must be >= %d (only -1 is allowed as negative — means unlimited)",
				location, accType, value, QuotaUnlimited))
		}
		if value > MaxQuotaValue {
			errs = append(errs, fmt.Errorf("%s[%q]: quota value %d exceeds the maximum of %d",
				location, accType, value, MaxQuotaValue))
		}
	}
	return errors.Join(errs...)
}
