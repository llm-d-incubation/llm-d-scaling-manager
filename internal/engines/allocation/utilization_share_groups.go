package allocation

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
)

// MaxShareBudgetGPUs bounds the budget a group is evaluated over. No cluster
// holds a million GPUs of one type, so a larger budget is a quota written as
// "effectively unlimited", and it is treated as unlimited: sharing it would
// mean placing replicas one at a time across a bound nothing enforces. It also
// keeps free + held clear of integer overflow.
const MaxShareBudgetGPUs = 1 << 20

// ShareVariant is the variant of a role a transfer acts on.
type ShareVariant struct {
	Name string
	// GPUs is one replica of it.
	GPUs int
	// Current is its replica count now.
	Current int
	// Min and Max are its replica bounds; Max 0 is unbounded.
	Min, Max int
	// PodGPUs is each pod's GPUs in one replica; nil when unknown.
	PodGPUs []int
}

// ShareCovers reports whether a donor replica's pods can fund a receiver
// replica's (§6.5): every receiver pod needs a donor pod of its own at least
// its size, since nothing shows that several smaller donor pods share a node.
// The donor's surplus returns to the free budget. With either shape unknown it
// falls back to the replicas' totals.
func ShareCovers(donor, receiver ShareVariant) bool {
	return decision.PodsCover(donor.PodGPUs, donor.GPUs, receiver.PodGPUs, receiver.GPUs)
}

func shareVariantOf(vc variantRecord, stateMap map[string]domain.VariantReplicaState) ShareVariant {
	st := stateMap[vc.VariantName]
	v := ShareVariant{Name: vc.VariantName, GPUs: gpusPerReplicaFromState(stateMap, vc.VariantName), Current: st.CurrentReplicas}
	if st.MinReplicas != nil {
		v.Min = *st.MinReplicas
	}
	if st.MaxReplicas != nil {
		v.Max = *st.MaxReplicas
	}
	v.PodGPUs = st.PodGPUs
	return v
}

// ShareRoleOrigin names the model and role a ShareRole was built from.
type ShareRoleOrigin struct {
	Namespace string
	ModelID   string
	Role      string
}

// ShareGroup is one closed budget group of the utilization-share optimizer: an
// accelerator type paired with a budget scope (proposal §2, §6.1).
type ShareGroup struct {
	AcceleratorType string
	// Scope is the namespace of a namespace-quota group, "" for the cluster group.
	Scope string
	// Budget is what the group plans over: the budget's free GPUs plus what its
	// planned roles hold now. Models that are not planned -- frozen, or spanning
	// several accelerator types -- hold GPUs that are simply not added back, so
	// they come out of the budget exactly once.
	Budget int
	Roles  []ShareRole
	// Committed is the GPUs each role holds now: scheduled pods, terminating
	// ones included (HeldReplicas), falling back to the replica count when the
	// pods could not be listed. The ledger adjusts it for transfers in flight.
	Committed map[string]int
	// Filled is, per role, the GPUs of its replicas every pod of which is
	// scheduled (FilledReplicas): what a receiver's fill is judged by, so an
	// LWS group whose leader alone is bound does not end a fill. Nil judges
	// fills by Committed.
	Filled map[string]int
	// Give and Grow name the variant of each role that gives a replica (the
	// most expensive with replicas above its floor) and the one that grows (the
	// cheapest with room below its ceiling), as rescale reclaims and fills.
	// A role absent from Give has nothing it can give.
	Give, Grow map[string]ShareVariant
	// Variants lists every variant of each planned role. Once the optimizer is
	// active it owns all their targets, not only the ones a transfer touches.
	Variants map[string][]ShareVariant
	// ClusterFree bounds what a fill may place this cycle: the free GPUs of the
	// type under every cluster bound -- the physical inventory and a
	// cluster-scope quota, intersected (mergeConstraints) -- which a namespace
	// quota can exceed (as in applyRescale). math.MaxInt when no cluster bound
	// is in force.
	//
	// It was PhysicalFree. Once a physical limiter and quotas compose it is no
	// longer physical: it is the tightest cluster-wide figure, and a
	// namespace-scope quota's aggregate (the sum of its namespace caps) is not
	// one -- that quota bounds a namespace group through its own budget instead.
	//
	// Groups that draw on one cluster key share it: two groups on two products
	// of one family (a cluster quota of {NVIDIA-H100-80GB-HBM3: 8,
	// NVIDIA-H100-SXM5-80GB: 8} on 8 physical H100s), or two namespace groups on
	// one type, would each fill the whole figure. Their ClusterFree is that key's
	// free split between them (splitClusterFree), so the fills together never
	// place more than it holds.
	//
	// So with a namespace-scope quota alone ClusterFree is unbounded, and that
	// is safe: a namespace group's Budget is its own cap (net of what it holds)
	// and the idle fill never places past Budget (shareIdle, fillIdleShare), so
	// every namespace stays within its own cap and their sum within the sum of
	// the caps. The old aggregate-free bound was tighter in one case only: when
	// ANOTHER namespace already holds more than its own cap, the aggregate's
	// free figure fell below this namespace's, coupling one tenant's fill to
	// another's overage. That coupling is gone.
	//
	// The cluster group's is what the cluster bounds leave after the namespace
	// groups' claims (BuildShareGroups), before that split.
	ClusterFree int
	// PoolCarve is the warm pools' unheld target taken out of Budget.
	PoolCarve int
	// OverQuota is how many GPUs the group's scope holds past what its quota
	// allows on the group's type (overQuota), taken out of Budget: a scope
	// over its quota plans to come back within it. 0 when it is within.
	OverQuota int
	// Thresholds is each role's scale-up threshold k_r.
	Thresholds map[string]float64
	// DownThresholds is each role's scale-down boundary: the utilization
	// below which today's optimizer scales it down.
	DownThresholds map[string]float64
	// StartSeconds is, for each role with a measured variant, how long a new
	// replica of its slowest measured variant takes to become Ready
	// (domain.AnalyzerResult.RoleStartSeconds); a role not measured is absent.
	StartSeconds map[string]float64
	// StartSeeded names each role of StartSeconds whose start is its
	// ScaledObject's replicaStartSeconds seed, nothing of it measured
	// (domain.AnalyzerResult.RoleStartSeeded).
	StartSeeded map[string]bool
	// BusiestQueueLoad is, for each role with a reading above zero, the
	// deepest per-pod waiting queue over its variants on this group's
	// accelerator, as a fraction of the saturation queue
	// (domain.AnalyzerResult.VariantBusiestQueueLoad).
	BusiestQueueLoad map[string]float64
	Origins          map[string]ShareRoleOrigin
	// Frozen lists the models of this group that were not planned this cycle,
	// as "namespace/model: why".
	Frozen []string

	listed int // the planned roles' GPUs by replica count, for the budget
}

