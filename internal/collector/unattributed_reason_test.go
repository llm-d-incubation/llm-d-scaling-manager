package collector

import (
	"context"
	"strings"
	"testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// labelsLocator returns a mock whose GetPodLabels answers with the given labels
// for every pod. Attribution itself is not exercised here — these tests are about
// how an already-failed attribution is CLASSIFIED, which is what decides whether
// an operator chases a warm spare or a real bug.
func labelsLocator(labels map[string]string) *mockLocator {
	return &mockLocator{
		getPodLabelsFunc: func(_ context.Context, _, _ string) map[string]string { return labels },
	}
}

func TestUnattributedReason(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		wantReason string
		detailHas  string
	}{
		{
			name:       "ordinary pod nothing owns",
			labels:     map[string]string{"app": "something-else"},
			wantReason: constants.PodMappingMissUnresolved,
			detailHas:  "ownerReferences",
		},
		{
			// The case that must NOT be filed as a bug: a warm spare is doing
			// exactly what it is supposed to do.
			name:       "FMA launcher with no binding is a warm spare",
			labels:     map[string]string{constants.ComponentLabelKey: constants.LauncherComponent},
			wantReason: constants.PodMappingMissUnboundLauncher,
			detailHas:  "warm spare",
		},
		{
			// The case that IS worth chasing: FMA said these two are paired, and
			// the pairing still led nowhere.
			name: "FMA launcher whose declared partner did not resolve",
			labels: map[string]string{
				constants.ComponentLabelKey:    constants.LauncherComponent,
				constants.DualPodsPairLabelKey: "fma-requester-x",
			},
			wantReason: constants.PodMappingMissPairingUnresolved,
			detailHas:  "fma-requester-x",
		},
		{
			// A pairing label on something that is not a launcher is not an FMA
			// provider — the requester carries one too, and it resolves normally.
			name:       "pairing label alone does not make it a launcher",
			labels:     map[string]string{constants.DualPodsPairLabelKey: "some-pod"},
			wantReason: constants.PodMappingMissUnresolved,
			detailHas:  "ownerReferences",
		},
		{
			name:       "no labels at all",
			labels:     nil,
			wantReason: constants.PodMappingMissUnresolved,
			detailHas:  "ownerReferences",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, detail := unattributedReason(labelsLocator(tt.labels), context.Background(), "ns", "pod")
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
			if tt.detailHas != "" && !strings.Contains(detail, tt.detailHas) {
				t.Errorf("detail = %q, want it to mention %q", detail, tt.detailHas)
			}
		})
	}
}

// A nil locator must classify rather than panic: the collector runs this on the
// failure path, and a panic there would take out the whole collection cycle.
func TestUnattributedReason_NilLocator(t *testing.T) {
	reason, detail := unattributedReason(nil, context.Background(), "ns", "pod")
	if reason != constants.PodMappingMissUnresolved {
		t.Errorf("reason = %q, want %q", reason, constants.PodMappingMissUnresolved)
	}
	if detail == "" {
		t.Error("want a detail explaining why, got empty")
	}
}

func TestIsFMALauncher(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"launcher", map[string]string{constants.ComponentLabelKey: constants.LauncherComponent}, true},
		{"another component", map[string]string{constants.ComponentLabelKey: "inference"}, false},
		{"no component label", map[string]string{"app": "x"}, false},
		{"no labels", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFMALauncher(labelsLocator(tt.labels), context.Background(), "ns", "pod"); got != tt.want {
				t.Errorf("isFMALauncher = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsFMALauncher_NilLocator(t *testing.T) {
	if isFMALauncher(nil, context.Background(), "ns", "pod") {
		t.Error("a nil locator must not report a pod as a launcher")
	}
}
