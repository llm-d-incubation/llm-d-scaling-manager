package allocation

// NoOpLimiter is a Limiter that provides no constraints: it is not a
// ConstraintProvider, so the optimizer sees no resource caps from it. Useful as
// the zero value of the Limiter interface — pipelines that want to disable
// limiting without rewiring, and tests that construct an engine without
// exercising the limiter path.
type NoOpLimiter struct {
	name string
}

// NewNoOpLimiter constructs a NoOpLimiter with the given name. The name
// surfaces in logs and decision traces.
func NewNoOpLimiter(name string) *NoOpLimiter {
	return &NoOpLimiter{name: name}
}

// Name returns the no-op limiter identifier.
func (l *NoOpLimiter) Name() string { return l.name }

// Ensure NoOpLimiter implements Limiter.
var _ Limiter = (*NoOpLimiter)(nil)