// UnsharedExcludedNamespace is why a model of a namespace a namespace-scoped
// quota excludes is planned in no group when no finite cluster-scoped quota
// covers its accelerator type acc: such a namespace bypasses the namespace
// quota, so the only budget it could be planned over is the other tenants'
// free quota, which does not count what it holds. A cluster quota of -1 for
// the type is unlimited and counts as none.
func UnsharedExcludedNamespace(acc string) string {
	return fmt.Sprintf("excluded from the namespace quota, and no finite scope: cluster quota covers %s; add one to share it", acc)
}

// ShareGroupOptions controls which groups BuildShareGroups returns.
type ShareGroupOptions struct {
	// ClusterIsQuota reports whether a cluster-scoped quota bounds the cluster
	// group: a cluster-scope quota entry is declared, alone or beside a
	// physical limiter. A namespace-scoped quota is not one: it bounds only the
	// namespaces it caps. When ClusterIsQuota is false the cluster-scope budget
	// is the physical inventory, and the cluster group is built only with
	// PhysicalGroups.
	ClusterIsQuota bool
	PhysicalGroups bool
	// Unshared, when set, is called for each model planned in no group
	// because no budget bounds it, with why (UnsharedExcludedNamespace(acc)).
	// Today's optimizer keeps such a model.
	Unshared func(req ModelScalingRequest, why string)
	// NamespaceEnabled reports whether a namespace-quota group takes part. Nil
	// means every namespace does.
	NamespaceEnabled func(namespace string) bool
	// Weight resolves a request's model weight. clusterScope is true when the
	// model is planned in the cluster group, where models of different
	// namespaces share one budget.
	Weight func(req ModelScalingRequest, clusterScope bool) float64
	// Exclude reports why a request must not be planned in its group, or "".
	// An excluded model is frozen: today's optimizer keeps it, and its GPUs
	// stay outside the group's budget.
	Exclude func(req ModelScalingRequest, clusterScope bool) string
	// ReserveGPUs are held out of every group's budget, so a scale-from-zero
	// wake finds them free in the quota without waiting for a transfer
	// (proposal section 7.2). Spending them leaves the group over its budget,
	// which the planner pays back first (SharePlan.Refills).
	ReserveGPUs int
	// PoolUnheld is, per namespace and accelerator, what the warm pools want
	// and do not hold. It is carved out of the budget of every group whose
	// bound a pool's growth spends -- its namespace's own quota group, and the
	// cluster group, which every namespace's GPUs count against -- so the pools
	// can grow into GPUs the optimizer would otherwise fill (proposal section
	// 7.2).
	PoolUnheld map[string]map[string]int
	// Left is what today's optimizer pass left, when one ran before: every
	// group's budget and ClusterFree are then net of what the pass ordered for
	// the models it does NOT plan -- the frozen ones, the other accelerators',
	// the excluded namespaces'. What it ordered for a planned model is not
	// taken: the share overrides that model's target. Nil plans on the
	// constraints as they are.
	Left *PassLeft
}

// PlannedModels is the models (namespace/modelID) the groups plan: every
// model a role of which is in some group.
func PlannedModels(groups []ShareGroup) map[string]bool {
	out := map[string]bool{}
	for _, g := range groups {
		for _, o := range g.Origins {
			out[utils.GetNamespacedKey(o.Namespace, o.ModelID)] = true
		}
	}
	return out
}

