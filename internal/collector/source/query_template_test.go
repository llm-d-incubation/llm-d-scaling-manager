package source

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("EscapePromQLValue", func() {
	It("returns empty string unchanged", func() {
		Expect(EscapePromQLValue("")).To(Equal(""))
	})

	It("escapes backslashes", func() {
		Expect(EscapePromQLValue(`\`)).To(Equal(`\\`))
		Expect(EscapePromQLValue(`foo\bar`)).To(Equal(`foo\\bar`))
		Expect(EscapePromQLValue(`\\`)).To(Equal(`\\\\`))
	})

	It("escapes double quotes", func() {
		Expect(EscapePromQLValue(`"`)).To(Equal(`\"`))
		Expect(EscapePromQLValue(`foo"bar`)).To(Equal(`foo\"bar`))
		Expect(EscapePromQLValue(`""`)).To(Equal(`\"\"`))
	})

	It("escapes backslashes before quotes so order is correct", func() {
		// Backslash first, then quote - result is \\ then \"
		Expect(EscapePromQLValue(`\""`)).To(Equal(`\\\"\"`))
	})

	It("leaves safe characters unchanged", func() {
		Expect(EscapePromQLValue("test-ns")).To(Equal("test-ns"))
		Expect(EscapePromQLValue("my-model-id")).To(Equal("my-model-id"))
		Expect(EscapePromQLValue("a1b2c3")).To(Equal("a1b2c3"))
	})

	It("prevents PromQL label injection by escaping malicious payload", func() {
		// If unescaped, this could close the label and inject another: namespace="other"
		malicious := `prod",namespace="other"`
		escaped := EscapePromQLValue(malicious)
		Expect(escaped).To(Equal(`prod\",namespace=\"other\"`))
		// The escaped value should be safe to embed in: metric{namespace="<value>"}
		// i.e. metric{namespace="prod\",namespace=\"other\""} is one literal label value
		Expect(escaped).NotTo(ContainSubstring(`namespace="other`))
	})
})

var _ = Describe("QueryList runtime registration", func() {
	var list *QueryList

	BeforeEach(func() {
		list = NewQueryList()
	})

	Describe("Upsert", func() {
		It("adds a new query", func() {
			Expect(list.Upsert(QueryTemplate{Name: "q1", Template: "up"})).To(Succeed())
			Expect(list.Get("q1").Template).To(Equal("up"))
		})

		It("replaces an existing query without error (unlike Register)", func() {
			Expect(list.Register(QueryTemplate{Name: "q1", Template: "v1"})).To(Succeed())
			Expect(list.Register(QueryTemplate{Name: "q1", Template: "v2"})).NotTo(Succeed())

			Expect(list.Upsert(QueryTemplate{Name: "q1", Template: "v2"})).To(Succeed())
			Expect(list.Get("q1").Template).To(Equal("v2"))
		})

		It("rejects an empty name", func() {
			Expect(list.Upsert(QueryTemplate{Template: "up"})).NotTo(Succeed())
		})

		It("rejects an empty template", func() {
			Expect(list.Upsert(QueryTemplate{Name: "q1"})).NotTo(Succeed())
		})
	})

	Describe("Remove", func() {
		It("deletes a registered query", func() {
			Expect(list.Upsert(QueryTemplate{Name: "q1", Template: "up"})).To(Succeed())
			list.Remove("q1")
			Expect(list.Get("q1")).To(BeNil())
		})

		It("is a no-op for an unknown name", func() {
			Expect(func() { list.Remove("missing") }).NotTo(Panic())
		})

		It("frees the name so Register can reuse it", func() {
			Expect(list.Register(QueryTemplate{Name: "q1", Template: "v1"})).To(Succeed())
			list.Remove("q1")
			Expect(list.Register(QueryTemplate{Name: "q1", Template: "v2"})).To(Succeed())
			Expect(list.Get("q1").Template).To(Equal("v2"))
		})
	})
})
