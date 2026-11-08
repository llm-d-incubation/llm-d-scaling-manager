package controllerstatus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"gopkg.in/yaml.v3"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

const testNamespace = "scaling-manager-system"

// writes counts the Create and Update calls that reached the fake client, and
// lets a test fail the next few of either kind.
type writes struct {
	creates, updates int
	failCreate       []error
	failUpdate       []error
}

func newTestPublisher(t *testing.T, w *writes, objs ...client.Object) (*Publisher, client.Client) {
	t.Helper()
	t.Setenv("POD_NAME", "scaling-manager-controller-manager-abc")
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			w.creates++
			if len(w.failCreate) > 0 {
				err := w.failCreate[0]
				w.failCreate = w.failCreate[1:]
				return err
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			w.updates++
			if len(w.failUpdate) > 0 {
				err := w.failUpdate[0]
				w.failUpdate = w.failUpdate[1:]
				return err
			}
			return cl.Update(ctx, obj, opts...)
		},
	})
	return New(c, c, testNamespace), base
}

func stored(t *testing.T, c client.Client) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: ConfigMapName}, &cm); err != nil {
		t.Fatalf("reading %s: %v", ConfigMapName, err)
	}
	return &cm
}

var configMapsResource = schema.GroupResource{Resource: "configmaps"}

func TestFlushCreatesOnceAndSkipsUnchanged(t *testing.T) {
	w := &writes{}
	p, c := newTestPublisher(t, w)
	ctx := context.Background()

	if err := p.flush(ctx); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	cm := stored(t, c)
	if cm.Labels[labelName] != labelNameValue || cm.Labels[labelComponent] != componentValue {
		t.Errorf("labels = %v", cm.Labels)
	}
	if got := cm.Data[KeyFormatVersion]; got != FormatVersion {
		t.Errorf("formatVersion = %q", got)
	}
	if got := cm.Data[KeyLeader]; got != "scaling-manager-controller-manager-abc" {
		t.Errorf("leader = %q, want POD_NAME", got)
	}
	for _, k := range []string{KeyPolicyNamespace, KeyPolicyConfigMap, KeyPolicyResourceVersion, KeyPolicyAccepted,
		KeyPolicyError, KeyPolicyIgnored, KeyLimiter, KeyLimiterWarnings, KeyUtilizationShare, KeyUtilizationShareError, KeyControllerInstance} {
		if _, ok := cm.Data[k]; !ok {
			t.Errorf("key %q missing", k)
		}
	}

	// Unchanged content: no write at all, however often it is flushed or set.
	p.SetUtilizationShare(constants.UtilizationShareModeOff, nil)
	for range 3 {
		if err := p.flush(ctx); err != nil {
			t.Fatalf("repeat flush: %v", err)
		}
	}
	if w.creates != 1 || w.updates != 0 {
		t.Errorf("writes = %d creates, %d updates; want 1, 0", w.creates, w.updates)
	}
}

func TestFlushUpdatesOnChange(t *testing.T) {
	w := &writes{}
	p, c := newTestPublisher(t, w)
	ctx := context.Background()
	if err := p.flush(ctx); err != nil {
		t.Fatal(err)
	}
	p.SetUtilizationShare(constants.UtilizationShareModeInvalid, errors.New("optimizer.type: unknown"))
	if err := p.flush(ctx); err != nil {
		t.Fatal(err)
	}
	cm := stored(t, c)
	if cm.Data[KeyUtilizationShare] != constants.UtilizationShareModeInvalid ||
		cm.Data[KeyUtilizationShareError] != "optimizer.type: unknown" {
		t.Errorf("data = %v", cm.Data)
	}
	if w.creates != 1 || w.updates != 1 {
		t.Errorf("writes = %d creates, %d updates; want 1, 1", w.creates, w.updates)
	}
}

func TestFlushUpdatesExistingConfigMap(t *testing.T) {
	existing := &corev1.ConfigMap{}
	existing.Name, existing.Namespace = ConfigMapName, testNamespace
	existing.Data = map[string]string{KeyFormatVersion: "0", "stale": "x"}
	w := &writes{}
	p, c := newTestPublisher(t, w, existing)
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	cm := stored(t, c)
	if _, ok := cm.Data["stale"]; ok || cm.Data[KeyFormatVersion] != FormatVersion {
		t.Errorf("data = %v, want the desired content only", cm.Data)
	}
	if cm.Labels[labelComponent] != componentValue {
		t.Errorf("labels = %v", cm.Labels)
	}
	if w.creates != 0 || w.updates != 1 {
		t.Errorf("writes = %d creates, %d updates; want 0, 1", w.creates, w.updates)
	}
}

