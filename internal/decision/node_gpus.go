package decision

import (
	"maps"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// NodeGPU is one GPU node as the usage refresher last saw it: its accelerator,
// capacity, the GPUs requested by pods scheduled to it, its labels, whether it
// is cordoned, and the taints a pod must tolerate to land on it.
type NodeGPU struct {
	Accelerator   string
	Capacity      int
	Used          int
	Labels        map[string]string
	Unschedulable bool
	// Taints is the node's NoSchedule and NoExecute taints, other than those
	// that already make it Unschedulable: a GPU pool's own taint, which only
	// the pods it exists for tolerate.
	Taints []corev1.Taint
	// LWSGroup is set when a pod of a LeaderWorkerSet group (one carrying
	// the group-key label) holds the node or is nominated for it. A group of
	// an exclusive-topology LeaderWorkerSet lands in no topology domain
	// holding a pod of another group: its pods' anti-affinity refuses it.
	LWSGroup bool
	// LWSGroups is the groups whose pods hold the node or are nominated for
	// it, each "namespace/<group-unique-hash>", sorted; set with LWSGroup. A
	// node a group leaves still holds the others (allocation.ShareNode).
	LWSGroups []string
}

// Free is the node's GPUs no scheduled pod requests.
func (n NodeGPU) Free() int { return max(0, n.Capacity-n.Used) }

// NodeGPUsMaxAge is how old a node snapshot may be and still be used to place
// pods. Older, the holes it shows may have been filled.
const NodeGPUsMaxAge = 2 * time.Minute

// NodeGPUStore holds the latest per-node GPU snapshot.
type NodeGPUStore struct {
	mu    sync.RWMutex
	nodes map[string]NodeGPU
	at    time.Time
}

// Publish replaces the snapshot.
func (s *NodeGPUStore) Publish(nodes map[string]NodeGPU, now time.Time) {
	c := make(map[string]NodeGPU, len(nodes))
	for k, n := range nodes {
		n.Labels = maps.Clone(n.Labels)
		n.Taints = slices.Clone(n.Taints)
		c[k] = n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes, s.at = c, now
}

// Latest returns a copy of the snapshot, or nil when there is none or it is
// older than maxAge.
func (s *NodeGPUStore) Latest(maxAge time.Duration, now time.Time) map[string]NodeGPU {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.nodes == nil || now.Sub(s.at) > maxAge {
		return nil
	}
	c := make(map[string]NodeGPU, len(s.nodes))
	for k, n := range s.nodes {
		n.Labels = maps.Clone(n.Labels)
		n.Taints = slices.Clone(n.Taints)
		c[k] = n
	}
	return c
}

// Read hands the snapshot, and when it was taken, to read, without copying it,
// when there is one no older than maxAge, and reports whether it did. read
// must not modify the map or keep it: it is the store's own, replaced
// wholesale by the next Publish. It may keep a node's Labels and Taints, which
// Publish copied and nothing writes afterwards. For a reader that builds its
// own structure from every node on a hot path -- Latest copies every node's
// labels, which on a large cluster costs more than the read itself.
func (s *NodeGPUStore) Read(maxAge time.Duration, now time.Time, read func(map[string]NodeGPU, time.Time)) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.nodes == nil || now.Sub(s.at) > maxAge {
		return false
	}
	read(s.nodes, s.at)
	return true
}

// DefaultNodeGPUs is the process-wide store, published by the usage refresher.
var DefaultNodeGPUs = &NodeGPUStore{}
