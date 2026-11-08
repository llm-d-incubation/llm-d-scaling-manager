package allocation

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/kueue"
)

// InFlightFormat is the version of the persisted in-flight ledger
// (PersistedInFlight.Version) this build writes and reads. A ledger of another
// version is ignored.
const InFlightFormat = 1

// Bounds on the persisted in-flight ledger. It is written to the
// controller-status ConfigMap, whose whole content must stay well under the
// 1 MiB object limit, and read back from a ConfigMap anyone who can write it
// may have edited. Past maxInFlightPersisted entries, or maxInFlightBytes of
// them, the entries whose holds end soonest are left out; a ledger read back
// past them, or with a figure this controller does not write, has those
// entries dropped.
const (
	maxInFlightPersisted = 512
	maxInFlightBytes     = 128 << 10
	// maxInFlightPods bounds one replica's pods.
	maxInFlightPods = 256
	// maxTargetName bounds a scale target's name: a Kubernetes object name
	// is at most 253 characters.
	maxTargetName = 253
)

// PersistedInFlight is the in-flight ledger as the controller status keeps it
// (MarshalInFlight, RestoreInFlight): per variant key ("namespace/name"), the
// replicas still held and until when. Only what a hold needs is kept -- the
// replica's shape, how many replicas are to hold a node and how many do, the
// partial groups' placed pods, and the scale target whose pods to count.
type PersistedInFlight struct {
	Version int                               `json:"version"`
	Entries map[string]PersistedInFlightEntry `json:"entries,omitempty"`
}

// PersistedInFlightEntry is one variant's replicas in flight.
type PersistedInFlightEntry struct {
	Accelerator string                  `json:"accelerator,omitempty"`
	PodGPUs     []int                   `json:"podGPUs,omitempty"`
	GPUs        int                     `json:"gpus"`
	Tolerations []corev1.Toleration     `json:"tolerations,omitempty"`
	DomainKey   string                  `json:"domainKey,omitempty"`
	Placements  []domain.PodPlacement   `json:"placements,omitempty"`
	Flavor      *kueue.FlavorPlacement  `json:"flavor,omitempty"`
	Want        int                     `json:"want"`
	Placed      int                     `json:"placed"`
	Expires     time.Time               `json:"expires"`
	TTLSeconds  int64                   `json:"ttlSeconds"`
	Partial     []domain.PartialReplica `json:"partial,omitempty"`
	// TargetKind and TargetName name the variant's scale target, so the
	// restored entry counts its pods (RestoreInFlightWith). Absent from a
	// ledger an earlier controller wrote.
	TargetKind string `json:"targetKind,omitempty"`
	TargetName string `json:"targetName,omitempty"`
}

// MarshalInFlight is the ledger's live entries as of now, JSON, for the
// controller-status ConfigMap: every entry still holding a replica (before
// its expiry, fewer replicas placed than wanted), bounded by
// maxInFlightPersisted and maxInFlightBytes -- past them, the entries whose
// holds end soonest are left out. Each entry is encoded under the lock: its
// shape's tolerations, templates and flavor are shared with the live entry.
func (l *InFlightLedger) MarshalInFlight(now time.Time) (string, error) {
	type encoded struct {
		raw     json.RawMessage
		expires time.Time
	}
	live := map[string]encoded{}
	if l != nil {
		l.mu.Lock()
		for k, e := range l.entries {
			if !now.Before(e.expires) {
				continue
			}
			placed := e.base
			for _, s := range e.seen {
				placed = max(placed, s.n)
			}
			if e.want <= placed {
				continue
			}
			pe := PersistedInFlightEntry{Accelerator: e.shape.Accelerator, PodGPUs: e.shape.PodGPUs,
				GPUs: e.shape.GPUs, Tolerations: e.shape.Tolerations, DomainKey: e.shape.DomainKey,
				Placements: e.shape.Placements, Flavor: e.shape.Flavor, Want: e.want, Placed: placed,
				Expires: e.expires.UTC(), TTLSeconds: int64((e.ttl + time.Second - 1) / time.Second),
				TargetKind: e.target.Kind, TargetName: e.target.Name}
			for _, p := range e.partial {
				pe.Partial = append(pe.Partial, p.PartialReplica)
			}
			raw, err := json.Marshal(pe)
			if err != nil {
				l.mu.Unlock()
				return "", fmt.Errorf("in-flight ledger: %w", err)
			}
			live[k] = encoded{raw: raw, expires: e.expires}
		}
		l.mu.Unlock()
	}
	// The holds that end last are kept first; on a tie, by key.
	keys := slices.SortedFunc(maps.Keys(live), func(a, b string) int {
		return cmp.Or(live[b].expires.Compare(live[a].expires), cmp.Compare(a, b))
	})
	out := struct {
		Version int                        `json:"version"`
		Entries map[string]json.RawMessage `json:"entries,omitempty"`
	}{Version: InFlightFormat}
	size := 0
	for _, k := range keys {
		if len(out.Entries) >= maxInFlightPersisted {
			break
		}
		if size += len(k) + len(live[k].raw); size > maxInFlightBytes {
			break
		}
		if out.Entries == nil {
			out.Entries = map[string]json.RawMessage{}
		}
		out.Entries[k] = live[k].raw
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("in-flight ledger: %w", err)
	}
	return string(raw), nil
}

