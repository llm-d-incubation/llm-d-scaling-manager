package variant

import "k8s.io/apimachinery/pkg/runtime"

// An event recorder takes a runtime.Object, and one caller still hands it a
// variant (scalefromzero); the others use EventTarget.
var (
	_ runtime.Object = (*Variant)(nil)
	_ runtime.Object = (*List)(nil)
)
