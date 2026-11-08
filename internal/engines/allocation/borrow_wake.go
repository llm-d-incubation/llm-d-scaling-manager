package allocation

import (
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
)

// WakeReplica is one replica a scale-from-zero wake orders.
type WakeReplica struct {
	Variant     string
	Accelerator string
	GPUs        int
}

// WakeDraws is what a wake of ns's replicas takes that the next Kueue read
// does not show yet (N3), as in-flight entries for the shared record: the
// cohort's free pool it grows through past a queue's guaranteed figure, as
// draws on its cohort's chunks; and its growth into a reclaiming queue's
// nominal as Reclaim entries -- what Kueue admits before preempting
// (Reclaim.After, whose pool part comes off the pool and whose kept part
// takes nothing from it, M-m1) and what it reclaims from borrowers
// (Reclaim.GPUs). Without them the steady-state engine, in the same window,
// borrows the very GPUs the wake took.
//
// constraints are the wake's own, before WithinNominal drops the borrowing
// figures; prior is, per accelerator, what the tick's earlier wakes of ns
// already grew it by; reclaim is the namespace's Reclaim left for this tick
// (ReclaimableNominal less the tick's earlier wakes). The replicas grow ns in
// order, as PlaceReplicasReclaiming takes them. A draw's GPUs are capped at
// the chunks' free figure: what is not free Kueue does not admit. Nil when
// ns grows within its guaranteed figure on a queue that cannot reclaim.
func WakeDraws(constraints []*ResourceConstraints, ns, modelID string, reps []WakeReplica, prior map[string]int,
	reclaim map[string]Reclaim) []BorrowDraw {
	type typeState struct {
		// read is set when a constraint carries ns's borrowing figures;
		// limit is its guaranteed figure (negative: unbounded), used what
		// it holds.
		read        bool
		limit, used int
		reclaim     Reclaim
		chunks      []SharedPoolChunk
		free        []int
	}
	states := map[string]*typeState{}
	own := ownFlavorsOf(constraints)
	state := func(acc string) *typeState {
		for k, s := range states {
			if accelerator.SameName(k, acc) {
				return s
			}
		}
		s := &typeState{}
		states[acc] = s
		for _, c := range constraints {
			if c == nil || (c.NamespaceBorrow[ns] == nil && c.NamespaceNominal[ns] == nil) {
				continue
			}
			k, ok := accelerator.FindKey(c.NamespacePools[ns], acc)
			if !ok {
				continue
			}
			p := c.NamespacePools[ns][k]
			s.read, s.limit, s.used = true, p.Limit, p.Used
			if c.NamespaceBorrow[ns] == nil {
				// Read and not counted: the pool Limit is nominal; the
				// queues' guaranteed figure is what needs no pool.
				if nk, ok := accelerator.FindKey(c.NamespaceNominal[ns], acc); ok {
					s.limit = c.NamespaceNominal[ns][nk].Guaranteed
				}
			}
			var flavors []string
			if fk, ok := accelerator.FindKey(own[ns], acc); ok {
				flavors = own[ns][fk]
			}
			for _, chunk := range c.SharedPools {
				if !slices.Contains(c.NamespaceCohorts[ns], chunk.Cohort) || !chunk.serves(acc) ||
					(len(flavors) > 0 && !slices.Contains(flavors, chunk.Flavor)) {
					continue
				}
				s.chunks = append(s.chunks, chunk)
				s.free = append(s.free, chunk.Free)
			}
			break
		}
		fresh := reclaimFor(constraints, ns, acc)
		s.reclaim = Reclaim{After: fresh.After, Kept: fresh.Kept}
		if k, ok := accelerator.FindKey(reclaim, acc); ok {
			s.reclaim.GPUs, s.reclaim.needsHole = reclaim[k].GPUs, reclaim[k].needsHole
		}
		if k, ok := accelerator.FindKey(prior, acc); ok {
			s.used += prior[k]
		}
		return s
	}

	var out []BorrowDraw
	for _, r := range reps {
		g := max(r.GPUs, 1)
		s := state(r.Accelerator)
		if !s.read {
			continue // no borrowing figures read for ns: nothing to record
		}
		// Past the guaranteed figure, through the pool; within it, a
		// reclaiming queue's growth takes the pool first, then reclaims.
		past := 0
		if s.limit >= 0 {
			past = max(0, s.used+g-s.limit) - max(0, s.used-s.limit)
		}
		s.used += g
		// Placed without needing a node hole (PlaceReplicasReclaiming): the
		// governor watches it under the placement timeout (NoHole, M3). One
		// that did find a hole places, and the watch never fires.
		noHole := past == 0 && s.reclaim.exempt(g)
		took := s.reclaim.take(g - past)
		s.reclaim = s.reclaim.sub(took)
		// Past guaranteed, through the pool as a draw; within it, the pool
		// part of After goes in the Reclaim entry below, and the kept part
		// takes nothing from the pool (M-m1).
		pool := past
		for i := range s.chunks {
			if pool <= 0 {
				break
			}
			n := min(pool, s.free[i])
			if n <= 0 {
				continue
			}
			s.free[i] -= n
			pool -= n
			ch := s.chunks[i]
			placement := ch.FlavorPlacement
			out = append(out, BorrowDraw{Namespace: ns, ModelID: modelID, Variant: r.Variant, Cohort: ch.Cohort,
				Flavor: ch.Flavor, Resource: ch.Resource, GPUs: n, Replicas: 1, Target: 1, Placement: &placement, Wake: true})
		}
		if took.GPUs > 0 || took.After > 0 {
			d := BorrowDraw{Namespace: ns, ModelID: modelID, Variant: r.Variant, GPUs: took.GPUs, After: took.After, Kept: took.Kept,
				Replicas: 1, Target: 1, Reclaim: true, Accelerator: r.Accelerator, Wake: true, NoHole: noHole}
			if len(s.chunks) > 0 {
				d.Cohort, d.Flavor, d.Resource = s.chunks[0].Cohort, s.chunks[0].Flavor, s.chunks[0].Resource
			}
			out = append(out, d)
		}
	}
	return out
}

