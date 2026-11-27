package steadystate

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// Replicas a variant asked for that never arrive (issue #224).
//
// A node that advertises devices it cannot allocate takes a pod, refuses it at
// admission, and the ReplicaSet replaces it on the same node, over and over.
// The node's health is the platform's to fix: the device plugin, the GPU
// operator, node remediation. What the scaling manager owes is to not count
// those replicas as supply and to say why they are missing, read from the
// pods, so the person on call goes to the node and not to the scaler.
//
// Ready supply already counts only replicas that serve. What this adds is the
// report, and one change to anticipated supply: a pod not Ready after twice
// its role's known start time is no longer counted as on its way
// (withholdOverdueArrivals).

const (
	// notArrivingStartFactor times the role's start time is how long a
	// shortfall lasts before it is reported, and how old a starting pod is
	// before it stops counting as on its way. The start time is a running
	// estimate, so a start a little over it is ordinary; twice it is not.
	notArrivingStartFactor = 2.0
	// notArrivingFloor is the least that wait can be, so a fast-starting
	// model does not report a rollout in progress.
	notArrivingFloor = 5 * time.Minute
	// notArrivingEventEvery is how often the Warning event repeats while the
	// cause stays the same. A new cause is reported at once.
	notArrivingEventEvery = 15 * time.Minute
	// notArrivingClearHold is how long a reported variant must have what it
	// asked for before the report lifts.
	notArrivingClearHold = time.Minute
	// notArrivingUnknown is the reason when the variant's pods could not be
	// listed.
	notArrivingUnknown = "unknown"
)

// notArrivingTracker is the state the report keeps between cycles.
type notArrivingTracker struct {
	// variants is keyed namespace/variant.
	variants map[string]*notArrivingVariant
	// startByRole is each role's start time in seconds from the last cycle
	// that analysed its model (domain.AnalyzerResult.RoleStartSeconds),
	// keyed namespace/model/role. A role with neither a measurement nor a
	// seed is absent.
	startByRole map[string]float64
}

// notArrivingVariant is one variant whose ask is not met.
type notArrivingVariant struct {
	namespace, name string
	// since is when the shortfall was first seen, or when a replica last
	// arrived beyond maxReady while it lasted: no replica has arrived since.
	// Kept in memory only: after a restart a shortfall waits out the
	// threshold again.
	since time.Time
	// maxReady is the most Ready replicas seen while the shortfall lasted.
	maxReady int
	// metAt is when a reported shortfall was first seen met; zero while it
	// is not.
	metAt time.Time
	// reported is set while the shortfall is reported.
	reported bool
	// reason is the cause of the last Warning event.
	reason string
	// lastEvent is when the last Warning event was recorded.
	lastEvent time.Time
	// line is the variant's controller-status line.
	line string
}

func (e *Engine) notArrivingState() *notArrivingTracker {
	if e.notArriving == nil {
		e.notArriving = &notArrivingTracker{
			variants:    map[string]*notArrivingVariant{},
			startByRole: map[string]float64{},
		}
	}
	return e.notArriving
}

func startRoleKey(namespace, modelID, role string) string {
	return namespace + "/" + modelID + "/" + domain.CanonicalRole(role)
}

// noteRoleStarts keeps each analysed role's start time for the next cycle.
func (t *notArrivingTracker) noteRoleStarts(requests []allocation.ModelScalingRequest) {
	for _, req := range requests {
		if req.CompositeSignal.Result == nil {
			continue
		}
		for role, s := range req.CompositeSignal.Result.RoleStartSeconds {
			if s > 0 {
				t.startByRole[startRoleKey(req.Namespace, req.ModelID, role)] = s
			}
		}
	}
}

