package preflight

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// DefaultMonitorInterval is how often the controller runs the checks itself
// when it is told to (--self-check-interval).
const DefaultMonitorInterval = time.Minute

// ReadyOK is the ready status value when no check fails.
const ReadyOK = "ok"

// Event reasons on the controller-status ConfigMap, one per change of a
// check.
const (
	EventReasonCheckChanged = "PreflightCheckChanged"
)

// MonitorSink receives each run: one value per check, by check name, and the
// ready value (controllerstatus.Publisher.SetPreflight).
type MonitorSink interface {
	SetPreflight(checks map[string]string, ready string)
}

// Monitor runs the preflight checks inside the controller, at start and every
// Interval, and publishes each check and ready in the controller-status
// ConfigMap: the install from the one release manifest has no preflight Job
// (proposal PR #404, "What you get"). Each change
// of a check's verdict is also an Event on that ConfigMap.
//
// It runs as the controller's ServiceAccount, so a read the controller may
// not make is UNKNOWN, never a pass.
type Monitor struct {
	Env      *Env
	Sink     MonitorSink
	Interval time.Duration
	// Recorder, when set, records an Event per changed check on
	// StatusObject.
	Recorder record.EventRecorder
	// StatusObject names the controller-status ConfigMap the Events are on.
	StatusObject *corev1.ConfigMap
	Log          logr.Logger

	last map[string]Status
}

// NeedLeaderElection is true: the status ConfigMap is the leader's.
func (*Monitor) NeedLeaderElection() bool { return true }

// Start runs the checks until ctx ends. It implements manager.Runnable.
func (m *Monitor) Start(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = DefaultMonitorInterval
	}
	wait.UntilWithContext(ctx, func(ctx context.Context) { m.Once(ctx) }, interval)
	return nil
}

// Once runs every check once and publishes the result.
func (m *Monitor) Once(ctx context.Context) Report {
	rep := Run(ctx, m.Env)
	if ctx.Err() != nil {
		return rep
	}
	checks, ready := Summarize(rep)
	if m.Sink != nil {
		m.Sink.SetPreflight(checks, ready)
	}
	m.recordChanges(rep)
	return rep
}

// Summarize turns a report into the status values: per check, the lower-case
// verdict, the detail and the fix ("fail: <detail>; fix: <fix>"); and ready,
// ReadyOK when no check fails, else "not ready: " and the failing checks.
// Two results under one check name are joined with " | ".
func Summarize(rep Report) (checks map[string]string, ready string) {
	checks = map[string]string{}
	var failing []string
	for _, r := range rep.Results {
		v := strings.ToLower(string(statusWord(r.Status))) + ": " + r.Detail
		if r.Fix != "" {
			v += "; fix: " + strings.ReplaceAll(r.Fix, "\n", " ")
		}
		if prev, ok := checks[r.Check]; ok {
			v = prev + " | " + v
		}
		checks[r.Check] = v
		if r.Status == Fail && !slices.Contains(failing, r.Check) {
			failing = append(failing, r.Check)
		}
	}
	if len(failing) == 0 {
		return checks, ReadyOK
	}
	slices.Sort(failing)
	return checks, "not ready: " + strings.Join(failing, ", ") + " (see the preflight.<check> keys)"
}

// statusWord maps PASS to ok, the word the status keys use; the rest keep
// their name.
func statusWord(s Status) Status {
	if s == Pass {
		return "ok"
	}
	return s
}

// recordChanges records an Event per check whose verdict changed since the
// last run, the first run included.
func (m *Monitor) recordChanges(rep Report) {
	now := map[string]Status{}
	for _, r := range rep.Results {
		if prev, ok := now[r.Check]; !ok || worse(r.Status, prev) {
			now[r.Check] = r.Status
		}
	}
	if m.Recorder != nil && m.StatusObject != nil {
		for _, name := range slices.Sorted(maps.Keys(now)) {
			if before, ok := m.last[name]; ok && before == now[name] {
				continue
			}
			typ := corev1.EventTypeNormal
			if now[name] == Fail || now[name] == Warn {
				typ = corev1.EventTypeWarning
			}
			m.Recorder.Event(m.StatusObject, typ, EventReasonCheckChanged,
				fmt.Sprintf("preflight check %s is %s", name, strings.ToLower(string(statusWord(now[name])))))
		}
	}
	m.last = now
}

// worse orders the verdicts FAIL > UNKNOWN > WARN > PASS.
func worse(a, b Status) bool {
	rank := map[Status]int{Pass: 0, Warn: 1, Unknown: 2, Fail: 3}
	return rank[a] > rank[b]
}

// NewControllerEnv builds the Env the controller's Monitor runs with: the
// controller's own credentials, cluster scope, each call bounded by timeout,
// and Prometheus's CA from where the Deployment mounts Secret
// scaling-manager-prometheus-ca (read again on every run, so a Secret
// created later is picked up).
func NewControllerEnv(restCfg *rest.Config, namespace string, timeout time.Duration) (*Env, error) {
	cfg := rest.CopyConfig(restCfg)
	cfg.Timeout = timeout
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	env := &Env{Kube: kube, Dynamic: dyn, Namespace: namespace, Scope: ScopeCluster,
		DialTLS: DialTLS, CheckTimeout: timeout}
	env.NewPrometheus = func(pc PrometheusConfig) (Prometheus, error) {
		if pc.CA == nil {
			if b, err := os.ReadFile(config.PrometheusCAFromSecretPath); err == nil {
				pc.CA = b
			}
		}
		return NewHTTPPrometheus(pc)
	}
	return env, nil
}

// StatusObjectRef returns the controller-status ConfigMap as the object the
// Monitor's Events name.
func StatusObjectRef(namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: ControllerStatusConfigMap, Namespace: namespace},
	}
}
