package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// The whole-node utilization-share specs (label utilization-share-wholenode)
// need GPU nodes the size of one large-model replica: a product whose
// schedulable nodes each advertise wholeNodeGPUs, several of them. The default
// kind cluster (one product per 16-GPU node) cannot express "one replica is one
// node", so these specs run on their own cluster shape, which
// make test-e2e-ushare-wholenode-with-setup creates
// (docs/developer-guide/testing.md).
const (
	// wholeNodeGPUs is one node's GPUs, and one replica of the large model.
	wholeNodeGPUs = 8
	// requireWholeNodeE2EEnv turns these specs' skips -- no such nodes, no
	// Prometheus -- into failures. The make target sets it: a run that
	// checked nothing must not go green.
	requireWholeNodeE2EEnv = "REQUIRE_WHOLENODE_E2E"
	// wholeNodeWindow is the ScaledObjects' scale-down window: short, so a
	// release takes about a minute.
	wholeNodeWindow = int32(30)
)

// wholeNodeEnv is one whole-node spec's cluster: the GPU product, the nodes
// it lays its models on, its namespace, and a namespace for the pause pods
// that close every other GPU.
type wholeNodeEnv struct {
	key, product, gpuRN string
	// nodes are the schedulable nodes of the product with wholeNodeGPUs
	// GPUs, all free when the spec started, by name.
	nodes        []string
	ns, fillerNS string
	// shared is true when ns is not the spec's own -- the controller's
	// namespace, for a namespace-scoped controller -- so what the spec creates
	// there is removed one by one in cleanup, not with the namespace.
	shared bool
}

// wholeNodeUnavailable fails the spec when REQUIRE_WHOLENODE_E2E=true and
// skips it otherwise: on a cluster of another shape the spec has nothing to
// show.
func wholeNodeUnavailable(msg string) {
	if os.Getenv(requireWholeNodeE2EEnv) == boolTrue {
		Fail(msg + " (" + requireWholeNodeE2EEnv + "=true)")
	}
	Skip(msg)
}

// wholeNodeProm is the suite's Prometheus client; unreachable, it fails the
// spec under REQUIRE_WHOLENODE_E2E and skips it otherwise.
func wholeNodeProm() *utils.PrometheusClient {
	GinkgoHelper()
	pc := promClientForCheck()
	if pc == nil {
		wholeNodeUnavailable("no Prometheus client could be built")
	}
	if _, err := pc.QueryWithRetry(ctx, "vector(1)"); err != nil {
		wholeNodeUnavailable("Prometheus is not reachable from the test host: " + err.Error())
	}
	return pc
}

// setUpWholeNode finds minNodes free whole nodes of one GPU product, and
// creates the spec's namespace (prefix) and its filler namespace, both
// removed in cleanup. The controller may patch pods in the spec's namespace.
func setUpWholeNode(prefix string, minNodes int) *wholeNodeEnv {
	GinkgoHelper()
	e := findWholeNodes(minNodes)
	e.ns = newSpecNamespace(prefix)
	e.fillerNS = newSpecNamespace(prefix + "filler-")
	// The optimizer marks donor pods (a deletion cost), which is a pod patch.
	Expect(fixtures.GrantPodWrites(ctx, k8sClient, e.ns, cfg.LLMSMNamespace)).To(Succeed())
	return e
}

