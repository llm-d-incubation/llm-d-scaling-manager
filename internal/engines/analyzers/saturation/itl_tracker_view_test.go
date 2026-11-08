package saturation

// itlBaselines and itlWindowLens are the tests' view of the ITL tracker's
// state, read the way the maps it replaced were: by key.
func (a *SaturationAnalyzer) itlBaselines() map[string]float64 { return a.itlTracker.Baselines() }

func (a *SaturationAnalyzer) itlWindowLens() map[string]int { return a.itlTracker.WindowLens() }
