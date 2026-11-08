package domain

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
)

// PodPlacement is the node placement a variant's pod template asks for: the
// parts the scheduler enforces whatever flavor Kueue later adds. The
// optimizer's node view intersects it with a flavor's own placement when it
// fits a borrowed replica, so a replica pinned by its own nodeSelector to a
// pool the flavor's nodes are not in is not counted as placed. Tolerations are
// not part of it: node fit reads them for every replica
// (VariantMetadata.Tolerations).
type PodPlacement struct {
	// NodeSelector is the template's spec.nodeSelector.
	NodeSelector map[string]string
	// RequiredNodeAffinity is the template's
	// requiredDuringSchedulingIgnoredDuringExecution node affinity; nil when
	// it has none.
	RequiredNodeAffinity *corev1.NodeSelector
}

// PlacementOf returns the placement of one pod spec, or nil when the spec
// constrains nothing.
func PlacementOf(spec *corev1.PodSpec) *PodPlacement {
	if spec == nil {
		return nil
	}
	p := PodPlacement{NodeSelector: maps.Clone(spec.NodeSelector)}
	if a := spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		p.RequiredNodeAffinity = a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.DeepCopy()
	}
	if len(p.NodeSelector) == 0 && p.RequiredNodeAffinity == nil {
		return nil
	}
	return &p
}