// UnmarshalInFlight parses a persisted in-flight ledger. A ledger that is not
// JSON, or of another version than InFlightFormat, is an error: the caller
// ignores it. Fields it does not know are ignored.
func UnmarshalInFlight(s string) (PersistedInFlight, error) {
	var p PersistedInFlight
	if s == "" {
		return PersistedInFlight{Version: InFlightFormat}, nil
	}
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return PersistedInFlight{}, fmt.Errorf("in-flight ledger: %w", err)
	}
	if p.Version != InFlightFormat {
		return PersistedInFlight{}, fmt.Errorf("in-flight ledger: version %d, this build reads %d", p.Version, InFlightFormat)
	}
	return p, nil
}

// RestoreInFlight loads a persisted ledger after a restart, normally before
// the first node view is built. Each entry still within its hold (its expiry
// after now) is restored as it was persisted: the same replicas held, on the
// same clock -- the restart neither extends a hold nor ends it -- and a
// partial group's placed pods, so its other pods stay held in their domain.
// That is what keeps a replica whose pods are Unschedulable, or a
// LeaderWorkerSet group whose leader holds a node and whose workers do not,
// held across a restart: the first cycle after it only updates an entry it
// finds (InFlightAdmission.ExistingOnly). RestoreInFlight counts no pods for
// a restored entry until a steady-state cycle admits its variant again and
// supplies the count (RestoreInFlightWith does, from the persisted scale
// target); a cycle that decides the variant without admitting it, or no
// longer sees it, ends it (DropRestoredExcept).
//
// A variant the ledger already holds is left as it is: a restore that comes
// late never overwrites a newer entry. An entry with a figure this controller
// does not write -- no "/" in its key, a want at or below its placed count or
// past maxLedgerReplicas, more partial groups than replicas held, a TTL under
// InFlightTTL -- is dropped first; of the rest, at most maxInFlightPersisted
// are restored, those whose holds end last, as MarshalInFlight keeps them. A
// TTL is cut to maxTTL (at least InFlightTTL: the longest hold the engine
// starts), and an expiry further than the TTL from now is brought back to it.
// It returns how many entries it restored, and how many it dropped as out of
// bounds, for the caller to log.
func (l *InFlightLedger) RestoreInFlight(p PersistedInFlight, now time.Time, maxTTL time.Duration) (restored, dropped int) {
	return l.RestoreInFlightWith(p, now, maxTTL, nil)
}

