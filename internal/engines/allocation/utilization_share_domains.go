package allocation

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The utilization share's node fit judges a receiver's group as the node
// view does (NodeView.place): an exclusive-topology LeaderWorkerSet's pods in
// one domain, a domain holding one group, and each pod by its own template --
// the leader's or the workers' (podTemplates). The idle fill places through a
// node view built from the share's picture (ShareFitPods); the donor-set
// search, which opens holes the view does not model, judges templates by the
// view's functions (sharePodNodes) and domains by the same rule.

// ShareReplicaFit is one receiver replica as the share's node fit places it.
type ShareReplicaFit struct {
	// PodGPUs is each pod's GPUs, leader first.
	PodGPUs []int
	// DomainKey is the node label every pod of a group must share a value
	// of (an exclusive-topology LeaderWorkerSet), "" for none.
	DomainKey string
	// Placements is the pod templates' nodeSelector and required node
	// affinity: one template, or the leader's then the workers'. Nil judges
	// none.
	Placements []domain.PodPlacement
}

// domainKey is the label r's pods must share a value of, and whether r is a
// group bound to one domain: as the node view places it, a topology key and
// more than one pod taking GPUs. "" and false otherwise.
func (r ShareReplicaFit) domainKey() (string, bool) {
	pods := 0
	for _, g := range r.PodGPUs {
		if g > 0 {
			pods++
		}
	}
	if r.DomainKey == "" || pods < 2 {
		return "", false
	}
	return r.DomainKey, true
}

// receiverFit is receiver rc's replica of grow as the share's node fit
// places it.
func (in SharePlanInput) receiverFit(rc string, grow ShareVariant) ShareReplicaFit {
	return ShareReplicaFit{PodGPUs: grow.PodGPUs, DomainKey: in.DomainKey[rc], Placements: in.Placements[rc]}
}

// ShareFitPods places one replica's pods on nodes as the node view places a
// replica (NodeView.place): largest first, each into the node with the
// smallest free count that holds it and that its template admits. A group
// with an exclusive topology lands in one domain, each tried in turn, and
// only in one that holds no group yet (ShareNode.Groups, and ShareNode.Claimed
// for its own label key); a node without the label is in none. It spends what
// it placed and claims the domain a group took, and reports whether every pod
// fitted; it spends nothing when not. Where no donor is needed it places as
// the node-aware set search does.
func ShareFitPods(nodes map[string]ShareNode, r ShareReplicaFit) bool {
	v := shareNodeView(nodes)
	before := maps.Clone(v.domains)
	if !v.place(replicaFit{pods: r.PodGPUs, domain: r.DomainKey, templates: r.Placements}) {
		return false
	}
	for i := range v.slots {
		info := nodes[v.slots[i].name]
		info.Free = v.free(i)
		nodes[v.slots[i].name] = info
	}
	for id := range v.domains {
		if _, had := before[id]; !had {
			key, value, _ := strings.Cut(id, "\x00")
			claimDomain(nodes, key, value)
		}
	}
	return true
}

// ShareNoNodeFits reports whether no node can hold one replica r even if every
// pod the group could give on it went: each node holds its free GPUs plus
// others, the GPUs that pods of the group's other roles hold on it, and no
// topology domain is held -- a group in one may be among those pods. Pods the
// scaling manager does not plan, and the receiver's own, stay. It reports
// false for a replica with no known pod shape, and for no nodes.
//
// The answer does not change as the other roles give or keep their spare: a
// pod that goes moves its GPUs from others to the node's free GPUs.
func ShareNoNodeFits(nodes map[string]ShareNode, others map[string]int, r ShareReplicaFit) bool {
	if len(nodes) == 0 || len(r.PodGPUs) == 0 {
		return false
	}
	all := make(map[string]ShareNode, len(nodes))
	for n, info := range nodes {
		all[n] = ShareNode{Free: info.Free + others[n], Labels: info.Labels}
	}
	return !ShareFitPods(all, r)
}

// shareClaimOwner is the owner of a domain a share picture's claim holds on
// its node view (shareNodeView).
const shareClaimOwner = "share-claimed"

