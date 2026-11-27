package domain

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// CanonicalRole reads an empty role as RoleBoth. Every role comparison goes
// through it -- the analyzers, aggregation and the composite -- so a variant
// with no role label is keyed the same way everywhere.
func CanonicalRole(role string) string {
	if role == "" {
		return RoleBoth
	}
	return role
}

// HasPrefill reports whether rolesByVariant (variant name to role) names a
// prefill variant: the model is split into prefill and decode, and its decode
// replicas' waiting counts hold requests waiting for their KV from prefill.
// Whether the prefill variant has a Ready replica does not matter; the split
// is the model's shape.
func HasPrefill(rolesByVariant map[string]string) bool {
	for _, role := range rolesByVariant {
		if CanonicalRole(role) == RolePrefill {
			return true
		}
	}
	return false
}

// RoleOfVC is vc's role, canonicalized (CanonicalRole).
func RoleOfVC(vc VariantCapacity) string {
	return CanonicalRole(vc.Role)
}

// The thresholds a reading's role is read by (RoleSignals).
const (
	// prefillMaxOutputTokens is the most output tokens a request a prefill
	// pod is read to make. A prefill pod makes one token a request -- the
	// router sends it max_tokens=1 and hands the KV on -- so its
	// AvgOutputTokens reads 1; a pod above this generates.
	prefillMaxOutputTokens = 1.5
	// minPromptTokensComputed is the fewest prompt tokens a request an
	// engine that runs prompts computes: at least the prompt's last token,
	// even on a full prefix-cache hit. A decode pod that received the
	// prompt's KV computes none.
	minPromptTokensComputed = 1.0
)

// RoleSignals tallies what one pod's engine readings say its role is, one
// reading per row Observe is given. A reading reads:
//
//   - one token: about one output token a request (AvgOutputTokens at
//     most 1.5). That is a prefill pod's, and also an aggregated pod's
//     under a workload of one-token requests, so the variant reads prefill
//     only where something corroborates it (RoleTally.Read);
//   - decode: more than one output token a request, and KV moving through
//     NIXL or under one prompt token computed a request while serving
//     (PrefillComputedTokenRate about 0);
//   - both (aggregated): more than one output token a request, prompt
//     tokens computed while serving, and no KV moving;
//   - nothing, else: an idle pod, or an engine that does not export the
//     series (SGLang has no computed-prompt counter and no NIXL series, so
//     its decode and aggregated pods read nothing).
//
// KV moving is NIXL bytes or transfer time counted at the pod, in either
// direction. vLLM counts a transfer on the side that issues it
// (NixlKVConnectorStats.record_transfer, from _pop_done_transfers): with
// the pull connector, NixlConnector, which llm-d's P/D deploys, that is the
// decode pod, which reads the prompt's KV, and the prefill pod counts
// neither the bytes nor the time; with the push connector
// (NixlPushConnector, v0.34.0 and later) it is the prefill pod, which
// writes it, and the decode pod counts nothing. Read as moving rather than
// arriving, it serves both.
//
// The labels are the source of truth where they exist (ResolveRole); the
// signals stand in where they do not, which is how both the controller and
// the capacity planner tell an unlabelled pod's role without reading its
// name.
type RoleSignals struct {
	// Readings counts every row observed; OneToken, Decode and Both the
	// ones that read each.
	Readings, OneToken, Decode, Both int
	// OneTokenKVMoved counts the one-token readings with KV moving at the
	// pod: a prefill pod that pushes its KV, which corroborates prefill.
	OneTokenKVMoved int
	// KVMoved and NoPrompt count the decode readings by what read them. A
	// reading can count in both.
	KVMoved, NoPrompt int
}

