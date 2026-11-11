package preflight

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/accelerator"
)

// controllerSelector finds scaling manager controllers: control-plane is what
// makes one a controller (the warm pool carries the name label too).
const controllerSelector = "app.kubernetes.io/name=scaling-manager,control-plane=controller-manager"

// userMonitoringLabel is the label OpenShift's user-workload monitoring reads
// on a namespace: "false" keeps it out.
const userMonitoringLabel = "openshift.io/user-monitoring"

// checkOverlap: no other controller manages a namespace this one would (the
// installer's single-install guard, deploy/lib/single_install.sh).
func checkOverlap(ctx context.Context, s *state) ([]Result, string) {
	list, err := s.env.Kube.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: controllerSelector})
	if err != nil {
		return []Result{unknown("deployments in every namespace (to find other scaling manager controllers)", err)}, ""
	}
	mine, named := s.managed()
	var clash, peers []string
	for _, d := range list.Items {
		if d.Namespace == s.env.Namespace {
			continue // this install: an upgrade, not a second controller
		}
		watched, scoped := watchedBy(d)
		id := d.Namespace + "/" + d.Name
		switch {
		case !scoped:
			clash = append(clash, id+" is cluster-wide and already manages every namespace")
		case !named:
			clash = append(clash, fmt.Sprintf("%s manages %s, which this cluster-wide install would manage too", id, strings.Join(watched, ",")))
		default:
			var both []string
			for _, w := range watched {
				if slices.Contains(mine, w) {
					both = append(both, w)
				}
			}
			if len(both) > 0 {
				clash = append(clash, fmt.Sprintf("%s already manages %s", id, strings.Join(both, ",")))
			} else {
				peers = append(peers, id)
			}
		}
	}
	if len(clash) > 0 {
		return []Result{{Status: Fail,
			Detail: "a second controller for the same workloads: " + strings.Join(clash, "; "),
			Fix:    "install in a namespace no other controller manages, or remove the other controller first"}}, ""
	}
	if len(peers) > 0 {
		return []Result{{Status: Pass, Detail: "other controllers manage other namespaces (" + listText(peers, 5) + "); they share the GPUs, so bound each with a quota"}}, ""
	}
	return []Result{{Status: Pass, Detail: "no other scaling manager controller in the cluster"}}, ""
}

// watchedBy reads what a controller Deployment manages from its
// --watch-namespace argument, as the installer does; false when it has none
// (cluster-wide). $(VAR) references are resolved from the container's env,
// $(POD_NAMESPACE) to the Deployment's namespace.
func watchedBy(d appsv1.Deployment) ([]string, bool) {
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return nil, false
	}
	c := d.Spec.Template.Spec.Containers[0]
	env := map[string]string{"POD_NAMESPACE": d.Namespace}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			env[e.Name] = e.Value
		} else if e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.namespace" {
			env[e.Name] = d.Namespace
		}
	}
	expand := func(v string) string {
		for range 3 {
			for k, val := range env {
				v = strings.ReplaceAll(v, "$("+k+")", val)
			}
		}
		return v
	}
	args := append(append([]string(nil), c.Command...), c.Args...)
	for i, a := range args {
		var v string
		switch {
		case strings.HasPrefix(a, "--watch-namespace="):
			v = strings.TrimPrefix(a, "--watch-namespace=")
		case a == "--watch-namespace" && i+1 < len(args):
			v = args[i+1]
		default:
			continue
		}
		if w := splitList(expand(v)); len(w) > 0 {
			return w, true
		}
	}
	return nil, false
}

