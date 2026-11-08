package saturation

// SwitchITLTierHysteresis is the replay switch for the latch on the ITL
// line's tier (issue ev-shindin/llm-scaling-manager#495, itl.Admission.TierHysteresis). Off, each cycle
// chooses its tier alone, as before.
const SwitchITLTierHysteresis = "saturation-itl-tier-hysteresis"

// SwitchITLClearsOnNewRevision is the replay switch for the window across a
// rollout (itl.Tracker.RolloutRows). This line takes no rows mid-rollout. On,
// the first rows after it clear the window when their revision is new, which
// keeps B and the slope and puts the tier latch back: the line holds its slope
// until 10 new readings arrive. Off, new pods' readings join the old pods', and
// a full fit spans two engines and rewrites B.
const SwitchITLClearsOnNewRevision = "saturation-itl-clears-on-a-new-revision"

// tierHysteresisOn reports SwitchITLTierHysteresis.
func (a *SaturationAnalyzer) tierHysteresisOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.itlTierHysteresis
}