// RestoreInFlightWith is RestoreInFlight with each restored entry counting
// its variant's pods from the moment it is restored: placedFor, given the
// variant key and the persisted scale target, returns the count (as
// InFlightAdmission.Placed), or nil when it cannot count them. A restored
// hold whose pods land before any cycle admits its variant then ends, as a
// live entry does, instead of holding its hole until it expires. An entry
// persisted without a scale target (by an earlier controller) counts none.
// placedFor is called under the ledger's lock: it must only build the count,
// not run it.
func (l *InFlightLedger) RestoreInFlightWith(p PersistedInFlight, now time.Time, maxTTL time.Duration,
	placedFor func(key string, target ScaleTargetRef) func() (Placed, bool)) (restored, dropped int) {
	if l == nil {
		return 0, 0
	}
	var keys []string
	for k, pe := range p.Entries {
		switch {
		case !persistedEntryOK(k, pe):
			dropped++
		case now.Before(pe.Expires): // else its hold ended while the controller was down
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(p.Entries[b].Expires.Compare(p.Entries[a].Expires), cmp.Compare(a, b))
	})
	if len(keys) > maxInFlightPersisted {
		dropped += len(keys) - maxInFlightPersisted
		keys = keys[:maxInFlightPersisted]
	}
	limit := int64(max(maxTTL, InFlightTTL) / time.Second)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		pe := p.Entries[k]
		if _, held := l.entries[k]; held {
			continue
		}
		ttl := time.Duration(min(pe.TTLSeconds, limit)) * time.Second
		e := &inFlightEntry{
			shape: ReplicaShape{Variant: k, Accelerator: pe.Accelerator, PodGPUs: pe.PodGPUs, GPUs: pe.GPUs,
				Tolerations: pe.Tolerations, DomainKey: pe.DomainKey, Placements: pe.Placements, Flavor: pe.Flavor},
			want: pe.Want, base: pe.Placed, expires: minTime(pe.Expires, now.Add(ttl)), ttl: ttl,
			target: ScaleTargetRef{Kind: pe.TargetKind, Name: pe.TargetName},
		}
		if placedFor != nil && e.target.Name != "" {
			e.placed = placedFor(k, e.target)
		}
		// The placed pods are in any snapshot taken after the restart: seen
		// at the zero time, none is charged again (holdPartial).
		for _, r := range pe.Partial {
			e.partial = append(e.partial, partialSeen{PartialReplica: r, at: make([]time.Time, len(r.PlacedGPUs))})
		}
		l.entries[k] = e
		l.restored[k] = true
		restored++
	}
	return restored, dropped
}

// DropRestoredExcept ends every entry RestoreInFlight restored that no
// admission has updated since and that keep does not name. A steady-state
// cycle fitted to nodes keeps the variants it admits and those it did not
// decide while their variant is still there (a model it skipped, for example
// because its metrics are missing): their restored holds wait, on their own
// clock, for a cycle that admits them. It ends those of a variant deleted
// while the controller was down, or decided and no longer asking: such an
// entry, counting no pods of its own, would hold its hole until it expires.
// An entry kept unadmitted stays restored, for the next cycle to judge.
func (l *InFlightLedger) DropRestoredExcept(keep map[string]bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.restored {
		if _, live := l.entries[k]; !live || !keep[k] {
			delete(l.entries, k)
			delete(l.restored, k)
		}
	}
}

// persistedEntryOK reports whether a persisted entry keyed k holds only
// figures this controller writes (RestoreInFlight).
func persistedEntryOK(k string, pe PersistedInFlightEntry) bool {
	if !strings.Contains(k, "/") || pe.Placed < 0 || pe.Want <= pe.Placed || pe.Want > maxLedgerReplicas || pe.GPUs < 0 {
		return false
	}
	if pe.TTLSeconds < int64(InFlightTTL/time.Second) || len(pe.PodGPUs) > maxInFlightPods || len(pe.Partial) > pe.Want-pe.Placed {
		return false
	}
	switch pe.TargetKind {
	case "", constants.DeploymentKind, constants.LeaderWorkerSetKind:
	default:
		return false
	}
	if len(pe.TargetName) > maxTargetName {
		return false
	}
	return !slices.ContainsFunc(pe.Partial, func(r domain.PartialReplica) bool { return len(r.PlacedGPUs) > maxInFlightPods })
}

// minTime is the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
