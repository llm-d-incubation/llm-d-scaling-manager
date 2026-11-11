// Package preflight checks, from inside the cluster and before an install,
// that the scaling manager's prerequisites are in place: KEDA, the scaler's
// mutual TLS, Prometheus and the model-server metrics, the controller's RBAC,
// Kueue, and no second controller for the same namespaces.
//
// It runs as `manager preflight check` in a Job the release attaches
// (scaling-manager-preflight.yaml), under a read-only ServiceAccount of its
// own. Each check prints one line, PASS, WARN, FAIL or UNKNOWN, and the
// command that fixes it. A read the API server refused, or one that timed out,
// is UNKNOWN: never PASS and never "absent". Only a NotFound is absent.
//
// Design: docs/proposals/install-from-release.md, "The preflight check".
package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Status is the verdict of one check.
type Status string

const (
	// Pass means the check was made and found nothing wrong.
	Pass Status = "PASS"
	// Warn means the install works, degraded or with a risk the line names.
	Warn Status = "WARN"
	// Fail means the install will not work until the fix is applied.
	Fail Status = "FAIL"
	// Unknown means the check could not be made: a read was refused, timed
	// out, or depends on something not there yet. It is not a pass.
	Unknown Status = "UNKNOWN"
)

// Names the release files and the controller use.
const (
	// ControllerServiceAccount is the controller's ServiceAccount.
	ControllerServiceAccount = "scaling-manager-controller-manager"
	// SettingsConfigMap is the settings ConfigMap the operator creates.
	SettingsConfigMap = "scaling-manager-settings"
	// LegacyConfigMap is the configuration an install made before v0.11.0
	// holds, read for a key the settings do not hold.
	LegacyConfigMap = "scaling-manager-manager-config"
	// ResultConfigMap is where a run writes its result as JSON.
	ResultConfigMap = "scaling-manager-preflight-result"
	// ResultKey is the data key of ResultConfigMap holding the JSON.
	ResultKey = "result.json"
	// PrometheusCASecret holds the CA that signs Prometheus's certificate.
	PrometheusCASecret = "scaling-manager-prometheus-ca"
	// KEDAMarkerConfigMap says KEDA's side of mutual TLS is in place.
	KEDAMarkerConfigMap = "scaling-manager-scaler-keda-auth"
	// ScalerNetworkPolicy admits KEDA to the scaler port.
	ScalerNetworkPolicy = "scaling-manager-external-scaler"
	// ScalerServingSecret is the scaler's serving certificate.
	ScalerServingSecret = "scaling-manager-external-scaler-tls"
	// ScalerClientSecret is KEDA's client certificate and the client CA.
	ScalerClientSecret = "scaling-manager-external-scaler-client-tls"
	// ClusterNamespace is the namespace the cluster-wide release files
	// install in; a preflight run there checks a cluster-wide install.
	ClusterNamespace = "scaling-manager-system"
)

