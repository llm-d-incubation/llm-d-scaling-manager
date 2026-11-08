package registry

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Trigger metadata keys. These are the whole per-variant configuration surface:
// KEDA delivers the trigger's `metadata` map verbatim as `scalerMetadata` on
// every RPC, so a value put there reaches the scaling manager without the scaling manager reading anything.
//
// They replace the llm-d.ai/* annotations. The `managed` annotation has no
// successor and needs none — KEDA only calls a scaler its trigger names, so
// being called is what being managed means.
const (
	// ModelIDKey identifies the model the variant serves. Required: it is the
	// grouping key for every multi-variant decision, and a variant that cannot be
	// attributed to a model cannot be optimized against one.
	ModelIDKey = "modelID"

	// VariantCostKey is the per-replica cost used by cost-aware optimization.
	// Optional; a non-negative decimal string.
	VariantCostKey = "variantCost"

	// ScalingPolicyKey names the scaling policy — a reusable TIER such as
	// "interactive", "standard" or "batch" — whose thresholds, analyzer selection
	// and scale-to-zero settings this variant scales under. Optional; when absent
	// the cluster default entry's defaultPolicy applies, and failing that the
	// default entry alone.
	//
	// The policy is named, not described, precisely so it is reusable: policies
	// carry no model identity, so one tier serves many models and changing the tier
	// changes all of them at once. That is what per-(model, namespace) config keys
	// cannot express — they bind settings to identity, so a fleet-wide change is an
	// edit per model.
	//
	// It rides trigger metadata because the trigger is already the registration:
	// selecting by policy name needs nothing watched, listed, or matched, unlike
	// resolving policy from an InferencePool's selector.
	ScalingPolicyKey = "scalingPolicy"

	// WarmPoolKey names the warm pool this variant may borrow from, when the
	// namespace holds more than one. Optional, and deliberately NOT boilerplate:
	// a namespace with a single pool needs no selection at all, because there is
	// nothing to disambiguate. Writing a pool name on every ScaledObject would
	// be ceremony in the case that is by far the most common.
	//
	// More than one pool is a real configuration rather than a hypothetical: a
	// warm copy is only reusable on the accelerator it was loaded on, so a
	// cluster with two GPU types needs a pool per type, and a tensor-parallel
	// variant needs Pods holding that many devices.
	//
	// When a namespace holds several pools and a variant names none, it gets no
	// warm copy and the scaling manager says so. Picking one for it would be a guess with a cost:
	// the wrong pool means a ~35 s load that can never serve.
	WarmPoolKey = "warmPool"

	// WarmPoolCopiesKey is how many warm copies of this variant the pool should
	// hold. Optional; a non-negative integer.
	//
	// Absent means AUTOMATIC, which is the default and the right answer for
	// almost everything: the pool decides from parking, popularity and miss
	// frequency, and holds at most one copy. Setting it takes that judgement
	// away, and is worth doing in two cases automatic mode cannot express.
	//
	//	"0"  never warm this variant -- it opts out of a pool it shares, freeing
	//	     the slot for models that gain more from it
	//	"1"  always keep one warm, whatever the pool's own ranking thinks
	//	"N"  keep N warm, so N scale-ups of this variant can bridge AT ONCE
	//
	// The last is the one automatic mode cannot do at all. A single warm copy
	// bridges a single scale-up, so a variant that scales twice in quick
	// succession takes a cold start for the second with free Pods sitting
	// beside it. It is also the only way to weight a shared pool toward one
	// model, since automatic mode holds one copy of each and no more.
	//
	// "1" is not the same as absent. Absent lets a quiet variant lose its slot
	// to a busier one; "1" pins it, which is what a low-traffic but
	// latency-critical model needs and what popularity ranking can never give.
	WarmPoolCopiesKey = "warmPoolCopies"

	// WarmPoolNameKey marks a ScaledObject as scaling a warm POOL rather than a
	// model, and names which pool it is.
	//
	// A pool is scaled the same way every other workload here is -- The scaling manager computes
	// a size, KEDA writes it -- which is what lets the controller stay read-only.
	// The alternative was a write permission on the pool Deployment, and
	// internal/controller/rbac.go is explicit that a licence to resize workloads
	// is the single permission a cluster admin is most right to refuse.
	//
	// Its presence is also what excuses this trigger from carrying a modelID: a
	// pool serves no model in particular, and every consumer that builds
	// variants skips these entries rather than inventing one.
	WarmPoolNameKey = "warmPoolName"

	// ScalerAddressKey is KEDA's own key naming this scaler's address. It is
	// consumed by KEDA, never by the scaling manager; named here only so it is not mistaken for a
	// scaling manager key when reading a trigger.
	ScalerAddressKey = "scalerAddress"

	// ReplicaStartSecondsKey seeds how long one replica of this variant takes to
	// become Ready.
	//
	// The demand floor projects the arriving queue forward over that time, so
	// the figure decides how much backlog a scale-up is sized for. A 0.6B model
	// and a GLM-5.2 differ by an order of magnitude -- 70 s against about 500 s
	// with a cold JIT cache -- which is too wide a range for any built-in
	// default to serve both.
	//
	// It is a SEED, not a setting: the saturation analyzer sizes a variant
	// with no start measured at it, blends it with the first starts measured,
	// and from the third distinct Pod uses the measurement alone
	// (saturation/start_est.go, learner PR 5c);
	// llmsm_replica_start_seconds_estimate carries a source label saying which
	// is in force.
	//
	// ParseMeta reads it as a plain decimal number of seconds in
	// [MinReplicaStartSeconds, MaxReplicaStartSeconds], 1 to 3600. Any other
	// value is ignored, as if unset, and never rejects the trigger: before it
	// was parsed any value was accepted, and a rejected trigger freezes the
	// model. The engine puts it on the analyzer input
	// (domain.VariantReplicaState.StartSeedSeconds), where the saturation
	// analyzer reads it, and a learner recording carries it.
	ReplicaStartSecondsKey = "replicaStartSeconds"

	// DefaultOutputTokensKey seeds how many tokens one request of this model
	// generates.
	//
	// The demand floor prices a request waiting in the router queue at the
	// generation length it will produce -- a queued request owes the decode role
	// that many tokens of work, and that is what makes a growing queue worth
	// ordering capacity for. Until a replica has COMPLETED a generation there is
	// no reading to price it with, and a queue priced at zero does not order:
	// measured on the shape-swap benchmark, 141 requests sat in the router queue
	// through a whole phase while decode's share of them was nothing.
	//
	// It is a SEED, not a setting, exactly like replicaStartSeconds: the fleet's
	// own average output length replaces it the moment one exists, so a figure of
	// the right order is what matters and an exact one buys nothing. The built-in
	// fallback is 512 tokens, a generic chat completion -- twelve times short of
	// this benchmark's 6000, which is why the key exists.
	//
	// It rides trigger metadata rather than the scaling-policy ConfigMap because
	// it is a fact about one workload's traffic, not a threshold a class of
	// workloads shares: a fleet-wide generation length is already what the
	// built-in fallback is. Optional; a non-negative whole number.
	//
	// A model's variants may each carry one -- there is a ScaledObject per role
	// under P/D -- and they are resolved to one figure for the model, because the
	// queue being priced is the model's. The LARGEST wins and a disagreement is
	// reported: under-pricing a queue is the failure this key exists to fix, so a
	// prefill trigger that says nothing must not drag decode's figure down.
	DefaultOutputTokensKey = "defaultOutputTokens"
)