// threshold is how long a shortfall or a starting pod of a role may last
// before the replica is taken as not arriving. known is false when the role's
// start time is neither measured nor seeded; the default start time is used
// then.
func (t *notArrivingTracker) threshold(namespace, modelID, role string) (d time.Duration, known bool) {
	s, known := t.startByRole[startRoleKey(namespace, modelID, role)]
	if !known {
		s = saturation.DefaultReplicaStartSeconds
	}
	return max(notArrivingFloor, time.Duration(notArrivingStartFactor*s*float64(time.Second))), known
}

// withholdOverdueArrivals stops counting as on its way a pod that has not
// become Ready in twice its role's start time: it moves the pod's age out of
// PendingAges and counts it in StuckReplicas, which anticipated supply
// subtracts. Only when the role's start time is known: with only the default,
// a slow model that was never measured would have its starting replicas
// ordered twice.
//
// A LeaderWorkerSet reports no pod ages (variantmeta.pendingAgeSeconds), so
// its groups are left as they were.
func (e *Engine) withholdOverdueArrivals(ctx context.Context, modelID string, metas []domain.VariantMetadata) {
	if e.notArriving == nil {
		return
	}
	for i := range metas {
		m := &metas[i]
		if len(m.PendingAges) == 0 {
			continue
		}
		limit, known := e.notArriving.threshold(m.Namespace, modelID, m.Role)
		if !known {
			continue
		}
		kept := make([]float64, 0, len(m.PendingAges))
		overdue := 0
		for _, age := range m.PendingAges {
			if age > limit.Seconds() {
				overdue++
				continue
			}
			kept = append(kept, age)
		}
		if overdue == 0 {
			continue
		}
		m.PendingAges = kept
		m.StuckReplicas += overdue
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Starting pods past twice their start time are not counted as arriving",
			"namespace", m.Namespace, "variant", m.VariantName, "overdue", overdue, "afterSeconds", limit.Seconds())
	}
}

// reportReplicasNotArriving compares, for every variant of the cycle, what
// its scale target asks for with what is Ready, and reports a shortfall that
// has made no progress for longer than the threshold: a Warning event (on a
// new cause, then every notArrivingEventEvery), llmsm_replicas_not_arriving,
// and a line in the controller-status ConfigMap. A Normal event follows when
// the replicas arrive.
//
// scaleTargets is the cycle's scale targets by namespace/model, then by
// namespace/target; e.cycleScaleTargets, keyed namespace/target, is the
// fallback. A variant with neither is treated as gone: a scale target that
// was deleted must not leave its series asserting a shortfall forever.
func (e *Engine) reportReplicasNotArriving(
	ctx context.Context,
	modelGroups map[string][]llmdvariant.Variant,
	scaleTargets map[string]map[string]scaletarget.ScaleTargetAccessor,
	requests []allocation.ModelScalingRequest,
	now time.Time,
) {
	t := e.notArrivingState()
	t.noteRoleStarts(requests)
	seen := map[string]bool{}
	models := map[string]bool{}
	for _, modelVAs := range modelGroups {
		for i := range modelVAs {
			va := &modelVAs[i]
			models[va.Namespace+"/"+va.Spec.ModelID+"/"] = true
			targetKey := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
			st := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.Spec.ModelID)][targetKey]
			if st == nil {
				st = e.cycleScaleTargets[targetKey]
			}
			if st == nil {
				continue
			}
			seen[utils.GetNamespacedKey(va.Namespace, va.Name)] = true
			e.observeArrival(ctx, t, va, st, now)
		}
	}
	for key, v := range t.variants {
		if seen[key] {
			continue
		}
		if v.reported {
			metrics.ClearReplicasNotArriving(v.namespace, v.name)
		}
		delete(t.variants, key)
	}
	// A start time is kept only while its model is here: a model deleted and
	// created again must not inherit one and have its pods withheld.
	for key := range t.startByRole {
		i := strings.LastIndex(key, "/")
		if !models[key[:i+1]] {
			delete(t.startByRole, key)
		}
	}
	var lines []string
	for _, v := range t.variants {
		if v.reported {
			lines = append(lines, v.line)
		}
	}
	e.Status.SetReplicasNotArriving(lines)
}

