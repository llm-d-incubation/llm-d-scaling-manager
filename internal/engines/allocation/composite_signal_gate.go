package allocation

// CompositeHasSignal reports whether composite carries a real capacity
// signal: a non-nil Result with at least one VariantCapacity whose decision
// path is not C4-no-signal. On the composite, Reason carries the decision
// path (C0-agree/C1-single/C4-no-signal), not an analyzer's
// own no-data/error/P0-store vocabulary.
//
// It tests whether the composite carries a usable signal, never which
// analyzer produced it. A check like
// CompositeSignal.Name == domain.SaturationAnalyzerName is always false,
// because the composite carries its own name.
//
// Deliberately NOT ResultIsInformative: that function's no-data/error
// sentinel check is calibrated to an ANALYZER's own Reason vocabulary. The
// composite's Reason means something different (which decision path
// produced this SO's N_com, not how PRC was measured) and DecisionNoSignal
// ("C4-no-signal") does not equal either analyzer sentinel string — reusing
// ResultIsInformative here would always report "usable" regardless of
// whether any SO actually had a signal.
//
// A model whose composite fails this check had no listed analyzer able to
// size any variant this cycle. The engine reports it as no-composite-signal.
// With saturation listed, the optimizer acts on saturation's own result, as
// it did before the composite existed; without, every role is held
// (RoleHold). Its replicas are
// charged to a quota either way (measuredThisCycle), since they hold their
// GPUs.
func CompositeHasSignal(composite NamedAnalyzerResult) bool {
	if composite.Result == nil {
		return false
	}
	for _, vc := range composite.Result.VariantCapacities {
		if DecisionPath(vc.Reason) != DecisionNoSignal {
			return true
		}
	}
	return false
}