func TestFlushRetriesConflict(t *testing.T) {
	w := &writes{}
	p, c := newTestPublisher(t, w)
	ctx := context.Background()
	if err := p.flush(ctx); err != nil {
		t.Fatal(err)
	}
	w.failUpdate = []error{apierrors.NewConflict(configMapsResource, ConfigMapName, errors.New("modified"))}
	p.SetUtilizationShare(constants.UtilizationShareModeShadow, nil)
	if err := p.flush(ctx); err != nil {
		t.Fatalf("flush after one conflict: %v", err)
	}
	if got := stored(t, c).Data[KeyUtilizationShare]; got != constants.UtilizationShareModeShadow {
		t.Errorf("utilizationShare = %q", got)
	}
	if w.updates != 2 {
		t.Errorf("updates = %d, want 2 (conflict, then success)", w.updates)
	}

	// A create that lost the race to another writer reads again and updates.
	w2 := &writes{failCreate: []error{apierrors.NewAlreadyExists(configMapsResource, ConfigMapName)}}
	p2, c2 := newTestPublisher(t, w2)
	if err := p2.flush(ctx); err != nil {
		t.Fatalf("flush after AlreadyExists: %v", err)
	}
	if w2.creates != 2 {
		t.Errorf("creates = %d, want 2", w2.creates)
	}
	stored(t, c2)
}

func TestFlushKeepsStateWhenForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(configMapsResource, ConfigMapName, errors.New("no Role"))
	w := &writes{failCreate: []error{forbidden}}
	p, c := newTestPublisher(t, w)
	ctx := context.Background()
	p.SetUtilizationShare(constants.UtilizationShareModeActive, nil)

	err := p.flush(ctx)
	if !apierrors.IsForbidden(err) {
		t.Fatalf("flush error = %v, want the Forbidden returned, not swallowed", err)
	}
	if p.written != nil {
		t.Errorf("written = %v after a failed write; want nothing recorded", p.written)
	}
	if got := p.Data()[KeyUtilizationShare]; got != constants.UtilizationShareModeActive {
		t.Errorf("desired utilizationShare = %q, want it kept", got)
	}

	// Once permitted, the next attempt writes what was kept.
	if err := p.flush(ctx); err != nil {
		t.Fatalf("flush after the Role appeared: %v", err)
	}
	if got := stored(t, c).Data[KeyUtilizationShare]; got != constants.UtilizationShareModeActive {
		t.Errorf("utilizationShare = %q", got)
	}
}

func TestRecordPolicy(t *testing.T) {
	w := &writes{}
	p, _ := newTestPublisher(t, w)
	p.RecordPolicy(nil, "policy-ns", "scaling-manager-scaling-policy-config", "42", errors.New("bad threshold"), nil)
	d := p.Data()
	if d[KeyPolicyNamespace] != "policy-ns" || d[KeyPolicyConfigMap] != "scaling-manager-scaling-policy-config" ||
		d[KeyPolicyResourceVersion] != "42" || d[KeyPolicyAccepted] != "false" || d[KeyPolicyError] != "bad threshold" {
		t.Errorf("data = %v", d)
	}
	p.RecordPolicy(nil, "policy-ns", "scaling-manager-scaling-policy-config", "43", nil, nil)
	d = p.Data()
	if d[KeyPolicyResourceVersion] != "43" || d[KeyPolicyAccepted] != "true" || d[KeyPolicyError] != "" ||
		d[KeyPolicyIgnored] != "" {
		t.Errorf("data = %v", d)
	}

	// An accepted entry with fields ignored as unusable: still accepted, and
	// every ignored field is named with its value and the reason.
	p.RecordPolicy(nil, "policy-ns", "scaling-manager-scaling-policy-config", "44", nil, []config.IgnoredField{
		{Field: "scaleToZero.retentionPeriod", Value: "5 minutes", Reason: errors.New("bad duration")},
		{Field: "shapeChangeHoldSeconds", Value: "-1", Reason: errors.New("must be >= 0")},
	})
	d = p.Data()
	want := `policy-ns/scaling-manager-scaling-policy-config: scaleToZero.retentionPeriod="5 minutes": bad duration; shapeChangeHoldSeconds="-1": must be >= 0`
	if d[KeyPolicyAccepted] != "true" || d[KeyPolicyIgnored] != want {
		t.Errorf("policyIgnored = %q, want %q (data = %v)", d[KeyPolicyIgnored], want, d)
	}

	// The next load without them clears the key.
	p.RecordPolicy(nil, "policy-ns", "scaling-manager-scaling-policy-config", "45", nil, nil)
	if got := p.Data()[KeyPolicyIgnored]; got != "" {
		t.Errorf("policyIgnored after a clean load = %q, want empty", got)
	}
}