// shareNodeView is a node view of the share's picture: each node holding its
// free GPUs, its labels, and a group when a group's pod is on it. A claimed
// domain is taken on the view under its own label key, as NodeView.domains
// takes a domain the view placed a group in, so a claim on one key refuses no
// domain of another. The accelerator and taints were judged when the picture
// was built (shareFreeNodesAfter), so the view judges neither.
func shareNodeView(nodes map[string]ShareNode) *NodeView {
	snap := make(map[string]decision.NodeGPU, len(nodes))
	for n, info := range nodes {
		snap[n] = decision.NodeGPU{Capacity: info.Free, Labels: info.Labels, LWSGroup: len(info.Groups) > 0}
	}
	v := NewNodeView(snap)
	for _, info := range nodes {
		for _, key := range info.Claimed {
			if value := info.Labels[key]; value != "" {
				v.domains[domainID(key, value)] = shareClaimOwner
			}
		}
	}
	return v
}

// ClaimShareDomains marks, per topology label key of taken, the nodes of each
// value listed Claimed: an exclusive-topology group this cycle already placed
// there (NodeView.TakenDomains).
func ClaimShareDomains(nodes map[string]ShareNode, taken map[string][]string) {
	for key, values := range taken {
		for _, value := range values {
			claimDomain(nodes, key, value)
		}
	}
}

// claimDomain marks the nodes of value of label key claimed for key.
func claimDomain(nodes map[string]ShareNode, key, value string) {
	if key == "" || value == "" {
		return
	}
	for n, info := range nodes {
		if info.Labels[key] == value && !slices.Contains(info.Claimed, key) {
			info.Claimed = append(slices.Clone(info.Claimed), key)
			nodes[n] = info
		}
	}
}

// domainClaimed reports whether a node of names is claimed for label key.
func domainClaimed(nodes map[string]ShareNode, names []string, key string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return slices.Contains(nodes[n].Claimed, key) })
}

// sharePodFit is one receiver pod as the donor-set search places it: its
// GPUs and the nodes its template admits.
type sharePodFit struct {
	gpus  int
	nodes []string
}

// sharePodNodes is r's pods that take GPUs, largest first (the leader first
// on a tie), each with the nodes of names it may land on: judged by its own
// template as the node view judges it (podTemplates, admitsPlacement), names'
// order kept.
func sharePodNodes(nodes map[string]ShareNode, names []string, r ShareReplicaFit) []sharePodFit {
	rf := replicaFit{pods: r.PodGPUs, templates: r.Placements}
	var out []sharePodFit
	for i, g := range r.PodGPUs {
		if g <= 0 {
			continue
		}
		admitted := names
		if tpl := podTemplates(rf, i); len(tpl) > 0 {
			admitted = slices.DeleteFunc(slices.Clone(names), func(n string) bool {
				return !admitsPlacement(&nodeSlot{labels: nodes[n].Labels}, tpl, nil)
			})
		}
		out = append(out, sharePodFit{gpus: g, nodes: admitted})
	}
	slices.SortStableFunc(out, func(a, b sharePodFit) int { return cmp.Compare(b.gpus, a.gpus) })
	return out
}

// freesGroups reports whether every LeaderWorkerSet group with a pod on a
// node of names (ShareNode.Groups) leaves with a donor group the search took
// (ShareUnit.Group): the domain then holds no group but the receiver's once
// the set lands. Judged per group, not per node: a node shared by a donor
// group's pod and another group's is not freed by the donor alone.
func (s *shareNodeSearch) freesGroups(names []string) bool {
	leaving := map[string]bool{}
	for r := range s.used {
		if u := s.in.DonorUnits[r.donor][r.unit]; u.Group != "" {
			leaving[u.Group] = true
		}
	}
	return !slices.ContainsFunc(names, func(n string) bool {
		return slices.ContainsFunc(s.in.Nodes[n].Groups, func(g string) bool { return !leaving[g] })
	})
}

// TakenDomains is, per topology label key, the values of it in which the view
// placed or holds an exclusive-topology group (NodeView.domains), for every
// owner exclude does not name, each sorted. Nil for a nil view.
func (v *NodeView) TakenDomains(exclude func(owner string) bool) map[string][]string {
	if v == nil {
		return nil
	}
	out := map[string][]string{}
	for _, id := range slices.Sorted(maps.Keys(v.domains)) {
		if exclude != nil && exclude(v.domains[id]) {
			continue
		}
		key, value, _ := strings.Cut(id, "\x00")
		out[key] = append(out[key], value)
	}
	return out
}