// Observe adds one reading of the pod.
func (s *RoleSignals) Observe(r ReplicaMetrics) {
	s.Readings++
	kv := kvMoved(r)
	serving := r.RequestRateKnown && r.RequestRate > 0
	computedKnown := serving && r.PrefillComputedTokenRateKnown
	noPrompt := computedKnown && r.PrefillComputedTokenRate/r.RequestRate < minPromptTokensComputed
	oneToken := r.AvgOutputTokens > 0 && r.AvgOutputTokens <= prefillMaxOutputTokens
	generates := r.AvgOutputTokens > prefillMaxOutputTokens
	switch {
	case oneToken:
		s.OneToken++
		if kv {
			s.OneTokenKVMoved++
		}
	case generates && (kv || noPrompt):
		s.Decode++
		if kv {
			s.KVMoved++
		}
		if noPrompt {
			s.NoPrompt++
		}
	case generates && computedKnown:
		s.Both++
	}
}

// kvMoved reports whether KV moved through NIXL at the pod in the reading,
// in either direction (RoleSignals).
func kvMoved(r ReplicaMetrics) bool {
	for _, t := range r.KVTransferRates {
		if t.Kind == KVTransferNIXL && t.ByteRate > 0 {
			return true
		}
	}
	return r.AvgKVTransferTimeKnown && r.AvgKVTransferTime > 0
}

// add is s with o's readings added.
func (s RoleSignals) add(o RoleSignals) RoleSignals {
	return RoleSignals{Readings: s.Readings + o.Readings, OneToken: s.OneToken + o.OneToken,
		Decode: s.Decode + o.Decode, Both: s.Both + o.Both, OneTokenKVMoved: s.OneTokenKVMoved + o.OneTokenKVMoved,
		KVMoved: s.KVMoved + o.KVMoved, NoPrompt: s.NoPrompt + o.NoPrompt}
}

// read is what most of the readings that read something read: prefill for
// one token (to be corroborated), decode or both; "" when none has more
// than half of them.
func (s RoleSignals) read() string {
	decided := s.OneToken + s.Decode + s.Both
	best, n := "", 0
	for _, c := range []struct {
		role string
		n    int
	}{{RolePrefill, s.OneToken}, {RoleDecode, s.Decode}, {RoleBoth, s.Both}} {
		if c.n > n {
			best, n = c.role, c.n
		}
	}
	if 2*n <= decided {
		return ""
	}
	return best
}

// Evidence names what the readings say, in one phrase: what they read, at
// how many readings, and the signal that read it.
func (s RoleSignals) Evidence() string {
	switch s.read() {
	case RolePrefill:
		return fmt.Sprintf("prefill at %d of %d readings (about 1 output token a request, KV moving at %d)",
			s.OneToken, s.Readings, s.OneTokenKVMoved)
	case RoleDecode:
		return fmt.Sprintf("decode at %d of %d readings (KV moving through NIXL at %d, no prompt computed at %d)",
			s.Decode, s.Readings, s.KVMoved, s.NoPrompt)
	case RoleBoth:
		return fmt.Sprintf("both at %d of %d readings (prompt computed and more than 1 output token a request)",
			s.Both, s.Readings)
	}
	return fmt.Sprintf("no role at %d readings (one token %d, decode %d, both %d)",
		s.Readings, s.OneToken, s.Decode, s.Both)
}

// VariantRoleSignals is one variant's RoleSignals, by Pod name, and its
// model.
type VariantRoleSignals struct {
	ModelID string
	Pods    map[string]RoleSignals
}

// Signals is every Pod's readings together; none for a nil v.
func (v *VariantRoleSignals) Signals() RoleSignals {
	var s RoleSignals
	if v == nil {
		return s
	}
	for _, p := range v.Pods {
		s = s.add(p)
	}
	return s
}

// Evidence names each Pod's evidence, in Pod order.
func (v *VariantRoleSignals) Evidence() string {
	if v == nil {
		return "no readings"
	}
	parts := make([]string, 0, len(v.Pods))
	for _, pod := range slices.Sorted(maps.Keys(v.Pods)) {
		parts = append(parts, pod+" "+v.Pods[pod].Evidence())
	}
	return strings.Join(parts, "; ")
}

