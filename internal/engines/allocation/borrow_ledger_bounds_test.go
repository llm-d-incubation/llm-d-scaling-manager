package allocation

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the persisted borrow ledger's bounds", func() {
	It("drops absurd values and caps the entry count on load", func() {
		at := time.Unix(1000, 0)
		l := BorrowLedger{TimeoutHeld: map[string]int{"b/huge": maxLedgerReplicas + 1, "b/neg": -1, "b/ok": 2}}
		g := NewBorrowGovernor(DefaultBorrowSettings())
		Expect(g.Restore(l)).To(Equal(1), "the absurd hold")
		Expect(g.Ledger().TimeoutHeld).To(Equal(map[string]int{"b/ok": 2}))

		many := BorrowLedger{TimeoutHeld: map[string]int{}, ModelCooldown: map[string]time.Time{}}
		for i := range maxLedgerItems + 10 {
			many.TimeoutHeld[fmt.Sprintf("b/v%05d", i)] = 1
			many.ModelCooldown[fmt.Sprintf("b/m%05d", i)] = at
		}
		g = NewBorrowGovernor(DefaultBorrowSettings())
		Expect(g.Restore(many)).To(Equal(20))
		Expect(g.Ledger().TimeoutHeld).To(HaveLen(maxLedgerItems))
		Expect(g.Ledger().ModelCooldown).To(HaveLen(maxLedgerItems))
	})

	It("does not let a restored idleSince from the future hold the release state", func() {
		now := time.Unix(50000, 0)
		future := now.Add(24 * time.Hour)
		g := NewBorrowGovernor(DefaultBorrowSettings())
		g.Restore(BorrowLedger{TimeoutHeld: map[string]int{"b/x-v": 1}, IdleSince: &future})
		s := DefaultBorrowSettings()
		g.HoldReleases(now, nil, nil)
		g.HoldReleases(now.Add(s.Cooldown+s.ReleaseActuation+time.Second), nil, nil)
		Expect(g.Ledger().TimeoutHeld).To(BeEmpty(), "the window counts from now, not from a day ahead")
	})
})
