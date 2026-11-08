package config

import "testing"

// Every key a Kueue borrowing entry takes is one the scaling-policy schema
// defines: the unknown-key check (UnknownScalingPolicyKeys) reports none of
// them, and each decodes to its field.
func TestKueueBorrowingKeysAreKnown(t *testing.T) {
	entry := `limiters:
- name: namespace-quota
  type: quota
  scope: namespace
  kueue:
    enabled: true
    refreshInterval: 30s
    countBorrowable: true
    placementTimeout: 5m
    borrowCooldown: 10m
    lenderDebounce: 60s
    borrowNamespaces: [team-a]
    controllerNamespace: kueue-system
`
	p, err := DecodeScalingPolicy(entry)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if unknown := UnknownScalingPolicyKeys(entry); unknown != nil {
		t.Errorf("Kueue borrowing keys reported as unknown: %v", unknown)
	}
	if len(p.Limiters) != 1 || p.Limiters[0].Kueue == nil {
		t.Fatalf("decoded %+v", p.Limiters)
	}
	k := p.Limiters[0].Kueue
	if k.CountBorrowable == nil || !*k.CountBorrowable || k.PlacementTimeout != "5m" || k.BorrowCooldown != "10m" ||
		k.LenderDebounce != "60s" || len(k.BorrowNamespaces) != 1 || k.ControllerNamespace != "kueue-system" {
		t.Errorf("decoded %+v", *k)
	}
	if unknown := UnknownScalingPolicyKeys(entry + "    borrowNamespace: [team-b]\n"); len(unknown) != 1 {
		t.Errorf("control: a misspelled key must be reported, got %v", unknown)
	}
}