// observeArrival updates one variant's shortfall and reports it.
func (e *Engine) observeArrival(
	ctx context.Context,
	t *notArrivingTracker,
	va *llmdvariant.Variant,
	st scaletarget.ScaleTargetAccessor,
	now time.Time,
) {
	key := utils.GetNamespacedKey(va.Namespace, va.Name)
	ask := int(utils.GetDesiredReplicas(st))
	ready := int(st.GetStatusReadyReplicas())
	v := t.variants[key]
	if ask <= ready {
		if v == nil {
			return
		}
		// The replicas must stay for notArrivingClearHold, reported or not,
		// or a replica that passes its probe once between crashes would lift
		// the report or restart the clock every time it did.
		if v.metAt.IsZero() {
			v.metAt = now
		}
		if now.Sub(v.metAt) < notArrivingClearHold {
			return
		}
		if v.reported {
			e.liftReport(va, v, ready, ask)
		}
		delete(t.variants, key)
		return
	}
	if v == nil {
		v = &notArrivingVariant{namespace: va.Namespace, name: va.Name, since: now, maxReady: ready}
		t.variants[key] = v
	}
	v.metAt = time.Time{}
	// The clock times no progress, not the shortfall: a replica arriving
	// beyond the most Ready this shortfall has seen restarts it, so a rollout
	// or a scale-up that lands one replica per start time is never reported,
	// and a replica that comes and goes is. A rising ask does not restart it,
	// or an optimizer ordering more into a storm would postpone the report
	// forever.
	if ready > v.maxReady {
		v.maxReady, v.since = ready, now
		if v.reported {
			e.liftReport(va, v, ready, ask)
		}
	}
	// The role the cycle's requests carry (variantRole), so the threshold
	// is the one noteRoleStarts filed for it.
	role := e.variantRole(va.Namespace, va.Name, st)
	limit, _ := t.threshold(va.Namespace, va.Spec.ModelID, role)
	if now.Sub(v.since) < limit {
		return
	}
	pods, err := variantmeta.ListPods(ctx, e.client, va.Namespace, st)
	cause := variantmeta.ArrivalCause{Reason: notArrivingUnknown}
	switch {
	case err != nil && v.reported:
		// A listing that failed once does not change what is reported.
		return
	case err != nil:
		cause.Detail = variantmeta.CutDetail("its pods could not be listed: " + err.Error())
	default:
		// Pods that ended count from one threshold before the clock started:
		// the rejections that started it, not last week's evictions. Pods on
		// their way count only once they are older than the threshold.
		cause = variantmeta.ExplainMissing(pods, v.since.Add(-limit), now.Add(-limit))
	}
	if cause.Reason == "" {
		// Every missing replica has a pod younger than the threshold, on its
		// way. A report already made stands: those pods have not served yet.
		// It lifts when a replica arrives, or changes if they stall.
		return
	}
	metrics.SetReplicasNotArriving(va.Namespace, va.Name, role, cause.Reason, ask-ready)
	v.line = notArrivingLine(va, role, ready, ask, v.since, cause)
	v.reported = true
	if cause.Reason != v.reason || now.Sub(v.lastEvent) >= notArrivingEventEvery {
		e.recordArrivalEvent(va, corev1.EventTypeWarning, constants.K8SEventReplicasNotArriving,
			notArrivingMessage(ready, ask, v.since, limit, cause))
		v.lastEvent, v.reason = now, cause.Reason
		ctrl.LoggerFrom(ctx).Info("Replicas asked for have not arrived",
			"namespace", va.Namespace, "variant", va.Name, "role", role, "ready", ready, "asked", ask,
			"noneArrivedSince", v.since.UTC().Format(time.RFC3339), "reason", cause.Reason, "pods", cause.Pods,
			"node", cause.Node, "detail", cause.Detail)
	}
}