// BuildShareGroups turns one cycle's requests and constraints into the groups
// the utilization-share optimizer evaluates. A model is planned when its
// composite signal is live and informative and all its variants share one
// accelerator type; otherwise it is frozen for the cycle (proposal §6.1 step 1).
//
// Committed is today's replica count, not yet the GPUs held including
// terminating pods: that count (VariantReplicaState.HeldReplicas) is a
// prerequisite of actuation, not of the shadow evaluation this serves.
func BuildShareGroups(requests []ModelScalingRequest, constraints []*ResourceConstraints, opts ShareGroupOptions) []ShareGroup {
	if len(constraints) == 0 {
		return nil
	}
	declared := declaredAccelerators(requests)
	available, namespaceQuota := clusterBudgets(constraints, declared...)
	availableByNS := mergeNamespaceConstraints(constraints, declared...)

	type key struct{ acc, scope string }
	groups := make(map[key]*ShareGroup)
	group := func(acc, scope string) *ShareGroup {
		k := key{acc, scope}
		g, ok := groups[k]
		if !ok {
			g = &ShareGroup{
				AcceleratorType:  acc,
				Scope:            scope,
				Committed:        map[string]int{},
				Filled:           map[string]int{},
				Give:             map[string]ShareVariant{},
				Grow:             map[string]ShareVariant{},
				Variants:         map[string][]ShareVariant{},
				Thresholds:       map[string]float64{},
				DownThresholds:   map[string]float64{},
				StartSeconds:     map[string]float64{},
				StartSeeded:      map[string]bool{},
				BusiestQueueLoad: map[string]float64{},
				Origins:          map[string]ShareRoleOrigin{},
			}
			groups[k] = g
		}
		return g
	}

	for _, req := range requests {
		records := recordsForRequest(req)
		if records == nil {
			continue
		}
		acc, single := singleAccType(records)
		if !single {
			continue // multi-accelerator: a fixed consumer; its GPUs are already out of the free budget
		}
		nsBudget, nsScoped := availableByNS[req.Namespace]
		scope := ""
		budgets := available
		if nsScoped {
			if opts.NamespaceEnabled != nil && !opts.NamespaceEnabled(req.Namespace) {
				continue
			}
			scope = req.Namespace
			budgets = nsBudget
		} else {
			// Every active namespace a namespace-scoped quota does not exclude
			// is in its pools, so a namespace outside them is excluded. Only a
			// cluster bound bounds it; without a finite one it is not shared,
			// whatever ClusterIsQuota and PhysicalGroups say.
			free, ok := available[budgetKey(available, acc)]
			if namespaceQuota && (!ok || isUnboundedBudget(free) || free > MaxShareBudgetGPUs) {
				if opts.Unshared != nil {
					opts.Unshared(req, UnsharedExcludedNamespace(acc))
				}
				continue
			}
			if !opts.ClusterIsQuota && !opts.PhysicalGroups {
				continue
			}
		}
		// The group takes the budget's spelling of the accelerator, so models that
		// spell one GPU differently share one group and its budget is found below.
		acc = budgetKey(budgets, acc)
		g := group(acc, scope)

		why := ""
		if opts.Exclude != nil {
			why = opts.Exclude(req, scope == "")
		}
		var roles []shareRoleInput
		if why == "" {
			roles, why = shareRolesForRequest(req, records, acc, opts.Weight, scope == "")
		}
		if why != "" {
			g.Frozen = append(g.Frozen, modelKey(req)+": "+why)
			continue
		}
		stateMap := buildStateMap(req.VariantStates)
		for _, r := range roles {
			o := ShareRoleOrigin{Namespace: req.Namespace, ModelID: req.ModelID, Role: r.role}
			g.Roles = append(g.Roles, r.ShareRole)
			g.Committed[r.Key] = roleHeldGPUs(records, stateMap, acc, r.role)
			g.Filled[r.Key] = roleFilledGPUs(records, stateMap, acc, r.role)
			g.listed += roleCurrentGPUs(records, stateMap, acc, r.role)
			g.Thresholds[r.Key] = req.CompositeSignal.ScaleUpThreshold
			g.DownThresholds[r.Key] = req.CompositeSignal.ScaleDownBoundary
			if r.startSeconds > 0 {
				g.StartSeconds[r.Key] = r.startSeconds
				if r.startSeeded {
					g.StartSeeded[r.Key] = true
				}
			}
			if r.busiestQueueLoad > 0 {
				g.BusiestQueueLoad[r.Key] = r.busiestQueueLoad
			}
			g.Origins[r.Key] = o
			vs := variantsForRole(variantsOnType(records, acc), r.role)
			for _, vc := range vs {
				g.Variants[r.Key] = append(g.Variants[r.Key], shareVariantOf(vc, stateMap))
			}
			// A held role keeps what it has: it is sized on its unheld demand
			// with a floor at what it holds (shareRolesForRequest) and gives
			// nothing until the hold ends.
			if give, ok := shareGiveVariant(vs, stateMap); ok && !r.Held {
				g.Give[r.Key] = give
			}
			if grow, ok := shareGrowVariant(vs, stateMap); ok {
				g.Grow[r.Key] = grow
			}
		}
	}

	keys := slices.SortedFunc(maps.Keys(groups), func(a, b key) int {
		return cmp.Or(cmp.Compare(a.acc, b.acc), cmp.Compare(a.scope, b.scope))
	})
	freeOf := func(k key) (int, bool) {
		free, ok := available[k.acc]
		if k.scope != "" {
			free, ok = availableByNS[k.scope][k.acc]
		}
		// Unlimited, unknown, or no real bound: nothing to "use all of"
		// (proposal §7.3).
		return free, ok && free >= 0 && free <= MaxShareBudgetGPUs
	}
	if opts.Left != nil {
		// What the pass ordered for the models no group plans is spent; what
		// it ordered for a planned model the share decides again.
		var kept []ShareGroup
		for _, k := range keys {
			if _, ok := freeOf(k); ok {
				kept = append(kept, *groups[k])
			}
		}
		available, availableByNS = maps.Clone(available), cloneNamespaceBudgets(availableByNS)
		opts.Left.Replay(available, availableByNS, PlannedModels(kept))
	}
	// Every group draws on the same cluster budget: a namespace group may fill
	// up to its own namespace's free quota, and every GPU it places is also
	// charged to the cluster budget (a cluster quota counts every namespace).
	// So the cluster group gets only what the cluster budget leaves after the
	// namespace groups' largest claim: the cluster free less, per accelerator
	// type, the sum of the namespace groups' claims, floored at 0. A namespace
	// claims its free quota in whole replicas of the smallest one it can still
	// grow: its group's grow step (growStepGPUs), which is all its fills place,
	// or a variant below its ceiling of a model no group plans (claimSteps),
	// which today's optimizer may grow. With 8-GPU replicas, 4 free GPUs place
	// nothing, and claiming them would leave a free node unused by every
	// group. A 1-GPU variant at its ceiling, or one the group does not grow,
	// does not shrink the step, and a namespace that can grow nothing on the
	// type claims nothing. (Capping each claim at the
	// cluster free first gives the same remainder: once one claim reaches it,
	// nothing is left either way.) This keeps the cluster group from counting
	// GPUs a namespace group may also fill. A claim is charged to the cluster
	// key that bounds the namespace group's type (budgetKey), the key the
	// cluster group of that type is formed on, and is net of what the pass
	// left (opts.Left). The namespace groups are bounded against each other,
	// and against the cluster group, by splitClusterFree.
	//
	// EVERY namespace the namespace quota caps claims, whether or not a group
	// formed for it this cycle: its models may grow into that headroom through
	// today's optimizer just the same. A namespace disabled for sharing
	// (NamespaceEnabled), or whose models span several accelerator types,
	// forms no group, and left out of the claims its whole free quota was idle
	// fill for the cluster group. A namespace with no model on the type yet is
	// rounded to the cluster group's smallest replica of that type, the best
	// guess; with no replica size known anywhere it is not rounded.
	//
	// A claim covers the namespace's free quota in whole replicas, its warm
	// pools' headroom included, so the cluster group carves a claimed
	// namespace's pools only up to what rounding left unclaimed (poolCarve,
	// unclaimed), and the pools of namespaces that made no claim whole.
	claimed := map[string]int{}
	unclaimed := map[string]map[string]int{}
	planned := map[string]bool{}
	for _, g := range groups {
		for _, o := range g.Origins {
			planned[utils.GetNamespacedKey(o.Namespace, o.ModelID)] = true
		}
	}
	steps := claimSteps(requests, availableByNS, planned)
	for _, ns := range slices.Sorted(maps.Keys(availableByNS)) {
		for _, acc := range slices.Sorted(maps.Keys(availableByNS[ns])) {
			nsFree, ok := freeOf(key{acc, ns})
			if !ok {
				continue
			}
			step := 0
			if g := groups[key{acc, ns}]; g != nil {
				step = growStepGPUs(g)
			}
			// The models no group plans grow through today's optimizer.
			unplanned, hasModel := steps[ns][acc]
			if unplanned > 0 && (step == 0 || unplanned < step) {
				step = unplanned
			}
			// A namespace whose models on the type all sit at their ceiling,
			// or are held from scaling up, places nothing: it claims none.
			placesNone := step == 0 && hasModel
			if g := groups[key{budgetKey(available, acc), ""}]; step <= 0 && g != nil {
				step = smallestReplicaGPUs(g)
			}
			rest := 0
			switch {
			case placesNone:
				rest = nsFree
			case step > 0:
				rest = nsFree % step
			}
			nsFree -= rest
			claimed[budgetKey(available, acc)] += nsFree
			if unclaimed[ns] == nil {
				unclaimed[ns] = map[string]int{}
			}
			unclaimed[ns][acc] = rest
		}
	}

	out := make([]ShareGroup, 0, len(groups))
	for _, k := range keys {
		g := groups[k]
		free, ok := freeOf(k)
		if !ok {
			continue
		}
		clusterFree := math.MaxInt
		if cf, bounded := clusterBudgetFor(available, k.acc); bounded && cf >= 0 && cf < math.MaxInt {
			clusterFree = cf
		}
		if k.scope == "" {
			free = max(0, free-claimed[k.acc])
			clusterFree = min(clusterFree, free)
		}
		// free is net of what the group's planned roles hold by replica
		// count: a namespace group's is its own namespace's quota, and the
		// cluster group's a budget over every namespace (mergeConstraints) -- a
		// cluster quota, charged what the scaling manager's variants hold, or the physical
		// inventory, charged every GPU on the nodes. So the budget adds back
		// that count -- not the held figure, which also counts terminating
		// pods. Those then show up as committed and not idle, which is the
		// point of counting them.
		if g.listed > MaxShareBudgetGPUs-free {
			continue
		}
		// free is floored at 0 (ResourcePool.Available), so a scope that
		// holds more than its quota reads as exactly full, and free plus what
		// it holds is what it holds, not what it may hold. Its overage comes
		// out of the budget, which the planner then pays back as a debt. Only
		// an allowance the group's own members spent past it is given back:
		// never what Kueue granted, nor a physical over-count
		// (namespaceAllowances, clusterAllowances, clusterOverQuota).
		if k.scope != "" {
			g.OverQuota = overQuota(namespaceAllowances(constraints, k.scope), k.acc)
		} else if opts.ClusterIsQuota {
			g.OverQuota = clusterOverQuota(clusterAllowances(constraints), k.acc, g.listed)
		}
		g.PoolCarve = poolCarve(opts.PoolUnheld, availableByNS, unclaimed, k.acc, k.scope)
		g.Budget = max(0, free+g.listed-g.OverQuota-opts.ReserveGPUs-g.PoolCarve)
		g.ClusterFree = clusterFree
		slices.SortFunc(g.Roles, func(a, b ShareRole) int { return cmp.Compare(a.Key, b.Key) })
		slices.Sort(g.Frozen)
		out = append(out, *g)
	}
	splitClusterFree(out, available)
	return out
}

