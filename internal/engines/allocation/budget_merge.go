package allocation

import (
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// The GPU budget model, shared by the optimizers, the placement check and the
// warm-pool headroom.
//
// Two kinds of bound apply to a decision. A CLUSTER bound is a provider whose
// per-type pools hold for every namespace: the physical inventory and a
// cluster-scope quota. Each is a closed allowlist -- a type it does not list has
// no GPUs under it -- and they are intersected (mergeConstraints). A NAMESPACE
// bound is a namespace-scope quota's per-namespace allowlist (NamespacePools),
// closed for the namespaces it holds and intersected across the providers that
// hold a namespace (mergeNamespaceConstraints). A namespace-scope quota's
// per-type Pools, the sum of its namespace caps, bounds nothing: a namespace it
// excludes bypasses it, and every namespace it holds is bounded by its own cap.
//
// With no cluster bound in force the cluster budget is nil, which reads as
// unbounded for every type: a namespace the quotas exclude scales freely, and a
// namespace with an allowance is bounded by that allowance alone.
//
// Accelerator names form a family/product tree (accelerator.Family). A budget
// key that is a family ("H100") bounds the SUM of what every product under it
// spends; a product key bounds that product alone, and never exceeds its family
// key (clampProducts). A spend under a product therefore also debits its family
// key (debitBudget), and the total a budget holds counts each family once
// (budgetTotal). Workloads find their key with accelerator.FindBudgetKey.

// isClusterBound reports whether c bounds the whole cluster: it is not a
// namespace-scope provider. A provider that carries namespace pools is one,
// whether or not it says so (NamespaceScoped is how one with no active
// namespace says it).
func isClusterBound(c *ResourceConstraints) bool {
	return c != nil && !c.NamespaceScoped && c.NamespacePools == nil
}

// clusterBounds returns the constraints that bound the whole cluster.
func clusterBounds(constraints []*ResourceConstraints) []*ResourceConstraints {
	var out []*ResourceConstraints
	for _, c := range constraints {
		if isClusterBound(c) {
			out = append(out, c)
		}
	}
	return out
}

// mergeConstraints intersects the cluster bounds into one budget per
// accelerator key: the free GPUs every cluster bound allows, at the smallest
// figure any of them gives. Nil when no cluster bound is in force, which is
// unbounded (see the package comment above); an empty, non-nil map denies
// everything. Used by both optimizers, the placement check and the warm-pool
// headroom.
//
// The keys are every spelling a cluster bound uses, spellings that differ only
// in case folded to the lexically smallest. A provider that does not list a
// name bounds it at 0, except the catch-all "unknown", which physical inventory
// never lists and a quota that declares it means. A family name a provider lists
// only through products is bounded by their sum (allowlistBound), so a physical
// pool of {H100: 5} beside a quota of {NVIDIA-H100-80GB-HBM3: 4,
// NVIDIA-H100-SXM5-80GB: 4} merges to {H100: 5, ...HBM3: 4, ...SXM5: 4}, and the
// family key keeps the two products to the 5 GPUs that exist.
//
// When some cluster bound does not list the family itself, the family is also
// bounded by the sum of what fits under every bound on each product
// (accelerator.ClusterAllowlistsBound): a physical pool of
// {...HBM3: 8, ...PCIE: 2} beside a quota of {...HBM3: 2, ...PCIE: 8} merges
// to H100: 4, not 10, since only 2 GPUs of each product fit under both.
//
// The unlimited sentinel (Limit < 0) is an unbounded budget, math.MaxInt, so the
// optimizer allocates the type up to the model's fair-share demand. A finite
// pool from any provider still wins through the minimum.
//
// declared are the accelerator names the caller's workloads declare. Each is
// materialized as a key of its own, bounded by what every cluster bound allows
// it (allowlistBound, per provider, BEFORE the merge). Without that, two
// answers are lost in the merged map, which no longer says where a key came
// from:
//
//   - an explicit family key is a fallback for the products a provider does not
//     list. A quota of {H100: 8, NVIDIA-H100-80GB-HBM3: 4} admits an
//     NVIDIA-H100-SXM5-80GB workload up to the family's 8, while a family key
//     the merge computed from products ({...HBM3: 4, ...SXM5: 4} beside a
//     physical {H100: 8}) keeps the deny. accelerator.FindBudgetKey, reading
//     the merged map alone, must deny both;
//   - a workload that names the bare family is bounded by the family's SUM, not
//     by one of its products, and debits through the family cascade.
func mergeConstraints(constraints []*ResourceConstraints, declared ...string) map[string]int {
	cluster := clusterBounds(constraints)
	if len(cluster) == 0 {
		return nil
	}
	names := resolvedNames(declared)
	keyed := make([]map[string]ResourcePool, 0, len(cluster))
	for _, c := range cluster {
		names = slices.AppendSeq(names, maps.Keys(c.Pools))
		keyed = append(keyed, c.Pools)
	}
	merged := make(map[string]int, len(names))
	for _, name := range distinctSpellings(names) {
		if !materialized(keyed, name) {
			continue
		}
		if budget, bounded := clusterBoundAll(keyed, name); bounded {
			merged[name] = budget
		}
	}
	clampProducts(merged)
	return merged
}

// mergeWithAllowanceKeys is mergeConstraints with every key a namespace
// allowance lists materialized too, as a declared name is (namespaceKeys).
// The budgets a namespace's allowance is bounded by are read from it -- the
// optimizer's pass, the placement check of a namespace the quotas hold, and
// the warm pool's per-namespace headroom -- so the cluster bound on a key a
// namespace lists is the same whatever names the cycle's workloads declare. A
// namespace allowance of {H100: -1} under a cluster quota of
// {NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 8} is bounded at 16, the
// sum of the products, not at 0 because no workload names the bare H100.
//
// The extra keys are for those reads alone. A namespace the quotas do not
// hold, the utilization share and the warm pool's cluster figure read
// mergeConstraints: there an extra family key would read as a listed sibling
// to accelerator.FindBudgetKey, and deny a product the cluster allows.
func mergeWithAllowanceKeys(constraints []*ResourceConstraints, declared ...string) map[string]int {
	return mergeConstraints(constraints, slices.Concat(declared, namespaceKeys(constraints))...)
}

// clusterBound is the budget one cluster bound puts on the accelerator called
// name, and whether it bounds it at all: accelerator.ClusterAllowlistBound
// over its pools, with an unlimited bound read as math.MaxInt. A name it does
// not list is bounded at 0 (a closed allowlist), and an unlisted "unknown" is
// not bounded.
func clusterBound(pools map[string]ResourcePool, name string) (int, bool) {
	avail, unlimited, bounds := accelerator.ClusterAllowlistBound(pools, name, poolBound)
	if bounds && unlimited {
		return math.MaxInt, true
	}
	return avail, bounds
}

// clusterBoundAll is clusterBound over several allowlists together:
// accelerator.ClusterAllowlistsBound, with an unlimited bound read as
// math.MaxInt.
func clusterBoundAll(allowlists []map[string]ResourcePool, name string) (int, bool) {
	avail, unlimited, bounds := accelerator.ClusterAllowlistsBound(allowlists, name, poolBound)
	if bounds && unlimited {
		return math.MaxInt, true
	}
	return avail, bounds
}

// allowlistBound is what one allowlist (pools keyed by accelerator) allows on
// name, whether that is unlimited, and whether it lists name at all:
// accelerator.AllowlistBound over the pools, which reads the family/product
// tree.
func allowlistBound(pools map[string]ResourcePool, name string) (avail int, unlimited, listed bool) {
	return accelerator.AllowlistBound(pools, name, poolBound)
}

// poolBound reads a pool as an allowlist bound: its free GPUs, unlimited for
// the Limit < 0 sentinel.
func poolBound(p ResourcePool) accelerator.Bound {
	return accelerator.Bound{Free: p.Available(), Unlimited: p.Limit < 0}
}

// declaredAccelerators is every accelerator name the requests' variants
// declare, for the merges to materialize (see mergeConstraints).
func declaredAccelerators(requests []ModelScalingRequest) []string {
	n := 0
	for _, req := range requests {
		n += len(req.Variants)
	}
	out := make([]string, 0, n)
	for _, req := range requests {
		for _, m := range req.Variants {
			out = append(out, m.AcceleratorName)
		}
	}
	return out
}

// materialized reports whether name becomes a key of a merge over these
// allowlists: always when one of them has it as a key (ignoring case), and a
// name only a workload declared when EVERY allowlist admits it. A declared
// name some allowlist denies stays absent: the merged map then denies it
// through accelerator.FindBudgetKey -- by a listed sibling product, or by the
// family key that allowlist bounds at 0 -- and carries no zero key for every
// type a workload happens to name.
func materialized(allowlists []map[string]ResourcePool, name string) bool {
	if hasKey(allowlists, name) {
		return true
	}
	for _, pools := range allowlists {
		if _, _, listed := allowlistBound(pools, name); !listed {
			return false
		}
	}
	return true
}

// hasKey reports whether one of the allowlists has name as a key, ignoring
// case.
func hasKey(allowlists []map[string]ResourcePool, name string) bool {
	for _, pools := range allowlists {
		for key := range pools {
			if strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

// resolvedNames is the declared names a merge materializes: every name but
// the unresolved placeholders, whose rule is their own ("unknown" is exempt
// from a cluster bound, and a closed namespace judges it in FitsGPUBudget).
func resolvedNames(declared []string) []string {
	out := make([]string, 0, len(declared))
	for _, n := range declared {
		if constants.IsAcceleratorResolved(n) {
			out = append(out, n)
		}
	}
	return out
}

// namespaceKeys is every accelerator name a provider's namespace allowances
// list, for mergeWithAllowanceKeys to materialize. Unordered, and a name two
// allowances list appears twice; the merge sorts and folds them
// (distinctSpellings) and drops the unresolved placeholders (resolvedNames).
func namespaceKeys(constraints []*ResourceConstraints) []string {
	var out []string
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for _, perType := range c.NamespacePools {
			out = slices.AppendSeq(out, maps.Keys(perType))
		}
	}
	return out
}

// clusterProducts is every product name a cluster bound lists, for a
// namespace merge to materialize: a namespace allowance of {H100: -1} under a
// cluster quota of {NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 8} then
// holds each product at what the namespace allows it, so the warm pool reads
// each at that product's cluster figure and not at the family's sum. A
// namespace whose allowance does not admit a product does not get it
// (materialized). Unordered, and it may repeat names.
func clusterProducts(constraints []*ResourceConstraints) []string {
	var out []string
	for _, c := range clusterBounds(constraints) {
		for key := range c.Pools {
			if !accelerator.IsFamily(key) {
				out = append(out, key)
			}
		}
	}
	return out
}

// distinctSpellings returns names with spellings that differ only in case
// folded to the lexically smallest, sorted so a merge is deterministic.
func distinctSpellings(names []string) []string {
	slices.Sort(names)
	var out []string
	for _, n := range names {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
			out = append(out, n)
		}
	}
	return out
}

// isUnboundedBudget reports whether a budget value is one of the two unlimited
// sentinels: math.MaxInt in a cluster or effective budget, a negative value in
// a namespace budget.
func isUnboundedBudget(v int) bool {
	return v < 0 || v == math.MaxInt
}

// clampProducts lowers every product key of budgets to the finite family key
// that covers it: a product never has more free than its family, and an
// unlimited product under a finite family is bounded by it.
func clampProducts(budgets map[string]int) {
	for p, pv := range budgets {
		if accelerator.IsFamily(p) {
			continue
		}
		for f, fv := range budgets {
			if isUnboundedBudget(fv) || !accelerator.IsProductOf(p, f) {
				continue
			}
			if isUnboundedBudget(pv) || pv > fv {
				pv = fv
			}
		}
		budgets[p] = pv
	}
}

// debitBudget spends n GPUs under key: from key itself, from every spelling of
// it that differs only in case, and from the family key that covers it, since
// every GPU a product spends is one of its family's (accelerator.Covers). The
// products under a debited family are then clamped to it (clampProducts). An
// unbounded value (math.MaxInt, or a negative namespace sentinel) is never drawn
// down, and no budget goes below 0. A nil budget is unbounded and is left nil.
func debitBudget(budgets map[string]int, key string, n int) {
	if n <= 0 {
		return
	}
	for other, v := range budgets {
		if !isUnboundedBudget(v) && accelerator.Covers(other, key) {
			budgets[other] = max(0, v-n)
		}
	}
	clampProducts(budgets)
}

// budgetTotal is the number of GPUs a budget still holds, saturating at
// math.MaxInt for an unbounded key or a nil (unbounded) budget. Used for the
// optimizer's "nothing left" stop check.
//
// Each family is counted once: by its family key when the budget has one --
// every product under it draws on it -- else by the sum of its product keys.
// Spellings that differ only in case are one key and count at the larger
// figure: every spend debits both, so the stop check, which must not stop while
// anything can still be spent, reads the one that has more left.
func budgetTotal(budgets map[string]int) int {
	if budgets == nil {
		return math.MaxInt
	}
	type family struct {
		family    int
		hasFamily bool
		products  map[string]int
	}
	families := map[string]*family{}
	for key, v := range budgets {
		if isUnboundedBudget(v) {
			v = math.MaxInt
		}
		name := strings.ToLower(accelerator.Family(key))
		f := families[name]
		if f == nil {
			f = &family{products: map[string]int{}}
			families[name] = f
		}
		if accelerator.IsFamily(key) {
			f.family, f.hasFamily = max(f.family, v), true
			continue
		}
		p := strings.ToLower(key)
		f.products[p] = max(f.products[p], v)
	}
	total := 0
	for _, f := range families {
		held := f.family
		if !f.hasFamily {
			held = 0
			for _, v := range f.products {
				held = saturatingAdd(held, v)
			}
		}
		total = saturatingAdd(total, held)
		if total == math.MaxInt {
			return total
		}
	}
	return total
}

// saturatingAdd adds two non-negative budgets, saturating at math.MaxInt.
func saturatingAdd(a, b int) int {
	if a == math.MaxInt || b == math.MaxInt || a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// budgetKey is the key of budgets that the accelerator a workload declares is
// spent from (accelerator.FindBudgetKey), or the declared name itself when
// budgets holds none, so a read finds no budget and a debit touches nothing.
// Every budget read and debit in the optimizers goes through it.
func budgetKey(budgets map[string]int, declared string) string {
	if key, ok := accelerator.FindBudgetKey(budgets, declared); ok {
		return key
	}
	return declared
}

// budgetOf is what budgets allows a workload declaring name: unbounded
// (math.MaxInt) for a nil budget, the value of its key, and 0 when budgets holds
// no key for it -- a non-nil budget is a closed allowlist.
func budgetOf(budgets map[string]int, name string) int {
	if budgets == nil {
		return math.MaxInt
	}
	if key, ok := accelerator.FindBudgetKey(budgets, name); ok {
		return budgets[key]
	}
	return 0
}

// clusterBudgetFor is the cluster bound on an accelerator a namespace allowance
// lists, and whether there is one: none for a nil (unbounded) cluster budget, the
// value of its key, and 0 for a type the cluster bounds do not list, since each
// of them is a closed allowlist. "unknown" is exempt, as in mergeConstraints:
// no physical inventory lists it, so a namespace that grants it is bounded by
// its own allowance alone.
func clusterBudgetFor(available map[string]int, name string) (int, bool) {
	if available == nil {
		return 0, false
	}
	if key, ok := accelerator.FindBudgetKey(available, name); ok {
		return available[key], true
	}
	if name == constants.DefaultAcceleratorName {
		return 0, false
	}
	return 0, true
}

// mergeNamespaceConstraints merges the per-(namespace, accelerator type) pools
// across providers into available-GPU budgets. Returns nil when no provider
// carries namespace pools, so the per-type-only path is unaffected.
//
// A namespace present in any provider's NamespacePools is materialized in the
// result even when its inner map is empty — its presence marks a CLOSED
// allowlist (deny-all when empty) that the optimizer enforces by allocating
// only the listed types.
//
// Several providers holding one namespace are intersected, like the cluster
// bounds: a type is allowed only if EVERY provider that holds the namespace
// allows it (allowlistBound, which reads the family/product tree), at the
// tightest figure among them, and is an explicit 0 otherwise. A provider that
// does not hold the namespace adds no rule for it.
//
// An unlimited (-1 sentinel) pool is carried through as a negative budget so
// the optimizer can distinguish "unlimited" from a finite cap; tighterBudget
// treats it as +infinity when taking the minimum.
//
// declared are materialized as keys in every namespace, as mergeConstraints
// does for the cluster bounds, so an explicit family key admits the products
// a holder does not list and a family name reads the family's sum.
//
// A family some holder lists only through products is bounded by the
// products that fit under every holder and every cluster bound
// (closedFamilyBudget), not by the holders' sum alone: the cluster figure for
// the family may come from products the namespace may not use. So is a family
// every holder lists when a holder also lists a product of it and some cluster
// bound lists the family only through products: an allowance of {H100: -1,
// NVIDIA-H100-SXM5-80GB: 0} under a cluster quota of {NVIDIA-H100-80GB-HBM3:
// 8, NVIDIA-H100-SXM5-80GB: 24} may use 8 H100s, the HBM3 ones, not 32.
func mergeNamespaceConstraints(constraints []*ResourceConstraints, declared ...string) map[string]map[string]int {
	holders := namespaceHolders(constraints)
	if len(holders) == 0 {
		return nil
	}
	var cluster []map[string]ResourcePool
	for _, c := range clusterBounds(constraints) {
		cluster = append(cluster, c.Pools)
	}
	merged := make(map[string]map[string]int, len(holders))
	for ns, pools := range holders {
		names := resolvedNames(declared)
		for _, perType := range pools {
			names = slices.AppendSeq(names, maps.Keys(perType))
		}
		inner := make(map[string]int, len(names))
		for _, name := range distinctSpellings(names) {
			if !materialized(pools, name) {
				continue
			}
			if accelerator.ClosesFamily(pools, name) {
				inner[name] = closedFamilyBudget(slices.Concat(pools, cluster), name)
				continue
			}
			for i, perType := range pools {
				budget := 0 // not allowed by this holder: denied
				if avail, unlimited, listed := allowlistBound(perType, name); listed {
					budget = avail
					if unlimited {
						budget = -1
					}
				}
				if i == 0 {
					inner[name] = budget
				} else {
					inner[name] = tighterBudget(inner[name], budget)
				}
			}
			if accelerator.ClosesFamily(cluster, name) && accelerator.ListsProduct(pools, name) {
				// The joint bound reads every holder's own figure too.
				inner[name] = closedFamilyBudget(slices.Concat(pools, cluster), name)
			}
		}
		clampProducts(inner)
		merged[ns] = inner // present (even if empty) marks a closed namespace
	}
	return merged
}

// closedFamilyBudget is what a namespace may use of a family it may use only
// through products (accelerator.ClosesFamily), given its allowlists and the cluster
// bounds' together: the sum, over every product any of them lists, of what
// fits under all of them (accelerator.ClusterAllowlistsBound), -1 when that is
// unlimited. A namespace allowance of {NVIDIA-H100-SXM5-80GB: -1} under a
// cluster quota of {NVIDIA-H100-80GB-HBM3: 8, NVIDIA-H100-PCIE-80GB: 8} may
// use 0 H100s: a workload that names the bare family is not handed the HBM3
// and PCIe GPUs its namespace may not use.
//
// The cluster bounds enter as the pass found them. The pass bounds the result
// by the cluster budget it has left (effectiveAvailable), as for any key.
func closedFamilyBudget(allowlists []map[string]ResourcePool, name string) int {
	avail, unlimited, _ := accelerator.ClusterAllowlistsBound(allowlists, name, poolBound)
	if unlimited {
		return -1
	}
	return avail
}

// namespaceHolders groups every provider's allowlist for each namespace it
// holds.
func namespaceHolders(constraints []*ResourceConstraints) map[string][]map[string]ResourcePool {
	holders := map[string][]map[string]ResourcePool{}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		for ns, perType := range c.NamespacePools {
			holders[ns] = append(holders[ns], perType)
		}
	}
	return holders
}
