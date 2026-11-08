package allocation

// The non-managed charge of a Kueue-backed quota (KU1).
//
// A quota is charged what the scaling manager's own variants hold
// (ManagedUsage): a static cap is an allowance for them alone. A figure read
// from Kueue is different. Kueue admits against everything it has reserved in
// a ClusterQueue -- a training job beside the serving variants included -- so
// the GPUs those other workloads hold are not there for the variants, whatever
// the figure says. Each queue's figures are therefore charged, before the
// static entry bounds them, with its non-managed charge: what Kueue has
// reserved in it less what the managed variants hold there. The managed usage
// is charged as before, against the charged figure.

import (
	"maps"
	"math"
	"slices"
	"time"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// ManagedUsageSetter is an Inventory that is told, before each Refresh, what
// the managed variants hold per namespace and accelerator type: the managed
// usage a Kueue-backed figure is charged against (chargeNonManaged).
type ManagedUsageSetter interface {
	SetManagedUsage(usageByNamespace map[string]map[string]int)
}

// SetManagedUsage records what the managed variants hold, per namespace and
// accelerator type, for the next Refresh to charge the Kueue figures with.
// Never called (nil), the figures are not charged.
func (q *QuotaInventory) SetManagedUsage(usageByNamespace map[string]map[string]int) {
	var usage map[string]map[string]int
	if usageByNamespace != nil {
		usage = make(map[string]map[string]int, len(usageByNamespace))
		for ns, perType := range usageByNamespace {
			usage[ns] = maps.Clone(perType)
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.managedUsage = usage
}

var _ ManagedUsageSetter = (*QuotaInventory)(nil)

// GatedSource reports the GPUs of managed pods at Kueue's admission gate, per
// namespace and accelerator ("" when the pod's node selector names none).
// BorrowInFlight is one: the gated pods its last borrowing cycle listed.
type GatedSource interface {
	GatedGPUs(now time.Time) map[string]map[string]int
}

var _ GatedSource = (*BorrowInFlight)(nil)

// SetGatedSource sets where the inventory reads the managed pods at Kueue's
// admission gate from: their GPUs are managed usage Kueue has not reserved,
// so they do not come off a queue's charge (admittedUsage). Nil reads none.
func (q *QuotaInventory) SetGatedSource(s GatedSource) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.gated = s
}

// admittedUsage is managed less the gated GPUs: per namespace, a gated pod's
// GPUs come off the type its node selector names (accelerator identity), and
// gated GPUs that name no type off the namespace's types in a stable order,
// each clamped at 0. A replica waiting at the gate holds managed usage but no
// reservation: taking it off Kueue's Reserved too would cancel the charge of
// another workload by as much, so the namespace would read the queue as full
// of its own replicas, the gated one included, and that replica -- held past
// what Kueue admits -- would never be seen as past its figure (KU1).
func admittedUsage(managed, gated map[string]map[string]int) map[string]map[string]int {
	if managed == nil || len(gated) == 0 {
		return managed
	}
	out := make(map[string]map[string]int, len(managed))
	for ns, perType := range managed {
		left := maps.Clone(perType)
		untyped := 0
		for acc, n := range gated[ns] {
			if acc == "" {
				untyped += n
				continue
			}
			if t, ok := accelerator.FindBudgetKey(left, acc); ok {
				left[t] = max(0, left[t]-n)
			}
		}
		for _, t := range slices.Sorted(maps.Keys(left)) {
			take := min(untyped, left[t])
			left[t] -= take
			untyped -= take
		}
		out[ns] = left
	}
	return out
}

// chargeNonManaged returns ext with every namespace's figures -- nominal,
// guaranteed and within nominal -- charged with the non-managed charge of the
// queues it is granted, and each borrowing row carrying its own (Charge).
//
// Per namespace, ClusterQueue, flavor and GPU resource, the charge is Kueue's
// Reserved less the GPUs that namespace's managed variants hold in that queue
// (namespaceCharge): another namespace sharing the queue is charged like any
// other workload. managed is what Kueue has admitted of the managed usage:
// the gated pods' GPUs are taken off it first (admittedUsage), so a replica
// waiting at the gate does not cancel another workload's charge.
//
// Which of a namespace's queues a type's GPUs are counted in is decided per
// accelerator type. A type one of its queues grants is counted there, and
// that queue's charge comes off the namespace's figure of the type. A type
// several of its queues grant (an untyped flavor grants every type) is
// ambiguous: which queue a replica is admitted through is not read, so the
// namespace's figure of it is the smallest of the queues' figures, each less
// what Kueue reserved in it plus what the namespace holds of the type,
// min_q(N_q - R_q) + M -- the headroom is then what the fullest queue has
// left. ext is returned as is when managed is nil (never set) or carries no
// rows.
func chargeNonManaged(ext config.ExternalQuotas, managed map[string]map[string]int) config.ExternalQuotas {
	if managed == nil || len(ext.Borrow) == 0 {
		return ext
	}
	out := ext
	out.Borrow = make(map[string][]config.ExternalBorrow, len(ext.Borrow))
	out.Namespace = maps.Clone(ext.Namespace)
	out.NamespaceGuaranteed = maps.Clone(ext.NamespaceGuaranteed)
	out.NamespaceWithinNominal = maps.Clone(ext.NamespaceWithinNominal)
	for ns, rows := range ext.Borrow {
		charged := namespaceCharge(rows, managed[ns])
		out.Borrow[ns] = charged
		chargeFigure(out.Namespace, ns, charged, func(r config.ExternalBorrow) int { return r.Nominal })
		chargeFigure(out.NamespaceGuaranteed, ns, charged, func(r config.ExternalBorrow) int { return r.Guaranteed })
		chargeFigure(out.NamespaceWithinNominal, ns, charged, func(r config.ExternalBorrow) int { return r.WithinNominal })
	}
	return out
}

// nsQueues is one namespace's rows grouped by ClusterQueue, in the order the
// rows name the queues, one row per flavor and resource.
type nsQueues struct {
	names []string
	rows  map[string][]config.ExternalBorrow
}

func queuesOfRows(rows []config.ExternalBorrow) nsQueues {
	qs := nsQueues{rows: map[string][]config.ExternalBorrow{}}
	for _, r := range rows {
		if !slices.Contains(qs.names, r.Queue) {
			qs.names = append(qs.names, r.Queue)
		}
		if !slices.ContainsFunc(qs.rows[r.Queue], func(o config.ExternalBorrow) bool {
			return o.Flavor == r.Flavor && o.Resource == r.Resource
		}) {
			qs.rows[r.Queue] = append(qs.rows[r.Queue], r)
		}
	}
	return qs
}

// granting is the queues that grant a replica of accType: a row naming that
// accelerator, or an untyped row, which admits any type.
func (qs nsQueues) granting(accType string) []string {
	var out []string
	for _, q := range qs.names {
		if slices.ContainsFunc(qs.rows[q], func(r config.ExternalBorrow) bool {
			return r.Accelerator == "" || accelerator.SameName(r.Accelerator, accType)
		}) {
			out = append(out, q)
		}
	}
	return out
}

// untypedQueues is the queues with an untyped row.
func (qs nsQueues) untypedQueues() []string {
	var out []string
	for _, q := range qs.names {
		if slices.ContainsFunc(qs.rows[q], func(r config.ExternalBorrow) bool { return r.Accelerator == "" }) {
			out = append(out, q)
		}
	}
	return out
}

// namespaceCharge returns one namespace's rows, each carrying the queue's
// charge as that namespace sees it (Charge): what Kueue reserved in the queue
// on the row's flavor and resource less what this namespace's own variants
// hold there (held). Another namespace's GPUs in a shared queue are charged
// like any other workload's: Kueue admits them against the same nominal, so
// they are not this namespace's to spend.
//
// A type one queue grants is held in that queue. A type several queues grant
// is held, for the charge, in each of them, and its rows' charge is not
// clamped at 0: R_q - M may be negative, so that min_q(N_q - Charge_q) is
// min_q(N_q - R_q) + M (chargeFigure).
func namespaceCharge(rows []config.ExternalBorrow, held map[string]int) []config.ExternalBorrow {
	qs := queuesOfRows(rows)
	ambiguous := map[string]bool{}
	left := map[string]map[string]int{} // per queue, the managed GPUs counted in it
	for _, t := range slices.Sorted(maps.Keys(held)) {
		if held[t] <= 0 {
			continue
		}
		g := qs.granting(t)
		ambiguous[t] = len(g) > 1
		for _, q := range g {
			if left[q] == nil {
				left[q] = map[string]int{}
			}
			left[q][t] += held[t]
		}
	}
	untypedAmbiguous := len(qs.untypedQueues()) > 1
	charge := map[string]map[string]int{} // per queue, per "flavor/resource"
	for _, q := range qs.names {
		charge[q] = map[string]int{}
		for _, r := range typedFirst(qs.rows[q]) {
			amb := r.Accelerator == "" && untypedAmbiguous
			if r.Accelerator != "" {
				amb = len(qs.granting(r.Accelerator)) > 1
			}
			if r.Accelerator == "" {
				// An untyped line holds the types no typed line of its queue
				// names: ambiguous when one of them is.
				for t, n := range left[q] {
					if n > 0 && ambiguous[t] && !slices.ContainsFunc(qs.rows[q], func(o config.ExternalBorrow) bool {
						return o.Accelerator != "" && accelerator.SameName(o.Accelerator, t)
					}) {
						amb = true
					}
				}
			}
			limit := r.Reserved
			if amb {
				limit = math.MaxInt
			}
			c := r.Reserved - takeManaged(left[q], r, qs.rows[q], limit)
			if !amb {
				c = max(0, c)
			}
			charge[q][r.Flavor+"/"+r.Resource] = c
		}
	}
	out := slices.Clone(rows)
	for i := range out {
		out[i].Charge = charge[out[i].Queue][out[i].Flavor+"/"+out[i].Resource]
	}
	return out
}

// chargeFigure charges namespace ns's grant in caps (nominal, guaranteed or
// within nominal, x reading that figure off a row) with its queues' charges,
// each figure clamped at 0. Per type: one granting queue's charge comes off
// the grant; several granting queues (ambiguous) give the smallest of their
// figures less their charges. A namespace caps does not hold, or a type no
// queue grants, is left as is.
func chargeFigure(caps map[string]config.ExternalCaps, ns string, rows []config.ExternalBorrow, x func(config.ExternalBorrow) int) {
	c, ok := caps[ns]
	if !ok {
		return
	}
	qs := queuesOfRows(rows)
	chargeOnly := func(r config.ExternalBorrow) int { return r.Charge }
	left := func(r config.ExternalBorrow) int { return x(r) - r.Charge }
	c.ByType = maps.Clone(c.ByType)
	for t, n := range c.ByType {
		switch g := qs.granting(t); len(g) {
		case 0:
		case 1:
			c.ByType[t] = max(0, n-lineSum(qs.rows[g[0]], t, chargeOnly))
		default:
			least := math.MaxInt
			for _, q := range g {
				least = min(least, lineSum(qs.rows[q], t, left))
			}
			c.ByType[t] = max(0, least)
		}
	}
	if c.HasUntyped {
		switch uq := qs.untypedQueues(); len(uq) {
		case 0:
		case 1:
			c.Untyped = max(0, c.Untyped-untypedSum(qs.rows[uq[0]], chargeOnly))
		default:
			least := math.MaxInt
			for _, q := range uq {
				least = min(least, untypedSum(qs.rows[q], left))
			}
			c.Untyped = max(0, least)
		}
	}
	caps[ns] = c
}

// lineSum is v summed over one queue's lines of accType: its rows naming the
// accelerator, or, when none does, its untyped rows (untypedSum).
func lineSum(rows []config.ExternalBorrow, accType string, v func(config.ExternalBorrow) int) int {
	n, typed := 0, false
	for _, r := range rows {
		if r.Accelerator != "" && accelerator.SameName(r.Accelerator, accType) {
			n += v(r)
			typed = true
		}
	}
	if typed {
		return n
	}
	return untypedSum(rows, v)
}

// untypedSum is v over one queue's untyped rows: summed per resource, the
// largest resource's sum, as clusterQueueCapsAt reads untyped lines.
func untypedSum(rows []config.ExternalBorrow, v func(config.ExternalBorrow) int) int {
	byRes := map[string]int{}
	for _, r := range rows {
		if r.Accelerator == "" {
			byRes[r.Resource] += v(r)
		}
	}
	if len(byRes) == 0 {
		return 0
	}
	return slices.Max(slices.Collect(maps.Values(byRes)))
}

// typedFirst is rows with the ones naming an accelerator first, each group in
// flavor and resource order: an untyped line takes the managed usage no typed
// line of its queue names.
func typedFirst(rows []config.ExternalBorrow) []config.ExternalBorrow {
	out := slices.Clone(rows)
	slices.SortStableFunc(out, func(a, b config.ExternalBorrow) int {
		switch {
		case (a.Accelerator == "") != (b.Accelerator == ""):
			if a.Accelerator == "" {
				return 1
			}
			return -1
		case a.Flavor != b.Flavor:
			if a.Flavor < b.Flavor {
				return -1
			}
			return 1
		case a.Resource < b.Resource:
			return -1
		case a.Resource > b.Resource:
			return 1
		}
		return 0
	})
	return out
}

// takeManaged takes, from the managed usage left in a queue, at most limit
// GPUs that row r's line holds: the types naming its accelerator, or, for an
// untyped line, the types no typed line of the queue names.
func takeManaged(left map[string]int, r config.ExternalBorrow, rows []config.ExternalBorrow, limit int) int {
	taken := 0
	for _, t := range slices.Sorted(maps.Keys(left)) {
		if taken >= limit {
			break
		}
		matches := accelerator.SameName(r.Accelerator, t)
		if r.Accelerator == "" {
			matches = !slices.ContainsFunc(rows, func(o config.ExternalBorrow) bool {
				return o.Accelerator != "" && accelerator.SameName(o.Accelerator, t)
			})
		}
		if !matches {
			continue
		}
		n := min(left[t], limit-taken)
		left[t] -= n
		taken += n
	}
	return taken
}
