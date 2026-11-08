package allocation

// Eligible reports whether nr may contribute to the composite aggregation:
// a non-nil Result, an informative one (ResultIsInformative — at least one
// VariantCapacity whose Reason is not a no-data/error sentinel), and Live.
//
// The same rule gates both directions of the composite by design: a stale
// or uninformative analyzer neither raises demand (it cannot become a
// contributor to the replica need) nor blocks scale-down (it is equally
// excluded from any all-agree spare-capacity gate). There is no separate,
// looser rule for one direction.
//
// Eligible is distinct from whether a particular SO's N is defined — an
// eligible analyzer can still fail to contribute to one SO because its N
// there is undefined; that is a per-contribution skip, checked separately,
// never folded into eligibility. ResolveSO checks Eligible first, before
// anything about the SO.
func Eligible(nr NamedAnalyzerResult) bool {
	return nr.Result != nil && ResultIsInformative(nr) && nr.Live
}
