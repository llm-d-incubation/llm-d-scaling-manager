package allocation

import (
	"cmp"
	"container/heap"
	"math"
	"slices"
)

// ShareRole is one role's input to the utilization-share allocation: a role of
// a P/D model, or the single synthetic role of an aggregated one. All GPU
// quantities are whole GPUs except Need and Capacity, which are continuous.
//
// See docs/proposals/utilization-share-optimizer.md, sections 4 and 5.
type ShareRole struct {
	// Key identifies the role (namespace, model, role) and breaks ties, so the
	// allocation is deterministic for identical input.
	Key string
	// Weight is the model's weight, already clamped into the class range.
	Weight float64
	// Need is N_r: the GPUs that put the role exactly at its scale-up threshold.
	Need float64
	// Floor is F_r, the GPUs the role cannot give: its minReplicaCount, and
	// at least one replica while it runs one (the last is scale-to-zero's).
	Floor int
	// MinFloor is the part of Floor the owner configured: the GPUs its
	// minReplicaCount holds. Reports that tell the owner to lower a floor
	// count only this.
	MinFloor int
	// Ceiling is C_r, the GPUs its maxReplicaCount allows. Zero is unbounded.
	Ceiling int
	// ReplicaGPUs is g_r, one replica of the role.
	ReplicaGPUs int
	// Capacity is what the role's GPUs serve, in the order it keeps and grows
	// them: the first step is the GPUs it gives last. Past the last step a GPU
	// serves as much as one of the role's best variant. Nil when every GPU
	// serves alike; Need is then also the role's utilization in GPUs.
	Capacity []ShareCapacityStep
	// Held is whether the role keeps what it holds this cycle: its analyzer
	// holds it, or it may not scale down. Floor is then what it holds, and
	// ShareCapSpare counts its headroom above Need, not above that Floor.
	Held bool
}

// ShareCapacityStep is a run of a role's GPUs that serve alike: the replicas
// of one variant.
type ShareCapacityStep struct {
	// GPUs is the run's length; +Inf for a variant with no ceiling.
	GPUs float64
	// PerGPU is what one of its GPUs serves, as a fraction of what a GPU of
	// the role's best variant serves.
	PerGPU float64
}

// serves is what g GPUs of the role serve, counted in GPUs of its best
// variant: g itself when every GPU serves alike. The role's utilization at g
// GPUs is serves(Need)·k / serves(g).
func (r ShareRole) serves(g float64) float64 {
	if len(r.Capacity) == 0 || !(g > 0) {
		return g
	}
	s := 0.0
	for _, st := range r.Capacity {
		take := math.Min(g, st.GPUs)
		s += take * st.PerGPU
		g -= take
		if g <= 0 {
			return s
		}
	}
	return s + g
}

// Utilization is the role's utilization holding held GPUs, threshold being
// its scale-up threshold: what its need serves, times threshold, over what
// held serves. It is Need·threshold / held when every GPU serves alike.
func (r ShareRole) Utilization(held, threshold float64) float64 {
	return r.serves(r.Need) * threshold / r.serves(held)
}

// Score is the role's ShareScore holding held GPUs, judged on what its GPUs
// serve: ShareScore(held, Need, Weight) when every GPU serves alike.
func (r ShareRole) Score(held float64) float64 {
	return ShareScore(r.serves(held), r.serves(r.Need), r.Weight)
}

// gpusFor is the inverse of serves: the GPUs at which the role serves c,
// counted in GPUs of its best variant. It is c itself while every step it
// walks serves as much as best.
func (r ShareRole) gpusFor(c float64) float64 {
	if len(r.Capacity) == 0 || !(c > 0) {
		return c
	}
	g, left, uniform := 0.0, c, true
	for _, st := range r.Capacity {
		uniform = uniform && st.PerGPU == 1
		if left <= st.GPUs*st.PerGPU {
			g, left = g+left/st.PerGPU, 0
			break
		}
		g, left = g+st.GPUs, left-st.GPUs*st.PerGPU
	}
	if uniform {
		return c
	}
	return g + left
}

func (r ShareRole) ceiling() float64 {
	if r.Ceiling <= 0 {
		return math.Inf(1)
	}
	return float64(r.Ceiling)
}