// DefaultVariantCost matches the default the Variant type carries, so
// a trigger that omits variantCost ranks exactly where an unset one always has.
const DefaultVariantCost = "10.0"

// MinReplicaStartSeconds and MaxReplicaStartSeconds bound the
// replicaStartSeconds seed ParseMeta accepts. A second is below any start
// measured (the fastest, a snapshot restore, is about 15 s), so a smaller
// figure is a slip, such as a fraction of a minute. An hour is the bound the
// collector puts on a measured start (a Ready transition later than that is
// not read as a start).
const (
	MinReplicaStartSeconds = 1.0
	MaxReplicaStartSeconds = 3600.0
)

// Meta is the validated trigger metadata for one variant.
type Meta struct {
	// ModelID is the model this variant serves.
	ModelID string
	// VariantCost is the per-replica cost, as a decimal string. Kept in string
	// form because that is what variant.ConfigSpec holds; validated
	// here so no consumer has to.
	VariantCost string
	// ScalingPolicy names the reusable policy tier this variant scales under, or
	// is empty to take the cluster default.
	ScalingPolicy string
	// WarmPool names the warm pool this variant may borrow from, or is empty to
	// take the namespace's only pool.
	WarmPool string
	// WarmPoolName is set when this trigger scales a warm POOL rather than a
	// model. When it is set, nothing else here is meaningful.
	WarmPoolName string
	// WarmPoolCopies is how many warm copies to hold, or nil for automatic. A
	// POINTER because zero is a real setting -- never warm this -- and has to be
	// distinguishable from "not specified".
	WarmPoolCopies *int
	// DefaultOutputTokens is the seeded generation length, or 0 when the trigger
	// carries none. Not a pointer: zero and absent mean the same thing here --
	// there is no sense in which a request generates no tokens, so there is
	// nothing for an explicit 0 to express that absence does not.
	DefaultOutputTokens int
	// ReplicaStartSeconds is the seeded replica start time, in seconds, or 0
	// when the trigger carries none or one ParseMeta ignored. Zero is never a
	// seed: no replica becomes Ready in no time.
	ReplicaStartSeconds float64
	// ReplicaStartSecondsIgnored is the replicaStartSeconds value as written
	// when ParseMeta ignored it -- not a plain decimal number, or outside
	// [MinReplicaStartSeconds, MaxReplicaStartSeconds] -- and empty otherwise. A bad seed is not
	// a trigger error (see ReplicaStartSecondsKey); this is what lets the
	// caller say it was ignored.
	ReplicaStartSecondsIgnored string
}