// A namespace-scoped controller is charged only what its own variants hold, so
// a scope: cluster quota caps that controller, not the cluster. The status says
// so while it applies, and drops the key when it no longer does.
func TestClusterQuotaOnANamespaceScopedControllerIsPublished(t *testing.T) {
	const key = "clusterQuotaPerController" // spelled out: the data contract
	clusterQuota := &config.ScalingPolicy{Limiters: []config.QuotaLimiterConfig{{
		Name: "cluster", Type: "quota", Scope: config.QuotaScopeCluster, ClusterQuotas: map[string]int{"H100": 8},
	}}}
	namespaceQuota := &config.ScalingPolicy{Limiters: []config.QuotaLimiterConfig{{
		Name: "tenants", Type: "quota", Scope: config.QuotaScopeNamespace,
		NamespaceQuotas: map[string]map[string]int{"team-a": {"H100": 8}},
	}}}

	cfg := config.NewTestConfig()
	config.SetWatchNamespaceForTest(cfg, "team-a")
	p, _ := newTestPublisher(t, &writes{})
	cfg.UpdateClusterPolicy(clusterQuota)
	p.RecordClusterPolicy(cfg, "scaling-manager-policy", "scaling-manager-scaling-policy-config", "1", nil)
	if got := p.Data()[key]; !strings.Contains(got, "caps this controller, not the cluster") ||
		!strings.Contains(got, "If it is the only controller reading this policy, that is the same cap") {
		t.Errorf("%s = %q on a namespace-scoped controller under a cluster quota, want the warning", key, got)
	}

	cfg.UpdateClusterPolicy(namespaceQuota)
	p.RecordClusterPolicy(cfg, "scaling-manager-policy", "scaling-manager-scaling-policy-config", "2", nil)
	if got, found := p.Data()[key]; found {
		t.Errorf("%s = %q under a namespace quota, want it absent", key, got)
	}

	// A cluster-scoped controller's cluster quota caps the cluster.
	cluster := config.NewTestConfig()
	cluster.UpdateClusterPolicy(clusterQuota)
	q, _ := newTestPublisher(t, &writes{})
	q.RecordClusterPolicy(cluster, "scaling-manager-system", "scaling-manager-scaling-policy-config", "1", nil)
	if got, found := q.Data()[key]; found {
		t.Errorf("%s = %q on a cluster-scoped controller, want it absent", key, got)
	}
}

// A namespace-scoped controller under the inventory alone ignores a
// physicalGroups from a shared policy: the optimizer keeps running (the mode is
// not invalid) and the note says why. A quota on the namespace drops the note.
func TestIgnoredPhysicalGroupsIsANoteNotAnInvalidMode(t *testing.T) {
	const key = "utilizationShareNote" // spelled out: the data contract
	policy := func(limiters string) *config.ScalingPolicy {
		var p config.ScalingPolicy
		doc := limiters + "optimizer:\n  type: utilizationShare\n  utilizationShare:\n    physicalGroups: true\n"
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatal(err)
		}
		return &p
	}
	cfg := config.NewTestConfig()
	config.SetWatchNamespaceForTest(cfg, "team-a")
	p, _ := newTestPublisher(t, &writes{})
	cfg.UpdateClusterPolicy(policy("limiters:\n  - type: gpu-inventory\n"))
	p.RecordClusterPolicy(cfg, "scaling-manager-policy", "scaling-manager-scaling-policy-config", "1", nil)
	d := p.Data()
	if d[KeyUtilizationShare] != constants.UtilizationShareModeActive || d[KeyUtilizationShareError] != "" {
		t.Errorf("utilizationShare = %q, error = %q; want active and no error", d[KeyUtilizationShare], d[KeyUtilizationShareError])
	}
	if !strings.Contains(d[key], "physicalGroups is ignored") {
		t.Errorf("%s = %q under gpu-inventory alone, want the note", key, d[key])
	}

	cfg.UpdateClusterPolicy(policy("limiters:\n  - type: quota\n    name: tenants\n    scope: namespace\n" +
		"    namespaceQuotas:\n      team-a:\n        H100: 8\n  - type: gpu-inventory\n"))
	p.RecordClusterPolicy(cfg, "scaling-manager-policy", "scaling-manager-scaling-policy-config", "2", nil)
	if got, found := p.Data()[key]; found {
		t.Errorf("%s = %q under a namespace quota, want it absent", key, got)
	}
}

