package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deny refuses one verb on one resource, everywhere or in one namespace.
func deny(verb, resource, ns string) func(authorizationv1.ResourceAttributes) bool {
	return func(a authorizationv1.ResourceAttributes) bool {
		return a.Verb != verb || a.Resource != resource || (ns != "" && a.Namespace != ns)
	}
}

// watchBoth makes the controller's Deployment name team-a and team-b, so the
// watchNamespaces setting may name both (the controller only lets it narrow).
func watchBoth(f *fixture) {
	f.add(controllerDeployment(testNS, "--watch-namespace="+testNS+",team-b"))
}

func TestControllerRBAC(t *testing.T) {
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"not installed yet", func(f *fixture) { f.remove("ServiceAccount", ControllerServiceAccount) }, Unknown, "run preflight again after applying"},
		{"ServiceAccount forbidden", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("get", "serviceaccounts")) }, Unknown, "forbidden"},
		{"cannot list deployments", func(f *fixture) { f.sar = deny("list", "deployments", "") }, Fail, "list deployments.apps in " + testNS},
		{"cannot read a watched namespace", func(f *fixture) {
			f.settings[keyWatchNamespaces] = testNS + ",team-b"
			watchBoth(f)
			f.sar = deny("list", "pods", "team-b")
		}, Fail, "list pods in team-b"},
		{"SubjectAccessReview forbidden", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("create", "subjectaccessreviews")) }, Unknown, "forbidden"},
		{"no node read, no limiter", func(f *fixture) { f.sar = deny("list", "nodes", "") }, Warn, "not list nodes"},
		{"no node read, gpu-inventory", func(f *fixture) {
			f.sar = deny("list", "nodes", "")
			f.settings[keyLimiter] = "gpu-inventory"
		}, Fail, "never scales up"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "rbac.controller")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
	// The fix for a watched namespace names its model-namespace file.
	f := newFixture(t)
	f.settings[keyWatchNamespaces] = testNS + ",team-b"
	watchBoth(f)
	f.sar = deny("list", "pods", "team-b")
	if r := only(t, f.run(t), "rbac.controller"); !strings.Contains(r.Fix, `s/__MODEL_NAMESPACE__/team-b/g`) {
		t.Errorf("fix %q", r.Fix)
	}
}

// Decision 5: the narrowed namespace readers name each watched namespace,
// and preflight prints the patch that adds one.
func TestNamespaceReaders(t *testing.T) {
	patch := `kubectl patch clusterrole scaling-manager-policy-namespace-reader-role-team-a --type json -p '[{"op":"add","path":"/rules/0/resourceNames/-","value":"team-b"}]'`
	t.Run("missing namespace", func(t *testing.T) {
		f := newFixture(t)
		f.settings[keyWatchNamespaces] = testNS + ",team-b"
		watchBoth(f)
		r := only(t, f.run(t), "rbac.namespaces")
		if r.Status != Fail || r.Fix != patch {
			t.Errorf("got %s %q\nfix  %q\nwant %q", r.Status, r.Detail, r.Fix, patch)
		}
	})
	t.Run("not applied yet", func(t *testing.T) {
		f := newFixture(t).remove("ClusterRole", "scaling-manager-policy-namespace-reader-role-"+testNS)
		f.settings[keyWatchNamespaces] = testNS + ",team-b"
		watchBoth(f)
		r := only(t, f.run(t), "rbac.namespaces")
		if r.Status != Warn || !strings.Contains(r.Fix, patch) {
			t.Errorf("got %s %q fix %q", r.Status, r.Detail, r.Fix)
		}
	})
	t.Run("named", func(t *testing.T) {
		f := newFixture(t).remove("ClusterRole", "scaling-manager-policy-namespace-reader-role-"+testNS)
		f.add(namespaceReader(testNS, testNS, "scaling-manager-policy", "team-b"))
		f.settings[keyWatchNamespaces] = testNS + ",team-b"
		watchBoth(f)
		if r := only(t, f.run(t), "rbac.namespaces"); r.Status != Pass {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		f := newFixture(t)
		f.kubeReact = append(f.kubeReact, forbidden("get", "clusterroles"))
		if r := only(t, f.run(t), "rbac.namespaces"); r.Status != Unknown {
			t.Errorf("got %s %q", r.Status, r.Detail)
		}
	})
	t.Run("Kueue reader too", func(t *testing.T) {
		f := newFixture(t)
		f.settings[keyWatchNamespaces] = testNS + ",team-b"
		watchBoth(f)
		f.settings[keyKueue] = valueTrue
		f.add(&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "scaling-manager-kueue-reader-role-" + testNS},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{"kueue.x-k8s.io"}, Resources: []string{"clusterqueues"}, Verbs: []string{"get"}},
				{APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: []string{testNS}, Verbs: []string{"get"}},
			},
		})
		rs := results(f.run(t), "rbac.namespaces")
		if len(rs) != 2 || rs[1].Status != Fail || !strings.Contains(rs[1].Fix, `kueue-reader-role-team-a --type json -p '[{"op":"add","path":"/rules/1/resourceNames/-","value":"team-b"}]'`) {
			t.Errorf("got %+v", rs)
		}
	})
	t.Run("cluster scope", func(t *testing.T) {
		f := newFixture(t)
		f.scope = ScopeCluster
		if rep := f.run(t); !notApplicable(rep, "rbac.namespaces") {
			t.Errorf("should not apply: %+v", results(rep, "rbac.namespaces"))
		}
	})
}