// Claim is the role's need raised to its floor and capped at its ceiling:
// N̄_r = min(max(N_r, F_r), C_r).
func (r ShareRole) Claim() float64 {
	return math.Min(math.Max(r.Need, float64(r.Floor)), r.ceiling())
}

// ShareHeadroomReplicas is the replicas of headroom a role of weight w is
// given above its claim when a weight-1 role is given n
// (utilizationShare.maxHeadroomReplicas): w·n, rounded up.
func ShareHeadroomReplicas(w float64, n int) int {
	if n <= 0 || !(w > 0) {
		return 0
	}
	// No quota holds more replicas than GPUs; the bound keeps a huge weight
	// from overflowing the conversion.
	return int(math.Min(math.Ceil(w*float64(n)-1e-9), MaxShareBudgetGPUs))
}

// ShareCapSpare returns roles with each one's ceiling lowered to its claim,
// rounded up to whole replicas, plus ShareHeadroomReplicas(weight, n): the
// most of the spare the targets give it. The rest stays unassigned in the
// quota, where the idle fill raises a model whose demand rises without a
// drain to wait for (proposal §5.1). Claims are unchanged: the lowered ceiling
// is never below one. A negative n, a claim that is not finite, and a limit
// too large to hold leave the ceiling as it is, as does a limit of zero, which
// would read as unbounded -- a role with no claim takes no spare anyway.
//
// A held role (ShareRole.Held) is given its headroom above its need, and never
// a ceiling below what it holds. Above its Floor -- what it holds -- each cycle
// the hold lasts would raise it a replica more.
func ShareCapSpare(roles []ShareRole, n int) []ShareRole {
	out := slices.Clone(roles)
	if n < 0 {
		return out
	}
	for i, r := range out {
		g := float64(max(r.ReplicaGPUs, 1))
		claim := r.Claim()
		if r.Held {
			claim = math.Min(r.Need, r.ceiling())
		}
		limit := g * (math.Ceil(claim/g-1e-9) + float64(ShareHeadroomReplicas(r.Weight, n)))
		if r.Held {
			limit = math.Max(limit, float64(r.Floor))
		}
		if math.IsNaN(limit) || limit < 1 || limit > MaxShareBudgetGPUs {
			continue
		}
		if c := int(limit); r.Ceiling <= 0 || c < r.Ceiling {
			out[i].Ceiling = c
		}
	}
	return out
}

// ShareScore is z, the one score that orders both regimes (proposal §5.4):
// headroom divided by weight when the role holds at least its need, shortfall
// multiplied by weight when it holds less. A heavier role's headroom counts
// for less and its shortfall for more. A role with no need has no score.
func ShareScore(held, need, weight float64) float64 {
	if need <= 0 {
		return math.Inf(1)
	}
	x := held/need - 1
	if x >= 0 {
		return x / weight
	}
	return x * weight
}

// ShareSpare is S = B − Σ N̄_r over the roles: positive when the budget covers
// every claim, negative when it is short (proposal §5.1).
func ShareSpare(roles []ShareRole, budget float64) float64 {
	sum := 0.0
	for _, r := range roles {
		sum += r.Claim()
	}
	return budget - sum
}

// WholeReplicaCoverage reports whether whole replicas allow every role its
// claim: Σ g_r · ⌈N̄_r / g_r⌉ ≤ budget. Only then is "every role gets at least
// its claim" guaranteed; otherwise a role may end up one replica below it
// (proposal §5.1).
func WholeReplicaCoverage(roles []ShareRole, budget int) bool {
	sum := 0
	for _, r := range roles {
		g := max(r.ReplicaGPUs, 1)
		sum += g * int(math.Ceil(r.Claim()/float64(g)-1e-9))
	}
	return sum <= budget
}