func TestPodListDeniedIsPublishedWhileItHolds(t *testing.T) {
	p, _ := newTestPublisher(t, &writes{})
	p.SetPodListDenied(true)
	if got := p.Data()["podListDenied"]; !strings.Contains(got, "cannot list Pods cluster-wide") ||
		!strings.Contains(got, "if none do, nothing is miscounted") {
		t.Errorf("podListDenied = %q while denied, want the warning", got)
	}
	p.SetPodListDenied(false)
	if got, found := p.Data()["podListDenied"]; found {
		t.Errorf("podListDenied = %q once listed, want it absent", got)
	}
}

// With a separate policy namespace, policyIgnored comes from the controller's
// own ConfigMap and the version from the policy namespace's. Recording either
// must not overwrite the other.
func TestSeparatePolicyKeepsIgnoredAndVersionApart(t *testing.T) {
	w := &writes{}
	p, _ := newTestPublisher(t, w)
	ignored := []config.IgnoredField{
		{Field: "shapeChangeHoldSeconds", Value: "-1", Reason: errors.New("must be >= 0")},
	}
	want := `own-ns/scaling-manager-scaling-policy-config: shapeChangeHoldSeconds="-1": must be >= 0`

	p.RecordClusterPolicy(nil, "policy-ns", "scaling-manager-scaling-policy-config", "7", nil)
	p.RecordIgnored(nil, "own-ns", "scaling-manager-scaling-policy-config", ignored)
	d := p.Data()
	if d[KeyPolicyResourceVersion] != "7" || d[KeyPolicyIgnored] != want {
		t.Errorf("after RecordIgnored: data = %v", d)
	}

	p.RecordClusterPolicy(nil, "policy-ns", "scaling-manager-scaling-policy-config", "8", nil)
	d = p.Data()
	if d[KeyPolicyResourceVersion] != "8" || d[KeyPolicyIgnored] != want {
		t.Errorf("a cluster policy load changed policyIgnored: data = %v", d)
	}

	p.RecordIgnored(nil, "own-ns", "scaling-manager-scaling-policy-config", nil)
	d = p.Data()
	if d[KeyPolicyResourceVersion] != "8" || d[KeyPolicyIgnored] != "" {
		t.Errorf("after a clean own ConfigMap: data = %v", d)
	}
}

// policyIgnored names its source and lists at most ten fields: a tenant
// decides how many entries the ConfigMap has.
func TestPolicyIgnoredNamesItsSourceAndCapsTheList(t *testing.T) {
	ignored := make([]config.IgnoredField, 13)
	for i := range ignored {
		ignored[i] = config.IgnoredField{Field: "shapeChangeHoldSeconds", Value: "-1", Reason: errors.New("must be >= 0")}
	}
	got := ignoredText("own-ns", "cm", ignored)
	if !strings.HasPrefix(got, "own-ns/cm: ") {
		t.Errorf("policyIgnored = %q, want it to start with its source", got)
	}
	if n := strings.Count(got, "shapeChangeHoldSeconds"); n != 10 {
		t.Errorf("policyIgnored lists %d fields, want 10", n)
	}
	if !strings.HasSuffix(got, "; +3 more") {
		t.Errorf("policyIgnored = %q, want it to end with the count not listed", got)
	}
	if got := ignoredText("own-ns", "cm", nil); got != "" {
		t.Errorf("policyIgnored with nothing ignored = %q, want empty", got)
	}
}

func TestNilPublisherIsSafe(t *testing.T) {
	var p *Publisher
	p.RecordPolicy(nil, "a", "b", "1", nil, nil)
	p.RecordClusterPolicy(nil, "a", "b", "1", nil)
	p.RecordIgnored(nil, "a", "b", nil)
	p.SetUtilizationShare(constants.UtilizationShareModeOff, nil)
	if p.Data() != nil {
		t.Error("nil publisher returned data")
	}
}

