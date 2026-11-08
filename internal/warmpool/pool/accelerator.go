package pool

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// AcceleratorOf reports the GPU model a node carries, or "" if it declares none.
//
// Vendors are walked in REVERSE order and the first match wins, matching
// gpunodes.discoverNodeGPUTypes so a multi-vendor node resolves to the same
// accelerator here as it does in capacity accounting. Two answers for one node
// would be worse than either.
func AcceleratorOf(node *corev1.Node) string {
	for i := len(constants.VendorResources) - 1; i >= 0; i-- {
		vendor := constants.VendorResources[i]
		if model, ok := node.Labels[vendor.ProductLabel]; ok && model != "" {
			return model
		}
		for _, alias := range vendor.ProductLabelAliases {
			if model, ok := node.Labels[alias]; ok && model != "" {
				return model
			}
		}
	}
	return ""
}

// AcceleratorRequiredBy reports the GPU model a workload demands, or "" if it
// demands none in particular.
//
// Both forms are read, and reading only one is the trap. llm-d's modelservice
// chart pins the accelerator with nodeAffinity:
//
//	affinity:
//	  nodeAffinity:
//	    requiredDuringSchedulingIgnoredDuringExecution:
//	      nodeSelectorTerms:
//	      - matchExpressions:
//	        - key: nvidia.com/gpu.product
//	          operator: In
//	          values: [NVIDIA-H100-80GB-HBM3]
//
// and writes NO nodeSelector at all -- verified on a live deployment. A check
// that consulted nodeSelector alone would find nothing, conclude the workload
// accepts any accelerator, and never fire on the layout it exists to protect.
//
// Only a REQUIRED term counts. Preferred affinity is a hint the scheduler may
// ignore, so a warm copy placed on the strength of one could be wrong; and only
// a single-valued In or Equals is a requirement this can act on -- a term
// listing three acceptable models says the workload is portable between them,
// which is not a constraint the pool needs to enforce.
func AcceleratorRequiredBy(spec *corev1.PodSpec) string {
	for _, key := range acceleratorLabelKeys() {
		if model, ok := spec.NodeSelector[key]; ok && model != "" {
			return model
		}
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		return ""
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		return ""
	}
	keys := acceleratorLabelKeys()
	for _, term := range required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if len(expr.Values) != 1 {
				continue
			}
			if expr.Operator != corev1.NodeSelectorOpIn && expr.Operator != corev1.NodeSelectorOpNotIn {
				continue
			}
			if expr.Operator == corev1.NodeSelectorOpNotIn {
				continue // says what it will not take, not what it needs
			}
			for _, key := range keys {
				if expr.Key == key {
					return expr.Values[0]
				}
			}
		}
	}
	return ""
}

// gpusDeclaredBy reports how many devices a container asks for, and whether it
// asks for any, whichever vendor's resource names them.
//
// Reading nvidia.com/gpu alone -- which capacityOf did -- makes a pool on any
// other hardware read as holding nothing. A Pod with eight MI300Xs asks for
// amd.com/gpu, so no container looked like the one running engines: the fit
// check floored the Pod at a single device, and the pool's GPUs never reached
// the inventory the optimizer spends. Wrong in both directions, and silent on
// exactly the clusters where a warm pool is worth the most.
//
// Limits before requests, as a device-plugin resource is not overcommittable:
// the limit is what the Pod holds, and a container that sets only requests gets
// that number anyway.
//
// Vendors are walked in REVERSE order, the order AcceleratorOf walks them, so a
// Pod naming two vendors' resources resolves the same way its node does.
func gpusDeclaredBy(c *corev1.Container) (int64, bool) {
	for i := len(constants.VendorResources) - 1; i >= 0; i-- {
		name := corev1.ResourceName(constants.VendorResources[i].ResourceName)
		if q, ok := c.Resources.Limits[name]; ok {
			return q.Value(), true
		}
		if q, ok := c.Resources.Requests[name]; ok {
			return q.Value(), true
		}
	}
	return 0, false
}

// acceleratorLabelKeys is every node label key that names a GPU model.
func acceleratorLabelKeys() []string {
	var keys []string
	for i := len(constants.VendorResources) - 1; i >= 0; i-- {
		vendor := constants.VendorResources[i]
		keys = append(keys, vendor.ProductLabel)
		keys = append(keys, vendor.ProductLabelAliases...)
	}
	return keys
}

// AcceleratorMismatch reports whether a warm copy of a model that requires
// `wanted` would be useless in a Pod holding `held`, and why.
//
// Only a PROVEN mismatch blocks. An unknown on either side allows: the pool
// cannot read the node in an install without cluster RBAC, and refusing every
// admission there would disable the pool silently, which is the failure this
// whole configuration surface exists to stop. A warm copy on the wrong
// accelerator costs one wasted load; a pool that quietly warms nothing costs
// every GPU it holds, forever.
func AcceleratorMismatch(wanted, held string) (string, bool) {
	if wanted == "" || held == "" || wanted == held {
		return "", false
	}
	return "needs " + wanted + ", this Pod is on " + held, true
}
