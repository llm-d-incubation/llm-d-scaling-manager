package allocation

import (
	"sync/atomic"
	"time"
)

// BorrowCooldowns lets the scale-from-zero engine read the steady-state
// engine's borrowing cooldowns (BorrowGovernor.CoolingDown) without owning
// the governor: the steady-state engine publishes the governor it runs, the
// wake reads it. Read-only: nothing here changes a cooldown.
//
// Safe for concurrent use.
type BorrowCooldowns struct {
	g atomic.Pointer[BorrowGovernor]
}

// DefaultBorrowCooldowns is the process-wide one both engines share.
var DefaultBorrowCooldowns = &BorrowCooldowns{}

// Publish makes g the governor CoolingDown reads; nil when the steady-state
// engine runs none.
func (c *BorrowCooldowns) Publish(g *BorrowGovernor) {
	if c != nil {
		c.g.Store(g)
	}
}

// CoolingDown reports whether the model ("namespace", "modelID") is in its
// borrowing cooldown now (BorrowGovernor.CoolingDown); false while no
// governor is published.
func (c *BorrowCooldowns) CoolingDown(namespace, modelID string, now time.Time) bool {
	if c == nil {
		return false
	}
	g := c.g.Load()
	return g != nil && g.CoolingDown(namespace, modelID, now)
}