// plainDecimal is a decimal number as an operator writes one: digits, an
// optional fraction and an optional exponent ("120", "15.5", "1e2"). It
// leaves out what strconv.ParseFloat also reads -- hex ("0x1p6"),
// underscores, NaN, Inf -- and surrounding space.
var plainDecimal = regexp.MustCompile(`^[+]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// parseReplicaStartSeconds reads a replicaStartSeconds value: the seed, or 0
// and false when raw is not a plain decimal number of seconds in
// [MinReplicaStartSeconds, MaxReplicaStartSeconds].
func parseReplicaStartSeconds(raw string) (float64, bool) {
	if !plainDecimal.MatchString(raw) {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < MinReplicaStartSeconds || v > MaxReplicaStartSeconds {
		return 0, false
	}
	return v, true
}

// ParseMeta validates a trigger's metadata.
//
// Errors name the key and the value, because the operator's only view of this is
// the scaling manager's log: KEDA does not surface a scaler's opinion of its own trigger metadata
// anywhere on the ScaledObject, so an unhelpful message here is the whole
// diagnostic.
func ParseMeta(metadata map[string]string) (Meta, error) {
	// A pool's trigger names no model because it serves none. Returned early so
	// the model-shaped validation below does not reject it, and so callers have
	// one field to test rather than a rule to remember.
	if poolName := metadata[WarmPoolNameKey]; poolName != "" {
		return Meta{WarmPoolName: poolName}, nil
	}

	modelID := metadata[ModelIDKey]
	if modelID == "" {
		return Meta{}, fmt.Errorf("trigger metadata %q is required and must not be empty; "+
			"a ScaledObject that scales a warm pool sets %q instead", ModelIDKey, WarmPoolNameKey)
	}

	cost := metadata[VariantCostKey]
	if cost == "" {
		cost = DefaultVariantCost
	}
	costVal, err := strconv.ParseFloat(cost, 64)
	if err != nil {
		return Meta{}, fmt.Errorf("trigger metadata %q must be a number, got %q: %w", VariantCostKey, cost, err)
	}
	if costVal < 0 {
		return Meta{}, fmt.Errorf("trigger metadata %q must be non-negative, got %v", VariantCostKey, costVal)
	}

	// Parsed here rather than at the point of use so a bad value is rejected
	// with the trigger that carried it, where the operator can act on it.
	var copies *int
	if raw := metadata[WarmPoolCopiesKey]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return Meta{}, fmt.Errorf("trigger metadata %q must be a non-negative whole number, got %q",
				WarmPoolCopiesKey, raw)
		}
		copies = &n
	}

	// Validated at the trigger that carried it, for the same reason
	// warmPoolCopies is: the operator's only view of a rejected value is this
	// error, so rejecting it later -- or defaulting around it -- would lose the
	// one diagnostic there is.
	outputTokens := 0
	if raw := metadata[DefaultOutputTokensKey]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return Meta{}, fmt.Errorf("trigger metadata %q must be a non-negative whole number of tokens, got %q",
				DefaultOutputTokensKey, raw)
		}
		outputTokens = n
	}

	// Ignored, never refused, unlike the keys above: before it was parsed any
	// value was accepted, and refusing the trigger now would freeze a model
	// over a figure nothing reads yet.
	var startSeconds float64
	var startIgnored string
	if raw := metadata[ReplicaStartSecondsKey]; raw != "" {
		if v, valid := parseReplicaStartSeconds(raw); valid {
			startSeconds = v
		} else {
			startIgnored = raw
		}
	}

	return Meta{
		ModelID:                    modelID,
		VariantCost:                cost,
		ScalingPolicy:              metadata[ScalingPolicyKey],
		WarmPool:                   metadata[WarmPoolKey],
		WarmPoolCopies:             copies,
		DefaultOutputTokens:        outputTokens,
		ReplicaStartSeconds:        startSeconds,
		ReplicaStartSecondsIgnored: startIgnored,
	}, nil
}

// ScalesAWarmPool reports whether a trigger's metadata scales a warm pool rather
// than a model. Callers that build variants must skip these: a pool has no
// model, no engine options and nothing to autoscale on demand for.
func ScalesAWarmPool(metadata map[string]string) bool {
	return metadata[WarmPoolNameKey] != ""
}

// Trigger metadata keys that tune a warm POOL. They appear only on a trigger
// that carries WarmPoolNameKey.
//
// The pool's configuration lives with its ScaledObject rather than on the
// Deployment that supplies its Pods, because a warm pool is a scaling manager concept that
// happens to have Pods rather than a workload scaling manager happens to manage. Nothing
// outside the scaling manager reads it, nothing outside the scaling manager creates it, and there is no sensible
// way to operate one except through the scaling manager -- so its declaration belongs with
// everything else the scaling manager is told, and the invariant stays whole: The scaling manager manages what
// it is CALLED about, with no exceptions.
//
// It also puts the reserve beside the envelope it has to fit inside. The rule
// that decides whether a pool can warm anything at all -- maxReplicaCount must
// exceed the reserve -- is now checkable within one object.
const (
	// WarmPoolSleepMinSizeKey is the floor on FREE Pods: the reserve kept for
	// the next spike. Admission draws on free-minus-reserve, so a reserve that
	// reaches maxReplicaCount leaves a budget of zero forever.
	WarmPoolSleepMinSizeKey = "warmPoolSleepMinSize"
	// WarmPoolMaxHoldKey bounds how long a borrowed Pod may serve before it is
	// returned regardless, so a scale-up that never arrives cannot turn the
	// reserve into permanent capacity for one variant. A Go duration.
	WarmPoolMaxHoldKey = "warmPoolMaxHold"
	// WarmPoolMinHoldKey FLOORS how long a borrowed Pod serves: it is not handed
	// back as excess until it has been lent this long, even once the ordinary
	// replicas report Ready.
	//
	// Ready is not the same as useful -- a replica that has just passed its
	// probe has an empty KV and prefix cache -- so returning the bridge at that
	// instant swaps a warm Pod for a cold one at the crossover. A Go duration;
	// absent or zero leaves the behaviour unchanged.
	//
	// It is bounded by warmPoolMaxHold, which always wins, and waived entirely
	// while another variant is short.
	WarmPoolMinHoldKey = "warmPoolMinHold"
	// WarmPoolRetainedKey makes the pool the workload's SERVING capacity rather
	// than a bridge over a scale-up.
	//
	// The difference is what happens to a Pod that is lent and still wanted. A
	// bridge is temporary by design: the ordinary replicas are starting, and the
	// hold timeout reclaims the Pod if they never arrive. A retained pool has no
	// ordinary replicas coming -- the model is too large to start on demand, at
	// 300+ seconds -- so the same timeout would hand the Pod back every
	// warmPoolMaxHold and take it again on the next pass, paying a drain, a
	// sleep and a wake each time, with a gap in service around each one.
	//
	// Set it and the hold timeout no longer applies: a lent Pod goes back when
	// the variant stops needing it, and not before.
	WarmPoolRetainedKey = "warmPoolRetained"
	// WarmPoolSwitchSpareKey is how little spare capacity a model must have
	// before a RETAINED pool will switch to it, as a PERCENT of its own supply.
	//
	// A percent rather than an absolute, because the pool compares models of
	// different sizes: ten thousand spare tokens means one thing on a model with
	// a hundred thousand and another on a model with eleven.
	//
	// A candidate below it only wins if the model currently awake is NOT also
	// below it. Switching between two models that are both short moves the
	// shortage and pays a drain and a wake to do it. Unset or 0 leaves only the
	// scale-up rule.
	WarmPoolSwitchSpareKey = "warmPoolSwitchSpareThreshold"
	// WarmPoolMinSwitchIntervalKey is the shortest time between two switches of
	// one retained pool. A Go duration; defaults to 10m.
	//
	// A switch drains the outgoing model's in-flight requests, sleeps it, and
	// wakes the incoming one. Without a floor, two models either side of the
	// threshold trade the GPUs back and forth and spend most of their time doing
	// neither's work.
	WarmPoolMinSwitchIntervalKey = "warmPoolMinSwitchInterval"
	// WarmPoolPreloadTopKey warms this many of the busiest variants without
	// waiting for each to miss first.
	WarmPoolPreloadTopKey = "warmPoolPreloadTop"
	// WarmPoolGPUMemoryUtilizationKey overrides --gpu-memory-utilization for
	// warm copies, trading KV cache for warm-set size.
	WarmPoolGPUMemoryUtilizationKey = "warmPoolGPUMemoryUtilization"
)

// PoolMeta is a warm pool's tuning, as its trigger declares it. Every field is
// optional; an absent one leaves the controller's own default in place.
type PoolMeta struct {
	Name         string
	SleepMinSize *int
	MaxHold      *time.Duration
	MinHold      *time.Duration
	// Retained makes the pool serving capacity rather than a bridge. See
	// WarmPoolRetainedKey.
	Retained *bool
	// SwitchSpareThreshold is the spare-capacity percent below which a retained
	// pool will switch to a model. See WarmPoolSwitchSpareKey.
	SwitchSpareThreshold *float64
	// MinSwitchInterval floors the time between two switches of one retained
	// pool. See WarmPoolMinSwitchIntervalKey.
	MinSwitchInterval    *time.Duration
	PreloadTop           *int
	GPUMemoryUtilization float64
}

// ParsePoolMeta reads a pool trigger's tuning.
//
// A value that does not parse is REFUSED rather than defaulted, and the whole
// trigger with it. A pool's knobs decide how many GPUs it holds and whether it
// can warm anything; silently ignoring one and carrying on would leave an
// operator looking at a number that is not in force anywhere.
func ParsePoolMeta(metadata map[string]string) (PoolMeta, error) {
	name := metadata[WarmPoolNameKey]
	if name == "" {
		return PoolMeta{}, fmt.Errorf("trigger metadata %q is required on a warm pool trigger", WarmPoolNameKey)
	}
	out := PoolMeta{Name: name}

	if raw := metadata[WarmPoolSleepMinSizeKey]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a non-negative whole number, got %q",
				WarmPoolSleepMinSizeKey, raw)
		}
		out.SleepMinSize = &n
	}
	if raw := metadata[WarmPoolMaxHoldKey]; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a positive duration such as 2m, got %q",
				WarmPoolMaxHoldKey, raw)
		}
		out.MaxHold = &d
	}
	if raw := metadata[WarmPoolMinHoldKey]; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a non-negative duration such as 90s, got %q",
				WarmPoolMinHoldKey, raw)
		}
		out.MinHold = &d
	}
	if raw := metadata[WarmPoolRetainedKey]; raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be true or false, got %q",
				WarmPoolRetainedKey, raw)
		}
		out.Retained = &v
	}
	if raw := metadata[WarmPoolSwitchSpareKey]; raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		// A PERCENT, so 0-100. Rejecting >100 catches the operator who wrote a
		// fraction: 0.2 meaning "20%" is a valid percent too, and reads as
		// "switch only when a model is down to its last fifth of a percent" --
		// which is silently never rather than obviously wrong.
		if err != nil || v < 0 || v > 100 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a percent between 0 and 100, got %q",
				WarmPoolSwitchSpareKey, raw)
		}
		out.SwitchSpareThreshold = &v
	}
	if raw := metadata[WarmPoolMinSwitchIntervalKey]; raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a non-negative duration such as 10m, got %q",
				WarmPoolMinSwitchIntervalKey, raw)
		}
		out.MinSwitchInterval = &d
	}
	if raw := metadata[WarmPoolPreloadTopKey]; raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a non-negative whole number, got %q",
				WarmPoolPreloadTopKey, raw)
		}
		out.PreloadTop = &n
	}
	if raw := metadata[WarmPoolGPUMemoryUtilizationKey]; raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 || v > 1 {
			return PoolMeta{}, fmt.Errorf("trigger metadata %q must be a fraction above 0 and at most 1, got %q",
				WarmPoolGPUMemoryUtilizationKey, raw)
		}
		out.GPUMemoryUtilization = v
	}
	return out, nil
}