// claimSteps is, per namespace in availableByNS and per its budget key, the
// fewest GPUs one replica takes of any variant on that type that today's
// optimizer may grow for a model no share group plans (planned, by
// namespace/model): frozen, spanning several accelerator types, or in a
// namespace that forms no group. Whole replicas of it are what those models
// can place there. A variant at its maxReplicas, or of a role the composite
// holds from scaling up, grows nothing, so it does not count. The step is 0
// when the namespace has a model on the type but none of those variants can
// grow, and absent when it has no model on the type.
func claimSteps(requests []ModelScalingRequest, availableByNS map[string]map[string]int, planned map[string]bool) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, req := range requests {
		budgets, ok := availableByNS[req.Namespace]
		if !ok {
			continue
		}
		stateMap := buildStateMap(req.VariantStates)
		for _, vc := range recordsForRequest(req) {
			k := budgetKey(budgets, vc.AcceleratorName)
			if out[req.Namespace] == nil {
				out[req.Namespace] = map[string]int{}
			}
			cur := out[req.Namespace][k]
			out[req.Namespace][k] = cur
			if planned[modelKey(req)] || !unplannedCanGrow(req, vc, stateMap) {
				continue
			}
			if gpus := gpusPerReplicaFromState(stateMap, vc.VariantName); cur == 0 || gpus < cur {
				out[req.Namespace][k] = gpus
			}
		}
	}
	return out
}