func TestKueue(t *testing.T) {
	kueueCRDs := func(f *fixture) {
		for _, n := range []string{"clusterqueues", "localqueues", "resourceflavors", "workloads"} {
			f.dyn = append(f.dyn, crd(n+".kueue.x-k8s.io"))
		}
	}
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
	}{
		{"not installed", func(*fixture) {}, Fail},
		{"installed, readable", kueueCRDs, Pass},
		{"installed, controller may not read", func(f *fixture) { kueueCRDs(f); f.sar = deny("list", "clusterqueues", "") }, Fail},
		{"CRD read forbidden", func(f *fixture) { f.dynReact = append(f.dynReact, forbidden("get", "customresourcedefinitions")) }, Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.settings[keyKueue] = valueTrue
			f.settings[keyLimiter] = limiterQuota
			f.settings[keyQuotas] = testQuota
			tc.change(f)
			if r := only(t, f.run(t), "kueue"); r.Status != tc.want {
				t.Errorf("got %s %q, want %s", r.Status, r.Detail, tc.want)
			}
		})
	}
}

func TestOverlap(t *testing.T) {
	nsArgs := []string{"--watch-namespace=$(LLMSM_WATCH_NAMESPACE)"}
	withEnv := func(ns, watch string) func(*fixture) {
		return func(f *fixture) {
			d := controllerDeployment(ns, nsArgs...)
			d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "LLMSM_WATCH_NAMESPACE", Value: watch}}
			f.add(d)
		}
	}
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
	}{
		{"cluster-wide peer", func(f *fixture) { f.add(controllerDeployment(ClusterNamespace)) }, Fail, "cluster-wide"},
		{"peer manages team-a", withEnv("ops", testNS), Fail, "ops/" + ControllerDeployment + " already manages " + testNS},
		{"peer manages its own", withEnv("team-b", "$(POD_NAMESPACE)"), Pass, "team-b/"},
		{"this install upgrades", func(f *fixture) { f.add(controllerDeployment(testNS, nsArgs...)) }, Pass, "no other"},
		{"forbidden", func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("list", "deployments")) }, Unknown, "forbidden"},
		{"cluster install beside a namespaced one", func(f *fixture) {
			f.scope = ScopeCluster
			withEnv("team-b", "$(POD_NAMESPACE)")(f)
		}, Fail, "this cluster-wide install would manage too"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "overlap")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
		})
	}
}

func TestAccelerators(t *testing.T) {
	gpuNode := node("gpu-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H200"})
	cases := []struct {
		name   string
		quotas string
		change func(*fixture)
		want   Status
	}{
		{"family matches the product", testQuota, func(f *fixture) { f.add(gpuNode) }, Pass},
		{"product spelled as the node does", "NVIDIA-H200=8", func(f *fixture) { f.add(gpuNode) }, Pass},
		{"key matches nothing", "H100=8", func(f *fixture) { f.add(gpuNode) }, Warn},
		{"no GPU node", testQuota, func(*fixture) {}, Warn},
		{"nodes forbidden", testQuota, func(f *fixture) { f.kubeReact = append(f.kubeReact, forbidden("list", "nodes")) }, Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.settings[keyLimiter] = limiterQuota
			f.settings[keyQuotas] = tc.quotas
			tc.change(f)
			if r := only(t, f.run(t), "accelerators"); r.Status != tc.want {
				t.Errorf("got %s %q, want %s", r.Status, r.Detail, tc.want)
			}
		})
	}
}