// ContinuousShareTargets returns Ĝ_r, the continuous allocation the tolerance
// band is judged against (proposal §5.1, §6.1): every role at its claim, the
// spare shared as headroom proportional to weight, or a shortfall shared by
// inverse weight, water-filled over floors and ceilings. Roles with no need
// hold their floor and take no share.
func ContinuousShareTargets(roles []ShareRole, budget float64) map[string]float64 {
	out := make(map[string]float64, len(roles))
	active := make([]ShareRole, 0, len(roles))
	pool := budget
	for _, r := range roles {
		if r.Need <= 0 {
			out[r.Key] = float64(r.Floor)
			pool -= float64(r.Floor)
			continue
		}
		active = append(active, r)
	}
	// Each pass solves for the common factor over the unpinned roles, then pins
	// the roles the solution takes past a bound -- but only on the side that
	// overshoots more. Pinning a ceiling frees GPUs that can lift a role the
	// same pass found below its floor, and pinning a floor takes GPUs that can
	// bring a role below its ceiling: pinning both sides at once fixes a role
	// at a bound the true solution does not hold it at. The side with the
	// larger total violation is at its bound in the solution, so it is safe to
	// pin. A pinned role's GPUs leave the pool; the rest are solved again. Each
	// pass pins at least one role or finishes, so it ends within len(roles)
	// passes.
	for len(active) > 0 {
		sumN, sumWN, sumNoverW := 0.0, 0.0, 0.0
		for _, r := range active {
			sumN += r.Need
			sumWN += r.Weight * r.Need
			sumNoverW += r.Need / r.Weight
		}
		spare := pool - sumN
		target := func(r ShareRole) float64 {
			if spare >= 0 {
				return r.Need * (1 + spare/sumWN*r.Weight)
			}
			return r.Need * (1 + spare/sumNoverW/r.Weight)
		}
		over, under := 0.0, 0.0
		for _, r := range active {
			g := target(r)
			if g > r.ceiling() {
				over += g - r.ceiling()
			} else if g < float64(r.Floor) {
				under += float64(r.Floor) - g
			}
		}
		pinCeilings, pinFloors := over >= under, under >= over
		var keep []ShareRole
		pinned := false
		for _, r := range active {
			g := target(r)
			switch {
			case pinFloors && g < float64(r.Floor):
				out[r.Key] = float64(r.Floor)
				pool -= float64(r.Floor)
				pinned = true
			case pinCeilings && g > r.ceiling():
				out[r.Key] = r.ceiling()
				pool -= r.ceiling()
				pinned = true
			default:
				keep = append(keep, r)
			}
		}
		if !pinned {
			for _, r := range keep {
				out[r.Key] = math.Max(0, target(r))
			}
			break
		}
		active = keep
	}
	return out
}

// IntegerShareTargets returns G*_r, the whole-replica allocation (proposal
// §5.4). Every role starts at its floor; then, while some role can take one
// more replica, the next replica goes to the candidate with the lowest score z
// -- ties to the higher weight, then to the key. A candidate has demand, a next
// replica that fits the remaining budget, and room below its ceiling. Any
// leftover smaller than every candidate's replica stays unallocated.
func IntegerShareTargets(roles []ShareRole, budget int) map[string]int {
	out := make(map[string]int, len(roles))
	left := budget
	h := &shareHeap{}
	for _, r := range roles {
		out[r.Key] = r.Floor
		left -= r.Floor
	}
	for _, r := range roles {
		if r.Need > 0 {
			heap.Push(h, shareEntry{role: r, held: out[r.Key]})
		}
	}
	// A popped role whose replica does not fit is dropped for good: the
	// remaining budget only shrinks, so it never fits later.
	for h.Len() > 0 {
		e := heap.Pop(h).(shareEntry)
		g := max(e.role.ReplicaGPUs, 1)
		if g > left || float64(e.held+g) > e.role.ceiling() {
			continue
		}
		e.held += g
		left -= g
		out[e.role.Key] = e.held
		heap.Push(h, e)
	}
	return out
}

type shareEntry struct {
	role ShareRole
	held int
}

func (e shareEntry) z() float64 { return e.role.Score(float64(e.held)) }

// shareHeap is a min-heap on (z, -weight, key).
type shareHeap []shareEntry