// Result is one check's verdict, as printed and as written to the result
// ConfigMap.
type Result struct {
	Check  string `json:"check"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Scope is what the checked install manages.
type Scope string

const (
	// ScopeNamespace is one controller managing its own namespace, or the
	// namespaces watchNamespaces names.
	ScopeNamespace Scope = "namespace"
	// ScopeCluster is one controller for the cluster.
	ScopeCluster Scope = "cluster"
)

// Env is what a run reads from. Tests fill it with fakes.
type Env struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// Namespace is the controller's namespace, where the Job runs.
	Namespace string
	Scope     Scope
	// NewPrometheus builds the Prometheus client from the settings and the
	// CA read from PrometheusCASecret (nil when there is none).
	NewPrometheus func(cfg PrometheusConfig) (Prometheus, error)
	// CheckTimeout bounds each check; a check that runs out is UNKNOWN.
	CheckTimeout time.Duration
	Now          func() time.Time
}

// Report is a whole run.
type Report struct {
	Mode          string         `json:"mode"`
	Namespace     string         `json:"namespace"`
	Scope         Scope          `json:"scope"`
	Time          string         `json:"time"`
	Summary       map[Status]int `json:"summary"`
	NotApplicable []string       `json:"notApplicable,omitempty"`
	Results       []Result       `json:"results"`
}

// Failed reports whether any check failed: the run then exits 1.
func (r Report) Failed() bool { return r.Summary[Fail] > 0 }

// ExitCode is the Job's exit code: 1 on any FAIL, else 0. UNKNOWN does not
// fail the Job; the summary line says it is not a pass.
func (r Report) ExitCode() int {
	if r.Failed() {
		return 1
	}
	return 0
}

// check is one named check. run returns its results (usually one), or none
// with a reason when it does not apply to this install.
type check struct {
	name string
	run  func(ctx context.Context, s *state) (results []Result, notApplicable string)
}

// checks is every check of `check` mode, in the order they print. The order
// matters: later checks read what earlier ones found (the Prometheus client,
// the PodMonitors).
var checks = []check{
	{"settings", checkSettings},
	{"keda.crd", checkKEDACRD},
	{"keda.version", checkKEDAVersion},
	{"keda.networkpolicy", checkKEDANetworkPolicy},
	{"keda.mtls", checkKEDAMTLS},
	{"prometheus.reachable", checkPrometheusReachable},
	{"metrics.model", checkModelMetrics},
	{"podmonitor.port", checkPodMonitorPort},
	{"prometheus.selects", checkPrometheusSelects},
	{"openshift.user-monitoring", checkOpenShiftUserMonitoring},
	{"rbac.controller", checkControllerRBAC},
	{"rbac.namespaces", checkNamespaceReaders},
	{"kueue", checkKueue},
	{"overlap", checkOverlap},
	{"accelerators", checkAccelerators},
	{"activation", checkActivation},
}

// CheckNames lists the checks `check` mode runs, in order.
func CheckNames() []string {
	names := make([]string, len(checks))
	for i, c := range checks {
		names[i] = c.name
	}
	return names
}

// Run makes every check against env and returns the report.
func Run(ctx context.Context, env *Env) Report {
	if env.CheckTimeout <= 0 {
		env.CheckTimeout = 20 * time.Second
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	s := &state{env: env}
	rep := Report{
		Mode:      "check",
		Namespace: env.Namespace,
		Scope:     env.Scope,
		Time:      env.Now().UTC().Format(time.RFC3339),
		Summary:   map[Status]int{Pass: 0, Warn: 0, Fail: 0, Unknown: 0},
	}
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, env.CheckTimeout)
		results, na := c.run(cctx, s)
		cancel()
		if len(results) == 0 {
			if na == "" {
				// A check that says nothing has not been made.
				results = []Result{{Status: Unknown, Detail: "the check returned no verdict"}}
			} else {
				rep.NotApplicable = append(rep.NotApplicable, c.name+" ("+na+")")
				continue
			}
		}
		for _, r := range results {
			if r.Check == "" {
				r.Check = c.name
			}
			rep.Results = append(rep.Results, r)
			rep.Summary[r.Status]++
		}
	}
	return rep
}

// Print writes the report as the Job logs it: one line per check, its fix
// indented under it, then the summary.
func Print(w io.Writer, rep Report) {
	width := 0
	for _, r := range rep.Results {
		width = max(width, len(r.Check))
	}
	for _, r := range rep.Results {
		_, _ = fmt.Fprintf(w, "%-7s %-*s  %s\n", r.Status, width, r.Check, r.Detail)
		if r.Fix != "" {
			for i, line := range strings.Split(r.Fix, "\n") {
				lead := "fix:"
				if i > 0 {
					lead = "    "
				}
				_, _ = fmt.Fprintf(w, "        %s %s\n", lead, line)
			}
		}
	}
	_, _ = fmt.Fprintf(w, "%d PASS, %d WARN, %d FAIL, %d UNKNOWN\n",
		rep.Summary[Pass], rep.Summary[Warn], rep.Summary[Fail], rep.Summary[Unknown])
	if rep.Summary[Unknown] > 0 {
		_, _ = fmt.Fprintln(w, "UNKNOWN is not a pass: those checks were not made. Each line says what was refused or missing.")
	}
	if len(rep.NotApplicable) > 0 {
		_, _ = fmt.Fprintf(w, "not applicable: %s\n", strings.Join(rep.NotApplicable, "; "))
	}
}

// WriteResult creates or updates ResultConfigMap in the controller's
// namespace with the report as JSON.
func WriteResult(ctx context.Context, env *Env, rep Report) error {
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the result: %w", err)
	}
	data := map[string]string{
		ResultKey: string(body),
		"summary": fmt.Sprintf("%d PASS, %d WARN, %d FAIL, %d UNKNOWN",
			rep.Summary[Pass], rep.Summary[Warn], rep.Summary[Fail], rep.Summary[Unknown]),
	}
	cms := env.Kube.CoreV1().ConfigMaps(env.Namespace)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ResultConfigMap,
			Namespace: env.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of":  "scaling-manager",
				"app.kubernetes.io/instance": env.Namespace,
			},
		},
		Data: data,
	}
	_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ConfigMap %s in %s: %w", ResultConfigMap, env.Namespace, err)
	}
	live, err := cms.Get(ctx, ResultConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading ConfigMap %s in %s: %w", ResultConfigMap, env.Namespace, err)
	}
	live.Data = data
	if _, err := cms.Update(ctx, live, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating ConfigMap %s in %s: %w", ResultConfigMap, env.Namespace, err)
	}
	return nil
}

// readVerdict turns a failed API read into a status and a phrase: NotFound is
// absent (ok=true, absent=true), anything else, Forbidden and timeouts
// included, is UNKNOWN with what was refused.
func readVerdict(err error, what string) (absent bool, why string) {
	switch {
	case apierrors.IsNotFound(err):
		return true, what + " not found"
	case apierrors.IsForbidden(err):
		return false, "cannot read " + what + ": forbidden"
	case apierrors.IsUnauthorized(err):
		return false, "cannot read " + what + ": unauthorized"
	default:
		return false, "cannot read " + what + ": " + firstLine(err.Error())
	}
}

// unknown is the UNKNOWN result for a read that failed with err.
func unknown(what string, err error) Result {
	_, why := readVerdict(err, what)
	return Result{Status: Unknown, Detail: why}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}

// listText joins up to n items, saying how many more there are.
func listText(items []string, n int) string {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	if len(sorted) <= n {
		return strings.Join(sorted, ", ")
	}
	return strings.Join(sorted[:n], ", ") + fmt.Sprintf(" (+%d more)", len(sorted)-n)
}
