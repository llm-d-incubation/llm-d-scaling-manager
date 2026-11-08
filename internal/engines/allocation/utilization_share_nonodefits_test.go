package allocation

import "testing"

// A replica fits a node when its free GPUs plus what the group's other pods
// hold there cover it: those pods can go. Pods outside the group stay, so a
// node they half fill never holds it. The answer is the same before and after
// one of the group's pods goes.
func TestShareNoNodeFits(t *testing.T) {
	g8 := ShareReplicaFit{PodGPUs: []int{8}}
	for _, tc := range []struct {
		name   string
		nodes  map[string]ShareNode
		others map[string]int
		fit    ShareReplicaFit
		want   bool
	}{
		{"half of every node is held outside the group", map[string]ShareNode{"n2": {Free: 0}, "n3": {Free: 0}},
			map[string]int{"n2": 4, "n3": 4}, g8, true},
		{"the same after the group's pods on n2 went", map[string]ShareNode{"n2": {Free: 4}, "n3": {Free: 0}},
			map[string]int{"n3": 4}, g8, true},
		{"the group's pods fill n2", map[string]ShareNode{"n2": {Free: 0}}, map[string]int{"n2": 8}, g8, false},
		{"free GPUs and the group's pods together", map[string]ShareNode{"n2": {Free: 3}}, map[string]int{"n2": 5}, g8, false},
		{"a node already free", map[string]ShareNode{"n2": {Free: 8}}, nil, g8, false},
		{"pod shape unknown: not judged", map[string]ShareNode{"n2": {}}, nil, ShareReplicaFit{}, false},
		{"no node information: not judged", nil, nil, g8, false},
		{"a group held in its domain: the hold may leave with the group's pods",
			map[string]ShareNode{
				"a": {Free: 0, Labels: map[string]string{"rack": "r1"}, Groups: []string{"ns/x"}},
				"b": {Free: 0, Labels: map[string]string{"rack": "r1"}, Groups: []string{"ns/x"}},
			}, map[string]int{"a": 8, "b": 8}, ShareReplicaFit{PodGPUs: []int{8, 8}, DomainKey: "rack"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShareNoNodeFits(tc.nodes, tc.others, tc.fit); got != tc.want {
				t.Fatalf("ShareNoNodeFits = %v, want %v", got, tc.want)
			}
		})
	}
}