// newSpecNamespace creates a namespace named from prefix, deleted in cleanup.
func newSpecNamespace(prefix string) string {
	GinkgoHelper()
	nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix},
	}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	name := nsObj.Name
	DeferCleanup(func() {
		_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	return name
}

// findWholeNodes finds minNodes free whole nodes of one GPU product; the
// caller sets the namespaces.
func findWholeNodes(minNodes int) *wholeNodeEnv {
	GinkgoHelper()
	e := &wholeNodeEnv{}
	var ok bool
	e.key, e.product, e.gpuRN, ok = fixtures.DiscoverAccelerator(ctx, k8sClient)
	if !ok {
		wholeNodeUnavailable("no GPU product label on any schedulable node")
	}

	// scan lists the product's schedulable nodes of wholeNodeGPUs GPUs, and
	// those of them with every GPU free.
	scan := func(g Gomega) (sized, free, busy []string) {
		nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: e.key + "=" + e.product})
		g.Expect(err).NotTo(HaveOccurred())
		for i := range nodes.Items {
			n := &nodes.Items[i]
			if !nodeSchedulable(n) {
				continue
			}
			q, has := n.Status.Allocatable[corev1.ResourceName(e.gpuRN)]
			if !has || q.Value() != wholeNodeGPUs {
				continue
			}
			sized = append(sized, n.Name)
			f, err := fixtures.FreeGPUsOnNode(ctx, k8sClient, n, e.gpuRN)
			g.Expect(err).NotTo(HaveOccurred())
			if f == wholeNodeGPUs {
				free = append(free, n.Name)
			} else {
				busy = append(busy, fmt.Sprintf("%s (%d free)", n.Name, f))
			}
		}
		sort.Strings(free)
		return sized, free, busy
	}
	if sized, _, _ := scan(Default); len(sized) < minNodes {
		wholeNodeUnavailable(fmt.Sprintf("needs %d schedulable %s nodes of %d GPUs each, found %d; create the cluster with "+
			"CLUSTER_GPU_TYPE=nvidia CLUSTER_GPUS=%d CLUSTER_NODES=%d (make test-e2e-ushare-wholenode-with-setup)",
			minNodes, e.product, wholeNodeGPUs, len(sized), wholeNodeGPUs, minNodes+1))
	}
	// An earlier spec's namespace is deleted in its cleanup, which does not
	// wait: its pods may still hold GPUs for a while. Every node must be
	// free, not only the ones this spec lays models on: a GPU freed later on
	// another node would be one the layout did not close.
	By("Waiting for earlier specs' pods to leave the product's nodes")
	Eventually(func(g Gomega) {
		_, free, busy := scan(g)
		g.Expect(busy).To(BeEmpty(), "GPU pods left behind on the product's nodes")
		g.Expect(len(free)).To(BeNumerically(">=", minNodes))
		e.nodes = free[:minNodes]
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
	By(fmt.Sprintf("Using whole nodes %v of %s (%s)", e.nodes, e.product, e.gpuRN))
	return e
}

// nodeSchedulable reports whether pods may land on n: not cordoned, and no
// NoSchedule or NoExecute taint.
func nodeSchedulable(n *corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			return false
		}
	}
	return true
}

// closeAllBut takes the free GPUs of every node of the product with pause
// pods, leaving leave[node] free on the nodes named there -- the tainted
// control-plane included, since the controller's node picture counts it. The
// layout a spec builds then decides where every model pod can go. It returns
// the pause pods by node.
func (e *wholeNodeEnv) closeAllBut(leave map[string]int) map[string][]string {
	GinkgoHelper()
	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: e.key + "=" + e.product})
	Expect(err).NotTo(HaveOccurred())
	fillers := map[string][]string{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		free, err := fixtures.FreeGPUsOnNode(ctx, k8sClient, n, e.gpuRN)
		Expect(err).NotTo(HaveOccurred())
		if take := free - leave[n.Name]; take > 0 {
			name, err := fixtures.FillNodeGPUs(ctx, k8sClient, e.fillerNS, n.Name, e.gpuRN, take)
			Expect(err).NotTo(HaveOccurred())
			fillers[n.Name] = append(fillers[n.Name], name)
		}
	}
	e.waitFillersRunning()
	return fillers
}

// waitFillersRunning waits for every pause pod in the filler namespace to run.
func (e *wholeNodeEnv) waitFillersRunning() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(e.fillerNS).List(ctx, metav1.ListOptions{LabelSelector: "app=gpu-filler"})
		g.Expect(err).NotTo(HaveOccurred())
		for _, p := range pods.Items {
			g.Expect(p.Status.Phase).To(Equal(corev1.PodRunning), p.Name)
		}
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
}

// model creates a model's Deployment of replicas pods of gpus GPUs each, its
// Service and its ServiceMonitor; the ServiceMonitor goes in cleanup, the rest
// with the namespace.
func (e *wholeNodeEnv) model(ms, modelID string, replicas int32, gpus int64, opts ...fixtures.ModelServiceOption) {
	GinkgoHelper()
	opts = append([]fixtures.ModelServiceOption{
		func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(replicas) },
		fixtures.WithGPUs(e.gpuRN, gpus),
	}, opts...)
	Expect(fixtures.EnsureModelService(ctx, k8sClient, e.ns, ms, ms+"-pool", modelID, cfg.UseSimulator, 2, opts...)).To(Succeed())
	if e.shared {
		ns := e.ns
		DeferCleanup(func() {
			_ = fixtures.DeleteModelService(context.Background(), k8sClient, ns, ms)
			_ = k8sClient.CoreV1().Services(ns).Delete(context.Background(), ms+"-service", metav1.DeleteOptions{})
		})
	}
	e.serve(ms, ms+"-decode")
}