func TestStartWritesAndCoalesces(t *testing.T) {
	w := &writes{}
	p, c := newTestPublisher(t, w)
	p.debounce = 50 * time.Millisecond
	if !p.NeedLeaderElection() {
		t.Fatal("the status must be written by the leader only")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	read := func() map[string]string {
		var cm corev1.ConfigMap
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: ConfigMapName}, &cm); err != nil {
			return nil
		}
		return cm.Data
	}
	waitFor("the first write", func() bool { return read() != nil })

	// Three changes inside one debounce window land as one update.
	p.SetUtilizationShare(constants.UtilizationShareModeShadow, nil)
	p.RecordPolicy(nil, testNamespace, "scaling-manager-scaling-policy-config", "7", nil, nil)
	p.SetUtilizationShare(constants.UtilizationShareModeActive, nil)
	waitFor("the coalesced write", func() bool {
		d := read()
		return d[KeyPolicyResourceVersion] == "7" && d[KeyUtilizationShare] == constants.UtilizationShareModeActive
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned %v", err)
	}
	if w.creates != 1 || w.updates != 1 {
		t.Errorf("writes = %d creates, %d updates; want 1, 1", w.creates, w.updates)
	}
}

// TestFlushCreateLosesRaceToAnotherWriter: between this process's read (no
// ConfigMap) and its create, another writer -- a previous leader still
// finishing its term -- creates the ConfigMap with its own content. The create
// fails AlreadyExists; the flush must re-read and overwrite with its content,
// not give up or report the race as a failure.
func TestFlushCreateLosesRaceToAnotherWriter(t *testing.T) {
	w := &writes{}
	p, c := newTestPublisher(t, w)
	raced := false
	p.writer = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			w.creates++
			if !raced {
				raced = true
				other := &corev1.ConfigMap{}
				other.Name, other.Namespace = ConfigMapName, testNamespace
				other.Data = map[string]string{KeyLeader: "the-other-leader"}
				if err := cl.Create(ctx, other); err != nil {
					t.Fatalf("seeding the race winner: %v", err)
				}
				return apierrors.NewAlreadyExists(configMapsResource, ConfigMapName)
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			w.updates++
			return cl.Update(ctx, obj, opts...)
		},
	})
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("flush after losing the create race: %v", err)
	}
	cm := stored(t, c)
	if got := cm.Data[KeyLeader]; got != "scaling-manager-controller-manager-abc" {
		t.Errorf("leader = %q, want this process's content written over the race winner's", got)
	}
	if cm.Labels[labelComponent] != componentValue {
		t.Errorf("labels = %v", cm.Labels)
	}
	if w.creates != 1 || w.updates != 1 {
		t.Errorf("writes = %d creates, %d updates; want 1 (lost), 1", w.creates, w.updates)
	}
	if p.written == nil {
		t.Error("written not recorded after the race was resolved")
	}
}

// TestReportErrThrottlesUnchangedError: an unchanged error is logged once per
// errorRelogEvery, a changed one at once, and recovery re-arms it.
func TestReportErrThrottlesUnchangedError(t *testing.T) {
	p, _ := newTestPublisher(t, &writes{})
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	var logged []string
	logger := funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{})
	forbidden := apierrors.NewForbidden(configMapsResource, ConfigMapName, errors.New("no Role"))

	p.reportErr(logger, forbidden)
	if len(logged) != 1 {
		t.Fatalf("first error: %d lines logged, want 1", len(logged))
	}
	if !strings.Contains(logged[0], "scaling-manager-controller-status-role") {
		t.Errorf("a Forbidden must carry the RBAC hint: %s", logged[0])
	}
	now = now.Add(errorRelogEvery - time.Second)
	p.reportErr(logger, forbidden)
	if len(logged) != 1 {
		t.Errorf("unchanged error inside %v: %d lines, want 1", errorRelogEvery, len(logged))
	}
	now = now.Add(time.Second)
	p.reportErr(logger, forbidden)
	if len(logged) != 2 {
		t.Errorf("unchanged error after %v: %d lines, want 2", errorRelogEvery, len(logged))
	}
	p.reportErr(logger, errors.New("connection refused"))
	if len(logged) != 3 {
		t.Errorf("changed error: %d lines, want 3 (logged at once)", len(logged))
	}

	p.reportRecovered(logger)
	if len(logged) != 4 {
		t.Errorf("recovery: %d lines, want 4", len(logged))
	}
	p.reportErr(logger, errors.New("connection refused"))
	if len(logged) != 5 {
		t.Errorf("the same error after a recovery: %d lines, want 5 (re-armed)", len(logged))
	}
}
