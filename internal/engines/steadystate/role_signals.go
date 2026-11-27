package steadystate

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// SwitchRolesFromSignals is the engine switch for reading an unlabelled
// variant's P/D role from its pods' engine signals (issue #502), with the
// classifier the capacity planner reads a capture's roles with
// (domain.RoleTally, domain.ResolveRole).
//
// Off (the default), a variant whose scale target declares no llm-d.ai/role
// reads both, as before, and nothing below runs. On, it reads the role its
// engine signals decide, and keeps it through cycles that decide nothing
// (roleMemory). That changes what the analyzers do with the variant:
//
//   - pricing: a variant read decode fits, and is priced from, its ITL
//     line, where an aggregated one's line is reported and not priced
//     (saturation itlLineReportOnly); one read prefill fits no line;
//   - learned state: the ITL line's learned-state key carries the role
//     (saturation itlLineKeyForRole, "|both" for an aggregated variant), so
//     a variant that changes role files and reads its line under another
//     key;
//   - the P/D path: a model with any variant whose role is not both is
//     disaggregated (collectV2ModelRequest), and its roles are priced and
//     scaled apart.
//
// Off from the constructor until evidence flips the default.
const SwitchRolesFromSignals = "engine-roles-from-signals"

// roleMemoryTTL is how long an unlabelled variant keeps the last role its
// signals decided through cycles that decide none: an idle decode variant
// (no KV moving, no request) does not fall back to both between bursts. A
// variant not decided again within it reads the default again.
const roleMemoryTTL = 30 * time.Minute

// SetSwitchesForReplay sets the engine's switches by name:
// SwitchRolesFromSignals. A name it does not know is an error, and sets
// nothing.
func (e *Engine) SetSwitchesForReplay(switches map[string]bool) error {
	rest := maps.Clone(switches)
	delete(rest, SwitchRolesFromSignals)
	if err := learnrec.UnknownSwitches("steadystate", rest); err != nil {
		return err
	}
	if on, ok := switches[SwitchRolesFromSignals]; ok {
		e.rolesFromSignals = on
	}
	return nil
}

// decidedRole is a role the signals decided, and when.
type decidedRole struct {
	role string
	at   time.Time
}

// roleMemory is, by namespace/variant, each unlabelled variant's last
// decided role (decided), and each labelled variant's role its signals last
// read against its label (disagreed), so the disagreement is logged once
// per change rather than every cycle.
type roleMemory struct {
	mu        sync.Mutex
	decided   map[string]decidedRole
	disagreed map[string]string
}

// newRoleMemory is an empty roleMemory.
func newRoleMemory() *roleMemory {
	return &roleMemory{decided: map[string]decidedRole{}, disagreed: map[string]string{}}
}

// roleMemory is e.roles: NewEngine builds it, and an Engine built as a
// literal builds it on first use.
func (e *Engine) roleMemory() *roleMemory {
	if e.roles == nil {
		e.roles = newRoleMemory()
	}
	return e.roles
}

// remembered is key's last decided role while within roleMemoryTTL of now;
// an older one is forgotten.
func (m *roleMemory) remembered(key string, now time.Time) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.decided[key]
	if !ok {
		return "", false
	}
	if now.Sub(d.at) > roleMemoryTTL {
		delete(m.decided, key)
		return "", false
	}
	return d.role, true
}

// decide records key's decided role at now.
func (m *roleMemory) decide(key, role string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decided[key] = decidedRole{role: role, at: now}
}

// disagreement records the role key's signals read against its label, ""
// for none, and reports whether it changed.
func (m *roleMemory) disagreement(key, read string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disagreed[key] == read {
		return false
	}
	if read == "" {
		delete(m.disagreed, key)
	} else {
		m.disagreed[key] = read
	}
	return true
}

