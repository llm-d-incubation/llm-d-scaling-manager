package accelerator

import "testing"

func intBound(v int) Bound { return Bound{Free: v, Unlimited: v < 0} }

func TestClusterAllowlistsBound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allowlists []map[string]int
		acc        string
		free       int
		unlimited  bool
		bounds     bool
	}{
		{"products swapped: what fits under both", []map[string]int{{hbm3: 8, pcie: 2}, {hbm3: 2, pcie: 8}}, "H100", 4, false, true},
		{"one product listed by one only", []map[string]int{{hbm3: 8, pcie: 8}, {hbm3: 8}}, "H100", 8, false, true},
		{"a family key beside products", []map[string]int{{"H100": 24, hbm3: 1}, {hbm3: 8, pcie: 2}}, "H100", 3, false, true},
		{"a product in two spellings counts once", []map[string]int{{hbm3: 8, pcie: 8}, {"H100": 24, "nvidia-h100-80gb-hbm3": 2}}, "H100", 10, false, true},
		{"family keys alone: the tightest", []map[string]int{{"H100": 24, hbm3: 1}, {"h100": 16}}, "H100", 16, false, true},
		{"a family key caps the sum", []map[string]int{{"H100": 4}, {hbm3: 8, pcie: 8}}, "H100", 4, false, true},
		{"unlimited products, each in one", []map[string]int{{hbm3: -1, pcie: 8}, {hbm3: 8, pcie: -1}}, "H100", 16, false, true},
		{"unlimited everywhere", []map[string]int{{hbm3: -1}, {"H100": -1}}, "H100", 0, true, true},
		{"no product of the family", []map[string]int{{"A100": 8}, {"H100": 8}}, "H100", 0, false, true},
		{"one allowlist", []map[string]int{{hbm3: 4, sxm5: 4}}, "h100", 8, false, true},
		{"a product: the tightest", []map[string]int{{hbm3: 8}, {"H100": 2}}, hbm3, 2, false, true},
		{"a product one does not list", []map[string]int{{hbm3: 8}, {pcie: 8}}, hbm3, 0, false, true},
		{"unknown listed by one", []map[string]int{{"unknown": 4}, {hbm3: 8}}, "unknown", 4, false, true},
		{"unknown listed by none", []map[string]int{{hbm3: 8}}, "unknown", 0, false, false},
		{"no allowlist", nil, "H100", 0, false, false},
	} {
		free, unlimited, bounds := ClusterAllowlistsBound(tc.allowlists, tc.acc, intBound)
		if free != tc.free || unlimited != tc.unlimited || bounds != tc.bounds {
			t.Errorf("%s: ClusterAllowlistsBound(%v, %q) = (%d, %v, %v), want (%d, %v, %v)",
				tc.name, tc.allowlists, tc.acc, free, unlimited, bounds, tc.free, tc.unlimited, tc.bounds)
		}
	}
}

func TestClosesFamily(t *testing.T) {
	for _, tc := range []struct {
		allowlists []map[string]int
		name       string
		want       bool
	}{
		{[]map[string]int{{"H100": 8}}, "H100", false},
		{[]map[string]int{{"h100": 8}}, "H100", false},
		{[]map[string]int{{"H100": 8}, {hbm3: 8}}, "H100", true},
		{[]map[string]int{{"H100": 8}, {"A100": 8}}, "H100", true},
		{[]map[string]int{{hbm3: 8}}, hbm3, false},
		{[]map[string]int{{hbm3: 8}}, "unknown", false},
		{[]map[string]int{{hbm3: 8}}, "", false},
		{nil, "H100", false},
	} {
		if got := ClosesFamily(tc.allowlists, tc.name); got != tc.want {
			t.Errorf("ClosesFamily(%v, %q) = %v, want %v", tc.allowlists, tc.name, got, tc.want)
		}
	}
}