// Decision 3: active sharing needs the pod-writer grant in each sharing
// namespace, and preflight prints the apply line for each one missing.
func TestActivation(t *testing.T) {
	active := func(f *fixture) {
		f.settings[keyUtilizationShare] = "active"
		f.settings[keyLimiter] = limiterQuota
		f.settings[keyQuotas] = testQuota
		f.settings[keyWatchNamespaces] = testNS + ",team-b"
		watchBoth(f)
	}
	f := newFixture(t)
	active(f)
	f.sar = deny("patch", "pods", "team-b")
	r := only(t, f.run(t), "activation")
	want := `sed -e "s/__NAMESPACE__/team-a/g" -e "s/__MODEL_NAMESPACE__/team-b/g" scaling-manager-activate.yaml | kubectl apply -f -`
	if r.Status != Fail || r.Fix != want {
		t.Errorf("got %s %q\nfix  %q\nwant %q", r.Status, r.Detail, r.Fix, want)
	}
	f = newFixture(t)
	active(f)
	if r := only(t, f.run(t), "activation"); r.Status != Pass {
		t.Errorf("granted: %s %q", r.Status, r.Detail)
	}
	f = newFixture(t)
	active(f)
	f.kubeReact = append(f.kubeReact, forbidden("create", "subjectaccessreviews"))
	if r := only(t, f.run(t), "activation"); r.Status != Unknown {
		t.Errorf("review forbidden: %s %q", r.Status, r.Detail)
	}
	f = newFixture(t)
	f.settings[keyUtilizationShare] = "shadow"
	f.settings[keyLimiter] = limiterQuota
	f.settings[keyQuotas] = testQuota
	if rep := f.run(t); !notApplicable(rep, "activation") {
		t.Errorf("shadow: activation should not apply")
	}
}

// Every API read the run makes, refused at once: no check may PASS on what it
// could not read, and none may say a thing is absent.
func TestEverythingForbiddenIsNeverAPass(t *testing.T) {
	f := newFixture(t)
	for _, verb := range []string{"get", "list"} {
		f.kubeReact = append(f.kubeReact, forbidden(verb, "*"))
		f.dynReact = append(f.dynReact, forbidden(verb, "*"))
	}
	f.kubeReact = append(f.kubeReact, forbidden("create", "subjectaccessreviews"))
	rep := f.run(t)
	var b strings.Builder
	Print(&b, rep)
	for _, r := range rep.Results {
		if r.Status == Pass {
			t.Errorf("%s PASSED on forbidden reads: %s", r.Check, r.Detail)
		}
		if r.Status != Unknown {
			// A FAIL or WARN here is a refused read taken for an absent
			// object.
			t.Errorf("%s is %s on forbidden reads, want UNKNOWN: %s", r.Check, r.Status, r.Detail)
		}
	}
	if t.Failed() {
		t.Log(b.String())
	}
}

func TestExitCodeAndResultConfigMap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	env := f.env(t)
	var out, errb bytes.Buffer
	if code := Execute(ctx, env, true, &out, &errb); code != 0 {
		t.Fatalf("healthy: exit %d\n%s%s", code, out.String(), errb.String())
	}
	res, err := env.Kube.CoreV1().ConfigMaps(testNS).Get(ctx, ResultConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("result ConfigMap: %v", err)
	}
	var rep Report
	if err := json.Unmarshal([]byte(res.Data[ResultKey]), &rep); err != nil {
		t.Fatalf("result JSON: %v", err)
	}
	if rep.Summary[Fail] != 0 || len(rep.Results) == 0 || res.Labels["app.kubernetes.io/part-of"] != "scaling-manager" {
		t.Fatalf("result %+v labels %v", rep.Summary, res.Labels)
	}

	// A FAIL exits 1 and updates the same ConfigMap.
	f.settings[keyPrometheusURL] = ""
	_, _ = env.Kube.CoreV1().ConfigMaps(testNS).Update(ctx, cm(testNS, SettingsConfigMap, f.settings), metav1.UpdateOptions{})
	out.Reset()
	if code := Execute(ctx, env, true, &out, &errb); code != 1 {
		t.Fatalf("with a FAIL: exit %d, want 1\n%s", code, out.String())
	}
	res, _ = env.Kube.CoreV1().ConfigMaps(testNS).Get(ctx, ResultConfigMap, metav1.GetOptions{})
	if !strings.Contains(res.Data["summary"], "1 FAIL") {
		t.Fatalf("summary after the update: %q", res.Data["summary"])
	}
	if !strings.Contains(out.String(), "FAIL    prometheus.reachable") || !strings.Contains(out.String(), "        fix: kubectl patch configmap "+SettingsConfigMap) {
		t.Fatalf("output:\n%s", out.String())
	}

	// UNKNOWN alone does not fail the Job, and the summary says it is no pass.
	rep = Report{Summary: map[Status]int{Pass: 1, Unknown: 2}}
	if rep.ExitCode() != 0 {
		t.Fatal("UNKNOWN failed the run")
	}
	var b strings.Builder
	Print(&b, rep)
	if !strings.Contains(b.String(), "1 PASS, 0 WARN, 0 FAIL, 2 UNKNOWN\n") || !strings.Contains(b.String(), "is not a pass") {
		t.Fatalf("summary:\n%s", b.String())
	}
}

// A result that cannot be written is said, and leaves the exit code alone.
func TestResultWriteRefused(t *testing.T) {
	f := newFixture(t)
	f.kubeReact = append(f.kubeReact, forbidden("create", "configmaps"))
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), f.env(t), true, &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "was not written") {
		t.Fatalf("stderr %q", errb.String())
	}
}
