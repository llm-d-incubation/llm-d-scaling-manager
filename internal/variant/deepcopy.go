package variant

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out.
func (in *ActuationStatus) DeepCopyInto(out *ActuationStatus) {
	*out = *in
}

// DeepCopy creates a new ActuationStatus copy of the receiver.
func (in *ActuationStatus) DeepCopy() *ActuationStatus {
	if in == nil {
		return nil
	}
	out := new(ActuationStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *OptimizedAlloc) DeepCopyInto(out *OptimizedAlloc) {
	*out = *in
	in.LastRunTime.DeepCopyInto(&out.LastRunTime)
	if in.NumReplicas != nil {
		in, out := &in.NumReplicas, &out.NumReplicas
		*out = new(int32)
		**out = **in
	}
}

// DeepCopy creates a new OptimizedAlloc copy of the receiver.
func (in *OptimizedAlloc) DeepCopy() *OptimizedAlloc {
	if in == nil {
		return nil
	}
	out := new(OptimizedAlloc)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *ConfigSpec) DeepCopyInto(out *ConfigSpec) {
	*out = *in
}

// DeepCopy creates a new variant.ConfigSpec copy of the receiver.
func (in *ConfigSpec) DeepCopy() *ConfigSpec {
	if in == nil {
		return nil
	}
	out := new(ConfigSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *Spec) DeepCopyInto(out *Spec) {
	*out = *in
	out.ScaleTargetRef = in.ScaleTargetRef
	if in.MinReplicas != nil {
		in, out := &in.MinReplicas, &out.MinReplicas
		*out = new(int32)
		**out = **in
	}
	out.ConfigSpec = in.ConfigSpec
}

// DeepCopy creates a new variant.Spec copy of the receiver.
func (in *Spec) DeepCopy() *Spec {
	if in == nil {
		return nil
	}
	out := new(Spec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *Status) DeepCopyInto(out *Status) {
	*out = *in
	in.DesiredOptimizedAlloc.DeepCopyInto(&out.DesiredOptimizedAlloc)
	out.Actuation = in.Actuation
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a new variant.Status copy of the receiver.
func (in *Status) DeepCopy() *Status {
	if in == nil {
		return nil
	}
	out := new(Status)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *Variant) DeepCopyInto(out *Variant) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy creates a new Variant copy of the receiver.
func (in *Variant) DeepCopy() *Variant {
	if in == nil {
		return nil
	}
	out := new(Variant)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *Variant) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *List) DeepCopyInto(out *List) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Variant, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a new variant.List copy of the receiver.
func (in *List) DeepCopy() *List {
	if in == nil {
		return nil
	}
	out := new(List)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *List) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