// serve creates the Service and ServiceMonitor of model server ms, whose pods
// carry app=app.
func (e *wholeNodeEnv) serve(ms, app string, opts ...fixtures.ServiceOption) {
	GinkgoHelper()
	Expect(fixtures.EnsureService(ctx, k8sClient, e.ns, ms, app, 8000, opts...)).To(Succeed())
	Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, e.ns, ms, app)).To(Succeed())
	monitor := ms + "-monitor"
	DeferCleanup(func() {
		_ = crClient.Delete(context.Background(), &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: monitor, Namespace: cfg.MonitoringNS},
		})
	})
}

// scale sets a Deployment's replicas.
func (e *wholeNodeEnv) scale(dep string, replicas int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		d, err := k8sClient.AppsV1().Deployments(e.ns).Get(ctx, dep, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		d.Spec.Replicas = ptr.To(replicas)
		_, err = k8sClient.AppsV1().Deployments(e.ns).Update(ctx, d, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, time.Minute, 2*time.Second).Should(Succeed())
}

// waitPlaced waits until n live pods of app are on a node, so the next model
// created cannot take their place.
func (e *wholeNodeEnv) waitPlaced(app string, n int) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		placed := 0
		for node, c := range e.podsByNode(app) {
			if node != "" {
				placed += c
			}
		}
		g.Expect(placed).To(Equal(n), unreadyPods(e.ns))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

// waitReady waits until each Deployment has the given Ready replicas, saying
// why each pod is not Ready when it fails.
func (e *wholeNodeEnv) waitReady(want map[string]int32, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		for dep, n := range want {
			d, err := k8sClient.AppsV1().Deployments(e.ns).Get(ctx, dep, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(d.Status.ReadyReplicas).To(Equal(n), dep+"\n"+unreadyPods(e.ns))
		}
	}, timeout, 5*time.Second).Should(Succeed())
}

// register creates a model's ScaledObject on the scaling manager's metric and waits until KEDA
// wired it; it goes in cleanup. kind is the scale target's kind, "" for a
// Deployment.
func (e *wholeNodeEnv) register(so, target, modelID string, minR, maxR int32, kind string,
	extra ...fixtures.ScaledObjectOption) {
	GinkgoHelper()
	opts := append([]fixtures.ScaledObjectOption{
		fixtures.WithScalingManagerTriggerMetadata(modelID, "10.0"),
		fixtures.WithScaledObjectScaleDownStabilizationWindow(wholeNodeWindow),
	}, extra...)
	if kind != "" {
		opts = append(opts, fixtures.WithScaledObjectScaleTargetKind(kind))
	}
	Expect(fixtures.EnsureScaledObject(ctx, crClient, e.ns, so, target, target, minR, maxR, cfg.MonitoringNS, opts...)).To(Succeed())
	ns := e.ns
	DeferCleanup(func() { _ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, so) })
	waitForKEDAExternalMetricWired(e.ns, target, so)
}

// lowerFloor sets a registered model's minReplicas to 1, keeping maxReplicas
// at maxR, so it has replicas to give.
func (e *wholeNodeEnv) lowerFloor(so, target, modelID string, maxR int32, kind string) {
	GinkgoHelper()
	opts := []fixtures.ScaledObjectOption{
		fixtures.WithScalingManagerTriggerMetadata(modelID, "10.0"),
		fixtures.WithScaledObjectScaleDownStabilizationWindow(wholeNodeWindow),
	}
	if kind != "" {
		opts = append(opts, fixtures.WithScaledObjectScaleTargetKind(kind))
	}
	Expect(fixtures.EnsureScaledObject(ctx, crClient, e.ns, so, target, target, 1, maxR, cfg.MonitoringNS, opts...)).To(Succeed())
}

