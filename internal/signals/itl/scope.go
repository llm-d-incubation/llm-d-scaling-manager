package itl

import "strings"

// PriorScope is the physics a one-sided prior on B is read within (issue
// ev-shindin/llm-scaling-manager#467; BPrior): a learned B counts toward a variant's prior only when its
// key is the same model on the same accelerator and GPU count, under the
// same engine arguments (capacity.EngineParams.ArgsTag), in the same
// canonical role. Keys are the analyzers' window keys,
// namespace|model|variant|accelerator|gpus, so the variant may differ: a
// learned B is a property of the hardware and the engine, as the shared
// lines are. The role is known only for the variants of the model being
// analyzed (RoleOf), so a key of another namespace is out of scope: the
// rule errs toward the floor, never toward another engine's intercept.
//
// NewPriorScope builds one; the zero PriorScope holds no key at all.
type PriorScope struct {
	key, args string
	argsOf    func(key string) string
	roleOf    func(variant string) string
}

// NewPriorScope is the scope of the variant whose window key is key, whose
// readings run under engine arguments args ("" when unknown: then only its
// own key is in scope). argsOf is the engine arguments a key was last
// fitted under, "" when unknown, and roleOf a variant's canonical role by
// name, "" when unknown; both are read lazily, only for a key that matches
// the variant's model, accelerator and GPU count, so the caller passes its
// own lookups (under its own lock) rather than a copy.
func NewPriorScope(key, args string, argsOf func(key string) string, roleOf func(variant string) string) PriorScope {
	return PriorScope{key: key, args: args, argsOf: argsOf, roleOf: roleOf}
}

// Key is the variant's own window key.
func (s PriorScope) Key() string { return s.key }

// windowKeyFields splits a window key into its five fields; ok is false for
// any other shape.
func windowKeyFields(key string) (f [5]string, ok bool) {
	parts := strings.Split(key, "|")
	if len(parts) != 5 {
		return f, false
	}
	copy(f[:], parts)
	return f, true
}

// In reports whether key is in the scope of s: the variant's own key, or a
// key of the same namespace, model, accelerator and GPU count, the same
// known engine arguments, and the same known role.
func (s PriorScope) In(key string) bool {
	if s.key == "" {
		return false
	}
	if key == s.key {
		return true
	}
	if s.args == "" || s.argsOf == nil || s.roleOf == nil {
		return false
	}
	own, ok := windowKeyFields(s.key)
	if !ok {
		return false
	}
	other, ok := windowKeyFields(key)
	if !ok || other[0] != own[0] || other[1] != own[1] || other[3] != own[3] || other[4] != own[4] {
		return false
	}
	role := s.roleOf(own[2])
	return role != "" && s.roleOf(other[2]) == role && s.argsOf(key) == s.args
}

// ScopedBaselines is the learned intercepts of t's keys in scope s.
func (t *Tracker) ScopedBaselines(s PriorScope) []float64 {
	var out []float64
	for key, b := range t.Baselines() {
		if s.In(key) {
			out = append(out, b)
		}
	}
	return out
}
