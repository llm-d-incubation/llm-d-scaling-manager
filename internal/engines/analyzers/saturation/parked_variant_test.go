package saturation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
)

// A variant parked at zero replicas reaches no Analyze call, and a fleet that
// wakes after a long park is swept before Analyze runs. NoteVariantsPresent is
// the stamp the engine gives every variant that still exists, ahead of the
// sweep; these specs pin what that keeps and what it does not.
var _ = Describe("a variant that still exists keeps what it learned", func() {
	const (
		ns    = "ns"
		model = "m"
		accel = "NVIDIA-H200"
	)
	var (
		a   *SaturationAnalyzer
		now time.Time
	)
	timeout := capacity.DefaultHorizons().VariantTimeout

	BeforeEach(func() {
		a = NewSaturationAnalyzer(capacity.NewStore())
		now = time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC)
		a.now = func() time.Time { return now }
	})

	// learn gives a variant the two fitted figures a wake depends on, under
	// the key Analyze builds for it, stamped as Analyze stamps it.
	learn := func(variant, accelerator string, gpus int) string {
		key := a.itlWindowKey(ns, model, variant, accelerator, gpus)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.noteVariantSeen(key, now)
		a.itlTracker.SetBaseline(key, 0.012)
		a.startSeconds[key] = 70
		return key
	}
	// parkFor runs the engine's order -- stamp, then sweep -- in steps well
	// under the timeout, for longer than it in total.
	parkFor := func(total time.Duration, names ...string) {
		step := timeout / 4
		for elapsed := time.Duration(0); elapsed < total; elapsed += step {
			now = now.Add(step)
			a.NoteVariantsPresent(ns, model, names)
			a.EvictStaleHistory()
		}
	}

	It("keeps a parked variant's ITL baseline and start estimate past the variant timeout", func() {
		parked := learn("parked", accel, 1)
		deleted := learn("deleted", accel, 1)

		parkFor(2*timeout, "parked")

		Expect(a.itlBaselines()).To(HaveKey(parked), "the parked variant woke on the default ITL baseline")
		Expect(a.startSeconds).To(HaveKey(parked), "the parked variant lost its start estimate")
		// The control in the same run: the sweep does evict, so the case
		// above is kept by the stamp and not by a sweep that never fires.
		Expect(a.itlBaselines()).NotTo(HaveKey(deleted), "a variant no longer named must still age out")
		Expect(a.startSeconds).NotTo(HaveKey(deleted))
	})

	It("keeps a long-parked fleet's state on the cycle it wakes, when the sweep runs first", func() {
		// The whole fleet parked: the engine stops optimizing, so neither the
		// stamp nor the sweep runs for longer than the timeout. On the wake
		// cycle the engine stamps, then sweeps, then analyses.
		key := learn("v", accel, 1)
		now = now.Add(2 * timeout)

		a.NoteVariantsPresent(ns, model, []string{"v"})
		a.EvictStaleHistory()

		Expect(a.itlBaselines()).To(HaveKey(key), "the sweep took the state on the cycle the fleet woke")
	})

	It("keeps every key the variant learned under, and no other variant's", func() {
		// By name, not by the key Analyze would rebuild: a parked variant has
		// no Pods to observe its accelerator from.
		h200 := learn("v", accel, 1)
		h100 := learn("v", "NVIDIA-H100", 2)
		longerName := learn("v2", accel, 1)
		otherModel := a.itlWindowKey(ns, "other-model", "v", accel, 1)
		a.mu.Lock()
		a.noteVariantSeen(otherModel, now)
		a.itlTracker.SetBaseline(otherModel, 0.012)
		a.mu.Unlock()

		parkFor(2*timeout, "v")

		Expect(a.itlBaselines()).To(HaveKey(h200))
		Expect(a.itlBaselines()).To(HaveKey(h100))
		Expect(a.itlBaselines()).NotTo(HaveKey(longerName), `"v" must not keep "v2" alive`)
		Expect(a.itlBaselines()).NotTo(HaveKey(otherModel), "a variant name is scoped to its model")
	})

	It("keeps the variant's accelerator memo alive, so its key does not move on wake", func() {
		// Backdated from the analyzer's clock, which the sweep reads. Backdated
		// from the wall clock, this control stopped holding once the wall clock
		// was 30 days past the fixed date above, less the retention.
		a.mu.Lock()
		a.lastAccelerator[ns+"/v"] = acceleratorMemo{name: accel, lastUsed: now.Add(-30 * 24 * time.Hour)}
		a.lastAccelerator[ns+"/gone"] = acceleratorMemo{name: accel, lastUsed: now.Add(-30 * 24 * time.Hour)}
		a.mu.Unlock()

		a.NoteVariantsPresent(ns, model, []string{"v"})
		a.EvictStaleHistory()

		Expect(a.lastAccelerator).To(HaveKey(ns+"/v"), "the parked variant's accelerator memo was swept")
		Expect(a.lastAccelerator).NotTo(HaveKey(ns+"/gone"), "the control: a memo nobody names is still swept")
	})
})