// unplannedCanGrow reports whether today's optimizer may add a replica of vc:
// it is below its maxReplicas, and the composite does not hold its role from
// scaling up.
func unplannedCanGrow(req ModelScalingRequest, vc variantRecord, stateMap map[string]domain.VariantReplicaState) bool {
	st := stateMap[vc.VariantName]
	if st.MaxReplicas != nil && *st.MaxReplicas > 0 && st.CurrentReplicas >= *st.MaxReplicas {
		return false
	}
	role := vc.Role
	if role == "" {
		role = domain.RoleBoth
	}
	return !req.CompositeSignal.Holds[role].NoScaleUp
}

// growStepGPUs is the smallest step g can grow by: the fewest GPUs one
// replica of a variant it grows (Grow) takes, since its fills and transfers
// place only those. A 1-GPU variant g does not grow places nothing, so it does
// not count: beside an 8-GPU variant g grows, it let g claim GPUs only an
// 8-GPU replica could use. It is 0 when g grows nothing: every role is at its
// ceiling, so its fills place nothing.
func growStepGPUs(g *ShareGroup) int {
	step := 0
	for _, v := range g.Grow {
		if v.GPUs > 0 && (step == 0 || v.GPUs < step) {
			step = v.GPUs
		}
	}
	return step
}

// smallestReplicaGPUs is the fewest GPUs one replica of any of g's variants
// takes. It is 0 when no variant says.
func smallestReplicaGPUs(g *ShareGroup) int {
	step := 0
	for _, vs := range g.Variants {
		for _, v := range vs {
			if v.GPUs > 0 && (step == 0 || v.GPUs < step) {
				step = v.GPUs
			}
		}
	}
	return step
}

// clusterBudgets returns, per accelerator key, the free GPUs under the
// providers whose pools bound every namespace -- a cluster-scoped quota, or
// the physical inventory, intersected (mergeConstraints) -- and whether any
// provider is namespace-scoped. A namespace-scoped quota's pools are the sum of
// the caps of the namespaces it lists and are net of only their holdings: a
// namespace it excludes holds GPUs that sum never counts, so a cluster group
// planned over it would grow its budget one for one with what that namespace
// holds.
func clusterBudgets(constraints []*ResourceConstraints, declared ...string) (map[string]int, bool) {
	namespaced := slices.ContainsFunc(constraints, func(c *ResourceConstraints) bool {
		return c != nil && !isClusterBound(c)
	})
	return mergeConstraints(constraints, declared...), namespaced
}

// overQuota is how many GPUs are held under allowlists past what they allow
// on the accelerator called name, read as the budgets read them
// (accelerator.ClusterAllowlistsBound): the free figure floored at 0 less the
// same figure unfloored. 0 when nothing is held past the bound, and when no
// finite bound covers name. A namespace under its quota and a namespace at it
// both read 0; a namespace holding 10 under a quota of 7 reads 3.
func overQuota(allowlists []map[string]ResourcePool, name string) int {
	free, unlimited, bounds := accelerator.ClusterAllowlistsBound(allowlists, name, poolBound)
	if !bounds || unlimited {
		return 0
	}
	left, _, _ := accelerator.ClusterAllowlistsBound(allowlists, name, func(p ResourcePool) accelerator.Bound {
		return accelerator.Bound{Free: p.Limit - p.Used, Unlimited: p.Limit < 0}
	})
	return max(0, free-left)
}

// clusterOverQuota is how many GPUs the cluster group, whose planned roles
// hold own on the accelerator called name, gives back of its cluster quotas'
// overage (overQuota). A cluster quota counts every namespace, and the group
// plans only some of them. While the GPUs held outside the group fit within
// the quota (the overage is at most own), the group holds the overage past
// what the others leave it, and gives it all back. When they already pass the
// quota by themselves, the overage is theirs, and the group gives back only
// what it alone holds past the quota: a group is never planned down for
// another group's overage.
func clusterOverQuota(allowlists []map[string]ResourcePool, name string, own int) int {
	over := overQuota(allowlists, name)
	if over <= own {
		return over
	}
	limit, unlimited, bounds := accelerator.ClusterAllowlistsBound(allowlists, name, func(p ResourcePool) accelerator.Bound {
		return accelerator.Bound{Free: p.Limit, Unlimited: p.Limit < 0}
	})
	if !bounds || unlimited {
		return 0
	}
	return max(0, own-limit)
}

// clusterAllowances is the pools of every cluster quota (clusterBounds) a
// group gives GPUs back against: not the physical inventory, whose count past
// its capacity is a stale read (a pod counted beside the victims it
// nominated), not an allowance spent; and not a quota bounded by Kueue
// (KueueBounded), which admitted what is held and preempts what it must.
func clusterAllowances(constraints []*ResourceConstraints) []map[string]ResourcePool {
	var out []map[string]ResourcePool
	for _, c := range clusterBounds(constraints) {
		if c.ProviderName != PhysicalLimiterName && !c.KueueBounded {
			out = append(out, c.Pools)
		}
	}
	return out
}

// namespaceAllowances is the allowlists of namespace ns a group gives GPUs
// back against: every provider's (namespaceHolders), but of a provider bounded
// by Kueue (KueueBounded) only the types whose borrowing it counted
// (CountedBorrow), held past what Kueue may admit. Its other figures are
// Kueue's, charged with what Kueue reserved for others and, for a namespace
// on several queues, with the smallest queue's reach (KU1, KU2): they say how
// far the namespace may grow, not what Kueue admitted, and Kueue preempts
// what it must.
func namespaceAllowances(constraints []*ResourceConstraints, ns string) []map[string]ResourcePool {
	var out []map[string]ResourcePool
	for _, c := range constraints {
		if c == nil {
			continue
		}
		perType, ok := c.NamespacePools[ns]
		if !ok {
			continue
		}
		if !c.KueueBounded {
			out = append(out, perType)
			continue
		}
		counted := map[string]ResourcePool{}
		for accType, p := range perType {
			if c.CountedBorrow[ns][accType] {
				counted[accType] = p
			}
		}
		if len(counted) > 0 {
			out = append(out, counted)
		}
	}
	return out
}

