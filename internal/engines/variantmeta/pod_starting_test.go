package variantmeta

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// podStarting decides whether a not-Ready Pod counts as capacity on its way.
//
// It gates anticipated supply, which is what the engine subtracts from demand
// before ordering -- so a Pod wrongly called "starting" withholds a scale-up for
// as long as it exists, and one wrongly called stuck orders a replica twice.
//
// The Unschedulable case is here because the first version of this function
// missed it: an unschedulable Pod is phase Pending with an EMPTY
// ContainerStatuses, so every container check waved it through. It is also the
// case the change was written for -- a fleet blocked on GPU quota.
func TestPodStarting(t *testing.T) {
	waiting := func(reason string) corev1.Pod {
		return corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: reason},
				},
			}},
		}}
	}

	cases := []struct {
		name string
		pod  corev1.Pod
		want bool
		why  string
	}{
		{
			name: "pending and scheduled is starting",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: true,
			why:  "the ordinary case: created, placed, pulling or loading",
		},
		{
			name: "running but not yet ready is starting",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			want: true,
			why:  "an engine loading weights is Running long before it is Ready",
		},
		{
			name: "unschedulable is NOT starting",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonUnschedulable,
				}},
			}},
			want: false,
			why: "GPU quota. No node, so no containers, so no container status " +
				"to inspect -- the check has to be on the condition",
		},
		{
			name: "succeeded is terminal",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			want: false,
		},
		{
			name: "failed is terminal",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			want: false,
		},
		{name: "image pull backoff", pod: waiting("ImagePullBackOff"), want: false},
		{name: "err image pull", pod: waiting("ErrImagePull"), want: false},
		{name: "crash loop backoff", pod: waiting("CrashLoopBackOff"), want: false},
		{name: "invalid image name", pod: waiting("InvalidImageName"), want: false},
		{
			name: "container creating is starting",
			pod:  waiting("ContainerCreating"),
			want: true,
			why: "the normal waiting reason on the way up; refusing it would " +
				"call every fresh Pod stuck",
		},
		{
			name: "pod initializing is starting",
			pod:  waiting("PodInitializing"),
			want: true,
		},
		{
			name: "scheduling gated is NOT starting",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonSchedulingGated,
				}},
			}},
			want: false,
			why: "Kueue quota: gated is Pending with no containers for as " +
				"long as the gate holds, which is the same shape as " +
				"Unschedulable and the same story",
		},
		{
			name: "a transient scheduler error is still starting",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionFalse,
					Reason: "SchedulerError",
				}},
			}},
			want: true,
			why: "only the two reasons that persist are refused; refusing " +
				"every PodScheduled=False would call a Pod mid-scheduling stuck",
		},
		{
			name: "init container in backoff is NOT starting",
			pod: corev1.Pod{Status: corev1.PodStatus{
				// Phase Pending with EMPTY ContainerStatuses: the failure is
				// only in InitContainerStatuses, which is what made this the
				// same blind spot Unschedulable had. The engine Pods this
				// repo ships fetch weights in an init container.
				Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff",
						},
					},
				}},
			}},
			want: false,
			why: "an init container that cannot pull never becomes Ready, so " +
				"the Pod withholds the scale-up for as long as it exists",
		},
		{
			name: "init container still pulling is starting",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "PodInitializing",
						},
					},
				}},
			}},
			want: true,
			why:  "the ordinary way up for a Pod that fetches weights first",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := podStarting(&tc.pod); got != tc.want {
				t.Fatalf("podStarting = %v, want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}
