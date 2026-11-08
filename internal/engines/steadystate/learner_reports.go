package steadystate

import (
	"context"
	"maps"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learn"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

const (
	// learnerSeriesMissingMax is how long a variant's learner series outlive
	// its absence from the registry, whatever a read of its scale target
	// says. Without it a series whose target can never be read, or whose
	// ScaledObject was deleted and left its Deployment behind, would stay
	// for the life of the process. A day: longer than a KEDA outage is
	// expected to last, and short enough that a leaked series is seen to end.
	learnerSeriesMissingMax = 24 * time.Hour
	// learnerTargetCheckInterval is how often the scale targets of variants
	// missing from the registry are read. The client is the manager's cached
	// one, but that is not this code's to assume: at this rate an uncached
	// client costs one read per missing variant per minute, not per cycle.
	learnerTargetCheckInterval = time.Minute
)

// learnerVariantID names one variant that has learner series.
type learnerVariantID struct {
	namespace, modelID, name string
}

// The learners' reports reach the metrics here (analyzer-learner.md, 6.2 and
// 6.3): the analyzer logs, the engine publishes.

// learnerReporter is the part of a built-in analyzer that hands over its
// learners' reports, by namespace|model: the saturation analyzer's, and the
// slo and throughput analyzers' own lines.
type learnerReporter interface {
	TakeLearnerReports() map[string][]learn.Report
}

// publishLearnerReports publishes what the cycle's Analyze calls reported, at
// the end of the cycle.
func (e *Engine) publishLearnerReports() {
	for _, entry := range e.analyzers.builtins() {
		reporter, ok := entry.analyzer.(learnerReporter)
		if !ok {
			continue
		}
		reports := reporter.TakeLearnerReports()
		for _, model := range slices.Sorted(maps.Keys(reports)) {
			e.metricsEmitter.RecordLearnerReports(reports[model], e.learnerTarget)
		}
	}
}

// learnerTarget is the scale target the registry holds for a variant.
func (e *Engine) learnerTarget(namespace, variant string) metrics.LearnerTarget {
	if e.Variants == nil {
		return metrics.LearnerTarget{}
	}
	entry, _ := e.Variants.Get(namespace, variant)
	return metrics.LearnerTarget{Kind: entry.Target.Kind, Name: entry.Target.Name}
}

// retainLearnerSeries deletes, once a cycle, the learner series of every
// variant that is really gone. A series answers how old the evidence is, and
// that grows more interesting the longer a variant is quiet, so nothing that
// merely stops a variant being seen may take it. Not the active set: a variant
// at zero replicas is in no model group. Not the registry alone: a KEDA outage
// past registry.DefaultTTL empties it, and one trigger typo drops a variant
// from the known set, and an alert on the age would then resolve just when it
// should fire.
//
// So a variant missing from the known set goes only when its scale target does:
// the read says NotFound, or the target is being deleted. Any other answer
// keeps the series: a Forbidden is not an absence. learnerSeriesMissingMax ends
// the wait whatever the reads say.
func (e *Engine) retainLearnerSeries(ctx context.Context) {
	known := map[learnerVariantID]bool{}
	for _, va := range utils.KnownVariants(ctx, e.Variants) {
		known[learnerVariantID{va.Namespace, va.Spec.ModelID, va.Name}] = true
	}
	now := e.clock.Now()
	due := now.Sub(e.learnerTargetsCheckedAt) >= learnerTargetCheckInterval
	checked := false
	gone := map[learnerVariantID]bool{}
	missing := map[learnerVariantID]time.Time{}
	for _, v := range e.metricsEmitter.LearnerVariants() {
		id := learnerVariantID{v.Namespace, v.ModelID, v.Variant}
		if known[id] {
			continue
		}
		since, ok := e.learnerMissingSince[id]
		if !ok {
			since = now
		}
		missing[id] = since
		switch {
		case now.Sub(since) >= learnerSeriesMissingMax:
			gone[id] = true
		case due && v.Target.Name != "":
			checked = true
			gone[id] = e.scaleTargetGone(ctx, v)
		}
	}
	maps.DeleteFunc(missing, func(id learnerVariantID, _ time.Time) bool { return gone[id] })
	e.learnerMissingSince = missing
	if checked {
		e.learnerTargetsCheckedAt = now
	}
	e.metricsEmitter.RetainLearnerSeries(func(namespace, modelID, name string) bool {
		return !gone[learnerVariantID{namespace, modelID, name}]
	})
}

// scaleTargetGone reports whether a variant's scale target is really gone:
// NotFound, or being deleted. One read through the controller's client, with
// no backoff as scaletarget.FetchScaleTarget has: a retry here would hold up
// the cycle, and the next read is learnerTargetCheckInterval away. A kind it
// does not read, and every other error, are not an absence.
func (e *Engine) scaleTargetGone(ctx context.Context, v metrics.LearnerVariant) bool {
	var obj client.Object
	switch v.Target.Kind {
	case constants.DeploymentKind, "":
		obj = &appsv1.Deployment{}
	case constants.LeaderWorkerSetKind:
		obj = &lwsv1.LeaderWorkerSet{}
	default:
		return false
	}
	if e.client == nil {
		return false
	}
	err := e.client.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: v.Target.Name}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return true
	case err != nil:
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not read the scale target of a variant missing from the registry; keeping its learner series",
			"namespace", v.Namespace, "variant", v.Variant, "scaleTarget", v.Target.Name, "error", err.Error())
		return false
	}
	return !obj.GetDeletionTimestamp().IsZero()
}