// splitClusterFree divides each finite cluster key's free GPUs between the
// groups that draw on it -- every group whose accelerator the key covers
// (accelerator.Covers: the key itself, or the family key of a product) -- when
// more than one does, so their fills together stay within it. Each group gets a
// share in proportion to the GPUs it could fill, its Budget less what it
// commits (an even split when none could), never more than its own figure;
// the remainder goes to the largest claims first, then in group order.
//
// Claims and shares are whole replicas of the smallest variant each group
// grows (growStepGPUs), as the namespace groups' claims on the cluster group
// are in BuildShareGroups: a share of 4 GPUs to a group of 8-GPU replicas
// places nothing, and splitting one free 8-GPU node 4 and 4 left it unused by
// every group. A group that grows nothing claims nothing. The remainder is
// handed out a replica at a time, round robin.
func splitClusterFree(groups []ShareGroup, available map[string]int) {
	for _, key := range slices.Sorted(maps.Keys(available)) {
		free := available[key]
		if isUnboundedBudget(free) {
			continue
		}
		var sharing []int
		for i := range groups {
			if accelerator.Covers(key, budgetKey(available, groups[i].AcceleratorType)) {
				sharing = append(sharing, i)
			}
		}
		if len(sharing) < 2 {
			continue
		}
		claims := make([]int, len(sharing))
		steps := make([]int, len(sharing))
		total := 0
		for j, i := range sharing {
			step := growStepGPUs(&groups[i])
			steps[j] = max(1, step)
			claims[j] = min(groups[i].ClusterFree, idleClaim(groups[i]))
			claims[j] -= claims[j] % steps[j]
			if step == 0 {
				claims[j] = 0 // it grows nothing, so its fill places nothing
			}
			total += claims[j]
		}
		if total <= free {
			// Every group's fill already fits beside the others'.
			for j, i := range sharing {
				groups[i].ClusterFree = min(groups[i].ClusterFree, max(claims[j], 0))
			}
			continue
		}
		shares := make([]int, len(sharing))
		given := 0
		for j := range sharing {
			if total > 0 {
				shares[j] = free * claims[j] / total
			} else {
				shares[j] = free / len(sharing)
			}
			shares[j] -= shares[j] % steps[j]
			given += shares[j]
		}
		order := make([]int, len(sharing))
		for j := range order {
			order[j] = j
		}
		slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(claims[b], claims[a]) })
		for progress := true; progress && given < free; {
			progress = false
			for _, j := range order {
				if given+steps[j] > free || (total > 0 && shares[j]+steps[j] > claims[j]) {
					continue
				}
				shares[j] += steps[j]
				given += steps[j]
				progress = true
			}
			if total == 0 {
				break // an even split: one replica more at most, as before
			}
		}
		for j, i := range sharing {
			groups[i].ClusterFree = min(groups[i].ClusterFree, shares[j])
		}
	}
}

// idleClaim is the GPUs a group's idle fill could want this cycle: its Budget
// less what its roles commit, never negative.
func idleClaim(g ShareGroup) int {
	idle := g.Budget
	for _, c := range g.Committed {
		idle -= c
	}
	return max(0, idle)
}

type shareRoleInput struct {
	ShareRole
	role string
	// startSeconds is the role's measured replica start, 0 when not measured
	// (domain.AnalyzerResult.RoleStartSeconds).
	startSeconds float64
	// startSeeded says startSeconds is a seed, not a measurement
	// (domain.AnalyzerResult.RoleStartSeeded).
	startSeeded bool
	// busiestQueueLoad is the deepest per-pod queue over the role's variants
	// on the group's accelerator, as a fraction of the saturation queue
	// (domain.AnalyzerResult.VariantBusiestQueueLoad).
	busiestQueueLoad float64
}

