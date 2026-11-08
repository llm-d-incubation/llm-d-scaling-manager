package scalefromzero

import (
	"context"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// freshClaims gives the test its own claim store with one releasing transfer
// on A100 in the cluster group, scored receiverZ.
func freshClaims(t *testing.T, receiverZ float64) {
	t.Helper()
	saved := decision.DefaultShareClaims
	decision.DefaultShareClaims = &decision.ShareClaimStore{}
	t.Cleanup(func() { decision.DefaultShareClaims = saved })
	decision.DefaultShareClaims.Publish(map[string][]decision.ShareClaimable{
		decision.ShareGroupKey("", "A100"): {{ID: "t0", ReceiverZ: receiverZ, DonorGPUs: 1}},
	}, time.Now())
}

// A wake's claim weighs the model under the tier its variants name, as steady
// state does. Resolved with no tier, a model on a critical tier (weight 4)
// claimed as a standard one (weight 1) and lost to a receiver of weight 2.
func TestClaimShareTransferWeighsTheVariantsTier(t *testing.T) {
	doc := "limiters:\n  - type: quota\n    name: q\n    scope: cluster\n    quotas:\n      A100: 16\n" +
		"optimizer:\n  type: utilizationShare\n"
	var def config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(doc), &def); err != nil {
		t.Fatal(err)
	}
	c := config.NewTestConfig()
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{
		config.GlobalDefaultsKey: def,
		"critical-tier":          {WeightClass: "critical"},
	})
	va := llmdvariant.Variant{}
	va.Spec.ScalingPolicy = "critical-tier"
	group := modelGroup{namespace: selNS, modelID: "m", variants: []llmdvariant.Variant{va}}
	candidates := []Candidate{cand("dec", domain.RoleDecode, "A100", 1, 1)}

	freshClaims(t, -2)
	e := &Engine{config: c}
	if _, ok := e.claimShareTransfer(context.Background(), group, candidates, nil, coverage{}); !ok {
		t.Fatal("a model on a critical tier (z=-4) did not claim from a receiver at z=-2")
	}

	// Control: the same model with no tier is standard (z=-1) and must not claim.
	freshClaims(t, -2)
	e = &Engine{config: c}
	untiered := modelGroup{namespace: selNS, modelID: "m2", variants: []llmdvariant.Variant{{}}}
	if _, ok := e.claimShareTransfer(context.Background(), untiered, candidates, nil, coverage{}); ok {
		t.Fatal("a standard model (z=-1) claimed from a receiver at z=-2")
	}
}

// requirePrefill on a model with no prefill variant must not stop its decode
// from claiming: there is no prefill to wait for. It asked for a decode and a
// prefill together, found no prefill, and so never claimed at all.
func TestClaimShareTransferDecodeOnlyModelWithRequirePrefill(t *testing.T) {
	freshClaims(t, -0.3)
	e := &Engine{config: pdShareConfig(t)}
	group := modelGroup{namespace: selNS, modelID: "m"}
	if !e.requirePrefill(group) {
		t.Fatal("setup: requirePrefill did not resolve")
	}
	candidates := []Candidate{cand("dec", domain.RoleDecode, "A100", 1, 1)}
	set, ok := e.claimShareTransfer(context.Background(), group, candidates, nil, coverage{})
	if !ok || len(set) != 1 || set[0].VariantName != "dec" {
		t.Fatalf("decode-only model claimed %+v (%v), want its decode", set, ok)
	}
}
