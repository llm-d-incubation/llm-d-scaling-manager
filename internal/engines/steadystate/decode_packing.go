package steadystate

import (
	"fmt"
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// What a TPOT objective (slo.tpot) made of each decode variant's packing
// (issue #316). The saturation analyzer holds a decode replica's k2 where its
// own ITL line reaches the target (k2-batch-held-at-itl-target), and says so
// per variant (saturation.TakeDecodePacking). This publishes it as
// llmsm_decode_packing_bound and llmsm_decode_packing_k2_tokens, and records
// one DecodePackingBoundInert Warning when a variant with a TPOT target is
// not bounded by it for decodePackingInertHold.

// decodePackingInertHold is how long a TPOT target must bound nothing before
// the Warning is recorded. A decode variant has no line of its own for its
// first minutes, and a line borrowed from a sibling while it learns one is
// ordinary; a bound still inert after this is not warming up.
const decodePackingInertHold = 15 * time.Minute

// decodePackingModel is what reportDecodePacking keeps for one model between
// cycles.
type decodePackingModel struct {
	namespace, modelID string
	// published is the variants whose series were published last cycle.
	published map[string]struct{}
	// inert is each variant whose TPOT target bounds nothing, keyed by
	// variant name.
	inert map[string]*decodePackingInert
}

// decodePackingInert is one variant's inert episode.
type decodePackingInert struct {
	// reason is why the bound does not apply (domain.DecodePacking.Reason)
	// on the last cycle; since is when the episode started.
	reason string
	since  time.Time
	// reported is whether the Warning was recorded for this episode.
	reported bool
}

// reportDecodePacking publishes the decode packing series for one model
// from what the saturation analyzer made of each decode variant's k2 on this
// cycle (saturation.TakeDecodePacking), deletes those of variants it no
// longer names, and records the inert-bound Warning. now is the cycle's
// time; variants are the model's Variants, for the Event's target.
func (e *Engine) reportDecodePacking(namespace, modelID string, packing map[string]domain.DecodePacking,
	variants map[string]*llmdvariant.Variant, now time.Time) {
	if e.decodePacking == nil {
		e.decodePacking = map[string]*decodePackingModel{}
	}
	modelKey := utils.GetNamespacedKey(namespace, modelID)
	state := e.decodePacking[modelKey]
	if state == nil {
		state = &decodePackingModel{namespace: namespace, modelID: modelID,
			published: map[string]struct{}{}, inert: map[string]*decodePackingInert{}}
		e.decodePacking[modelKey] = state
	}
	current := map[string]struct{}{}
	for _, variant := range slices.Sorted(maps.Keys(packing)) {
		p := packing[variant]
		current[variant] = struct{}{}
		e.metricsEmitter.RecordDecodePacking(namespace, modelID, variant, p)
		if !p.Inert() {
			delete(state.inert, variant)
			continue
		}
		// One episode while the bound stays inert, whatever the reason: a
		// reason that alternates (borrowed-line, then no-line) must not
		// restart the hold, nor re-arm the Warning. The message names the
		// reason in force when it is recorded.
		ep := state.inert[variant]
		if ep == nil {
			ep = &decodePackingInert{since: now}
			state.inert[variant] = ep
		}
		ep.reason = p.Reason
		if ep.reported || now.Sub(ep.since) < decodePackingInertHold {
			continue
		}
		if va := variantNamed(variants, namespace, variant); va != nil {
			e.recordDecodePackingInert(va, p.Reason)
		}
		ep.reported = true
	}
	for variant := range state.published {
		if _, still := current[variant]; !still {
			e.metricsEmitter.DeleteDecodePacking(namespace, modelID, variant)
		}
	}
	for variant := range state.inert {
		if _, still := current[variant]; !still {
			delete(state.inert, variant)
		}
	}
	state.published = current
}

// forgetDecodePacking deletes one model's decode packing series and its
// inert episodes: the model was not priced this cycle (onNothingObserved).
func (e *Engine) forgetDecodePacking(namespace, modelID string) {
	modelKey := utils.GetNamespacedKey(namespace, modelID)
	if _, ok := e.decodePacking[modelKey]; !ok {
		return
	}
	e.metricsEmitter.DeleteDecodePackingForModel(namespace, modelID)
	delete(e.decodePacking, modelKey)
}

// variantNamed is the Variant named name in namespace, or nil.
func variantNamed(variants map[string]*llmdvariant.Variant, namespace, name string) *llmdvariant.Variant {
	for _, va := range variants {
		if va != nil && va.Name == name && va.Namespace == namespace {
			return va
		}
	}
	return nil
}

// recordDecodePackingInert records the DecodePackingBoundInert Warning on
// the variant's ScaledObject. Straight on the recorder, as
// recordArrivalEvent: it is not a decision, and must not take the cycle's
// one decision event.
func (e *Engine) recordDecodePackingInert(va *llmdvariant.Variant, reason string) {
	if e.Recorder == nil {
		return
	}
	e.Recorder.Event(llmdvariant.EventTarget(va), corev1.EventTypeWarning, constants.K8SEventDecodePackingBoundInert,
		decodePackingInertMessage(reason))
}

// decodePackingInertMessage is the Warning's message for reason: what the
// TPOT target does not do, why, and what bounds decode meanwhile.
func decodePackingInertMessage(reason string) string {
	why := map[string]string{
		"no-line":            "the variant has no ITL line of its own yet; it fits one once its replicas run across the ITL band",
		"borrowed-line":      "its ITL line is borrowed from another variant until it fits its own across the ITL band",
		"one-sided-line":     "its ITL line is held one-sided at a prior until its replicas run across the ITL band",
		"above-band-line":    "its ITL line was fitted above the ITL band (analyzerTuning.itlBand)",
		"default-intercept":  "its ITL line rests on the default intercept, not one it measured at light load",
		"per-step-line":      "its ITL line is per engine step on a speculating engine, not per token",
		"target-unreachable": "the target is at or under the ITL its line reads at an empty cache, so no number of replicas meets it; raise slo.tpot",
		"bad-band":           "the ITL band (analyzerTuning.itlBand) is not a range inside (0, 1]",
		"no-kv-capacity":     "its replicas report no KV cache capacity, so there is no fill to hold k2 at",
	}[reason]
	if why == "" {
		why = "read the k2-itl-bound line at --v=3"
	}
	return fmt.Sprintf("slo.tpot is set but does not bound decode packing (%s): %s. Decode is sized as with no TPOT "+
		"target and its TPOT may run above the target. Meanwhile lower the decode's --max-num-seqs, or hold replicas "+
		"with minReplicaCount", reason, why)
}
