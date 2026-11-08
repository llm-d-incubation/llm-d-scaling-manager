package config

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// The table is the steady-state engine's former shareMode (llm-scaler main,
// internal/engines/steadystate/utilization_share_shadow.go), which
// UtilizationShareModeOf replaced: an error wins over everything, then an
// unselected optimizer is off whatever its shadow flag says, then shadow, then
// active. Every one of the eight input combinations is listed so a reordered
// switch cannot pass.
var _ = Describe("UtilizationShareModeOf", func() {
	bad := errors.New("optimizer.type: unknown")
	DescribeTable("maps (shadow, selected, err) to the mode the removed shareMode returned",
		func(shadow, selected bool, err error, want string) {
			Expect(UtilizationShareModeOf(UtilizationShare{Shadow: shadow}, selected, err)).To(Equal(want))
		},
		Entry("error, selected, shadow", true, true, bad, constants.UtilizationShareModeInvalid),
		Entry("error, selected", false, true, bad, constants.UtilizationShareModeInvalid),
		Entry("error, unselected, shadow", true, false, bad, constants.UtilizationShareModeInvalid),
		Entry("error, unselected", false, false, bad, constants.UtilizationShareModeInvalid),
		Entry("unselected, shadow", true, false, nil, constants.UtilizationShareModeOff),
		Entry("unselected", false, false, nil, constants.UtilizationShareModeOff),
		Entry("selected, shadow", true, true, nil, constants.UtilizationShareModeShadow),
		Entry("selected", false, true, nil, constants.UtilizationShareModeActive),
	)
})

var _ = Describe("Config.UtilizationShareMode", func() {
	DescribeTable("reads the mode, and its error, from the applied policy",
		func(doc string, want string, wantErr bool) {
			c := NewTestConfig()
			c.UpdateScalingPolicyConfig(map[string]ScalingPolicy{GlobalDefaultsKey: parsePolicy(doc)})
			mode, err := c.UtilizationShareMode()
			Expect(mode).To(Equal(want))
			if wantErr {
				Expect(err).To(HaveOccurred())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			// The two accessors must never disagree.
			us, selected, usErr := c.UtilizationShare()
			Expect(mode).To(Equal(UtilizationShareModeOf(us, selected, usErr)))
		},
		Entry("no optimizer block", quotaLimiters, constants.UtilizationShareModeOff, false),
		Entry("selected", quotaLimiters+`
optimizer:
  type: utilizationShare
`, constants.UtilizationShareModeActive, false),
		Entry("selected in shadow", quotaLimiters+`
optimizer:
  type: utilizationShare
  utilizationShare:
    shadow: true
`, constants.UtilizationShareModeShadow, false),
		Entry("unknown optimizer type", quotaLimiters+`
optimizer:
  type: bogus
`, constants.UtilizationShareModeInvalid, true),
		Entry("selected without limiters", `
optimizer:
  type: utilizationShare
`, constants.UtilizationShareModeInvalid, true),
	)
})
