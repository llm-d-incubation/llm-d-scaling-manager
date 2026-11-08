package allocation

// CompositeSignalName is the Name every composite NamedAnalyzerResult carries.
// The optimizer's input is always the composite — never literally
// saturation, even on the sat-only path — so a consumer that assumes
// ModelScalingRequest.CompositeSignal.Name is a specific analyzer's name is
// wrong; test the signal instead, as CompositeHasSignal does.
//
// Lives beside NamedAnalyzerResult, since the composite is an allocation
// concept and ModelScalingRequest.CompositeSignal already uses this word.
const CompositeSignalName = "CompositeSignal"
