package pool

import (
	"strings"
	"testing"
)

// TWO DATA-PARALLEL MODELS IN ONE POD MUST NOT SHARE A MESSAGING PORT.
//
// vLLM's data-parallel RPC port is a CONSTANT default, 29550 — unlike the
// data-parallel MASTER port, which vLLM picks from get_open_ports_list at
// startup. Verified against the running engine on the shared GPU cluster, 2026-08-31:
//
//	ParallelConfig.data_parallel_rpc_port  = 29550   (fixed)
//	ParallelConfig.data_parallel_master_port = 29500 (replaced at startup)
//
// A constant is fine where a Pod runs one engine. The whole premise of the warm
// pool is several engines in ONE Pod, so the second data-parallel model would
// bind a port the first already holds and fail to start — and it would fail at
// admission, which reads as a model that cannot be warmed rather than as a port
// conflict.
//
// The pool already guarantees a unique --port per instance, so deriving the
// messaging port from it inherits that uniqueness rather than needing a second
// allocator to stay in step.
func TestDataParallelInstancesGetDistinctRPCPorts(t *testing.T) {
	const dpOpts = "--model m --data-parallel-size 2"

	first := withAssignedPorts(dpOpts, BasePort)
	second := withAssignedPorts(dpOpts, BasePort+1)

	firstRPC := flagValue(first, "--data-parallel-rpc-port")
	secondRPC := flagValue(second, "--data-parallel-rpc-port")

	if firstRPC == "" || secondRPC == "" {
		t.Fatalf("a data-parallel instance must be given a messaging port: %q / %q", first, second)
	}
	if firstRPC == secondRPC {
		t.Errorf("both instances got --data-parallel-rpc-port %s; the second engine cannot bind it",
			firstRPC)
	}
	// The first keeps vLLM's own default, so the ordinary single-model Pod looks
	// exactly as an operator expects.
	if firstRPC != "29550" {
		t.Errorf("first instance rpc port = %s, want vLLM's own default 29550", firstRPC)
	}
}

// A NON-data-parallel engine gets no messaging port at all.
//
// Adding the flag unconditionally would put an unused argument on every warm
// copy, and a warm copy's options are compared against the ordinary replicas'
// to decide whether a resident instance matches what was asked for.
func TestANonDataParallelInstanceGetsNoRPCPort(t *testing.T) {
	opts := withAssignedPorts("--model m --tensor-parallel-size 2", BasePort)

	if strings.Contains(opts, "--data-parallel-rpc-port") {
		t.Errorf("a tensor-parallel engine is not data-parallel and needs no messaging port: %q", opts)
	}
	if flagValue(opts, "--port") != "9001" {
		t.Errorf("--port = %q, want the assigned 9001", flagValue(opts, "--port"))
	}
}

// dp=1 is not data parallelism, and must be treated as the plain case.
func TestDataParallelSizeOneIsNotDataParallel(t *testing.T) {
	opts := withAssignedPorts("--model m --data-parallel-size 1", BasePort)

	if strings.Contains(opts, "--data-parallel-rpc-port") {
		t.Errorf("--data-parallel-size 1 is a single engine and needs no messaging port: %q", opts)
	}
}

// The pool's ports are STRIPPED when instances are compared.
//
// optionsWithoutPort is what makes a resident instance comparable to a
// requested one: the port is the pool's choice and differs between Pods, while
// everything else is the engine's identity. The messaging port is assigned on
// exactly the same terms, so leaving it in would make an instance look different
// from the model that asked for it and cause an endless re-admission.
func TestTheAssignedMessagingPortIsStrippedForComparison(t *testing.T) {
	requested := "--model m --data-parallel-size 2"
	launched := withAssignedPorts(requested, BasePort+3)

	if got := optionsWithoutPort(launched); got != requested {
		t.Errorf("stripped = %q, want the caller's own options %q", got, requested)
	}
}