// variantRole is the role a variant of scale target st carries this cycle
// outside the analyzer input: the label it declares; else, with
// SwitchRolesFromSignals on, the role its signals last decided
// (resolveRoles); else both. replicas_not_arriving reads it, so a variant
// carries one role in one cycle. The scale-from-zero engine reads labels
// only: it runs at zero replicas, with no engine to read.
func (e *Engine) variantRole(namespace, variant string, st scaletarget.ScaleTargetAccessor) string {
	if role, ok := variantmeta.DeclaredRole(st); ok {
		return role
	}
	if e.rolesFromSignals {
		if role, ok := e.roleMemory().remembered(utils.GetNamespacedKey(namespace, variant), e.observeClock()); ok {
			return role
		}
	}
	return domain.RoleBoth
}

// resolveRoles is variantStates and variantMetadata with each variant's
// role resolved against its pods' engine signals this cycle, with
// SwitchRolesFromSignals on; off, they are returned as given. A variant
// whose scale target declares a role (variantmeta.DeclaredRole: prefill,
// decode or both; an unknown value declares none, and the signals decide)
// keeps it, and a line is logged when the signals start to read another.
// One that declares none takes the role the signals read (domain.RoleTally,
// domain.ResolveRole), and is remembered (roleMemory); a cycle whose signals
// read none keeps the role last decided within roleMemoryTTL, and the
// discovery default (both) after it. A variant whose scale target is not
// among variants and scaleTargets keeps its role: nothing says it was not
// declared. The slices are copied before any change.
func (e *Engine) resolveRoles(ctx context.Context, namespace, modelID string, replicaMetrics []domain.ReplicaMetrics,
	variantStates []domain.VariantReplicaState, variantMetadata []domain.VariantMetadata,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor, variants map[string]*llmdvariant.Variant,
) ([]domain.VariantReplicaState, []domain.VariantMetadata) {
	if !e.rolesFromSignals {
		return variantStates, variantMetadata
	}
	declared := make(map[string]bool, len(variants))
	labels := map[string]domain.RoleLabel{}
	for _, va := range variants {
		st, ok := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		if !ok {
			continue
		}
		label, labelled := variantmeta.DeclaredRole(st)
		declared[va.Name] = labelled
		if labelled {
			labels[va.Name] = domain.RoleLabel{ModelID: modelID, Role: label}
		}
	}
	readings := domain.RoleFromSignals(replicaMetrics).Read(labels)
	memory := e.roleMemory()
	now := e.observeClock()
	logger := ctrl.LoggerFrom(ctx)
	resolved := map[string]string{}
	for _, vs := range variantStates {
		labelled, known := declared[vs.VariantName]
		if !known {
			continue
		}
		key := utils.GetNamespacedKey(namespace, vs.VariantName)
		reading := readings[vs.VariantName]
		role, note := domain.ResolveRole(vs.VariantName, vs.Role, labelled, reading)
		switch {
		case labelled:
			read := ""
			if note != "" {
				read = reading.Role
			}
			if memory.disagreement(key, read) && note != "" {
				logger.Info("A variant's role label disagrees with its engine signals; the label is used",
					"model", modelID, "note", note)
			} else if note != "" {
				logger.V(logging.DEBUG).Info("Variant role label still disagrees with its engine signals",
					"model", modelID, "note", note)
			}
		case reading.Role != "":
			memory.decide(key, role, now)
			logger.V(logging.DEBUG).Info("Variant role from engine signals", "model", modelID, "note", note)
		default:
			if last, ok := memory.remembered(key, now); ok {
				role = last
				logger.V(logging.DEBUG).Info("Variant role kept from engine signals decided earlier",
					"model", modelID, "variant", vs.VariantName, "role", last, "note", note)
			} else if note != "" {
				logger.V(logging.DEBUG).Info("Variant role from engine signals", "model", modelID, "note", note)
			}
		}
		if role != vs.Role {
			resolved[vs.VariantName] = role
		}
	}
	if len(resolved) == 0 {
		return variantStates, variantMetadata
	}
	variantStates = slices.Clone(variantStates)
	for i := range variantStates {
		if role, ok := resolved[variantStates[i].VariantName]; ok {
			variantStates[i].Role = role
		}
	}
	variantMetadata = slices.Clone(variantMetadata)
	for i := range variantMetadata {
		if role, ok := resolved[variantMetadata[i].VariantName]; ok {
			variantMetadata[i].Role = role
		}
	}
	return variantStates, variantMetadata
}
