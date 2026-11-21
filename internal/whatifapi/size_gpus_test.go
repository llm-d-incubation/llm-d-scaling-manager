package whatifapi

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staircase is a fleet test from the fewest prefill replicas that hold at
// each decode count (pmin[d], 0 where none does, non-increasing as decode
// grows), with decode's own limit holding from the first count listed.
func staircase(pmin map[int]int) (decodeHolds, holds func(FleetCounts) (bool, error)) {
	floor := 0
	for d := range pmin {
		if floor == 0 || d < floor {
			floor = d
		}
	}
	at := func(d int) int {
		best := 0
		for dd, p := range pmin {
			if dd <= d && p > 0 && (best == 0 || p < best) {
				best = p
			}
		}
		return best
	}
	decodeHolds = func(n FleetCounts) (bool, error) { return n.Decode >= floor, nil }
	holds = func(n FleetCounts) (bool, error) {
		p := at(n.Decode)
		return n.Decode >= floor && p > 0 && n.Prefill >= p, nil
	}
	return decodeHolds, holds
}

// The issue's fleet: 9 prefill + 4 decode and 8 + 5 both hold, 13 replicas
// each. Counted in replicas the tie goes to fewer decode, 9 + 4; with 8-GPU
// prefill and 1-GPU decode replicas that is 76 GPUs against 69, so the
// search takes 8 + 5. With the roles' GPUs equal it is the fewest replicas,
// as before.
func TestSmallestFleet_FewestGPUs(t *testing.T) {
	decodeHolds, holds := staircase(map[int]int{4: 9, 5: 8, 6: 8, 7: 8})
	bound := FleetCounts{Prefill: 32, Decode: 32}
	for _, c := range []struct {
		name string
		gpus FleetCounts
		want FleetCounts
	}{
		{"replicas (no GPUs stated)", FleetCounts{}, FleetCounts{Prefill: 9, Decode: 4}},
		{"equal GPUs a replica", FleetCounts{Prefill: 2, Decode: 2}, FleetCounts{Prefill: 9, Decode: 4}},
		{"8-GPU prefill, 1-GPU decode", FleetCounts{Prefill: 8, Decode: 1}, FleetCounts{Prefill: 8, Decode: 5}},
		{"1-GPU prefill, 8-GPU decode", FleetCounts{Prefill: 1, Decode: 8}, FleetCounts{Prefill: 9, Decode: 4}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, _ := ReplicaCost(c.gpus)
			n, unmet, err := SmallestFleet(bound, w, decodeHolds, holds)
			require.NoError(t, err)
			require.Empty(t, unmet)
			assert.Equal(t, c.want, n)
		})
	}
	w, on := ReplicaCost(FleetCounts{Prefill: 8, Decode: 1})
	assert.Equal(t, MinimizesGPUs, on)
	assert.Equal(t, 76, FleetCounts{Prefill: 9, Decode: 4}.Cost(w))
	assert.Equal(t, 69, FleetCounts{Prefill: 8, Decode: 5}.Cost(w))
	w, on = ReplicaCost(FleetCounts{Prefill: 8})
	assert.Equal(t, MinimizesReplicas, on, "a role with no GPUs stated counts replicas")
	assert.Equal(t, FleetCounts{Prefill: 1, Decode: 1}, w)
}

// On random staircases and weights, the search finds what trying every
// fleet in the bound finds: the least cost, then the fewest replicas, then
// the fewest decode. Its early stop never skips a cheaper fleet.
func TestSmallestFleet_MatchesExhaustive(t *testing.T) {
	rng := rand.New(rand.NewSource(314))
	for i := range 2000 {
		bound := FleetCounts{Prefill: 1 + rng.Intn(16), Decode: 1 + rng.Intn(16)}
		pmin := map[int]int{}
		floor := 1 + rng.Intn(8)
		p := 1 + rng.Intn(24)
		for d := floor; d <= 16; d++ {
			pmin[d] = p
			p = max(1, p-rng.Intn(4))
		}
		w := FleetCounts{Prefill: 1 + rng.Intn(8), Decode: 1 + rng.Intn(8)}
		decodeHolds, holds := staircase(pmin)

		var want FleetCounts
		found := false
		for d := 1; d <= bound.Decode; d++ {
			for pp := 1; pp <= bound.Prefill; pp++ {
				n := FleetCounts{Prefill: pp, Decode: d}
				if ok, _ := holds(n); ok && (!found || cheaper(n, want, w)) {
					want, found = n, true
				}
			}
		}
		got, unmet, err := SmallestFleet(bound, w, decodeHolds, holds)
		require.NoError(t, err)
		if !found {
			assert.NotEmpty(t, unmet, "case %d: nothing holds in %v, yet the search found %v", i, bound, got)
			continue
		}
		require.Empty(t, unmet, "case %d", i)
		assert.Equal(t, want, got, "case %d: bound %v, weights %v, staircase %v", i, bound, w, pmin)
	}
}