// shortOf reports whether the controller's last two analyzer results, read
// from the time given, ask aggregated model modelID for a whole replica more
// than it holds (roleShortFor), and says what it read.
func (e *wholeNodeEnv) shortOf(modelID string, since time.Time) (bool, string) {
	return roleShortFor(e.ns, modelID, domain.RoleBoth, since, 2)
}

// load starts a load Job of workers against model server ms for an hour; the
// namespace takes it in cleanup.
func (e *wholeNodeEnv) load(job, ms, modelID string, workers int) {
	GinkgoHelper()
	j := buildThroughputSustainedLoadJob(e.ns, job, fmt.Sprintf("http://%s-service:8000/v1/completions", ms), modelID, workers, 3600)
	_, err := k8sClient.BatchV1().Jobs(e.ns).Create(ctx, j, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	if e.shared {
		ns := e.ns
		DeferCleanup(func() {
			_ = k8sClient.BatchV1().Jobs(ns).Delete(context.Background(), job, metav1.DeleteOptions{
				PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
			})
		})
	}
}

// stopLoad deletes a load Job and waits for its pods to go.
func (e *wholeNodeEnv) stopLoad(job string) {
	GinkgoHelper()
	Expect(k8sClient.BatchV1().Jobs(e.ns).Delete(ctx, job, metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})).To(Succeed())
	Eventually(func(g Gomega) {
		pods, err := k8sClient.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + job})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).To(BeEmpty())
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}

// podsByNode counts the live pods of app in the spec's namespace per node,
// "" for a pod not placed yet.
func (e *wholeNodeEnv) podsByNode(app string) map[string]int {
	GinkgoHelper()
	pods, err := k8sClient.CoreV1().Pods(e.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
	Expect(err).NotTo(HaveOccurred())
	out := map[string]int{}
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil {
			out[p.Spec.NodeName]++
		}
	}
	return out
}