func (h shareHeap) Len() int { return len(h) }
func (h shareHeap) Less(i, j int) bool {
	return cmp.Or(
		cmp.Compare(h[i].z(), h[j].z()),
		cmp.Compare(h[j].role.Weight, h[i].role.Weight),
		cmp.Compare(h[i].role.Key, h[j].role.Key),
	) < 0
}
func (h shareHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *shareHeap) Push(x any)   { *h = append(*h, x.(shareEntry)) }
func (h *shareHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// ShareIdleFloor is the utilization distance within which a role not past its
// threshold counts as in band whatever its GPU distance (proposal §6.1, §8.4):
// it stops idle models from churning on large relative misses of no
// consequence.
const ShareIdleFloor = 0.05

// ShareInBand reports whether a role holding committed GPUs is inside the band
// around its continuous target (proposal §6.1 step 3). The band is judged in
// GPUs, never narrower than half a replica; a role not past its scale-up
// threshold is also in band when its utilization is within ShareIdleFloor of
// the target's. threshold is the role's scale-up threshold k_r, so its
// utilization at g GPUs is serves(need)·k / serves(g): need·k / g when every
// GPU serves alike.
func ShareInBand(committed, target float64, r ShareRole, tolerance, threshold float64) bool {
	if math.Abs(committed-target) <= shareTol(target, r, tolerance) {
		return true
	}
	if committed <= 0 || target <= 0 {
		return false
	}
	u := r.Utilization(committed, threshold)
	ut := r.Utilization(target, threshold)
	return u <= threshold && math.Abs(u-ut) <= ShareIdleFloor
}

// ShareRoleVerdict is the per-role result of one evaluation.
type ShareRoleVerdict struct {
	Key string
	// Committed is Ḡ_r, the GPUs the role holds or is committed to.
	Committed int
	// Continuous is Ĝ_r and Integer is G*_r.
	Continuous float64
	Integer    int
	// Headroom is x_r = Committed / Need − 1, counted in what the GPUs serve:
	// the spike the role absorbs before it must scale. NaN when the role has no need.
	Headroom float64
	// InBand and Actionable are §6.1 steps 3 and 4: a role is actionable when it
	// is out of band AND off its integer target, so a move could fix it.
	InBand     bool
	Actionable bool
}

// ShareEvaluation is one group's evaluation.
type ShareEvaluation struct {
	// Spare is S = B_net − Σ claims; negative when the group is short.
	Spare float64
	// WholeReplicaCoverage reports whether whole replicas allow every claim.
	WholeReplicaCoverage bool
	// Roles are sorted by key.
	Roles []ShareRoleVerdict
	// ReplicasToMove is how many replicas the integer target would move: the
	// sum of every role's shortfall below G*, in replicas.
	ReplicasToMove int
}

// EvaluateShare evaluates one group: targets, the band and the actionable
// roles, for the committed allocation given (proposal §6.1 steps 2-4). It
// plans nothing. thresholds maps each role key to its scale-up threshold.
func EvaluateShare(roles []ShareRole, committed map[string]int, thresholds map[string]float64,
	budget int, tolerance float64) ShareEvaluation {
	cont := ContinuousShareTargets(roles, float64(budget))
	integ := IntegerShareTargets(roles, budget)
	ev := ShareEvaluation{
		Spare:                ShareSpare(roles, float64(budget)),
		WholeReplicaCoverage: WholeReplicaCoverage(roles, budget),
	}
	for _, r := range roles {
		c := committed[r.Key]
		v := ShareRoleVerdict{
			Key:        r.Key,
			Committed:  c,
			Continuous: cont[r.Key],
			Integer:    integ[r.Key],
			Headroom:   math.NaN(),
		}
		if r.Need > 0 {
			v.Headroom = r.serves(float64(c))/r.serves(r.Need) - 1
		}
		// Below its need, a role is never in band while its target covers
		// that need: it runs past its scale-up threshold, which today's
		// optimizer would answer with a replica. The band is never narrower
		// than half a replica, so for whole-node replicas it would otherwise
		// hide a shortfall of up to four GPUs -- 8 held against 8.5 needed,
		// target 11.7 -- and the role would never be actionable. A need above
		// the ceiling is judged against the ceiling, which is all the target
		// can cover: else a role one replica below its ceiling, needing more
		// than it, would be in band and never take its last replica.
		need := math.Min(r.Need, r.ceiling())
		belowNeed := float64(c) < need && v.Continuous >= need-1e-9
		v.InBand = !belowNeed && ShareInBand(float64(c), v.Continuous, r, tolerance, thresholds[r.Key])
		v.Actionable = !v.InBand && c != v.Integer
		if short := v.Integer - c; short > 0 {
			ev.ReplicasToMove += short / max(r.ReplicaGPUs, 1)
		}
		ev.Roles = append(ev.Roles, v)
	}
	slices.SortFunc(ev.Roles, func(a, b ShareRoleVerdict) int { return cmp.Compare(a.Key, b.Key) })
	return ev
}