// liftReport ends a variant's report because a replica arrived: its series
// goes, and a ReplicasArrived event says so. The event history is kept, so a
// report made again with the same cause inside notArrivingEventEvery is not
// announced twice.
func (e *Engine) liftReport(va *llmdvariant.Variant, v *notArrivingVariant, ready, ask int) {
	metrics.ClearReplicasNotArriving(va.Namespace, va.Name)
	v.reported = false
	e.recordArrivalEvent(va, corev1.EventTypeNormal, constants.K8SEventReplicasArrived,
		fmt.Sprintf("%d of %d replicas Ready: a replica arrived", ready, ask))
}

// recordArrivalEvent records straight on the recorder rather than through
// recordEvent, which keeps one event per variant per cycle: this report runs
// before the decisions, and would take the slot of the cycle's ScaledUp.
func (e *Engine) recordArrivalEvent(va *llmdvariant.Variant, eventType, reason, message string) {
	if e.Recorder == nil {
		return
	}
	e.Recorder.Event(llmdvariant.EventTarget(va), eventType, reason, message)
}

// notArrivingLine is a variant's controller-status line. It carries no age
// and no pod count, so the ConfigMap is rewritten only when the counts, the
// cause or the pods' message change, not every cycle.
func notArrivingLine(va *llmdvariant.Variant, role string, ready, ask int, since time.Time, c variantmeta.ArrivalCause) string {
	line := fmt.Sprintf("%s/%s (%s): %d/%d Ready, none arrived since %s: %s",
		va.Namespace, va.Name, role, ready, ask, since.UTC().Format(time.RFC3339), c.Reason)
	if c.Node != "" {
		line += " on node " + c.Node
	}
	if c.Detail != "" {
		line += ": " + c.Detail
	}
	return line
}

// notArrivingMessage is the Warning event's text: the shortfall, then the
// cause in the pods' words.
func notArrivingMessage(ready, ask int, since time.Time, limit time.Duration, c variantmeta.ArrivalCause) string {
	head := fmt.Sprintf("%d of %d replicas Ready and none has arrived since %s, longer than the %s a replica should take: ",
		ready, ask, since.UTC().Format(time.RFC3339), limit.Round(time.Second))
	on := ""
	if c.Node != "" {
		on = " on node " + c.Node
	}
	detail := ""
	if c.Detail != "" {
		detail = " (" + c.Detail + ")"
	}
	switch c.Reason {
	case variantmeta.NotArrivingAdmissionRejected:
		return head + fmt.Sprintf("%s rejected by the kubelet at admission%s%s. "+
			"The node offers resources it cannot allocate; its health is for the platform "+
			"(device plugin, GPU operator, node remediation) to fix. The scaling manager does not move pods off a node.",
			podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingUnschedulable:
		return head + fmt.Sprintf("%s unschedulable%s.", podCount(c.Pods), detail)
	case variantmeta.NotArrivingSchedulingGated:
		return head + fmt.Sprintf("%s held at a scheduling gate, such as Kueue admission%s.", podCount(c.Pods), detail)
	case variantmeta.NotArrivingImagePull:
		return head + fmt.Sprintf("%s cannot pull the image%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingCrashLoop:
		return head + fmt.Sprintf("%s crash-looping%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingContainerConfig:
		return head + fmt.Sprintf("%s whose container cannot be created%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingFailed:
		return head + fmt.Sprintf("%s failed%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingNotReady:
		return head + fmt.Sprintf("%s Running but not Ready%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingStarting:
		return head + fmt.Sprintf("%s still starting%s.", podCount(c.Pods), on)
	case variantmeta.NotArrivingNoPods:
		return head + "no pod exists for the missing replicas. The workload controller did not create them; " +
			"its FailedCreate events say why (often a ResourceQuota)."
	}
	return head + "the cause is not known" + detail + "."
}

// podCount is "1 pod" or "<n> pods".
func podCount(n int) string {
	if n == 1 {
		return "1 pod"
	}
	return fmt.Sprintf("%d pods", n)
}