// shareRolesForRequest builds one model's roles on acc, or says why the model
// is frozen this cycle. A P/D model is frozen whole when either role cannot be
// sized: equalizing one role against a guess about the other is the failure
// the freeze exists to prevent.
func shareRolesForRequest(req ModelScalingRequest, records []variantRecord, acc string,
	weight func(ModelScalingRequest, bool) float64, clusterScope bool) ([]shareRoleInput, string) {
	sig := req.CompositeSignal
	switch {
	case sig.Result == nil:
		return nil, "no analyzer result"
	case !sig.Live:
		return nil, "analyzer result not live"
	case !(sig.ScaleUpThreshold > 0):
		return nil, "no scale-up threshold"
	}
	w := 1.0
	if weight != nil {
		w = weight(req, clusterScope)
	}
	stateMap := buildStateMap(req.VariantStates)
	var out []shareRoleInput
	for _, role := range modelRolesOnType(records, acc) {
		hold := sig.Holds[role]
		if hold.NoScaleUp {
			return nil, "role " + role + " has no listed analyzer that can size it"
		}
		demand := sig.Result.TotalDemand
		if role != domain.RoleBoth {
			rc, ok := sig.RoleCapacities[role]
			if !ok {
				return nil, "role " + role + " has no demand reading"
			}
			demand = rc.TotalDemand
		}
		// A hold keeps the fleet where it is by raising its demand to the
		// bottom of the band (scaleDown x supply), which reads as a need of
		// 0.70/0.85 of what the role holds: plus a replica of headroom, every
		// idle fill raised the next, and two idle models filled a quota of 10
		// from 1 and 1 to 5 and 4 on a cluster. The role is sized on its demand
		// before the hold, and the hold's keep is kept by giving nothing and
		// by a floor at what it holds (below).
		before, held := sig.Result.HeldRoleDemand[role]
		if held {
			if raised := sig.Result.RoleDemand[role] - before; raised > 0 {
				demand = max(demand-raised, 0)
			}
		}
		// Another analyzer that sizes the role keeps what it needs at the
		// threshold the share sizes to: as demand, so the role is held no
		// deeper than its own analyzer would hold it and a shortfall is still
		// shared by weight. As a floor at the release target it held more
		// than saturation alone, even when the other analyzer agreed.
		demand = max(demand, sig.OtherFrameDemand(role, sig.ScaleUpThreshold))
		vs := variantsForRole(variantsOnType(records, acc), role)
		busy := 0.0
		for _, v := range vs {
			busy = max(busy, sig.Result.VariantBusiestQueueLoad[v.VariantName])
		}
		best, g, measured := bestVariantForRole(records, stateMap, acc, role)
		need := 0.0
		var capacity []ShareCapacityStep
		if demand > 0 && !measured {
			return nil, "role " + role + " has demand but no measured capacity"
		}
		if measured {
			// need is the GPU form of RequiredCapacity (demand / scale-up
			// threshold), counted in the GPUs the role holds and grows. The
			// curve is built with no demand too: the need the ledger
			// remembers for the role is judged on it.
			required := 0.0
			if demand > 0 {
				required = demand / sig.ScaleUpThreshold
			}
			need, capacity = roleNeedGPUs(required, vs, stateMap, best, g)
		}
		if math.IsNaN(need) || math.IsInf(need, 0) {
			// Every share sum -- the spare, the targets, the peaks that
			// decide contention -- would take it.
			return nil, "role " + role + " has a need that is not a finite number"
		}
		// No group holds more GPUs than this; a larger need, summed with
		// the others', would overflow every share sum to NaN.
		need = math.Min(need, MaxShareBudgetGPUs)
		if !measured && len(vs) > 0 {
			g = gpusPerReplicaFromState(stateMap, vs[0].VariantName)
		}
		minFloor := roleFloorGPUs(records, stateMap, acc, role)
		floor := minFloor
		// A role that holds a replica never gives its last one: parking a
		// model is scale-to-zero's decision -- with its retention and its
		// cheapest-variant rule -- not a transfer's (proposal section 7.2).
		for _, v := range vs {
			if stateMap[v.VariantName].CurrentReplicas > 0 {
				floor = max(floor, gpusPerReplicaFromState(stateMap, v.VariantName))
				break
			}
		}
		// A role that may not shrink keeps what it holds, and so does one the
		// analyzer holds: it gives nothing (BuildShareGroups), so a target
		// below what it holds promised another role GPUs no transfer could
		// take (issue ev-shindin/llm-scaling-manager#222). Its need stays the demand before the hold.
		if hold.NoScaleDown || held {
			floor = max(floor, roleCurrentGPUs(records, stateMap, acc, role))
		}
		ceiling := roleCeilingGPUs(vs, stateMap)
		out = append(out, shareRoleInput{
			ShareRole: ShareRole{
				Key:         modelKey(req) + "/" + role,
				Weight:      w,
				Need:        need,
				Floor:       floor,
				MinFloor:    minFloor,
				Ceiling:     ceiling,
				ReplicaGPUs: max(g, 1),
				Capacity:    capacity,
				Held:        held || hold.NoScaleDown,
			},
			role:             role,
			startSeconds:     sig.Result.RoleStartSeconds[role],
			startSeeded:      sig.Result.RoleStartSeeded[role],
			busiestQueueLoad: busy,
		})
	}
	return out, ""
}

// roleNeedGPUs is the GPUs at which a role serves required, the capacity
// that puts it at its scale-up threshold, counted in the GPUs it holds and
// grows, with the role's capacity curve (ShareRole.Capacity). Every share sum
// and band compares the need with the GPUs a role holds, and a GPU of a
// variant the role does not run is not one of them (issue ev-shindin/llm-scaling-manager#193).
//
// The curve walks the role's GPUs in the order it keeps them, the reverse of
// the order it gives them (shareGiveVariant): the least cost-efficient
// variant above its minReplicas first, down to it, then the next; then the
// replicas minReplicas keeps, in the same order. A role that needs less than
// it serves is priced on that order: it gives whole replicas while what it
// keeps still serves required, and the rest of the need is priced on the next
// replica it would give. A role that needs more grows the rest as a fill does:
// in its most cost-efficient variants first, each up to its maxReplicas, at
// what that variant serves per GPU. What no variant has room for is priced on
// best, the role's most cost-efficient measured variant (gpus GPUs per
// replica); the ceiling caps that part of the claim anyway.
//
// When every GPU the walk reaches serves as much as one of best -- a role with
// one variant, or none running -- the need is required in GPUs of best,
// computed as it always was. A variant with no capacity reading
// (hasCapacityReading) counts as dense as best. The curve is nil when every
// step serves as much as best.
//
// The curve counts CurrentReplicas, where Committed counts HeldReplicas: a
// terminating replica still holds its GPUs but no longer serves, and it is
// not one the role can give.
func roleNeedGPUs(required float64, vs []variantRecord, stateMap map[string]domain.VariantReplicaState,
	best variantRecord, gpus int) (float64, []ShareCapacityStep) {
	bestPerGPU := best.PerReplicaCapacity / float64(gpus)
	rel := func(vc variantRecord) float64 {
		if !hasCapacityReading(vc.PerReplicaCapacity) {
			return 1
		}
		p := vc.PerReplicaCapacity / float64(gpusPerReplicaFromState(stateMap, vc.VariantName))
		if p == bestPerGPU {
			return 1
		}
		return p / bestPerGPU
	}
	sorted := sortByCostEfficiencyAsc(vs)
	// The GPUs the role holds, in the order it gives them.
	var give []ShareCapacityStep
	for _, floored := range []bool{false, true} {
		for i := len(sorted) - 1; i >= 0; i-- {
			st := stateMap[sorted[i].VariantName]
			floor := 0
			if st.MinReplicas != nil {
				floor = max(*st.MinReplicas, 0)
			}
			n := st.CurrentReplicas - floor
			if floored {
				n = min(st.CurrentReplicas, floor)
			}
			if n > 0 {
				give = append(give, ShareCapacityStep{
					GPUs:   float64(n * gpusPerReplicaFromState(stateMap, sorted[i].VariantName)),
					PerGPU: rel(sorted[i]),
				})
			}
		}
	}
	slices.Reverse(give)
	steps := give
	// Then the GPUs it grows, as a fill grows them.
	for _, vc := range sorted {
		st := stateMap[vc.VariantName]
		room := math.Inf(1)
		if st.MaxReplicas != nil && *st.MaxReplicas > 0 {
			room = float64(max(0, *st.MaxReplicas-st.CurrentReplicas) * gpusPerReplicaFromState(stateMap, vc.VariantName))
		}
		if room > 0 {
			steps = append(steps, ShareCapacityStep{GPUs: room, PerGPU: rel(vc)})
		}
		if math.IsInf(room, 1) {
			break
		}
	}
	if !slices.ContainsFunc(steps, func(s ShareCapacityStep) bool { return s.PerGPU != 1 }) {
		steps = nil
	}
	return ShareRole{Capacity: steps}.gpusFor(required / bestPerGPU), steps
}

