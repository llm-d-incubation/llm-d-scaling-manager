package utils

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DumpDemandEvidence prints what actually produced — or failed to produce — the
// demand a scale-from-zero spec is waiting on.
//
// Every scale-from-zero assertion depends on a chain the suite never showed:
// a trigger Job sends requests, the EPP queues them because the model has no
// ready endpoint, and the scaling manager reads that queue. When the assertion fails, the
// controller log says only "no pending requests in flow control queue", which is
// the scaling manager's view of the LAST link and is equally consistent with every earlier link
// having broken — the job never started, the requests were rejected outright
// instead of queueing, or they queued under labels nothing matches.
//
// A -v=4 capture of one failing run showed the cost of not distinguishing those:
// two models were polled continuously for five minutes each, 2652 and 2576
// samples, and the queue was never once non-empty, while models that passed found
// it occupied within a dozen samples. That rules out a sampling race — a request
// waiting in the queue would be seen at 10Hz — but it cannot say which earlier
// link failed, because none of them was recorded.
//
// So this dumps the trigger Job and its pods' own output. The script echoes an HTTP
// code per request, so it shows whether requests were sent at all, whether they
// were answered immediately or blocked, and what the gateway said. That is what
// identified the cause: ten instant 404s carrying vLLM's "the model does not
// exist", meaning the request was DISPATCHED to a pod serving something else
// rather than queued — so the queue scaling manager polls was never going to fill.
//
// The other half of that picture, who was serving in the pool, is asserted up
// front by requireEmptyServingPool rather than reported after the fact.
//
// Best-effort throughout: this runs on an already-failing path, so every step
// reports what it could not collect and continues.
func DumpDemandEvidence(ctx context.Context, k8sClient *kubernetes.Clientset, namespace string, w io.Writer) {
	dumpTriggerJobs(ctx, k8sClient, namespace, w)
}

// triggerJobNameHints match the Jobs that generate scale-from-zero demand. They
// are matched by name because the suite labels them the same as every other test
// resource.
var triggerJobNameHints = []string{"trigger", "load", "burst"}

func dumpTriggerJobs(ctx context.Context, k8sClient *kubernetes.Clientset, namespace string, w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n=== Demand generators (trigger Jobs) in %s ===\n", namespace)

	jobs, err := k8sClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(w, "Failed to list Jobs: %v\n", err)
		return
	}

	found := 0
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !looksLikeTriggerJob(job.Name) {
			continue
		}
		found++
		_, _ = fmt.Fprintf(w, "\n--- Job %s (active=%d succeeded=%d failed=%d, created %s ago) ---\n",
			job.Name, job.Status.Active, job.Status.Succeeded, job.Status.Failed,
			time.Since(job.CreationTimestamp.Time).Truncate(time.Second))
		for _, c := range job.Status.Conditions {
			_, _ = fmt.Fprintf(w, "  condition %s=%s: %s %s\n", c.Type, c.Status, c.Reason, c.Message)
		}
		dumpJobPods(ctx, k8sClient, namespace, job, w)
	}

	if found == 0 {
		_, _ = fmt.Fprintf(w, "No trigger Job found. If the spec was waiting on demand, nothing was "+
			"generating it — check whether the Job was created, or was already swept by cleanup.\n")
	}
}

func looksLikeTriggerJob(name string) bool {
	lower := strings.ToLower(name)
	for _, hint := range triggerJobNameHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func dumpJobPods(ctx context.Context, k8sClient *kubernetes.Clientset, namespace string, job *batchv1.Job, w io.Writer) {
	pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + job.Name,
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "  Failed to list pods: %v\n", err)
		return
	}
	if len(pods.Items) == 0 {
		_, _ = fmt.Fprintf(w, "  No pod for this Job — it never started, so no request was ever sent.\n")
		return
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		_, _ = fmt.Fprintf(w, "  pod %s: phase=%s node=%s\n", pod.Name, pod.Status.Phase, pod.Spec.NodeName)
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				_, _ = fmt.Fprintf(w, "    container %s waiting: %s %s\n",
					cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
			if cs.State.Terminated != nil {
				_, _ = fmt.Fprintf(w, "    container %s terminated: exit=%d reason=%s\n",
					cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
			}
		}
		// The whole log, not a tail: the interesting part is the FIRST request —
		// whether it was answered at once or held — and a tail would cut it off.
		logs, err := k8sClient.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "    Failed to get logs: %v\n", err)
			continue
		}
		_, _ = fmt.Fprintf(w, "    --- pod output ---\n%s\n", indentLines(string(logs), "    "))
	}
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
