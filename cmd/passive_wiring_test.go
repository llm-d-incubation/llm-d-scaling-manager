package main

import (
	"reflect"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/scalefromzero"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/steadystate"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/passive"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/scaler"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool"
	warmpoolpool "github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// Every component in passiveComponents gets the resolver: its Passive field
// is set. The check walks the struct, so a component added to it and not
// wired fails here, not on a cluster where it scales a passive model.
func TestWirePassiveReachesEveryComponent(t *testing.T) {
	c := passiveComponents{
		SteadyState:   &steadystate.Engine{},
		ScaleFromZero: &scalefromzero.Engine{},
		Scaler:        &scaler.Server{},
		Demand:        &warmpool.Demand{},
		Guard:         &warmpoolpool.PassiveGuard{},
	}
	wirePassive(&passive.Resolver{}, c)
	v := reflect.ValueOf(c)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		comp := v.Field(i)
		if comp.IsNil() {
			t.Fatalf("%s: the test builds no component for it; add one", name)
		}
		field := comp.Elem().FieldByName("Passive")
		if !field.IsValid() {
			t.Fatalf("%s has no Passive field", name)
		}
		if field.IsNil() {
			t.Errorf("%s: wirePassive left its Passive unset", name)
		}
	}
}
