package steadystate

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
)

var _ = Describe("Engine.resolveRescaleFlags", func() {
	// Builds a config with a global default flag plus per-namespace defaults, then
	// checks the scope-coupled resolution: cluster from the global default, each
	// namespace flag from its OWN default only (no global fallback).
	newEngine := func(clusterOn bool) *Engine {
		c := config.NewTestConfig()
		c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {EnableRescale: clusterOn}})
		// team: its own flag on. plain: has a local config but flag off. absent: no config.
		c.UpdateScalingPolicyConfigForNamespace("team", map[string]config.ScalingPolicy{"default": {EnableRescale: true}})
		c.UpdateScalingPolicyConfigForNamespace("plain", map[string]config.ScalingPolicy{"default": {KvCacheThreshold: 0.8}})
		return &Engine{Config: c}
	}

	req := func(ns string) allocation.ModelScalingRequest {
		return allocation.ModelScalingRequest{Namespace: ns}
	}

	It("sets the cluster flag from the global default", func() {
		flags := newEngine(true).resolveRescaleFlags([]allocation.ModelScalingRequest{req("team")})
		Expect(flags.Cluster).To(BeTrue())
	})

	It("leaves the cluster flag off when the global default is off", func() {
		flags := newEngine(false).resolveRescaleFlags([]allocation.ModelScalingRequest{req("team")})
		Expect(flags.Cluster).To(BeFalse())
	})

	It("enables only namespaces whose own default sets the flag", func() {
		reqs := []allocation.ModelScalingRequest{
			req("team"), req("team"), // duplicate namespace must dedup, not double-count
			req("plain"),  // local config, flag off → excluded
			req("absent"), // no local config → excluded (no global fallback)
			req(""),       // empty namespace → skipped
		}
		flags := newEngine(true).resolveRescaleFlags(reqs)

		Expect(flags.ByNamespace).To(HaveKeyWithValue("team", true))
		Expect(flags.ByNamespace).ToNot(HaveKey("plain"), "cluster flag must not leak onto a namespace quota")
		Expect(flags.ByNamespace).ToNot(HaveKey("absent"))
		Expect(flags.ByNamespace).ToNot(HaveKey(""))
	})
})
