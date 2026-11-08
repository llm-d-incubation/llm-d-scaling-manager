// Package scaler implements KEDA's ExternalScaler gRPC service for the scaling manager.
//
// The scaling manager computes the desired replica count with its capacity model; this scaler
// delivers that decision to KEDA/HPA over the external-scaler contract, so KEDA
// actuates (no Prometheus round-trip, no prometheus-adapter). It resolves the
// scale target from the KEDA ScaledObject (namespace/name -> scaleTargetRef) and
// reads the scaling manager's latest decision from the in-memory store the actuator feeds
// (internal/decision).
package scaler

import (
	"context"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/passive"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// metricTargetSize is the target KEDA is told to divide by. One is the
// whole design: The scaling manager publishes the replica count it wants, so a target of
// 1 makes KEDA's arithmetic the identity and the HPA carries the scaling manager's figure
// rather than a ratio. Named because it appears twice -- in the response
// and in the log line that reports the response -- and two literals for
// one value is how a status message comes to disagree with the thing it
// describes.
const metricTargetSize = 1

// MetricName is the external metric the scaling manager exposes to KEDA/HPA. It is advertised
// with a target of 1, so HPA computes ceil(metricValue / 1) = metricValue and
// scales the target to exactly the scaling manager's desired replica count.
const MetricName = "scaling-manager-desired-replicas"

// activationTTL is the shortest time a positive decision is honoured without
// being refreshed. See Handler.honoursDecision and Handler.decisionTTL.
const activationTTL = 60 * time.Second

// scaleReader reads the scale subresource. The uncached API reader the manager
// hands out (manager.GetAPIReader) is a full client.Client and satisfies it,
// as does the fake client the tests use.
type scaleReader interface {
	client.SubResourceClientConstructor
	Scheme() *runtime.Scheme
}

// Handler implements pb.ExternalScalerServer.
type Handler struct {
	pb.UnimplementedExternalScalerServer
	// client MUST be an uncached reader (manager.GetAPIReader). A cached Get of a
	// ScaledObject lazily starts a cluster-wide LIST+WATCH informer for the kind
	// on first use — the watch this design exists to remove — and it would appear
	// the moment KEDA made its first call, with nothing in the code saying "watch".
	client client.Reader
	store  *decision.Store
	// registry is the set of workloads the scaling manager manages. Every RPC registers its ref
	// there — see observe.
	registry *registry.Registry
	// now is the clock used for decision freshness; overridden in tests.
	now func() time.Time
	// elected is closed once this replica holds the leader lease; nil means it
	// always does. Only the leader runs the engines that fill the store, so
	// before it closes the store's silence means "not the leader", not "no
	// decision yet" -- see leader.
	elected <-chan struct{}
	// interval is the engines' publish interval (GLOBAL_OPT_INTERVAL); nil
	// leaves decisionTTL at activationTTL. See WithDecisionInterval.
	interval func() time.Duration
	// undecided is the set of ScaledObjects currently answered without a
	// decision. See undecided and UndecidedTargets.
	undecided undecidedTargets
	// passive resolves which models are passive; nil is every model active.
	// See WithPassive.
	passive PassiveResolver
	// zeroRecheck is how often StreamIsActive re-reads a held variant it
	// reports inactive: PassiveZeroRecheck, shorter only in tests.
	zeroRecheck time.Duration
	// modes is each ScaledObject's passive mode as this handler last
	// answered it, so a change is logged once. See passiveMode.
	modes passiveModes
}

// NewHandler builds a Handler. A nil store falls back to decision.Default, and a
// nil reg to registry.Default.
//
// c must be uncached — see the client field.
func NewHandler(c client.Reader, store *decision.Store, reg *registry.Registry) *Handler {
	if store == nil {
		store = decision.Default
	}
	if reg == nil {
		reg = registry.Default
	}
	return &Handler{client: c, store: store, registry: reg, now: time.Now, zeroRecheck: PassiveZeroRecheck}
}

// WithElected makes the handler refuse decision-bearing calls until elected is
// closed (manager.Elected()). Nil leaves it answering as the leader, which is
// what a manager without leader election is. Returns h for chaining.
func (h *Handler) WithElected(elected <-chan struct{}) *Handler {
	h.elected = elected
	return h
}

// WithDecisionInterval tells the handler how often the engines republish a
// decision (config.OptimizationInterval), so a decision is not treated as stale
// between two ordinary publishes. Returns h for chaining.
func (h *Handler) WithDecisionInterval(interval func() time.Duration) *Handler {
	h.interval = interval
	return h
}

// UndecidedTargets returns how many ScaledObjects per namespace the scaler is
// currently answering without a decision (llmsm_scaler_undecided_targets). A
// ScaledObject leaves the count when a decided answer is given for it, when it
// is no longer registered, or undecidedStaleAfter after its last undecided
// answer.
func (h *Handler) UndecidedTargets() map[string]int {
	var live func(namespace, name string) bool
	if h.registry != nil {
		live = func(namespace, name string) bool {
			_, ok := h.registry.Get(namespace, name)
			return ok
		}
	}
	return h.undecided.counts(h.clock(), live)
}

// clock is h.now, or time.Now for a zero-value Handler (tests).
func (h *Handler) clock() time.Time {
	if h.now == nil {
		return time.Now()
	}
	return h.now()
}

// decisionTTL is how long a positive decision is honoured without being
// refreshed: activationTTL, or three publish intervals when that is longer. With
// GLOBAL_OPT_INTERVAL at or above activationTTL -- the OpenShift overlay ships
// 60 s -- a TTL of activationTTL alone expires decisions between ordinary
// publishes, and GetMetrics would then answer the current count in that gap.
//
// Three, not two: the engine loop is sliding (wait.UntilWithContext in
// internal/engines/executor), so the next cycle starts one interval after the
// previous one returns, and a variant is republished every interval plus the
// cycle's own duration (plus any retry backoff). Three intervals keep a healthy
// decision fresh through a cycle that takes up to twice the interval; two would
// call it stale after any cycle longer than one interval and pause a scale that
// is in flight.
func (h *Handler) decisionTTL() time.Duration {
	ttl := activationTTL
	if h.interval != nil {
		if three := 3 * h.interval(); three > ttl {
			ttl = three
		}
	}
	return ttl
}

// errNotLeader is what a replica that does not hold the lease answers with
// for anything that needs a decision. Unlike the no-decision case (see
// GetMetrics) it cannot answer the current count, because the leader may have
// decided something else, and two replicas answering differently would make
// the HPA follow whichever one KEDA's connection happens to reach.
//
// What KEDA does with it (v2.18.0): GetMetricsAndActivity returns the error
// (pkg/scalers/external_scaler.go), so the scale loop sees the trigger
// inactive and erroring (pkg/scaling/scale_handler.go getScaledObjectState),
// clears its scalers cache, and RequestScale (pkg/scaling/executor/
// scale_scaledobjects.go) sets Ready=False/TriggerError when there is no
// fallback and only logs when fallback.replicas is non-zero -- no scaling
// action in either case. The one exception is a ScaledObject someone wrote
// with fallback.replicas 0 and minReplicaCount 0, which that switch sends to
// scaleToZeroOrIdle after cooldownPeriod; the installer never writes that
// shape. On the HPA path the error counts as one failed read toward
// fallback.failureThreshold (pkg/fallback/fallback.go).
//
// KEDA does not look for the leader itself: it keeps one pooled connection
// per scalerAddress. The standby server closes connections after
// standbyConnectionAge, the client re-dials through the Service, and by then
// this replica is normally the leader -- the only supported reason to be here
// is the window between a pod becoming Ready and the outgoing pod's lease
// being released.
var errNotLeader = status.Error(codes.Unavailable,
	"this scaling manager replica does not hold the leader lease yet; decisions are made by the leader")

// leader reports whether this replica holds the lease. An empty decision store
// on a replica that does not is not "no decision yet" -- the leader may well
// have one -- so the calls that read the store check this first and refuse
// rather than answer 0.
func (h *Handler) leader() bool {
	if h.elected == nil {
		return true
	}
	select {
	case <-h.elected:
		return true
	default:
		return false
	}
}

// observe records that KEDA has asked about this ref. It is the discovery event:
// The scaling manager does not look for the workloads it manages, it manages the ones it is
// called about (docs/plans/engine/keda-driven-discovery.md).
//
// Called at the top of every RPC, including ones whose answer does not depend on
// the ref, because registration is the point and the answer is incidental. It
// never fails and never rejects: metadata that does not parse is still a
// registration — the enricher reports the bad trigger, once per change, as a
// log line and a Warning event on the ScaledObject, which is a far better
// diagnostic than a workload that silently never appears.
func (h *Handler) observe(ref *pb.ScaledObjectRef) {
	if ref == nil || h.registry == nil {
		return
	}
	h.registry.Observe(ref.GetNamespace(), ref.GetName(), ref.GetScalerMetadata())
}

// honoursDecision reports whether d still speaks for its target, or has gone
// stale and should be treated as "no opinion".
//
// The store is a latch: Set overwrites and nothing is ever removed, so without
// this a decision written once outlives whatever produced it. That is not
// hypothetical — a variant briefly running long enough for the saturation engine
// to publish "2" keeps isActive true forever after the target is scaled to zero,
// so the target is woken the instant KEDA asks, is never observed inactive, and
// NO engine can ever detect pending demand on it. Scale-from-zero is dead for
// that target until the process restarts.
//
// The rule is asymmetric, and the asymmetry is what makes it safe:
//
//   - "Keep this awake" (> 0) is a claim that must be kept current. Its
//     publishers normally re-publish well inside decisionTTL — the
//     scale-from-zero engine every 100ms while a queue is pending, the saturation
//     engine every optimize interval (GLOBAL_OPT_INTERVAL, default 15s) — so an
//     unrefreshed positive decision means nobody is making the claim any more,
//     and the honest answer is to look at the target instead (see
//     currentlyRunning). decisionTTL is at least three publish intervals, so a
//     deployment configuring GLOBAL_OPT_INTERVAL at or above activationTTL (the
//     OpenShift overlay ships 60s) does not expire decisions between publishes.
//
//   - "This should be asleep" (0) never expires. It is the resting state, and
//     expiring it would flip a target that is still draining back to active,
//     flapping against the scale-down that is already in flight.
//
// Note that expiry cannot strand a cold start: as soon as KEDA acts on an
// activation the target has replicas, so the currentlyRunning fallback answers
// "active" on its own. The activation decision only has to outlive KEDA's
// reaction, not the pod's startup.
//
// Scope: both the 0<->1 gate (isActive) and the replica count (GetMetrics) apply
// it, so the two never disagree about whether a decision exists. A stale
// decision in GetMetrics is answered like no decision at all -- the target's
// current replica count, counted under reason "stale" -- so the HPA holds
// rather than following a value nobody is publishing any more, and never
// collapses to 0.
func (h *Handler) honoursDecision(d decision.Decision) bool {
	if d.DesiredReplicas <= 0 {
		return true
	}
	return h.clock().Sub(d.UpdatedAt) <= h.decisionTTL()
}

// resolveTarget resolves the scale target of a ScaledObjectRef, cheapest
// source first.
//
//  1. the registry, if the enricher has already resolved this entry — the steady
//     state, and the reason this path costs no API traffic at all; and only then
//  2. an uncached read of the ScaledObject, for the window between a workload's
//     first call and its first enrichment pass.
//
// Callers resolve once per call and pass the result on, so answering with the
// current replica count costs one scale-subresource read on top of this.
//
// Step 1 matters because step 2 is uncached by necessity: making it cached would
// serve it from a cluster-wide informer, so without the registry hop every KEDA
// poll of every workload would be a real API request.
//
// The scale target is never taken from trigger metadata. ScaledObject.spec
// .scaleTargetRef is the authority on what a ScaledObject scales, and a second
// hand-written copy of that name can disagree with it -- silently attributing a
// variant's metrics to another workload. That is not hypothetical: cloning a
// ScaledObject to add a variant used to carry the field across and point the new
// entry at the original's Deployment.
func (h *Handler) resolveTarget(ctx context.Context, ref *pb.ScaledObjectRef) (registry.Target, error) {
	if ref == nil {
		return registry.Target{}, status.Error(codes.InvalidArgument, "scaledObjectRef is required")
	}
	if h.registry != nil {
		if e, ok := h.registry.Get(ref.GetNamespace(), ref.GetName()); ok && e.Target.Name != "" {
			return e.Target, nil
		}
	}
	var so kedav1alpha1.ScaledObject
	nn := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	if err := h.client.Get(ctx, nn, &so); err != nil {
		return registry.Target{}, status.Errorf(readErrorCode(err), "getting ScaledObject %s: %v", nn, err)
	}
	t := registry.TargetFromScaledObjectFor(&so, ref.GetScalerMetadata()[registry.ScalerAddressKey])
	if t.Name == "" {
		return registry.Target{}, status.Errorf(codes.FailedPrecondition, "ScaledObject %s has no scaleTargetRef.name", nn)
	}
	return t, nil
}

// readErrorCode maps a failed API read to the gRPC code that names its cause.
//
// KEDA shows the code on the trigger error, so it is the operator's first clue:
// NotFound means the object is gone; PermissionDenied means the scaling manager's own RBAC (or
// its credential) is the problem, which is never "not found"; anything else --
// a timeout, throttling, a refused connection -- is transient, and Unavailable
// is the retriable code KEDA simply asks again on.
func readErrorCode(err error) codes.Code {
	switch {
	case apierrors.IsNotFound(err):
		return codes.NotFound
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return codes.PermissionDenied
	default:
		return codes.Unavailable
	}
}

// decisionFor resolves the ref's scale target and returns it with the scaling manager's latest
// decision for it and whether one exists yet.
func (h *Handler) decisionFor(ctx context.Context, ref *pb.ScaledObjectRef) (registry.Target, decision.Decision, bool, error) {
	t, err := h.resolveTarget(ctx, ref)
	if err != nil {
		return registry.Target{}, decision.Decision{}, false, err
	}
	d, ok := h.store.Get(ref.Namespace, t.Name)
	return t, d, ok, nil
}

// GetMetricSpec advertises the scaling manager metric with a target of 1 so HPA scales the
// target to exactly the value GetMetrics returns.
//
// The response does not depend on the ref, but the call still registers it: KEDA
// asks for the spec when it starts managing a ScaledObject, which makes this the
// earliest notice the scaling manager gets that a workload exists.
//
// Answered whether or not this replica is the leader. It is the one call whose
// answer is static, and the answer KEDA gets when it reconciles a ScaledObject
// decides what metric the HPA carries -- refusing it there leaves the HPA on
// the Kubernetes CPU default until the next reconcile, which nothing may ever
// trigger (see Server.NeedLeaderElection). KEDA asks again on every poll --
// GetMetricSpecForScaling is a live gRPC call with no caching anywhere above
// it, driven by a pollingInterval timer and by the external-metrics path
// independently -- and those calls register the workload like any other.
func (h *Handler) GetMetricSpec(ctx context.Context, ref *pb.ScaledObjectRef) (*pb.GetMetricSpecResponse, error) {
	h.observe(ref)
	// KEDA builds the HPA's spec.metrics from this response. If the call never
	// arrives, the HPA is created with an empty metrics list, Kubernetes
	// defaults it to Resource/cpu, and the workload scales on a CPU metric a
	// cluster with no metrics-server cannot even evaluate -- while the
	// ScaledObject reports Ready=True and the scaling manager goes on publishing the correct
	// replica count every cycle. Nothing in the scaling manager's output distinguishes that
	// from working.
	//
	// Until this line, it was not distinguishable from the scaling manager's side at all:
	// IsActive logged every poll and this logged nothing, so the
	// inconsequential call was observable and the decisive one was not. Two CI
	// failures were investigated end to end without being able to answer "did
	// KEDA ever ask?", and the answer had to be reconstructed from the KEDA
	// operator's own log.
	//
	// V(1), beside IsActive, and NOT unconditionally: KEDA asks on every poll
	// (GetMetricSpecForScaling is uncached), and the samples here ship
	// pollingInterval: 5, so an unconditional line is one per ScaledObject per
	// poll forever at a level an operator cannot turn down. V(1) still reaches
	// them: the deployment passes no -v, so it runs at logging.DEFAULT, which
	// is 2.
	log.FromContext(ctx).V(1).Info("external scaler GetMetricSpec",
		"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(),
		"metricName", MetricName, "targetSize", metricTargetSize)
	return &pb.GetMetricSpecResponse{
		MetricSpecs: []*pb.MetricSpec{{MetricName: MetricName, TargetSize: metricTargetSize}},
	}, nil
}

// GetMetrics returns the scaling manager's desired replica count as the metric value.
//
// Before the first decision exists for the target -- every restart and every
// lease hand-off, until the new leader's first cycle publishes -- it answers
// the scale target's CURRENT replica count instead (see undecided). With a
// target of 1 the HPA then computes exactly the count it already has and
// holds. Neither alternative holds as cleanly:
//
//   - 0 is an answer the HPA acts on: it computes zero replicas, clamps to
//     minReplicas, and cuts every running fleet to its floor.
//   - An error is a trigger failure. KEDA's GetMetricsAndActivity returns on
//     a GetMetrics error before it ever calls IsActive
//     (pkg/scalers/external_scaler.go, v2.18.0), so the scale loop sees the
//     trigger inactive AND erroring. Without a fallback that is only a
//     TriggerError condition; with `fallback.replicas: 0` and minReplicaCount
//     0 it reaches scaleToZeroOrIdle (pkg/scaling/executor/
//     scale_scaledobjects.go, the isActive == false branch), which parks a
//     serving model once cooldownPeriod has passed, with no failureThreshold
//     in the way.
//
// Answering the current count keeps the activity KEDA reads truthful too,
// since its IsActive call now happens and reports the same current count.
// Only a target whose current count cannot be read is refused, with the code
// that names the read's failure.
//
// A decision older than decisionTTL is answered the same way (reason "stale"):
// isActive already treats it as no decision, and the two must agree.
//
// The answer is a replica COUNT, which holds only under the trigger's default
// metricType, AverageValue: the HPA computes ceil(value / 1), the count
// itself. Under metricType Value it computes ceil(currentReplicas * value / 1),
// so a decision of 3 on 3 replicas asks for 9, and the held count squares.
func (h *Handler) GetMetrics(ctx context.Context, req *pb.GetMetricsRequest) (*pb.GetMetricsResponse, error) {
	ref := req.GetScaledObjectRef()
	h.observe(ref)
	logger := log.FromContext(ctx)
	if !h.leader() {
		if h.standbyObserve(ctx, ref) {
			logger.V(1).Info("external scaler GetMetrics: passive (observe), answered before the leader lease is held",
				"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "metricName", MetricName, "value", 0)
			return &pb.GetMetricsResponse{
				MetricValues: []*pb.MetricValue{{MetricName: MetricName, MetricValue: 0}},
			}, nil
		}
		// At V(1), beside IsActive's line, because this is the poll path and
		// says the same thing every 5 s while a lease changes hands. It is
		// logged at all because the scaling manager used to refuse here in total silence: KEDA
		// recorded the refusal and the scaling manager did not, so the one component an
		// operator reads first had no trace of its own answer.
		logger.V(1).Info("external scaler GetMetrics refused: not the leader",
			"namespace", ref.GetNamespace(), "scaledObject", ref.GetName())
		return nil, errNotLeader
	}
	t, d, ok, err := h.decisionFor(ctx, ref)
	if err != nil {
		// The error goes back to KEDA, which logs it; this records that scaling manager is
		// where it came from.
		logger.V(1).Info("external scaler GetMetrics failed",
			"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "err", err)
		return nil, err
	}
	if mode := h.passiveMode(ctx, ref, t); mode.Passive() {
		value, err := h.passiveAnswer(ctx, ref, t, mode)
		if err != nil {
			logger.V(1).Info("external scaler GetMetrics failed: passive (hold) and the current replica count is unreadable",
				"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "err", err)
			return nil, err
		}
		h.undecided.clear(types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()})
		logger.V(1).Info("external scaler GetMetrics",
			"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(),
			"metricName", MetricName, "value", value, "decided", false, "passive", string(mode))
		return &pb.GetMetricsResponse{
			MetricValues: []*pb.MetricValue{{MetricName: MetricName, MetricValue: value}},
		}, nil
	}
	decided := ok && h.honoursDecision(d)
	value := int64(d.DesiredReplicas)
	if decided {
		key := types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()}
		if h.undecided.clear(key) {
			logger.V(1).Info("external scaler has a decision for this target again; answering it",
				"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "value", value)
		}
	} else {
		reason := constants.ScalerUndecidedNoDecision
		if ok {
			reason = constants.ScalerUndecidedStale
		}
		if value, err = h.undecidedAnswer(ctx, ref, t, reason); err != nil {
			logger.V(1).Info("external scaler GetMetrics failed: no usable decision and the current replica count is unreadable",
				"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "err", err)
			return nil, err
		}
	}
	// The VALUE KEDA actuates on. `decided` separates the scaling manager's answer from the
	// held current count, which otherwise look the same.
	logger.V(1).Info("external scaler GetMetrics",
		"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(),
		"metricName", MetricName, "value", value, "decided", decided)
	return &pb.GetMetricsResponse{
		MetricValues: []*pb.MetricValue{{MetricName: MetricName, MetricValue: value}},
	}, nil
}

// undecidedAnswer answers GetMetrics for target t with no usable decision: its
// current replica count, so the HPA holds. Each answer is counted under the
// reason the scaling manager has none (llmsm_scaler_undecided_answers_total), the ScaledObject is
// held in the undecided set (llmsm_scaler_undecided_targets) until a decided
// answer clears it, and the transition into the state is logged -- once per
// transition, not per poll, so a target that is decided and then loses its
// decision again is logged again.
func (h *Handler) undecidedAnswer(ctx context.Context, ref *pb.ScaledObjectRef, t registry.Target, reason string) (int64, error) {
	n, err := h.currentReplicas(ctx, ref, t)
	if err != nil {
		return 0, err
	}
	// A trigger ParseMeta rejects is skipped by every engine, so its target is
	// never decided -- unlike a fresh leader's, which is decided next cycle.
	if _, perr := registry.ParseMeta(ref.GetScalerMetadata()); perr != nil {
		reason = constants.ScalerUndecidedNotManaged
	}
	metrics.IncScalerUndecidedAnswer(ref.GetNamespace(), reason)
	logger := log.FromContext(ctx).V(1)
	key := types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()}
	// canLog: a disabled logger does not consume the transition's one line.
	if h.undecided.mark(key, reason, h.clock(), logger.Enabled()) {
		logger.Info("external scaler has no decision for this target; answering its current replica count so the HPA holds",
			"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(),
			"currentReplicas", n, "reason", reason)
	}
	return n, nil
}

// IsActive reports the target active unless the scaling manager has decided it needs zero
// replicas; before the first decision it mirrors the target's current replica
// count (see isActive).
//
// This is the poll path, used by a `type: external` trigger. A `type:
// external-push` trigger uses StreamIsActive instead, which reports the same
// predicate without waiting for a poll interval.
func (h *Handler) IsActive(ctx context.Context, ref *pb.ScaledObjectRef) (*pb.IsActiveResponse, error) {
	h.observe(ref)
	logger := log.FromContext(ctx)
	if !h.leader() {
		if h.standbyObserve(ctx, ref) {
			logger.V(1).Info("external scaler IsActive: passive (observe), answered before the leader lease is held",
				"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "active", false)
			return &pb.IsActiveResponse{Result: false}, nil
		}
		// Logged for the same reason GetMetrics' refusal is, and it was missed
		// there first: a refusal KEDA records and the scaling manager does not leaves the
		// component an operator reads first with no trace of its own answer.
		// Both handlers refuse identically, so logging one and not the other
		// just moved the blind spot.
		logger.V(1).Info("external scaler IsActive refused: not the leader",
			"namespace", ref.GetNamespace(), "scaledObject", ref.GetName())
		return nil, errNotLeader
	}
	active, err := h.isActive(ctx, ref)
	if err != nil {
		// The error goes back to KEDA, which logs it; this records that scaling manager is
		// where it came from.
		logger.V(1).Info("external scaler IsActive failed",
			"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "err", err)
		return nil, err
	}
	logger.V(1).Info("external scaler IsActive",
		"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "active", active)
	return &pb.IsActiveResponse{Result: active}, nil
}

// isActive is the shared activation predicate behind IsActive and
// StreamIsActive: active unless the scaling manager has decided the target needs zero replicas.
//
// Before the first decision exists the predicate falls back to the target's
// CURRENT replica count, which is the only honest answer: reporting active
// unconditionally would wake every workload parked at zero the moment KEDA
// first asks — defeating scale-to-zero and pre-empting the scale-from-zero
// engine, which never sees the workload as inactive. Reporting inactive
// unconditionally would be worse, scaling a running workload to zero before the scaling manager
// has looked at it. So: a target already at zero stays asleep until a decision
// (or the scale-from-zero engine) wakes it; a running target stays up.
//
// A decision that has gone stale takes the same fallback as no decision at all,
// so a target cannot be held awake by a value nobody is publishing any more —
// see honoursDecision.
func (h *Handler) isActive(ctx context.Context, ref *pb.ScaledObjectRef) (bool, error) {
	t, d, ok, err := h.decisionFor(ctx, ref)
	if err != nil {
		return false, err
	}
	return h.activeFor(ctx, ref, t, d, ok)
}

// activeFor is isActive for a target already resolved and its decision already
// read. Only a passive variant whose count cannot be read answers an error.
func (h *Handler) activeFor(ctx context.Context, ref *pb.ScaledObjectRef, t registry.Target, d decision.Decision, ok bool) (bool, error) {
	if mode := h.passiveMode(ctx, ref, t); mode.Passive() {
		return h.passiveActive(ctx, ref, t, mode)
	}
	if ok && h.honoursDecision(d) {
		return d.DesiredReplicas > 0, nil
	}
	return h.currentlyRunning(ctx, ref, t), nil
}

// currentlyRunning reports whether the scale target has replicas right now. It
// is only consulted without a usable decision. Any failure to read the target
// answers true — the safe direction, since a false negative would scale a
// running workload to zero on the strength of a failed lookup.
func (h *Handler) currentlyRunning(ctx context.Context, ref *pb.ScaledObjectRef, t registry.Target) bool {
	n, err := h.currentReplicas(ctx, ref, t)
	if err != nil {
		log.FromContext(ctx).V(1).Info("no decision yet and scale target unreadable; reporting active",
			"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "err", err)
		return true
	}
	return n > 0
}

// currentReplicas reads the desired replica count of scale target t from its
// scale subresource -- what KEDA and the HPA read, and the only place that
// knows a CRD's specReplicasPath. It is what the scaler answers with without a
// usable decision: IsActive turns it into "running or not", GetMetrics hands it
// to the HPA as is. A target with no readable scale subresource is an error,
// never a guessed 1. A failed read is a gRPC status error whose code names the
// cause (readErrorCode), for GetMetrics to pass on to KEDA.
func (h *Handler) currentReplicas(ctx context.Context, ref *pb.ScaledObjectRef, t registry.Target) (int64, error) {
	apiVersion := t.APIVersion
	if apiVersion == "" {
		apiVersion = "apps/v1" // KEDA's own default for scaleTargetRef
	}
	kind := t.Kind
	if kind == "" {
		kind = "Deployment" // KEDA's own default for scaleTargetRef
	}
	key := types.NamespacedName{Namespace: ref.GetNamespace(), Name: t.Name}
	sr, ok := h.client.(scaleReader)
	if !ok {
		return 0, status.Errorf(codes.Internal,
			"no decision yet, and the external scaler's reader (%T) cannot read the scale subresource of %s %s", h.client, kind, key)
	}
	var scale autoscalingv1.Scale
	if err := readScale(ctx, sr, schema.FromAPIVersionAndKind(apiVersion, kind), key, &scale); err != nil {
		return 0, status.Errorf(readErrorCode(err), "no decision yet, and reading the scale subresource of %s %s failed: %v", kind, key, err)
	}
	return int64(scale.Spec.Replicas), nil
}

// readScale reads the scale subresource of the gvk object at key into out. A
// kind the reader's scheme knows is read typed; any other kind (a CRD the scaling manager has
// no Go type for) is read unstructured and converted, since the controller-
// runtime client reads a subresource in the representation of its parent.
func readScale(ctx context.Context, sr scaleReader, gvk schema.GroupVersionKind, key types.NamespacedName, out *autoscalingv1.Scale) error {
	if typed, err := sr.Scheme().New(gvk); err == nil {
		if obj, ok := typed.(client.Object); ok {
			obj.SetNamespace(key.Namespace)
			obj.SetName(key.Name)
			return sr.SubResource("scale").Get(ctx, obj, out)
		}
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(key.Namespace)
	obj.SetName(key.Name)
	raw := &unstructured.Unstructured{}
	raw.SetGroupVersionKind(autoscalingv1.SchemeGroupVersion.WithKind("Scale"))
	if err := sr.SubResource("scale").Get(ctx, obj, raw); err != nil {
		return err
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(raw.Object, out)
}

// streamKeepalive bounds how long StreamIsActive stays silent. Activation is
// driven by decision-store wake-ups, so this only re-asserts the current state
// periodically — cheap insurance against a wake-up lost to a dropped stream or
// a decision written before KEDA opened it.
const streamKeepalive = 30 * time.Second

// StreamIsActive pushes activation to KEDA for a `type: external-push` trigger.
// It sends the current state immediately, then again whenever the target's
// decision changes it — so a workload sitting at zero is activated as soon as
// the decision lands, rather than up to one poll interval later. This is the
// path scale-from-zero rides: the engine that spots pending requests writes a
// non-zero decision, and that write wakes this stream.
//
// The stream lives until KEDA closes it (ctx cancellation), which returns nil —
// a closed stream is normal, not a failure. Errors resolving the scale target
// are returned, so KEDA surfaces a misconfigured ScaledObject instead of
// silently never activating.
func (h *Handler) StreamIsActive(ref *pb.ScaledObjectRef, stream pb.ExternalScaler_StreamIsActiveServer) error {
	ctx := stream.Context()
	logger := log.FromContext(ctx).WithValues(
		"namespace", ref.GetNamespace(), "scaledObject", ref.GetName())

	// Registered for the life of the stream rather than by timestamp. On a push
	// trigger this is the ONLY call a workload parked at zero ever receives —
	// KEDA does not poll IsActive, and the HPA does not query metrics for a
	// workload it is not scaling — so a TTL keyed on the last call would evict
	// exactly the entries whose purpose is to be woken from zero.
	if h.registry != nil && ref != nil {
		release := h.registry.Hold(ref.GetNamespace(), ref.GetName(), ref.GetScalerMetadata())
		defer release()
	}

	// Ended, not held, before the lease: the standby server that answers here
	// is stopped on election, and a stream parked here would hold that stop
	// open. Ended with OK rather than refused, deliberately: KEDA re-opens a
	// stream that closed cleanly after a steady 2 s, but backs off a failed
	// one 2, 4, 8 ... 60 s, so a refusal could leave a zero-parked workload
	// without its activation stream for a minute after election. The
	// registration above still happens, so the workload is known to the
	// engines the moment they start.
	if !h.leader() {
		// V(3): KEDA reopens every 2 s per parked workload for the whole wait
		// on the lease, and the default verbosity ships V(1) and V(2).
		logger.V(3).Info("external scaler StreamIsActive ended before the leader lease is held; KEDA will reopen it")
		return nil
	}

	// Resolve the target once, up front: it is fixed for the life of the
	// ScaledObject, and subscribing needs the resolved name.
	t, err := h.resolveTarget(ctx, ref)
	if err != nil {
		return err
	}
	updates, unsubscribe := h.store.Subscribe(ref.GetNamespace(), t.Name)
	defer unsubscribe()
	// target is the latest enrichment of the target resolved above, when there
	// is one: passive mode turns on what else the ScaledObject carries, which
	// can change while the stream is open. The target's name cannot.
	target := func() registry.Target {
		if h.registry != nil {
			if e, found := h.registry.Get(ref.GetNamespace(), ref.GetName()); found && e.Target.Name == t.Name {
				return e.Target
			}
		}
		return t
	}
	// activeNow re-reads the decision for the target resolved above; the target
	// itself does not move for the life of the stream.
	activeNow := func() (bool, error) {
		d, ok := h.store.Get(ref.GetNamespace(), t.Name)
		return h.activeFor(ctx, ref, target(), d, ok)
	}

	// Send the opening state before waiting on anything, so KEDA is never left
	// holding a stream that has said nothing.
	// An error ends the stream: only a passive variant whose replica count
	// cannot be read answers one, and reporting it active would wake a target
	// parked at zero on the strength of a failed read. KEDA reopens the stream.
	active, err := activeNow()
	if err != nil {
		logger.V(1).Info("external scaler StreamIsActive failed", "err", err)
		return err
	}
	if err := stream.Send(&pb.IsActiveResponse{Result: active}); err != nil {
		return err
	}
	logger.V(1).Info("external scaler StreamIsActive opened", "active", active)

	ticker := time.NewTicker(streamKeepalive)
	defer ticker.Stop()
	// A held variant answers its live count, which an operator changes by
	// hand; a decision-store wake-up does not follow that. While the stream
	// says inactive, a held variant is re-read this often, so a kubectl scale
	// from zero is reported active before KEDA's cooldown scales it back. The
	// read is the scale subresource through h.client, which is uncached by
	// design (see the field): one GET per tick, and only for a held variant
	// at zero with its stream open. Observe never reads a count, so it is not
	// re-read.
	recheck := time.NewTicker(h.zeroRecheck)
	defer recheck.Stop()

	// push sends next when it changes what KEDA was told.
	push := func(next bool) error {
		if next == active {
			return nil // nothing for KEDA to act on
		}
		active = next
		if err := stream.Send(&pb.IsActiveResponse{Result: active}); err != nil {
			return err
		}
		logger.V(1).Info("external scaler pushed activation change", "active", active)
		return nil
	}
	// keepalive re-asserts the current state whether or not it changed.
	keepalive := func(next bool) error {
		changed := next != active
		active = next
		if err := stream.Send(&pb.IsActiveResponse{Result: active}); err != nil {
			return err
		}
		if changed {
			logger.V(1).Info("external scaler pushed activation change", "active", active)
		}
		return nil
	}
	for {
		send := push
		select {
		case <-ctx.Done():
			logger.V(1).Info("external scaler StreamIsActive closed")
			return nil
		case <-updates:
		case <-ticker.C:
			send = keepalive
		case <-recheck.C:
			if !recheckDue(active, h.passiveMode(ctx, ref, target())) {
				continue
			}
		}
		next, err := activeNow()
		if err != nil {
			logger.V(1).Info("external scaler StreamIsActive failed", "err", err)
			return err
		}
		if err := send(next); err != nil {
			return err
		}
	}
}

// recheckDue reports whether the stream's recheck re-reads a variant: only a
// held one it reports inactive. Observe answers no count, and an active held
// variant is re-asserted by the keepalive.
func recheckDue(active bool, mode passive.Mode) bool {
	return !active && mode == passive.Hold
}

// PassiveZeroRecheck is how often StreamIsActive re-reads a held variant it
// is reporting inactive (Handler.zeroRecheck). See the recheck ticker there.
const PassiveZeroRecheck = 5 * time.Second
