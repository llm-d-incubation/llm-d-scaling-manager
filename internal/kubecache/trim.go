// Package kubecache trims what the manager's informer cache keeps of each object.
//
// On a cluster-scoped install the cached client lists Pods, Deployments and
// Nodes cluster-wide: the GPU usage walk counts every Pod on a GPU node, and
// discovery reads arbitrary scale targets, so none of those informers can be
// narrowed by a label selector without losing objects a reader needs. What can
// go is the part of each object no reader looks at, and on a large cluster that
// is most of it: managedFields on every object, kubectl's last-applied copy of
// the whole spec in an annotation, and the container image list on every Node.
package kubecache

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LastAppliedAnnotation is the annotation kubectl apply stores a full copy of
// the applied object in.
const LastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// Trim installs the transforms on opts: every cached object loses its
// managedFields and its last-applied annotation, and a Node also loses its
// status.images. No reader in this controller uses any of them; a reader that
// needs one must read through the API reader instead.
//
// A ByObject entry for Node that opts already holds keeps its settings and
// gains the transform only if it has none.
func Trim(opts *cache.Options) {
	opts.DefaultTransform = StripUnused
	if opts.ByObject == nil {
		opts.ByObject = map[client.Object]cache.ByObject{}
	}
	for obj, by := range opts.ByObject {
		if _, ok := obj.(*corev1.Node); ok {
			if by.Transform == nil {
				by.Transform = StripNode
				opts.ByObject[obj] = by
			}
			return
		}
	}
	opts.ByObject[&corev1.Node{}] = cache.ByObject{Transform: StripNode}
}

// StripUnused drops managedFields and the last-applied annotation. It is a
// toolscache.TransformFunc, so it is handed what the informer stores, including
// tombstones, which it returns unchanged.
func StripUnused(in any) (any, error) {
	obj, err := meta.Accessor(in)
	if err != nil {
		return in, nil
	}
	// Only written when present: setting nil managedFields on an object that
	// has none trips kubernetes/kubernetes#124337, as controller-runtime notes.
	if obj.GetManagedFields() != nil {
		obj.SetManagedFields(nil)
	}
	if ann := obj.GetAnnotations(); ann != nil {
		if _, ok := ann[LastAppliedAnnotation]; ok {
			delete(ann, LastAppliedAnnotation)
			obj.SetAnnotations(ann)
		}
	}
	return in, nil
}

// StripNode is StripUnused plus status.images, the largest part of a Node and
// one nothing here reads: Node readers use labels, allocatable, taints,
// unschedulable and conditions.
func StripNode(in any) (any, error) {
	if node, ok := in.(*corev1.Node); ok {
		node.Status.Images = nil
	}
	return StripUnused(in)
}

var (
	_ toolscache.TransformFunc = StripUnused
	_ toolscache.TransformFunc = StripNode
)