// A spare adds that many replicas to each role counted, with the GPUs at
// each role's GPUs per replica; a decode-only need keeps prefill at 0, and
// GPUs are left out where a counted role's are not stated.
func TestWithSpare(t *testing.T) {
	assert.Nil(t, WithSpare(FleetCounts{Prefill: 2, Decode: 2}, 0, FleetCounts{Prefill: 1, Decode: 1}))
	s := WithSpare(FleetCounts{Prefill: 8, Decode: 5}, 1, FleetCounts{Prefill: 8, Decode: 1})
	require.NotNil(t, s)
	assert.Equal(t, SpareFleet{Spare: 1, Fleet: FleetCounts{Prefill: 9, Decode: 6}, GPUs: ptrInt(78)}, *s)
	s = WithSpare(FleetCounts{Decode: 3}, 2, FleetCounts{Decode: 4})
	require.NotNil(t, s)
	assert.Equal(t, SpareFleet{Spare: 2, Fleet: FleetCounts{Decode: 5}, GPUs: ptrInt(20)}, *s)
	s = WithSpare(FleetCounts{Prefill: 2, Decode: 2}, 1, FleetCounts{Decode: 1})
	require.NotNil(t, s)
	assert.Nil(t, s.GPUs, "prefill's GPUs are not stated")
}

// A size reports what it minimised, the answer's GPUs where the inputs state
// both roles', and the fleet with the spare asked; a spare out of range is a
// 400, and a size with no spare keeps the id it had before spares.
func TestSizeGPUsAndSpare(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{gpus: &FleetCounts{Prefill: 8, Decode: 1}}, WhatifSchema: "schema-v3"}).Handler()
	id := inputsOf(t, h)
	req := sizeAt(id, 15, 0.05)
	out, _ := sizeOf(t, h, req)
	require.NotNil(t, out.Answer)
	assert.Equal(t, MinimizesGPUs, out.Minimizes)
	assert.Equal(t, FleetCounts{Prefill: 3, Decode: 3}, out.Answer.Fleet)
	require.NotNil(t, out.Answer.GPUs)
	assert.Equal(t, 27, *out.Answer.GPUs)
	assert.Equal(t, &FleetCounts{Prefill: 8, Decode: 1}, out.Answer.GPUsPerReplica)
	assert.Nil(t, out.Answer.WithSpare)
	assert.Zero(t, out.Spare)

	req.Spare = ptrInt(1)
	spared, _ := sizeOf(t, h, req)
	assert.NotEqual(t, out.SizeID, spared.SizeID)
	assert.Equal(t, 1, spared.Spare)
	require.NotNil(t, spared.Answer.WithSpare)
	assert.Equal(t, SpareFleet{Spare: 1, Fleet: FleetCounts{Prefill: 4, Decode: 4}, GPUs: ptrInt(36)}, *spared.Answer.WithSpare)

	for _, bad := range []int{-1, MaxSpare + 1} {
		req.Spare = ptrInt(bad)
		code, body := post(t, h, "/v1/size", req)
		assert.Equal(t, 400, code, string(body))
	}

	plain := New(Config{Resolver: &stubResolver{}, WhatifSchema: "schema-v3"}).Handler()
	pid := inputsOf(t, plain)
	p, _ := sizeOf(t, plain, sizeAt(pid, 15, 0.05))
	assert.Equal(t, MinimizesReplicas, p.Minimizes, "no GPUs stated: replicas")
	assert.Nil(t, p.Answer.GPUs)
	p0, _ := sizeOf(t, plain, SizeRequest{InputsID: pid, Traffic: sizeAt(pid, 15, 0.05).Traffic, SLO: sizeAt(pid, 15, 0.05).SLO, Spare: ptrInt(0)})
	assert.Equal(t, p.SizeID, p0.SizeID, "a spare of 0 is no spare")
}
