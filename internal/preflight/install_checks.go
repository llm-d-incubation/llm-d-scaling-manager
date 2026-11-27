package preflight

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// The checks the kubectl install proposal (PR #404, appendix B) adds:
// conflicts with another scaling manager, which policy is in force, a double
// scrape, and the EPP metric names this release reads.

// Keys of ControllerStatusConfigMap policy.source reads.
const (
	statusPolicyNamespace = "policyNamespace"
	statusPolicyConfigMap = "policyConfigMap"
	statusPolicyAccepted  = "policyAccepted"
	statusPolicyError     = "policyError"
	statusSettingsSource  = "settingsSource"
)

// doubleScrapeQuery returns each model-server pod scraped by more than one
// job: the inner group is one series per (pod, job), the outer count the
// jobs per pod. Counting series per job instead would pass a pod two jobs
// scrape, and fail a pod with several series under one job (an engine or
// model_name label per data-parallel rank or LoRA adapter).
const doubleScrapeQuery = `count by (namespace, pod) (group by (namespace, pod, job) (vllm:num_requests_running)) > 1`

// eppQueueSizeNames are the EPP flow-control queue metrics this release
// reads, the current name first (constants.EPPFlowControlQueueSize and its
// deprecated alias).
var eppQueueSizeNames = []string{"llm_d_epp_flow_control_queue_size", "inference_extension_flow_control_queue_size"}

// checkConflicts finds another scaling manager's controller bound where this
// one manages, from RoleBindings and ClusterRoleBindings, never from
// Deployments (a Deployment's env holds that tenant's settings). Two
// controllers on one workload each publish a replica count for it.
func checkConflicts(ctx context.Context, s *state) ([]Result, string) {
	ns := s.env.Namespace
	foreign := func(sub []rbacSubject) string {
		for _, x := range sub {
			if x.kind == "ServiceAccount" && x.name == ControllerServiceAccount && x.namespace != "" && x.namespace != ns {
				return x.namespace
			}
		}
		return ""
	}
	crbs, err := s.env.Kube.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		r := unknown("ClusterRoleBindings", err)
		r.Detail += " (the conflict check needs list on rolebindings and clusterrolebindings)"
		return []Result{r}, ""
	}
	clusterWide := map[string]bool{}
	for _, b := range crbs.Items {
		if !strings.HasPrefix(b.RoleRef.Name, "scaling-manager-manager-role") {
			continue
		}
		if other := foreign(subjectsOf(b.Subjects)); other != "" {
			clusterWide[other] = true
		}
	}
	rbs, err := s.env.Kube.RbacV1().RoleBindings("").List(ctx, metav1.ListOptions{})
	if err != nil {
		r := unknown("RoleBindings in every namespace", err)
		r.Detail += " (the conflict check needs list on rolebindings and clusterrolebindings)"
		return []Result{r}, ""
	}
	// inNS: model namespace -> the other controllers' namespaces bound there.
	inNS := map[string][]string{}
	for _, b := range rbs.Items {
		if !strings.HasPrefix(b.RoleRef.Name, "scaling-manager-manager-role") {
			continue
		}
		if other := foreign(subjectsOf(b.Subjects)); other != "" && !slices.Contains(inNS[b.Namespace], other) {
			inNS[b.Namespace] = append(inNS[b.Namespace], other)
		}
	}
	var fails, warns []string
	for _, other := range sortedKeys(clusterWide) {
		fails = append(fails, fmt.Sprintf("a cluster-wide scaling manager runs in %s: it manages the namespaces this one does", other))
	}
	managed := map[string]bool{ns: true}
	for _, w := range config.ParseWatchNamespaces(s.env.WatchNamespace) {
		managed[w] = true
	}
	for _, mns := range sortedKeys(inNS) {
		others := strings.Join(inNS[mns], ", ")
		if s.env.Scope == ScopeNamespace {
			if managed[mns] {
				fails = append(fails, fmt.Sprintf("namespace %s is also managed by the scaling manager in %s", mns, others))
			}
			continue
		}
		if s.env.Scope != ScopeCluster {
			continue
		}
		if s.env.manages(mns) {
			fails = append(fails, fmt.Sprintf("namespace %s is managed here and by the scaling manager in %s", mns, others))
		} else {
			warns = append(warns, fmt.Sprintf("the scaling manager in %s manages %s: do not add %s to %s here", others, mns, mns, config.SettingWatchNamespaces))
		}
	}
	switch {
	case len(fails) > 0:
		return []Result{{Status: Fail, Detail: strings.Join(append(fails, warns...), "; "),
			Fix: "list only the namespaces this install manages in its " + config.SettingWatchNamespaces + " setting, or uninstall one of the two"}}, ""
	case len(warns) > 0:
		return []Result{{Status: Warn, Detail: strings.Join(warns, "; ")}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("no other scaling manager is bound where this one manages (%d ClusterRoleBindings, %d RoleBindings read)",
		len(crbs.Items), len(rbs.Items))}}, ""
}

