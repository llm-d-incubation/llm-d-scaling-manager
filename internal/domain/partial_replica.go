package domain

// PartialReplica is one replica of a multi-pod scale target -- a
// LeaderWorkerSet group -- some but not all of whose pods hold a node
// (scheduled to it, or nominated for it): Node is a node one of them holds,
// and PlacedGPUs each placed pod's GPUs. The rest of its pods, created or
// not, hold no node yet.
type PartialReplica struct {
	Node       string
	PlacedGPUs []int
}