// RecordWake adds a wake's entries (WakeDraws), ordered now, each only when
// the record holds none for its variant and chunk yet: the wake loop
// re-decides a woken model every tick until its pods exist, and must not
// count the same replica twice.
//
// A reclaim entry recorded (growth into nominal Kueue preempts borrowers for)
// is also queued, once, for the next steady-state pass to yield for
// (PendingWakeReclaims): that pass lowers this controller's own borrowers in the
// cohort, as it does for a reclaim it orders itself (yieldReclaimed).
func (b *BorrowInFlight) RecordWake(draws []BorrowDraw, now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Unyielded past InFlightTTL: no pass took it (borrowing not counted in
	// between), and its replica has landed or gone.
	b.wakeReclaims = slices.DeleteFunc(b.wakeReclaims, func(d BorrowDraw) bool { return now.Sub(d.OrderedAt) > InFlightTTL })
	for _, d := range draws {
		k := keyOfDraw(d)
		if _, ok := b.entries[k]; ok || d.GPUs <= 0 || d.Replicas <= 0 {
			continue
		}
		d.OrderedAt, d.ResolvedAt = now, time.Time{}
		d.Wake = true
		b.entries[k] = d
		if d.Reclaim {
			b.wakeReclaims = append(b.wakeReclaims, d)
		}
	}
}

// PendingWakeReclaims returns the wakes' reclaim entries no steady-state pass
// has yielded for yet (GreedyByScoreOptimizer.WakeReclaims), ordered at most
// InFlightTTL before now and still live: the record still holds the wake's
// entry, in flight or lagging once its pods exist. One the record dropped --
// the wake that ordered it is gone -- lowers nobody, and is forgotten. They
// stay queued until a pass that yielded for them is reported
// (YieldedWakeReclaims), so a cycle that ran no greedy pass, or did not get
// to one, loses none. The queue is in memory: a restart forgets it. Nil when
// there are none.
func (b *BorrowInFlight) PendingWakeReclaims(now time.Time) []BorrowDraw {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wakeReclaims = slices.DeleteFunc(b.wakeReclaims, func(d BorrowDraw) bool {
		return now.Sub(d.OrderedAt) > InFlightTTL || !b.wakeLiveLocked(d)
	})
	if len(b.wakeReclaims) == 0 {
		return nil
	}
	return slices.Clone(b.wakeReclaims)
}

// wakeLiveLocked reports whether the record still holds the wake's entry d:
// in flight, or lagging once its pods exist.
func (b *BorrowInFlight) wakeLiveLocked(d BorrowDraw) bool {
	k := keyOfDraw(d)
	if e, ok := b.entries[k]; ok && e.Wake {
		return true
	}
	return slices.ContainsFunc(b.lagging, func(l BorrowDraw) bool { return l.Wake && keyOfDraw(l) == k })
}

// YieldedWakeReclaims forgets the wakes' reclaim entries a pass has yielded
// for (PendingWakeReclaims' answer handed to it): each is yielded for once,
// however long its replica takes to land.
func (b *BorrowInFlight) YieldedWakeReclaims(draws []BorrowDraw) {
	if b == nil || len(draws) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wakeReclaims = slices.DeleteFunc(b.wakeReclaims, func(d BorrowDraw) bool {
		return slices.ContainsFunc(draws, func(y BorrowDraw) bool {
			return keyOfDraw(y) == keyOfDraw(d) && y.OrderedAt.Equal(d.OrderedAt)
		})
	})
}

// WithoutVariants is u less the entries of ns's variants in own: a model's
// own wake, re-decided while its pods do not exist yet, is not refused for
// what its earlier decision took (as the steady placer re-places a
// variant's own draw, A4).
func (u Unreserved) WithoutVariants(ns string, own map[string]bool) Unreserved {
	if len(own) == 0 {
		return u
	}
	u.InFlight = slices.DeleteFunc(slices.Clone(u.InFlight), func(d BorrowDraw) bool {
		return d.Namespace == ns && own[d.Variant]
	})
	return u
}