// specReplicas is a Deployment's spec.replicas.
func (e *wholeNodeEnv) specReplicas(g Gomega, dep string) int32 {
	d, err := k8sClient.AppsV1().Deployments(e.ns).Get(ctx, dep, metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	return ptr.Deref(d.Spec.Replicas, 0)
}

// readyReplicas is a Deployment's status.readyReplicas.
func (e *wholeNodeEnv) readyReplicas(g Gomega, dep string) int32 {
	d, err := k8sClient.AppsV1().Deployments(e.ns).Get(ctx, dep, metav1.GetOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	return d.Status.ReadyReplicas
}

// holdUnfunded holds for d after the small models' floors dropped at
// floorDropped, and checks no donor transfer toward large model G starts. It
// records, as they change, the reasons G reports, G's replicas and the small
// models' (label names the spec in the MEASURE lines).
//
// The small models are idle. With mayFill false -- node information, so the
// idle fill places only where a pod fits -- no node can hold G's replica, so
// the small models keep their spare (no return for an unplaceable receiver),
// and G must stay at one replica and report no-node-fits: from its first
// reading on, in every reading. With mayFill true -- no node information, so
// no node fit can be judged -- their spare goes back to the quota by
// returns, and the idle fill may raise G into the returned GPUs blind; then
// G's growth must have come from an idle fill, not a transfer, and otherwise
// one reading must name the missing donor (no-compatible-donor or
// donors-at-floor).
func (e *wholeNodeEnv) holdUnfunded(label, modelG, depG string, small []string, start, floorDropped time.Time, d time.Duration, mayFill bool) {
	GinkgoHelper()
	pc := wholeNodeProm()
	var timeline []string
	seen := map[string]bool{}
	noFit := false
	Consistently(func(g Gomega) {
		lines := shareLines(controllerLogsSince(start), e.ns)
		g.Expect(setsTo(lines, modelG)).To(BeEmpty(), "a transfer to G started:\n%s", describeShare(lines, 20))
		gSpec := e.specReplicas(g, depG)
		if !mayFill {
			g.Expect(gSpec).To(Equal(int32(1)))
		}
		reasons, err := blockedReasons(pc, e.ns, modelG)
		g.Expect(err).NotTo(HaveOccurred())
		if !mayFill {
			// Once said, said every reading: the cause does not change
			// while the small models keep their spare.
			has := slices.Contains(reasons, constants.ScalingBlockedNoNodeFits)
			g.Expect(has || !noFit).To(BeTrue(), "G stopped reporting no-node-fits: %v after %v", reasons, timeline)
			noFit = noFit || has
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%v G=%d/%d", reasons, gSpec, e.readyReplicas(g, depG))
		for _, dep := range small {
			fmt.Fprintf(&b, " %s=%d", dep, e.specReplicas(g, dep))
		}
		if at := b.String(); !seen[at] {
			seen[at] = true
			timeline = append(timeline, fmt.Sprintf("+%s %s", time.Since(floorDropped).Round(time.Second), at))
		}
	}, d, 10*time.Second).Should(Succeed())
	wnMeasure(label+": G reasons and replicas (spec/ready) over time", strings.Join(timeline, "; "))
	lines := shareLines(controllerLogsSince(start), e.ns)
	wnMeasure(label+": spare returns", len(withMsg(lines, "spare returned")))
	var fills []string
	for _, l := range withMsg(lines, "filling from idle GPUs") {
		if roleOf(l.Receiver, modelG) {
			fills = append(fills, l.At.Sub(floorDropped).Round(time.Second).String())
		}
	}
	wnMeasure(label+": idle fills toward G (after floors dropped)", fills)
	wnMeasure(label+": G pods by node at the end", e.podsByNode(depG))
	if mayFill && e.specReplicas(Default, depG) > 1 {
		Expect(fills).NotTo(BeEmpty(), "G grew with neither a transfer nor an idle fill:\n%s", describeShare(lines, 40))
		return
	}
	if !mayFill {
		Expect(noFit).To(BeTrue(), "G was never reported no-node-fits: %v", timeline)
		return
	}
	named := false
	for k := range seen {
		named = named || strings.Contains(k, "no-compatible-donor") || strings.Contains(k, "donors-at-floor")
	}
	Expect(named).To(BeTrue(), "G was never reported blocked for want of a donor: %v", timeline)
}

// wnMeasure prints a measured figure and adds it to the report, so a run's
// numbers survive in its -v output and its JSON report.
func wnMeasure(name string, v any) {
	if d, ok := v.(time.Duration); ok {
		v = d.Round(time.Second)
	}
	GinkgoWriter.Printf("MEASURE %s: %v\n", name, v)
	AddReportEntry("MEASURE "+name, fmt.Sprint(v))
}

// shareLine is one utilization-share line of the controller's log for a
// group, the fields the whole-node specs read.
type shareLine struct {
	At time.Time
	// Msg is the message less its "Utilization share: " prefix.
	Msg          string
	ID           string   `json:"id"`
	Set          string   `json:"set"`
	Donor        string   `json:"donor"`
	Receiver     string   `json:"receiver"`
	Variant      string   `json:"variant"`
	Outcome      string   `json:"outcome"`
	Reason       string   `json:"reason"`
	Planned      bool     `json:"planned"`
	Returned     bool     `json:"returned"`
	SkippedWaits bool     `json:"skippedWaits"`
	Urgent       bool     `json:"urgent"`
	Pods         []string `json:"pods"`
	GPUs         int      `json:"gpus"`
}

const sharePrefix = "Utilization share: "

// shareLines parses the controller's utilization-share lines for the group of
// namespace ns out of logs, in order.
func shareLines(logs, ns string) []shareLine {
	var out []shareLine
	for _, line := range strings.Split(logs, "\n") {
		i := strings.Index(line, sharePrefix)
		if i < 0 || !strings.Contains(line, `"scope": "`+ns+`"`) {
			continue
		}
		j := strings.Index(line[i:], "{")
		if j < 0 {
			continue
		}
		var l shareLine
		if json.Unmarshal([]byte(line[i+j:]), &l) != nil {
			continue
		}
		l.Msg = strings.TrimSpace(line[i+len(sharePrefix) : i+j])
		l.At, _ = logLineTime(line)
		out = append(out, l)
	}
	return out
}

// withMsg keeps the lines whose message starts with msg.
func withMsg(lines []shareLine, msg string) []shareLine {
	var out []shareLine
	for _, l := range lines {
		if strings.HasPrefix(l.Msg, msg) {
			out = append(out, l)
		}
	}
	return out
}

// roleOf reports whether a role key (namespace/model/role) is model's.
func roleOf(key, modelID string) bool {
	return strings.Contains(key, "/"+modelID+"/")
}

// transferSet is one donor set, or a single transfer, toward a receiver, as
// the controller logged it.
type transferSet struct {
	ID       string
	Receiver string
	Members  []shareLine
	Started  time.Time
	// Raised is when the primary's donors had all released and the receiver
	// was raised; zero until then.
	Raised time.Time
	// Outcome is the primary's outcome, "" while it runs; Ended is when.
	Outcome string
	Ended   time.Time
}

// setsTo groups the transfers started toward receiver model modelID by set
// (a single transfer is its own set), with their raise and outcome.
func setsTo(lines []shareLine, modelID string) []transferSet {
	byID := map[string]*transferSet{}
	var order []string
	for _, l := range withMsg(lines, "transfer started") {
		id := l.Set
		if id == "" {
			id = l.ID
		}
		s, ok := byID[id]
		if !ok {
			s = &transferSet{ID: id, Started: l.At}
			byID[id] = s
			order = append(order, id)
		}
		if l.Receiver != "" {
			s.Receiver = l.Receiver
		}
		s.Members = append(s.Members, l)
	}
	for _, l := range lines {
		s, ok := byID[l.ID]
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(l.Msg, "released, raising the receiver"):
			s.Raised = l.At
		case strings.HasPrefix(l.Msg, "transfer ended"):
			s.Outcome, s.Ended = l.Outcome, l.At
		}
	}
	var out []transferSet
	for _, id := range order {
		if roleOf(byID[id].Receiver, modelID) {
			out = append(out, *byID[id])
		}
	}
	return out
}

// describeShare is the group's share lines, for a failure message: the last
// limit of them.
func describeShare(lines []shareLine, limit int) string {
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %s id=%s set=%s donor=%s receiver=%s outcome=%s reason=%s planned=%v returned=%v pods=%v\n",
			l.At.Format(time.TimeOnly), l.Msg, l.ID, l.Set, l.Donor, l.Receiver, l.Outcome, l.Reason, l.Planned, l.Returned, l.Pods)
	}
	if b.Len() == 0 {
		return "no matching utilization-share lines for the group"
	}
	return b.String()
}