// manages reports whether the checked cluster-scoped controller manages
// namespace: Manages when set, else WatchNamespace, where empty is every
// namespace.
func (e *Env) manages(namespace string) bool {
	if e.Manages != nil {
		return e.Manages(namespace)
	}
	list := config.ParseWatchNamespaces(e.WatchNamespace)
	return len(list) == 0 || slices.Contains(list, namespace)
}

type rbacSubject struct{ kind, name, namespace string }

func subjectsOf(in []rbacv1.Subject) []rbacSubject {
	out := make([]rbacSubject, 0, len(in))
	for _, x := range in {
		out = append(out, rbacSubject{x.Kind, x.Name, x.Namespace})
	}
	return out
}

// checkPolicySource says which scaling policy is in force, as the running
// controller reports it: the policy ConfigMap's namespace and name, whether
// its default entry was accepted, and whether the settings ConfigMap
// overrides it.
func checkPolicySource(ctx context.Context, s *state) ([]Result, string) {
	ns := s.env.Namespace
	cm, err := s.env.Kube.CoreV1().ConfigMaps(ns).Get(ctx, ControllerStatusConfigMap, metav1.GetOptions{})
	if err != nil {
		absent, why := readVerdict(err, "ConfigMap "+ControllerStatusConfigMap+" in "+ns)
		if absent {
			return nil, "the controller has not run yet"
		}
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	d := cm.Data
	where := fmt.Sprintf("ConfigMap %s in %s", d[statusPolicyConfigMap], d[statusPolicyNamespace])
	if d[statusPolicyNamespace] == "" {
		return []Result{{Status: Unknown, Detail: "the controller has not reported its policy yet"}}, ""
	}
	settings := "the settings ConfigMap overrides its default entry where it sets a key"
	if d[statusSettingsSource] == "" || d[statusSettingsSource] == "defaults" || d[statusSettingsSource] == "manager-config" {
		settings = "no settings ConfigMap overrides it"
	}
	if d[statusPolicyNamespace] != ns {
		settings = "a separate policy namespace: the settings' limiter and utilizationShare do not override it"
	}
	if accepted, err := strconv.ParseBool(d[statusPolicyAccepted]); err == nil && !accepted {
		return []Result{{Status: Fail, Detail: fmt.Sprintf("the policy in %s was rejected: %s", where, d[statusPolicyError]),
			Fix: "fix the default entry of " + where}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("the scaling policy is read from %s; %s", where, settings)}}, ""
}

// checkScrape finds model-server pods Prometheus scrapes more than once: a
// second scrape doubles every rate the analyzers read. A pod not scraped at
// all is metrics.model's.
func checkScrape(ctx context.Context, s *state) ([]Result, string) {
	if !s.promUsable {
		return nil, "Prometheus is not reachable from here"
	}
	samples, err := s.prom.Query(ctx, doubleScrapeQuery)
	if err != nil {
		return []Result{{Status: Unknown, Detail: "the double-scrape query failed: " + firstLine(err.Error())}}, ""
	}
	if len(samples) == 0 {
		return []Result{{Status: Pass, Detail: "no model-server pod is scraped by more than one job"}}, ""
	}
	var pods []string
	for _, x := range samples {
		pods = append(pods, x.Labels["namespace"]+"/"+x.Labels["pod"])
	}
	return []Result{{Status: Fail,
		Detail: "scraped by more than one job, which doubles every rate the controller reads: " + listText(pods, 5),
		Fix:    "keep one PodMonitor per model server: llm-d's monitoring component or scaling-manager-monitoring.yaml, not both"}}, ""
}

// checkEPPMetrics checks that Prometheus holds the EPP's flow-control queue
// metric under a name this release reads; without it scale-from-zero sees no
// demand.
func checkEPPMetrics(ctx context.Context, s *state) ([]Result, string) {
	if !s.promUsable {
		return nil, "Prometheus is not reachable from here"
	}
	samples, err := s.prom.Query(ctx, `group by (__name__) ({__name__=~"`+strings.Join(eppQueueSizeNames, "|")+`"})`)
	if err != nil {
		return []Result{{Status: Unknown, Detail: "the EPP metric query failed: " + firstLine(err.Error())}}, ""
	}
	var found []string
	for _, x := range samples {
		found = append(found, x.Labels["__name__"])
	}
	if len(found) == 0 {
		return []Result{{Status: Warn,
			Detail: "no EPP flow-control queue series under a name this release reads (" + strings.Join(eppQueueSizeNames, ", ") +
				"): scale-from-zero sees no demand. Flow control off on the EPP, or the EPP not scraped",
			Fix: "enable the flowControl feature gate on the EPP, and scrape it (appendix D of the kubectl install proposal)"}}, ""
	}
	sort.Strings(found)
	return []Result{{Status: Pass, Detail: "the EPP exports " + strings.Join(found, ", ")}}, ""
}