// roleCeilingGPUs is the GPUs a role's maxReplicas allow, summed over its
// variants. Zero means unbounded: one variant without a ceiling lets the role
// grow without limit.
func roleCeilingGPUs(variants []variantRecord, stateMap map[string]domain.VariantReplicaState) int {
	ceiling := 0
	for _, vc := range variants {
		st := stateMap[vc.VariantName]
		if st.MaxReplicas == nil || *st.MaxReplicas <= 0 {
			return 0
		}
		ceiling += *st.MaxReplicas * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return ceiling
}

// roleHeldGPUs sums the GPUs a role's variants on accType hold: HeldReplicas
// where the pods were listed, CurrentReplicas where they could not be.
func roleHeldGPUs(records []variantRecord, stateMap map[string]domain.VariantReplicaState, accType, role string) int {
	total := 0
	for _, vc := range variantsForRole(variantsOnType(records, accType), role) {
		st := stateMap[vc.VariantName]
		n := st.CurrentReplicas
		if st.HeldKnown {
			n = st.HeldReplicas
		}
		total += n * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return total
}

// roleFilledGPUs sums the GPUs of a role's replicas on accType every pod of
// which is scheduled: FilledReplicas where the pods were listed,
// CurrentReplicas where they could not be.
func roleFilledGPUs(records []variantRecord, stateMap map[string]domain.VariantReplicaState, accType, role string) int {
	total := 0
	for _, vc := range variantsForRole(variantsOnType(records, accType), role) {
		st := stateMap[vc.VariantName]
		n := st.CurrentReplicas
		if st.HeldKnown {
			n = st.FilledReplicas
		}
		total += n * gpusPerReplicaFromState(stateMap, vc.VariantName)
	}
	return total
}

// shareGiveVariant picks the variant a role gives a replica from: the least
// cost-efficient one that is above its floor.
func shareGiveVariant(vs []variantRecord, stateMap map[string]domain.VariantReplicaState) (ShareVariant, bool) {
	sorted := sortByCostEfficiencyAsc(vs)
	for i := len(sorted) - 1; i >= 0; i-- {
		st := stateMap[sorted[i].VariantName]
		floor := 0
		if st.MinReplicas != nil {
			floor = *st.MinReplicas
		}
		if st.CurrentReplicas > floor {
			return shareVariantOf(sorted[i], stateMap), true
		}
	}
	return ShareVariant{}, false
}

// shareGrowVariant picks the variant a role grows: the most cost-efficient one
// with room below its ceiling.
func shareGrowVariant(vs []variantRecord, stateMap map[string]domain.VariantReplicaState) (ShareVariant, bool) {
	for _, vc := range sortByCostEfficiencyAsc(vs) {
		st := stateMap[vc.VariantName]
		if st.MaxReplicas != nil && *st.MaxReplicas > 0 && st.CurrentReplicas >= *st.MaxReplicas {
			continue
		}
		return shareVariantOf(vc, stateMap), true
	}
	return ShareVariant{}, false
}

// poolCarve is a group's share of the warm pools' unheld target: a namespace
// group's own namespace's, the cluster group every namespace's -- a pool in a
// namespace with its own quota spends the cluster's quota too.
//
// A pool's accelerator is matched by identity, since the group carries the
// budget's spelling and the pool its own.
//
// A pool in a namespace the namespace quota caps carves at most that
// namespace's free quota of the type (availableByNS): its growth is bounded by
// no limiter, and uncapped, a pool wanting more than its tenant may hold
// shrank every tenant's share of the cluster group. A namespace no namespace
// quota caps, or with an unlimited quota of the type, is carved in full.
//
// In the cluster group, a namespace that claimed its free quota of the type
// (unclaimed holds it, keyed by namespace and that namespace's budget key,
// with what rounding the claim to whole replicas left out) carves at most
// that remainder: the claim already took the rest of the pool's headroom out
// of the cluster budget, and carving it again counted it twice.
func poolCarve(unheld, availableByNS, unclaimed map[string]map[string]int, acc, scope string) int {
	carveOf := func(ns string, byType map[string]int) int {
		want := max(0, byType[budgetKey(byType, acc)])
		budget := availableByNS[ns]
		if free, ok := budget[budgetKey(budget, acc)]; ok && free >= 0 && free <= MaxShareBudgetGPUs {
			want = min(want, free)
		}
		return want
	}
	if scope != "" {
		return carveOf(scope, unheld[scope])
	}
	sum := 0
	for ns, byType := range unheld {
		carve := carveOf(ns, byType)
		if budget, capped := availableByNS[ns]; capped {
			if rest, ok := unclaimed[ns][budgetKey(budget, acc)]; ok {
				carve = min(carve, rest)
			}
		}
		sum += carve
	}
	return sum
}