// blockedReasons is the scaling-blocked reasons Prometheus holds now for
// model modelID in namespace ns.
func blockedReasons(pc *utils.PrometheusClient, ns, modelID string) ([]string, error) {
	v, _, err := pc.API().Query(ctx, fmt.Sprintf(`%s{exported_namespace=%q,model_name=%q}`,
		constants.LLMSMModelScalingBlocked, ns, modelID), time.Now())
	if err != nil {
		return nil, err
	}
	vec, ok := v.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("query returned %s, not a vector", v.Type())
	}
	var out []string
	for _, s := range vec {
		if s.Value > 0 {
			out = append(out, string(s.Metric["reason"]))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// shareGauge reads one series of a utilization-share gauge for the group of
// namespace ns, 0 with ok false when there is none.
// startMeasuredLine is the fields of a replica-start-measured line the
// whole-node specs read (docs/reference/cycle-log.md).
type startMeasuredLine struct {
	Variant      string  `json:"variant"`
	Pods         int     `json:"pods"`
	SizedSeconds float64 `json:"sizedSeconds"`
}

// lastStartMeasured is the last replica-start-measured line in logs for a
// variant of model service ms in namespace ns, and whether there is one. The
// controller logs JSON after a level and a message; each line is decoded
// from its first brace.
func lastStartMeasured(logs, ns, ms string) (startMeasuredLine, bool) {
	var last startMeasuredLine
	found := false
	for _, ln := range strings.Split(logs, "\n") {
		if !strings.Contains(ln, "replica-start-measured") {
			continue
		}
		i := strings.Index(ln, "{")
		if i < 0 {
			continue
		}
		var m struct {
			startMeasuredLine
			Namespace string `json:"namespace"`
		}
		if json.Unmarshal([]byte(ln[i:]), &m) != nil || m.Namespace != ns || !strings.HasPrefix(m.Variant, ms+"-") {
			continue
		}
		last, found = m.startMeasuredLine, true
	}
	return last, found
}

func shareGauge(pc *utils.PrometheusClient, query string) (float64, bool) {
	v, _, err := pc.API().Query(ctx, query, time.Now())
	if err != nil {
		return 0, false
	}
	vec, ok := v.(model.Vector)
	if !ok || len(vec) == 0 {
		return 0, false
	}
	return float64(vec[0].Value), true
}
