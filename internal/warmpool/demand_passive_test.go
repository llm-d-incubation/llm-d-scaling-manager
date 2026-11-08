package warmpool

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// passiveVariantName and passiveNamespace are the variant registerVariant
// registers.
const (
	passiveVariantName = "qwen"
	passiveNamespace   = "tenant"
)

// TestAPassiveVariantIsNotWarmed: the planner gets no demand for a passive
// variant, so it never plans a copy or a borrow for it. The same variant
// active is the control.
func TestAPassiveVariantIsNotWarmed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		passive bool
		want    int
	}{
		{name: "passive", passive: true, want: 0},
		{name: "active (control)", passive: false, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, reg, _ := demandFor(t, []client.Object{deployment(1, vllmContainer())}, &fakeDatastore{pool: inferencePool()})
			registerVariant(reg, scaleTarget)
			d.Passive = func(namespace, variant string) bool {
				return tc.passive && namespace == passiveNamespace && variant == passiveVariantName
			}
			got, err := d.Variants(context.Background())
			if err != nil {
				t.Fatalf("Variants: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("Variants = %+v, want %d", got, tc.want)
			}
		})
	}
}