// checkAccelerators: with a quota, each quota key names an accelerator some
// node carries; a key that matches none bounds nothing.
func checkAccelerators(ctx context.Context, s *state) ([]Result, string) {
	st := s.loadSettings(ctx)
	quotas, err := parseQuotas(st.value(keyQuotas))
	if err != nil || len(quotas) == 0 {
		return nil, "no quotas are set"
	}
	nodes, err := s.env.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return []Result{unknown("nodes", err)}, ""
	}
	seen := map[string]bool{}
	for _, n := range nodes.Items {
		for _, key := range accelerator.GetProductKeys() {
			if v := n.Labels[key]; v != "" {
				seen[v] = true
			}
		}
	}
	if len(seen) == 0 {
		return []Result{{Status: Warn, Detail: "no node carries an accelerator label (" + strings.Join(accelerator.GetProductKeys(), ", ") + "), so no quota key can match one"}}, ""
	}
	var unmatched []string
	for key := range quotas {
		hit := false
		for v := range seen {
			if accelerator.Covers(key, v) || accelerator.Covers(key, accelerator.NormalizeAcceleratorName(v)) {
				hit = true
				break
			}
		}
		if !hit {
			unmatched = append(unmatched, key)
		}
	}
	names := slices.Sorted(maps.Keys(seen))
	if len(unmatched) > 0 {
		slices.Sort(unmatched)
		return []Result{{Status: Warn,
			Detail: fmt.Sprintf("quota keys %s match no node's accelerator (nodes carry %s): those quotas bound nothing", strings.Join(unmatched, ", "), listText(names, 8)),
			Fix:    fmt.Sprintf(`kubectl patch configmap %s -n %s --type merge -p '{"data":{"quotas":"<NAME>=<N>,..."}}'   # NAME as the nodes spell it, or its family (H200 for NVIDIA-H200)`, SettingsConfigMap, s.env.Namespace)}}, ""
	}
	return []Result{{Status: Pass, Detail: "every quota key matches a node's accelerator (" + listText(names, 8) + ")"}}, ""
}

// checkOpenShiftUserMonitoring: on OpenShift, user-workload monitoring
// scrapes the controller's and the model servers' namespaces. A namespace
// labelled openshift.io/user-monitoring=false is left out. The install files
// carry no Namespace, so the controller's namespace does not get the "true"
// label the make install gave it.
func checkOpenShiftUserMonitoring(ctx context.Context, s *state) ([]Result, string) {
	osh, why := s.isOpenShift()
	switch osh {
	case triNo:
		return nil, "not OpenShift"
	case triUnknown:
		return []Result{{Status: Unknown, Detail: "whether this is OpenShift is not known: " + why}}, ""
	}
	nss, named := s.managed()
	if !named {
		pods, _ := s.modelServers(ctx)
		nss = sortedKeys(pods)
	}
	all := append([]string{s.env.Namespace}, nss...)
	slices.Sort(all)
	all = slices.Compact(all)
	var off, unset []string
	for _, ns := range all {
		n, err := s.env.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			return []Result{unknown("namespace "+ns, err)}, ""
		}
		switch n.Labels[userMonitoringLabel] {
		case "false":
			off = append(off, ns)
		case valueTrue:
		default:
			if ns == s.env.Namespace {
				unset = append(unset, ns)
			}
		}
	}
	label := func(ns string) string {
		return fmt.Sprintf("kubectl label namespace %s %s=true --overwrite", ns, userMonitoringLabel)
	}
	if len(off) > 0 {
		var fix []string
		for _, ns := range off {
			fix = append(fix, label(ns))
		}
		return []Result{{Status: Fail,
			Detail: strings.Join(off, ", ") + " labelled " + userMonitoringLabel + "=false: user-workload monitoring does not scrape there",
			Fix:    strings.Join(fix, "\n")}}, ""
	}
	if len(unset) > 0 {
		return []Result{{Status: Warn,
			Detail: s.env.Namespace + " is not labelled " + userMonitoringLabel + "=true, which the make install sets on the controller's namespace",
			Fix:    label(s.env.Namespace)}}, ""
	}
	return []Result{{Status: Pass, Detail: "user-workload monitoring covers " + strings.Join(all, ", ")}}, ""
}

// sortedKeys returns a map's keys in order.
func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}
