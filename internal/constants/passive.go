package constants

// LLMSMPassiveMode is a gauge: 1 for the passive mode a variant is in, 0 for the
// others (PassiveModes). Labels: namespace, model_name, variant_name, mode.
// A variant's series go when it leaves the fleet.
const LLMSMPassiveMode = "llmsm_passive_mode"

// Values of LabelMode on LLMSMPassiveMode: how a variant's trigger answers KEDA.
const (
	// PassiveModeActive is ordinary scaling.
	PassiveModeActive = "active"
	// PassiveModeHold is passive with our trigger alone: it answers the live
	// replica count, so nothing moves it but an operator.
	PassiveModeHold = "hold"
	// PassiveModeObserve is passive beside other triggers: it answers 0 and
	// inactive, and KEDA scales on the others.
	PassiveModeObserve = "observe"
	// PassiveModeObserveUnsafe is observe where a scalingModifiers formula
	// names our trigger, so our 0 enters the formula. Unsupported.
	PassiveModeObserveUnsafe = "observe-unsafe"
)

// PassiveModes lists the LLMSMPassiveMode mode label values.
var PassiveModes = []string{PassiveModeActive, PassiveModeHold, PassiveModeObserve, PassiveModeObserveUnsafe}

// K8SEventPassiveRecommendation is the Normal event a passive variant gets
// where an active one would get ScaledUp, ScaledDown or ScaledToZero: what
// the controller would have done, without the claim that it did it.
const K8SEventPassiveRecommendation = "PassiveRecommendation"

// K8SEventPassiveRestartRequired is the Warning event on scaling-manager-manager-config
// when its LLMSM_PASSIVE differs from the one the controller read at start.
const K8SEventPassiveRestartRequired = "PassiveRestartRequired"
