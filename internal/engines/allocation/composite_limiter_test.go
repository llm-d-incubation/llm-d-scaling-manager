package allocation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// namedLimiter is a minimal Limiter test double: identity only, which is the
// whole of the Limiter contract.
type namedLimiter struct {
	name string
}

func (r *namedLimiter) Name() string { return r.name }

var _ = Describe("CompositeLimiter", func() {
	It("satisfies the Limiter interface", func() {
		var _ Limiter = (*CompositeLimiter)(nil)
	})

	It("returns its configured name", func() {
		c := NewCompositeLimiter("my-composite", nil)
		Expect(c.Name()).To(Equal("my-composite"))
	})

	It("exposes its constituents in declaration order", func() {
		a := &namedLimiter{name: "a"}
		b := &namedLimiter{name: "b"}
		comp := NewCompositeLimiter("composite", []Limiter{a, b})
		got := comp.Constituents()
		Expect(got).To(HaveLen(2))
		Expect(got[0].Name()).To(Equal("a"))
		Expect(got[1].Name()).To(Equal("b"))
	})

	It("exposes no constituents when constructed with none", func() {
		Expect(NewCompositeLimiter("composite", nil).Constituents()).To(BeEmpty())
	})
})
