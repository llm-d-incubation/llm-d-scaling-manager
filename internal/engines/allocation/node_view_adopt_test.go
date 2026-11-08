package allocation

import "testing"

// Review 187 minor (guard): adopt charges the topology domains a clone took
// to the adopting view's owner, set after the clone was made, as it charges
// the clone's debits; a clone records no owner of its own. Taking the owner
// back (without) frees the domain with the GPUs.
func TestAdoptChargesAClonesDomainsWithItsDebits(t *testing.T) {
	v := NewNodeView(racks([]string{"a"}))
	trial := v.Clone()
	if trial.owner != "" || trial.owned != nil {
		t.Fatalf("a clone records an owner: %q, %v", trial.owner, trial.owned)
	}
	if !trial.place(trial.fitOf(gang("ns/g1"))) {
		t.Fatal("setup: the gang does not fit the free rack")
	}
	defer v.setOwner(v.setOwner("ns/m"))
	v.adopt(trial)
	if owner := v.domains[domainID("rack", "a")]; owner != "ns/m" {
		t.Fatalf("the adopted domain is charged to %q, want the adopting owner ns/m", owner)
	}
	back := v.without(func(owner string) bool { return owner == "ns/m" })
	if !back.PlaceReplicas([]ReplicaShape{gang("ns/g2")}, false) {
		t.Fatal("taking the owner back freed its GPUs but not its rack")
	}
}
