package steadystate

import (
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The seed travels trigger metadata -> registry -> resolved policy. It does NOT
// travel the synthetic Variant spec, because that CR is being
// removed; these specs pin the hop that replaces it.
var _ = Describe("modelOutputSeed", func() {

	variant := func(ns, name string) llmdvariant.Variant {
		return llmdvariant.Variant{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		}
	}

	// A model under P/D has one ScaledObject per role, so "the model's variants"
	// is two registry entries that must resolve to one figure.
	decodeAndPrefill := func(decodeMeta, prefillMeta map[string]string) *registry.Registry {
		reg := registry.New(0)
		reg.Observe("ns", "m-decode-llmsm", decodeMeta)
		reg.Observe("ns", "m-prefill-llmsm", prefillMeta)
		return reg
	}

	base := map[string]string{registry.ModelIDKey: "m"}
	with := func(tokens string) map[string]string {
		return map[string]string{registry.ModelIDKey: "m", registry.DefaultOutputTokensKey: tokens}
	}
	vas := []llmdvariant.Variant{variant("ns", "m-decode-llmsm"), variant("ns", "m-prefill-llmsm")}

	It("takes the figure one role declares when the other declares none", func() {
		e := &Engine{Variants: decodeAndPrefill(with("6000"), base)}
		seed, conflicting := e.modelOutputSeed(vas)
		Expect(seed).To(Equal(6000),
			"a prefill trigger that generates nothing must not pull decode's figure down")
		Expect(conflicting).To(Equal([]int{6000}), "one distinct figure is not a disagreement")
	})

	It("takes the largest when the roles disagree, and names both", func() {
		e := &Engine{Variants: decodeAndPrefill(with("6000"), with("250"))}
		seed, conflicting := e.modelOutputSeed(vas)
		Expect(seed).To(Equal(6000), "under-pricing the queue is the failure this key exists to fix")
		Expect(conflicting).To(Equal([]int{250, 6000}), "sorted, so map order is not a change")
	})

	It("is zero when no trigger carries one, which hands the decision on", func() {
		e := &Engine{Variants: decodeAndPrefill(base, base)}
		seed, conflicting := e.modelOutputSeed(vas)
		Expect(seed).To(BeZero())
		Expect(conflicting).To(BeEmpty())
	})

	It("ignores a trigger whose metadata as a whole is unusable", func() {
		// No modelID: ParseMeta rejects the block, and every other consumer
		// skips it. Honouring one key off it would give this key its own rules.
		e := &Engine{Variants: decodeAndPrefill(
			map[string]string{registry.DefaultOutputTokensKey: "6000"},
			with("250"),
		)}
		seed, _ := e.modelOutputSeed(vas)
		Expect(seed).To(Equal(250))
	})

	It("ignores a value that is not a non-negative whole number", func() {
		e := &Engine{Variants: decodeAndPrefill(with("not-a-number"), with("250"))}
		seed, _ := e.modelOutputSeed(vas)
		Expect(seed).To(Equal(250),
			"ParseMeta rejects the block; the surviving trigger still seeds")
	})

	It("skips a variant the registry has never been called about", func() {
		reg := registry.New(0)
		reg.Observe("ns", "m-decode-llmsm", with("6000"))
		e := &Engine{Variants: reg}
		seed, _ := e.modelOutputSeed(vas)
		Expect(seed).To(Equal(6000))
	})

	It("is inert with no registry, rather than a panic", func() {
		e := &Engine{}
		seed, conflicting := e.modelOutputSeed(vas)
		Expect(seed).To(BeZero())
		Expect(conflicting).To(BeEmpty())
	})
})
