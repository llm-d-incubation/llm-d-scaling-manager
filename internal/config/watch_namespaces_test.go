package config

import (
	"slices"
	"strings"
	"testing"
)

func TestParseWatchNamespaces(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"team-a", []string{"team-a"}},
		{"team-a,team-b", []string{"team-a", "team-b"}},
		{" team-a , team-b ,", []string{"team-a", "team-b"}},
		{"team-b,team-a,team-b", []string{"team-b", "team-a"}},
		{",,", nil},
	}
	for _, c := range cases {
		if got := ParseWatchNamespaces(c.raw); !slices.Equal(got, c.want) {
			t.Errorf("ParseWatchNamespaces(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestWatchesEachListedNamespace(t *testing.T) {
	c := NewTestConfig()
	if c.Watches("team-a") {
		t.Error("a cluster-scoped controller reported watching team-a by name")
	}
	SetWatchNamespaceForTest(c, "team-a,team-b")
	for _, ns := range []string{"team-a", "team-b"} {
		if !c.Watches(ns) {
			t.Errorf("Watches(%q) = false on a controller watching team-a,team-b", ns)
		}
	}
	for _, ns := range []string{"team-c", "", "team-a,team-b"} {
		if c.Watches(ns) {
			t.Errorf("Watches(%q) = true on a controller watching team-a,team-b", ns)
		}
	}
}

// A namespace-scoped controller that manages several namespaces ignores
// physicalGroups like one that manages one. The note is written when any of
// them has no quota, and names the whole list.
func TestPhysicalGroupsIgnoredAcrossWatchedNamespaces(t *testing.T) {
	policy := func(doc string) map[string]ScalingPolicy {
		p, err := DecodeScalingPolicy(doc)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]ScalingPolicy{GlobalDefaultsKey: p}
	}
	const block = `
optimizer:
  type: utilizationShare
  utilizationShare:
    physicalGroups: true
`
	cases := []struct {
		name     string
		limiters string
		wantNote bool
	}{
		{"inventory only", `
limiters:
  - type: gpu-inventory
`, true},
		{"cluster quota bounds every namespace", `
limiters:
  - type: quota
    name: shared
    scope: cluster
    quotas:
      H100: 4
`, false},
		{"namespace quota excludes one of them", `
limiters:
  - type: quota
    name: tenants
    scope: namespace
    namespaceQuotas:
      default:
        H100: 4
    exclude: [team-b]
  - type: gpu-inventory
`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewTestConfig()
			SetWatchNamespaceForTest(c, "team-a,team-b")
			c.UpdateScalingPolicyConfig(policy(tc.limiters + block))
			us, selected, err := c.UtilizationShare()
			if err != nil || !selected {
				t.Fatalf("UtilizationShare() selected=%v err=%v", selected, err)
			}
			if us.PhysicalGroups {
				t.Error("physicalGroups kept on a controller watching team-a,team-b")
			}
			if got := us.Note != ""; got != tc.wantNote {
				t.Errorf("note = %q, want a note: %v", us.Note, tc.wantNote)
			}
			if tc.wantNote && !strings.Contains(us.Note, "it manages team-a,team-b") {
				t.Errorf("note %q does not name the watched namespaces", us.Note)
			}
		})
	}
}

// A value that names no namespace is cluster scope, decided once at load: not
// a namespace-scoped controller with an empty list.
func TestBlankWatchNamespaceIsClusterScope(t *testing.T) {
	for _, raw := range []string{" , ", ",", "   "} {
		c := NewTestConfig()
		SetWatchNamespaceForTest(c, raw)
		if c.WatchNamespace() != "" || c.WatchNamespaces() != nil {
			t.Errorf("WATCH_NAMESPACE=%q: WatchNamespace()=%q WatchNamespaces()=%q, want cluster scope", raw, c.WatchNamespace(), c.WatchNamespaces())
		}
	}
	if got := NormalizeWatchNamespace(" team-b , team-a,team-b "); got != "team-b,team-a" {
		t.Errorf("NormalizeWatchNamespace = %q, want team-b,team-a", got)
	}
}
