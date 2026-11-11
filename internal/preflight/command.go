package preflight

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// namespaceFile is where a Pod reads its own namespace.
const namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// prometheusCAFile is where the Job mounts PrometheusCASecret (optional), the
// way the controller does: the preflight reads no Secret through the API.
const prometheusCAFile = "/etc/scaling-manager/prometheus-ca/ca.crt"

// Main runs `manager preflight [check|verify] [flags]` and returns the exit
// code: 0 when no check failed, 1 when one did, 2 when it could not run.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	mode := "check"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("preflight "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	namespace := fs.String("namespace", "", "the controller's namespace (default: POD_NAMESPACE, else the Pod's own)")
	scope := fs.String("scope", "auto", "namespace, cluster, or auto: cluster in "+ClusterNamespace+", else namespace")
	timeout := fs.Duration("check-timeout", 20*time.Second, "time each check may take; one that runs out is UNKNOWN")
	writeResult := fs.Bool("write-result", true, "write the result to ConfigMap "+ResultConfigMap)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch mode {
	case "check":
	case "verify":
		_, _ = fmt.Fprintln(stderr, "preflight verify is not built yet; after the install, check the rollout and each ScaledObject's HPA by hand (docs/reference/preflight.md)")
		return 2
	default:
		_, _ = fmt.Fprintf(stderr, "preflight: unknown mode %q; use check\n", mode)
		return 2
	}
	ns := *namespace
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	if ns == "" {
		if b, err := os.ReadFile(namespaceFile); err == nil {
			ns = strings.TrimSpace(string(b))
		}
	}
	if ns == "" {
		_, _ = fmt.Fprintln(stderr, "preflight: the controller's namespace is not known; pass --namespace")
		return 2
	}
	sc := Scope(*scope)
	switch sc {
	case "auto":
		sc = ScopeNamespace
		if ns == ClusterNamespace {
			sc = ScopeCluster
		}
	case ScopeNamespace, ScopeCluster:
	default:
		_, _ = fmt.Fprintf(stderr, "preflight: --scope %q is not namespace, cluster or auto\n", *scope)
		return 2
	}
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "preflight: no Kubernetes configuration: %v\n", err)
		return 2
	}
	// Every API call is bounded, discovery too (ServerGroups takes no
	// context): a check that hangs is UNKNOWN, never a Job that runs on.
	cfg.Timeout = *timeout
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "preflight: %v\n", err)
		return 2
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "preflight: %v\n", err)
		return 2
	}
	var promCA []byte
	if b, err := os.ReadFile(prometheusCAFile); err == nil {
		promCA = b
	}
	env := &Env{Kube: kube, Dynamic: dyn, Namespace: ns, Scope: sc, NewPrometheus: NewHTTPPrometheus,
		PrometheusCA: promCA, DialTLS: DialTLS, CheckTimeout: *timeout}
	return Execute(ctx, env, *writeResult, stdout, stderr)
}

// Execute runs the checks, prints them, writes the result ConfigMap when
// asked, and returns the exit code. A result that cannot be written is said
// on stderr; it does not change the exit code.
func Execute(ctx context.Context, env *Env, writeResult bool, stdout, stderr io.Writer) int {
	_, _ = fmt.Fprintf(stdout, "scaling manager preflight check: namespace %s, %s scope\n", env.Namespace, env.Scope)
	rep := Run(ctx, env)
	Print(stdout, rep)
	if writeResult {
		wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := WriteResult(wctx, env, rep); err != nil {
			_, _ = fmt.Fprintf(stderr, "preflight: the result was not written to ConfigMap %s: %v\n", ResultConfigMap, err)
		} else {
			_, _ = fmt.Fprintf(stdout, "result: ConfigMap %s in %s, key %s\n", ResultConfigMap, env.Namespace, ResultKey)
		}
	}
	return rep.ExitCode()
}
