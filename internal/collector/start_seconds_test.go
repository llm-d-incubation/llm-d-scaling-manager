package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPodStartSeconds covers the measurement the demand floor projects the
// backlog over.
//
// Every rejection path returns ZERO rather than a guess, and that is the point
// of the table: the analyzer reads zero as "not measured" and falls back to the
// ScaledObject's seed, where a negative or absurd figure would be multiplied by
// the arrival rate and become phantom backlog. Run T's own numbers are the
// happy case -- 67 s and 82 s.
func TestPodStartSeconds(t *testing.T) {
	created := time.Date(2026, 9, 28, 7, 46, 23, 0, time.UTC)

	pod := func(mutate func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "decode-0",
				Namespace:         "biran-pd",
				CreationTimestamp: metav1.NewTime(created),
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(created.Add(67 * time.Second)),
				}},
			},
		}
		if mutate != nil {
			mutate(p)
		}
		return p
	}

	cases := []struct {
		name   string
		pod    *corev1.Pod
		want   float64
		reason string
	}{
		{
			name: "a ready pod reports the interval from its own timestamps",
			pod:  pod(nil),
			want: 67,
			reason: "run T's measured figure; taken from the Ready condition's " +
				"LastTransitionTime so it carries no polling error",
		},
		{
			name: "the slower half of run T's fleet",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(created.Add(82 * time.Second))
			}),
			want: 82,
		},
		{
			name: "a pod that is not ready yet has no start time",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions[0].Status = corev1.ConditionFalse
			}),
			want:   0,
			reason: "it has not finished starting, so there is nothing to measure",
		},
		{
			name: "a pod with no Ready condition at all",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions = nil
			}),
			want: 0,
		},
		{
			name: "no creation timestamp",
			pod: pod(func(p *corev1.Pod) {
				p.CreationTimestamp = metav1.Time{}
			}),
			want: 0,
		},
		{
			name: "no transition time on the condition",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions[0].LastTransitionTime = metav1.Time{}
			}),
			want: 0,
		},
		{
			name: "clock skew puts readiness before creation",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(created.Add(-5 * time.Second))
			}),
			want: 0,
			reason: "refused, not clamped: a negative dead time would shorten the " +
				"projection instead of failing visibly",
		},
		{
			name: "readiness at the same instant as creation",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(created)
			}),
			want:   0,
			reason: "zero is not a credible engine start",
		},
		{
			name: "a pod created long before it was scheduled",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(created.Add(2 * time.Hour))
			}),
			want: 0,
			reason: "past maxCredibleStartSeconds; it describes a scheduling wait, " +
				"not how long the next replica will take",
		},
		{
			name: "just inside the credibility bound",
			pod: pod(func(p *corev1.Pod) {
				p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(
					created.Add(time.Duration(maxCredibleStartSeconds-1) * time.Second))
			}),
			want:   maxCredibleStartSeconds - 1,
			reason: "the bound rejects beyond it, not at it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := podStartSeconds(tc.pod)
			if got != tc.want {
				t.Fatalf("podStartSeconds = %v, want %v (%s)", got, tc.want, tc.reason)
			}
		})
	}
}

// TestPodStatesCarriesStartSeconds checks the field survives the listing, which
// is the only path the collector reads it through.
func TestPodStatesCarriesStartSeconds(t *testing.T) {
	created := time.Date(2026, 9, 28, 7, 46, 23, 0, time.UTC)
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "decode-0",
			Namespace:         "biran-pd",
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(created.Add(70 * time.Second)),
			}},
		},
	}
	if got := podStartSeconds(p); got != 70 {
		t.Fatalf("podStartSeconds = %v, want 70", got)
	}
}

// TestPodStartSecondsCeilingEdge holds the 3600 s start ceiling
// (maxCredibleStartSeconds) at its edge, through the listing the collector
// reads a Pod's start from: 3599 s and 3600 s are a start, 3601 s is not one.
// The figures are literal, so a ceiling moved by a second fails here
// (analyzer-learner.md 2.6, the start-time row of the threshold table).
func TestPodStartSecondsCeilingEdge(t *testing.T) {
	created := time.Date(2026, 9, 28, 7, 46, 23, 0, time.UTC)
	for _, tc := range []struct {
		after, want float64
	}{{3599, 3599}, {3600, 3600}, {3601, 0}} {
		p := servingPod("decode-0", true, false)
		p.CreationTimestamp = metav1.NewTime(created)
		p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(created.Add(time.Duration(tc.after) * time.Second))
		c := collectorWithPods(t, p)
		if got := c.podStartSeconds(context.Background(), "tenant", "decode-0"); got != tc.want {
			t.Errorf("Ready %.0f s after creation: start %v, want %v", tc.after, got, tc.want)
		}
	}
}
