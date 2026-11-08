package allocation

import (
	"maps"
	"math/bits"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// podSearchMaxUnits bounds the exact pod search: a model with more than one
// replica size grows or sheds at most this many units a cycle, where a unit is the
// greatest common divisor of the pod sizes searched (4 GPUs for 4- and 8-GPU
// pods). Clamping a bound to a constant keeps the result monotone in the
// bound; the rest is taken on a later cycle.
//
// Worst case per model and cycle: every set is at most podSearchMaxUnits+1
// bits (65 words). Each variant's reachable set is built by binary-split
// shifts, at most 13 per variant; composing a role's pods keeps one suffix set
// per variant; and the joint search over roles, run once for the fill and
// once for the shed, shifts one set per reachable value of each role, at most
// 4097 x 65 word operations per role. BenchmarkPodSearchAtTheBound measures
// one joint search over two roles at the bound at 0.35-0.47 ms. roundExact
// runs the pair again at most once per distinct split below the target, and
// only while that split's room still exceeds the best plan so far, which in
// practice is about the largest pod's size in units.
const podSearchMaxUnits = 4096

// podBits is a set of small non-negative integers: GPU sums, in units.
type podBits []uint64

// newPodBits returns an empty set over 0..n.
func newPodBits(n int) podBits { return make(podBits, n/64+1) }

// zeroPodBits returns the set {0} over 0..n: the sum of no pods.
func zeroPodBits(n int) podBits {
	b := newPodBits(n)
	b[0] = 1
	return b
}

// has reports whether i is in b.
func (b podBits) has(i int) bool {
	return i >= 0 && i/64 < len(b) && b[i/64]&(1<<(uint(i)%64)) != 0
}

// unset removes i, which must be within b, from b.
func (b podBits) unset(i int) { b[i/64] &^= 1 << (uint(i) % 64) }

// orShifted sets b |= src << k, dropping what falls past b's last word. b and
// src may be the same set: the words are written from the top down, so each
// read is of a word not yet written.
func (b podBits) orShifted(src podBits, k int) {
	w, s := k/64, uint(k%64)
	for i := len(b) - 1; i >= w; i-- {
		j := i - w
		v := src[j] << s
		if s != 0 && j > 0 {
			v |= src[j-1] >> (64 - s)
		}
		b[i] |= v
	}
}

// maxAtMost is the largest member of b no greater than n, or -1.
func (b podBits) maxAtMost(n int) int {
	if n < 0 {
		return -1
	}
	n = min(n, len(b)*64-1)
	for i := n / 64; i >= 0; i-- {
		w := b[i]
		if i == n/64 && n%64 != 63 {
			w &= (1 << (uint(n%64) + 1)) - 1
		}
		if w != 0 {
			return i*64 + 63 - bits.LeadingZeros64(w)
		}
	}
	return -1
}

// podItem is a variant the search may add or remove pods of: size units a
// pod, at most count pods.
type podItem struct {
	name  string
	size  int
	count int
}

// podSums is the set of sums over 0..bound that items' pods make.
func podSums(items []podItem, bound int) podBits {
	b := zeroPodBits(bound)
	for _, it := range items {
		left := min(it.count, bound/max(it.size, 1))
		for take := 1; left > 0; take *= 2 {
			n := min(take, left)
			b.orShifted(b, n*it.size)
			left -= n
		}
	}
	return b
}

// composePods splits amount, a member of podSums(items, amount), into pods
// per item: as many of each as the items after it can still complete, in
// items' order, so the first items are preferred as a greedy pass prefers
// them.
func composePods(items []podItem, amount int) []int {
	suffix := make([]podBits, len(items)+1)
	suffix[len(items)] = zeroPodBits(amount)
	for i := len(items) - 1; i >= 0; i-- {
		suffix[i] = slices.Clone(suffix[i+1])
		left := min(items[i].count, amount/max(items[i].size, 1))
		for take := 1; left > 0; take *= 2 {
			n := min(take, left)
			suffix[i].orShifted(suffix[i], n*items[i].size)
			left -= n
		}
	}
	out := make([]int, len(items))
	rem := amount
	for i, it := range items {
		for n := min(it.count, rem/max(it.size, 1)); n >= 0; n-- {
			if suffix[i+1].has(rem - n*it.size) {
				out[i], rem = n, rem-n*it.size
				break
			}
		}
	}
	return out
}

// jointPods picks one member of each sets[i], at most caps[i], whose sum is
// the largest at most bound, and returns the picks (jointSplit). A larger
// bound or cap only adds choices, so the sum never falls as either rises.
func jointPods(sets []podBits, caps []int, bound int, ideal []int) []int {
	layers := jointLayers(sets, caps, bound)
	return jointSplit(layers, sets, caps, layers[len(sets)].maxAtMost(bound), ideal)
}

// jointLayers returns, for each i, the sums at most bound that one member of
// each of sets[:i], at most caps, can make.
func jointLayers(sets []podBits, caps []int, bound int) []podBits {
	layers := make([]podBits, len(sets)+1)
	layers[0] = zeroPodBits(bound)
	for i, s := range sets {
		layers[i+1] = newPodBits(bound)
		for a := s.maxAtMost(min(caps[i], bound)); a >= 0; a = s.maxAtMost(a - 1) {
			layers[i+1].orShifted(layers[i], a)
		}
	}
	return layers
}

// jointSplit splits sum, a member of the last of layers, into one member of
// each sets[i]: each role, from the last, takes the member nearest its ideal
// that the roles before it can complete, the smaller on a tie.
func jointSplit(layers, sets []podBits, caps []int, sum int, ideal []int) []int {
	out := make([]int, len(sets))
	for i := len(sets) - 1; i >= 0 && sum > 0; i-- {
		pick, best := 0, -1
		for a := sets[i].maxAtMost(min(caps[i], sum)); a >= 0; a = sets[i].maxAtMost(a - 1) {
			if !layers[i].has(sum - a) {
				continue
			}
			d := a - ideal[i]
			if d < 0 {
				d = -d
			}
			if best < 0 || d <= best {
				pick, best = a, d
			}
		}
		out[i] = pick
		sum -= pick
	}
	return out
}

// podUnit is the greatest common divisor of the GPUs per replica of
// variants, the unit the pod search counts in; 1 when there are none.
func podUnit(variants []variantRecord, stateMap map[string]domain.VariantReplicaState) int {
	g := 0
	for _, vc := range variants {
		a, b := gpusPerReplicaFromState(stateMap, vc.VariantName), g
		for b != 0 {
			a, b = b, a%b
		}
		g = a
	}
	return max(g, 1)
}

// toUnits converts gpus to whole units, at most podSearchMaxUnits.
func toUnits(gpus, unit int) int {
	return min(max(gpus, 0)/unit, podSearchMaxUnits)
}

// uniformPods reports whether every variant that can be sized (a capacity
// reading) takes one replica size, across all roles: the case
// reclaimWholeReplicas and roundFillsToReplicas round by summing GPUs. A model
// with more than one size, in one role or across roles, is rounded by an
// exact pod search.
func uniformPods(variants []variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	size := 0
	for _, vc := range variants {
		g := gpusPerReplicaFromState(stateMap, vc.VariantName)
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		if size != 0 && g != size {
			return false
		}
		size = g
	}
	return true
}

// fillItems are role's variants fillRole can grow, in its cost order, with
// the pods each has room for under its maxReplicas.
func fillItems(variants []variantRecord, role string, stateMap map[string]domain.VariantReplicaState,
	targets map[string]int, unit, bound int) []podItem {
	var out []podItem
	for _, vc := range sortByCostEfficiencyAsc(variantsForRole(variants, role)) {
		if vc.PerReplicaCapacity <= 0 {
			continue
		}
		size := gpusPerReplicaFromState(stateMap, vc.VariantName) / unit
		room := bound / max(size, 1)
		if st := stateMap[vc.VariantName]; st.MaxReplicas != nil && *st.MaxReplicas > 0 {
			room = min(room, max(*st.MaxReplicas-targets[vc.VariantName], 0))
		}
		out = append(out, podItem{name: vc.VariantName, size: size, count: room})
	}
	return out
}

// fillRoleExact adds role's replicas worth the most GPUs at most wantGPUs and
// returns the GPUs consumed. A role of one replica size is filled by fillRole;
// with mixed sizes the pods come from an exact search, so +8 on 6- and 4-GPU
// pods is two 4s, where a greedy pass took a 6 and stopped.
func fillRoleExact(variants []variantRecord, role string, stateMap map[string]domain.VariantReplicaState,
	targets map[string]int, wantGPUs int) int {
	if uniformReplicaGPUs(variants, role, stateMap) {
		return fillRole(variants, role, stateMap, targets, wantGPUs)
	}
	unit := podUnit(variantsForRole(variants, role), stateMap)
	bound := toUnits(wantGPUs, unit)
	items := fillItems(variants, role, stateMap, targets, unit, bound)
	amount := podSums(items, bound).maxAtMost(bound)
	spent := 0
	for i, n := range composePods(items, amount) {
		targets[items[i].name] += n
		spent += n * items[i].size * unit
	}
	return spent
}

// podPlan is one side of a model's change, its fill or its shed, for the
// exact search: per role, the sums of pods it can make (sets), at most caps
// each, the share it was asked for (ideal), and, for a shed, the pods
// themselves (items); bound caps the side's sum. All in units.
type podPlan struct {
	roles []string
	sets  []podBits
	caps  []int
	ideal []int
	items [][]podItem
	bound int
}

// fillPlan is the fill side: the roles not cut (tgt at or above cur) and not
// under a scale-up hold, each taking no more than its demand dem still asks
// for or its own share rounded up to one replica, the bound the greedy pass
// applies, and all of them no more than the summed growth of every role not
// cut. A held role is left out of the fill, which skips it anyway: in it, a
// shed paid for pods the fill never placed. Its growth still counts toward
// the bound, as it did when the role was searched.
func fillPlan(e NamedAnalyzerResult, records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, tgt, cur, dem, targets map[string]int, unit int) podPlan {
	var p podPlan
	growth := 0
	for _, role := range roles {
		if tgt[role] >= cur[role] {
			growth += tgt[role] - cur[role]
		}
	}
	p.bound = toUnits(growth, unit)
	for _, role := range roles {
		if tgt[role] < cur[role] || e.Holds[role].NoScaleUp {
			continue
		}
		want := tgt[role] - cur[role]
		limit := max(dem[role]-cur[role], ceilToReplica(want, roleReplicaStepGPUs(records, role, stateMap)))
		c := min(toUnits(limit, unit), p.bound)
		p.roles = append(p.roles, role)
		p.caps = append(p.caps, c)
		p.ideal = append(p.ideal, toUnits(want, unit))
		p.sets = append(p.sets, podSums(fillItems(records, role, stateMap, targets, unit, c), c))
	}
	return p
}

// shedPlan is the shed side: the roles cut (tgt below cur) and not under a
// scale-down hold, each no further than its cut rounded up to its smallest
// removable pod, its pods most expensive first, and all of them no more than
// the summed cut, each role counted no further than it can give. Each
// variant keeps its minReplicas, and a role keeps its last replica when it
// has a share, in tgt or in keep, or the cheapest-at-one rule protects it.
func shedPlan(e NamedAnalyzerResult, records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, tgt, cur, keep, targets map[string]int, unit int) podPlan {
	var p podPlan
	total := 0
	for _, role := range roles {
		if tgt[role] >= cur[role] || e.Holds[role].NoScaleDown {
			continue
		}
		cut := cur[role] - tgt[role]
		ordered := sortVariantsForScaleDown(e, variantsForRole(records, role))
		var its []podItem
		held, removable, step := 0, 0, 0
		for _, vc := range ordered {
			g := gpusPerReplicaFromState(stateMap, vc.VariantName)
			n := targets[vc.VariantName]
			held += n * g
			if vc.PerReplicaCapacity <= 0 {
				continue
			}
			if st := stateMap[vc.VariantName]; st.MinReplicas != nil {
				n -= *st.MinReplicas
			}
			if n <= 0 {
				continue
			}
			its = append(its, podItem{name: vc.VariantName, size: g / unit, count: n})
			removable += n * g
			if step == 0 || g < step {
				step = g
			}
		}
		top := toUnits(removable, unit)
		set := podSums(its, top)
		// Shedding all the role holds is the one way to reach its top sum.
		// It is barred when the role has a share or its cheapest variant
		// still holds a replica (the cheapest-at-one rule).
		if removable > 0 && removable == held && top == removable/unit &&
			(tgt[role] > 0 || keep[role] > 0 || targets[ordered[len(ordered)-1].VariantName] >= 1) {
			set.unset(top)
		}
		can := set.maxAtMost(top)
		if can <= 0 {
			continue
		}
		p.roles = append(p.roles, role)
		p.sets = append(p.sets, set)
		p.items = append(p.items, its)
		p.caps = append(p.caps, toUnits(ceilToReplica(cut, step), unit))
		p.ideal = append(p.ideal, toUnits(cut, unit))
		// Summed in GPUs: two cuts each short of a unit still make one.
		total += min(cut, can*unit)
	}
	p.bound = toUnits(total, unit)
	return p
}

// roundExact rounds the role shares tgt of a model with more than one replica
// size to whole pods, in place of roundFillsToReplicas and
// reclaimWholeReplicas: it sets each growing role's tgt to what its fill will
// place and sheds the cut roles' pods from targets. split is the role shares
// at any target, tgt being split(target).
//
// At one target, the fill is searched within the growing roles' growth and
// the shed within the cut roles' cut, each exactly over the pods' sums, and of
// every pair of a fill and a shed the model takes the one that leaves it the
// most GPUs without passing the room (planExact). The room is what the target
// adds above what the model holds, or what the shares add when they sum to
// more. Bounded by it, the model ends at or below its target, or where it is
// when the target is the lower: rounded alone, a role rounded up to a whole
// pod left the next cycle a cut no pod meets while the other role grew, and
// the model ended a partial pod above its target (#421). A model shrinking
// toward its target may now end up to a partial pod below it, where rounding
// alone left it above.
//
// That plan alone is not monotone in the target: one GPU more can turn a cut
// of a pod into a cut no pod meets, and the pair that shed the pod is gone.
// So the model takes the best plan of this target and every lower one, the
// one that leaves it the most GPUs, the plan of the highest such target among
// equals, the one whose shares are nearest this target's: a lower target's
// plan may shed a pod this target's cut does not meet
// (TestRescaleMixedEqualPlansTakeTheHighestTarget). No lower target's room
// exceeds this one's, so the model still ends at or below its target, or at
// the shares when floors put them above it. The scan stops at the first
// lower target whose room cannot beat the best so far, or once the best plan
// adds as much as its own fill can, since no lower target's room or fill is
// larger.
//
// A lower target's plan keeps the last replica of every role this target
// gives a share, since the rescale may not empty it. That is the one case in
// which a larger target can leave fewer GPUs: where a role's share rises from
// zero, the lower target's best plan emptied it, and no plan that keeps it
// reaches as far without passing the target
// (TestRescaleMixedKeepingARoleCanCostGPUs).
//
// Among pairs of equal GPUs the one that sheds most is taken, so a model at
// its target still moves a pod between its roles. A cut of a pod or more has
// a pair at or below the room that sheds one; when no pair is at or below the
// room, the model sheds what it can and grows nothing.
func roundExact(e NamedAnalyzerResult, records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, split func(int) map[string]int, target int, tgt, cur, dem, targets map[string]int) {
	unit := podUnit(records, stateMap)
	best := planExact(e, records, stateMap, roles, target, tgt, cur, dem, nil, targets, unit)
	prev := tgt
	for t := target - 1; t >= 0 && best.net < best.fillMax; t-- {
		shares := split(t)
		if maps.Equal(shares, prev) {
			continue
		}
		prev = shares
		if floorUnits(planRoom(roles, t, shares, cur), unit) <= best.net {
			break
		}
		if p := planExact(e, records, stateMap, roles, t, shares, cur, dem, tgt, targets, unit); p.net > best.net {
			best = p
		}
	}
	// A role the plan does not fill is capped at what it holds, so the fill
	// does not grow it: one growing at this target may be cut at the plan's.
	for _, role := range roles {
		if v, ok := best.tgt[role]; ok {
			tgt[role] = v
		} else {
			tgt[role] = min(tgt[role], cur[role])
		}
	}
	for name, n := range best.shed {
		targets[name] -= n
	}
}

// planRoom is what a plan of target with role shares tgt may add to the
// model, in GPUs: what the target adds above what the model holds cur, or
// what the shares add when they sum to more. Every role at its ceiling leaves
// the shares short of the target; bounded by the shares, a role whose cut no
// pod meets kept the other role from growing to its share with the target's
// GPUs unused.
func planRoom(roles []string, target int, tgt, cur map[string]int) int {
	shares, held := 0, 0
	for _, role := range roles {
		shares += tgt[role]
		held += cur[role]
	}
	return max(shares, target) - held
}

// exactPlan is a rounding of a model's role shares to whole pods: the GPUs
// each growing role ends at (tgt) and the pods shed per variant (shed). net
// is what the plan adds to the model, in units, and fillMax the most the
// plan's fill could add, which bounds the net of every plan of a lower target.
type exactPlan struct {
	tgt     map[string]int
	shed    map[string]int
	net     int
	fillMax int
}

// planExact rounds the role shares tgt of target to whole pods: of every pair
// of a fill within the growing roles' growth and a shed within the cut roles'
// cut, the one whose net is the largest at most the room (netPlan). A role
// with a share in keep keeps its last replica as one with a share in tgt does.
func planExact(e NamedAnalyzerResult, records []variantRecord, stateMap map[string]domain.VariantReplicaState,
	roles []string, target int, tgt, cur, dem, keep, targets map[string]int, unit int) exactPlan {
	fill := fillPlan(e, records, stateMap, roles, tgt, cur, dem, targets, unit)
	shed := shedPlan(e, records, stateMap, roles, tgt, cur, keep, targets, unit)
	fillLayers := jointLayers(fill.sets, fill.caps, fill.bound)
	fills := fillLayers[len(fillLayers)-1]
	shedLayers := jointLayers(shed.sets, shed.caps, shed.bound)
	grow, drop := netPlan(fills, fill.bound, shedLayers[len(shedLayers)-1], shed.bound,
		floorUnits(planRoom(roles, target, tgt, cur), unit))
	p := exactPlan{tgt: map[string]int{}, shed: map[string]int{}, net: grow - drop,
		fillMax: max(fills.maxAtMost(fill.bound), 0)}
	for i, a := range jointSplit(fillLayers, fill.sets, fill.caps, grow, fill.ideal) {
		p.tgt[fill.roles[i]] = cur[fill.roles[i]] + a*unit
	}
	for i, s := range jointSplit(shedLayers, shed.sets, shed.caps, drop, shed.ideal) {
		for j, n := range composePods(shed.items[i], s) {
			p.shed[shed.items[i][j].name] += n
		}
	}
	return p
}

// netPlan picks a fill grow from fills and a shed drop from sheds, each at
// most its bound, whose net grow-drop is the largest at most room, all in
// units; among equal nets, the larger drop. When no pair is at most room it
// sheds the most it can and grows nothing.
func netPlan(fills podBits, fillBound int, sheds podBits, shedBound, room int) (grow, drop int) {
	found := false
	for s := sheds.maxAtMost(shedBound); s >= 0; s = sheds.maxAtMost(s - 1) {
		g := fills.maxAtMost(min(fillBound, room+s))
		if g >= 0 && (!found || g-s > grow-drop) {
			grow, drop, found = g, s, true
		}
	}
	if !found {
		return 0, max(sheds.maxAtMost(shedBound), 0)
	}
	return grow, drop
}

// floorUnits is gpus in whole units, rounded down: a negative room of part
// of a unit is a whole unit below zero.
func floorUnits(gpus, unit int) int {
	if gpus >= 0 {
		return gpus / unit
	}
	return -((-gpus + unit - 1) / unit)
}
