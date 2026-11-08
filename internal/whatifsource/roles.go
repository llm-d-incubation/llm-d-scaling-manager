package whatifsource

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// RoleOf is one --role-of ROLE=SELECTOR: the Pods a label selector picks
// serve ROLE, as one variant.
//
// The selector is matched against a Pod's labels where kube-state-metrics
// exports them (kube_pod_labels, with --metric-labels-allowlist). Where it
// does not, the Pod's name stands in: a selector of exactly one equality,
// app=m-prefill say, picks the Pods whose names begin "m-prefill-", which is
// how a Deployment names its Pods. That role map is inferred, and the
// capture says so (FlagRolesInferred).
type RoleOf struct {
	// Role is prefill, decode or both.
	Role string
	// Variant names the variant: the selector's one equality value, else
	// the selector as written.
	Variant string
	// Selector is the parsed selector, and raw the text it came from.
	Selector labels.Selector
	raw      string
	// prefix is the Pod-name prefix the selector infers, "" when it has
	// more than one requirement or one that is not an equality.
	prefix string
}

// ParseRoleOf parses one --role-of value, ROLE=SELECTOR.
func ParseRoleOf(s string) (RoleOf, error) {
	role, sel, ok := strings.Cut(s, "=")
	if !ok || sel == "" {
		return RoleOf{}, fmt.Errorf("--role-of %q: want ROLE=SELECTOR, for example prefill=app=m-prefill", s)
	}
	switch role {
	case domain.RolePrefill, domain.RoleDecode, domain.RoleBoth:
	default:
		return RoleOf{}, fmt.Errorf("--role-of %q: role %q is not prefill, decode or both", s, role)
	}
	parsed, err := labels.Parse(sel)
	if err != nil {
		return RoleOf{}, fmt.Errorf("--role-of %q: %w", s, err)
	}
	reqs, _ := parsed.Requirements()
	if len(reqs) == 0 {
		return RoleOf{}, fmt.Errorf("--role-of %q: the selector selects every Pod", s)
	}
	r := RoleOf{Role: role, Variant: sel, Selector: parsed, raw: sel}
	if len(reqs) == 1 {
		op := reqs[0].Operator()
		if vals := reqs[0].Values().List(); (op == selection.Equals || op == selection.DoubleEquals) && len(vals) == 1 {
			r.Variant, r.prefix = vals[0], vals[0]+"-"
		}
	}
	return r, nil
}

// String is the flag as written.
func (r RoleOf) String() string { return r.Role + "=" + r.raw }

// matches reports whether the Pod named pod, with the kube-state-metrics
// labels podLabels (nil when none are exported for it), is one of r's, and
// whether that was inferred from its name. The labels decide when they
// carry every key the selector names; an allowlist that exports other keys
// only says nothing of these, and the name decides.
func (r RoleOf) matches(pod string, podLabels map[string]string) (match, inferred bool) {
	if l := ksmLabels(podLabels); len(l) > 0 && r.keysIn(l) {
		return r.Selector.Matches(l), false
	}
	if r.prefix == "" {
		return false, false
	}
	return strings.HasPrefix(pod, r.prefix), true
}

// keysIn reports whether l carries every key r's selector names.
func (r RoleOf) keysIn(l ksmLabels) bool {
	reqs, _ := r.Selector.Requirements()
	for _, req := range reqs {
		if !l.Has(req.Key()) {
			return false
		}
	}
	return true
}

// ksmLabels reads a selector's keys as kube-state-metrics writes them:
// label_ and the key with every character outside [a-zA-Z0-9_] made an
// underscore. A selector names llm-d.ai/role; the series carries
// label_llm_d_ai_role.
type ksmLabels map[string]string

// Has implements labels.Labels.
func (l ksmLabels) Has(key string) bool {
	_, ok := l[ksmLabelName(key)]
	return ok
}

// Get implements labels.Labels.
func (l ksmLabels) Get(key string) string { return l[ksmLabelName(key)] }

// Lookup implements labels.Labels.
func (l ksmLabels) Lookup(key string) (string, bool) {
	v, ok := l[ksmLabelName(key)]
	return v, ok
}

var ksmInvalid = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// ksmLabelName is the label kube-state-metrics exports a Kubernetes label
// key as.
func ksmLabelName(key string) string {
	return "label_" + ksmInvalid.ReplaceAllString(key, "_")
}