// RoleTally is each variant's RoleSignals, by variant name, as rows are
// observed: one reading per row, so rows taken over several cycles read
// each Pod at each of them.
type RoleTally map[string]*VariantRoleSignals

// Observe adds rows' readings.
func (t RoleTally) Observe(rows ...ReplicaMetrics) {
	for _, r := range rows {
		v := t[r.VariantName]
		if v == nil {
			v = &VariantRoleSignals{ModelID: r.ModelID, Pods: map[string]RoleSignals{}}
			t[r.VariantName] = v
		}
		s := v.Pods[r.PodName]
		s.Observe(r)
		v.Pods[r.PodName] = s
	}
}

// RoleFromSignals is the tally of rows.
func RoleFromSignals(rows []ReplicaMetrics) RoleTally {
	t := RoleTally{}
	t.Observe(rows...)
	return t
}

// RoleReading is what a variant's engine signals read (RoleTally.Read).
type RoleReading struct {
	// Role is the role read, "" for none.
	Role string
	// Signals is the variant's readings; nil for none.
	Signals *VariantRoleSignals
	// uncorroborated is a variant whose readings read one token with
	// nothing to say it is prefill.
	uncorroborated bool
}

// Evidence names the evidence, Pod by Pod.
func (r RoleReading) Evidence() string {
	if r.uncorroborated {
		return r.Signals.Evidence() + ", read as no role: no decode variant in the model and no KV moving " +
			"to say it is prefill rather than aggregated under one-token requests"
	}
	return r.Signals.Evidence()
}

// RoleLabel is a variant's stated role and its model (RoleTally.Read).
type RoleLabel struct {
	ModelID, Role string
}

// Read is each tallied variant's reading. A variant read decode or both
// reads so. One read prefill (one token a request) reads prefill only when
// something corroborates it: KV moving at its own Pods, or a variant of the
// same model that reads decode or is labelled decode (labels: each
// labelled variant's label, by variant name, whether it has rows or not).
// Without either it reads no role: a model serving one-token requests from
// aggregated Pods reads the same.
func (t RoleTally) Read(labels map[string]RoleLabel) map[string]RoleReading {
	decodePeer := map[string]bool{}
	for _, l := range labels {
		if l.Role == RoleDecode {
			decodePeer[l.ModelID] = true
		}
	}
	for _, v := range t {
		if v.Signals().read() == RoleDecode {
			decodePeer[v.ModelID] = true
		}
	}
	out := make(map[string]RoleReading, len(t))
	for name, v := range t {
		s := v.Signals()
		r := RoleReading{Role: s.read(), Signals: v}
		if r.Role == RolePrefill && s.OneTokenKVMoved == 0 && !decodePeer[v.ModelID] {
			r.Role, r.uncorroborated = "", true
		}
		out[name] = r
	}
	return out
}

// ResolveRole is variant's role from its role label and its engine signals.
// A label wins where there is one (labelled): the role is label, and note
// says so when the signals read another role. Without one, the signals
// decide where they read a role, and label (the caller's default) stands
// where they do not; note names the evidence either way. note is "" when
// there is nothing to say: a label the signals agree with, or no readings.
func ResolveRole(variant, label string, labelled bool, r RoleReading) (role, note string) {
	switch {
	case labelled:
		if r.Role != "" && r.Role != CanonicalRole(label) {
			return label, fmt.Sprintf("variant %s: labelled %s, engine signals read %s: %s",
				variant, CanonicalRole(label), r.Role, r.Evidence())
		}
		return label, ""
	case r.Role != "":
		return r.Role, fmt.Sprintf("variant %s: role %s from engine signals, no role label: %s",
			variant, r.Role, r.Evidence())
	case r.Signals.Signals().Readings > 0:
		return label, fmt.Sprintf("variant %s: no role label and engine signals read no role, read as %s: %s",
			variant, CanonicalRole(label), r.Evidence())
	}
	return label, ""
}
