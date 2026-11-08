package scalefromzero

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

func va(name, namespace, modelID, scaleTargetName string) llmdvariant.Variant {
	return llmdvariant.Variant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: llmdvariant.Spec{
			ModelID:        modelID,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: scaleTargetName},
		},
	}
}

// roleTarget builds a Deployment accessor whose pod template carries role, or no
// role label at all when role is "".
func roleTarget(role string) scaletarget.ScaleTargetAccessor {
	labels := map[string]string{}
	if role != "" {
		labels[variantmeta.RoleLabel] = role
	}
	return scaletarget.NewDeploymentAccessor(&appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
		},
	})
}

func TestGroupInactiveByModel(t *testing.T) {
	// Two variants of one model must land in ONE group: they are woken (or not)
	// as a single serving set against one GPU budget, and they share one EPP
	// queue. Splitting them would defeat both.
	vas := []llmdvariant.Variant{
		va("b-decode", "chat", "llama", "b-decode-deploy"),
		va("a-prefill", "chat", "llama", "a-prefill-deploy"),
		va("other", "chat", "mistral", "other-deploy"),
		va("elsewhere", "aaa-ns", "llama", "elsewhere-deploy"),
	}

	groups := groupInactiveByModel(vas, nil)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3 (llama@chat, mistral@chat, llama@aaa-ns)", len(groups))
	}

	// Same model in a different namespace is a different group: budgets and
	// pools are namespace-scoped.
	if groups[0].namespace != "aaa-ns" || groups[0].modelID != "llama" {
		t.Fatalf("groups[0] = %s/%s, want aaa-ns/llama (sorted by namespace then model)",
			groups[0].namespace, groups[0].modelID)
	}
	if groups[1].namespace != "chat" || groups[1].modelID != "llama" {
		t.Fatalf("groups[1] = %s/%s, want chat/llama", groups[1].namespace, groups[1].modelID)
	}
	if groups[2].modelID != "mistral" {
		t.Fatalf("groups[2] model = %s, want mistral", groups[2].modelID)
	}
	if got := len(groups[1].variants); got != 2 {
		t.Fatalf("chat/llama has %d variants, want 2 merged into one group", got)
	}
}

func TestGroupInactiveByModelIsStable(t *testing.T) {
	// Ordering must not depend on map iteration: the loop runs 10x a second and
	// an unstable order would make logs and concurrency behaviour nondeterministic.
	vas := []llmdvariant.Variant{
		va("v1", "ns-b", "m2", "t1"),
		va("v2", "ns-a", "m1", "t2"),
		va("v3", "ns-a", "m2", "t3"),
	}
	want := groupInactiveByModel(vas, nil)
	for i := 0; i < 20; i++ {
		got := groupInactiveByModel(vas, nil)
		for j := range want {
			if got[j].key() != want[j].key() {
				t.Fatalf("iteration %d: group %d = %s, want %s", i, j, got[j].key(), want[j].key())
			}
		}
	}
}

func TestActiveRoleCoverage(t *testing.T) {
	targets := map[string]scaletarget.ScaleTargetAccessor{
		utils.GetNamespacedKey("chat", "dec-deploy"):   roleTarget(domain.RoleDecode),
		utils.GetNamespacedKey("chat", "pre-deploy"):   roleTarget(domain.RolePrefill),
		utils.GetNamespacedKey("chat", "plain-deploy"): roleTarget(""),
	}

	tests := []struct {
		name        string
		active      []llmdvariant.Variant
		wantDecode  bool
		wantPrefill bool
	}{
		{
			name:   "no active variants covers nothing",
			active: nil,
		},
		{
			name:       "a running decode covers decode",
			active:     []llmdvariant.Variant{va("d", "chat", "llama", "dec-deploy")},
			wantDecode: true,
		},
		{
			// A prefill cannot serve on its own, so it must NOT mark decode
			// covered — otherwise scale-from-zero would decline to wake the
			// decode the model needs.
			name:        "a running prefill covers only prefill",
			active:      []llmdvariant.Variant{va("p", "chat", "llama", "pre-deploy")},
			wantPrefill: true,
		},
		{
			// An unlabelled workload is "both": it serves complete requests, so
			// it counts as decode.
			name:       "an unlabelled variant counts as decode",
			active:     []llmdvariant.Variant{va("v", "chat", "llama", "plain-deploy")},
			wantDecode: true,
		},
		{
			// No target in the map: RoleFromScaleTarget(nil) is RoleBoth, which
			// counts as decode. Over-reporting coverage only declines a wake for
			// a model something is already running — the safe direction.
			name:       "an unresolvable target counts as decode",
			active:     []llmdvariant.Variant{va("v", "chat", "llama", "missing-deploy")},
			wantDecode: true,
		},
		{
			name: "both roles running covers both",
			active: []llmdvariant.Variant{
				va("d", "chat", "llama", "dec-deploy"),
				va("p", "chat", "llama", "pre-deploy"),
			},
			wantDecode:  true,
			wantPrefill: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := activeRoleCoverage(tt.active, targets)[modelKey("chat", "llama")]
			if got.decode != tt.wantDecode || got.prefill != tt.wantPrefill {
				t.Fatalf("coverage = {decode:%v prefill:%v}, want {decode:%v prefill:%v}",
					got.decode, got.prefill, tt.wantDecode, tt.wantPrefill)
			}
		})
	}
}

func TestActiveRoleCoverageIsScopedPerModelAndNamespace(t *testing.T) {
	targets := map[string]scaletarget.ScaleTargetAccessor{
		utils.GetNamespacedKey("chat", "dec-deploy"): roleTarget(domain.RoleDecode),
	}
	covered := activeRoleCoverage(
		[]llmdvariant.Variant{va("d", "chat", "llama", "dec-deploy")}, targets)

	if !covered[modelKey("chat", "llama")].decode {
		t.Fatal("the running model should be covered")
	}
	if covered[modelKey("chat", "mistral")].decode {
		t.Fatal("a different model in the same namespace must not be covered")
	}
	if covered[modelKey("other-ns", "llama")].decode {
		t.Fatal("the same model in a different namespace must not be covered")
	}
}

// TestRefusalChangedLogsOncePerVerdict covers the 10Hz log-flood guard.
func TestRefusalChangedLogsOncePerVerdict(t *testing.T) {
	e := &Engine{}
	const key = "chat|llama"

	if !e.refusalChanged(key, OutcomeNoCapacity) {
		t.Fatal("the first refusal must be reported")
	}
	for i := 0; i < 50; i++ {
		if e.refusalChanged(key, OutcomeNoCapacity) {
			t.Fatal("an unchanged verdict must not be reported again")
		}
	}
	if !e.refusalChanged(key, OutcomePrefillRequired) {
		t.Fatal("a changed reason must be reported")
	}
	if e.refusalChanged("chat|other", OutcomeNoCapacity) != true {
		t.Fatal("a different model must be tracked independently")
	}

	// After a successful wake the slate is cleared, so a later refusal is
	// reported afresh rather than suppressed as unchanged.
	e.clearRefusal(key)
	if !e.refusalChanged(key, OutcomePrefillRequired) {
		t.Fatal("after clearRefusal the same reason must be reported again")
	}
}
