package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
)

// The utilization-share optimizer on the shape it was built for: a P/D model
// served by LeaderWorkerSets (docs/proposals/utilization-share-optimizer.md,
// sections 5.6 and 6.5). One namespace quota of 7 GPUs, spent:
//
//   - model A, idle: a Deployment of four 1-GPU replicas;
//   - model B, loaded, disaggregated: a decode LWS of one group of two 1-GPU
//     pods, and a prefill LWS of one 1-GPU pod.
//
// B's decode grows by a replica of two pods, and no donor replica -- one pod
// each -- can host it. Two of A's replicas are taken as one donor set, one pod
// each (section 6.5). The decode LWS is raised only once both are released.
// This exercises what the Deployment spec does not: P/D roles, LWS pod shapes
// read from the templates, an LWS receiver's held count by group, and a
// donor set on a real ReplicaSet.
//
// It needs a node with 7 GPUs of one product, which the kind emulator gives
// (CLUSTER_GPUS, 16 per node by default); on a cluster without one it skips,
// saying so.
var _ = Describe("Utilization share optimizer on a P/D LeaderWorkerSet model", Label("full", "utilization-share"), Ordered, func() {
	const (
		quotaGPUs = 7
		msA       = "ushpd-a"
		msDecode  = "ushpd-d"
		msPrefill = "ushpd-p"
		modelA    = "ushpd-model-a"
		modelB    = "ushpd-model-b"
		loadJob   = "ushpd-b-load"
		window    = int32(30)
	)
	var (
		ns, configName           string
		key, product, gpuResName string
	)
	depA := msA + "-decode"
	lwsDecode, lwsPrefill := msDecode+"-decode", msPrefill+"-decode"
	soA, soD, soP := msA+"-so", msDecode+"-so", msPrefill+"-so"

	BeforeAll(func() {
		var ok bool
		key, product, gpuResName, ok = fixtures.DiscoverAccelerator(ctx, k8sClient)
		if !ok {
			Skip("no GPU product label on any schedulable node")
		}
		if n := fixtures.AllocatableGPUsForProduct(ctx, k8sClient, product); n < quotaGPUs {
			Skip(fmt.Sprintf("needs a node with %d %s GPUs, the largest has %d", quotaGPUs, product, n))
		}

		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ushpd-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ns = nsObj.Name
		// The optimizer marks donor pods (a deletion cost), which is a pod patch;
		// make utilization-share-activate grants it per namespace, and so does this.
		Expect(fixtures.GrantPodWrites(ctx, k8sClient, ns, cfg.LLMSMNamespace)).To(Succeed())
		By(fmt.Sprintf("Using namespace %s with a %d-GPU quota on %s", ns, quotaGPUs, product))
		DeferCleanup(func() {
			_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		})

		configName = scalingPolicyConfigMapName()
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, configName, ns, product, quotaGPUs,
			fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })

		By("Creating idle model A at four 1-GPU replicas, and P/D model B on two LeaderWorkerSets")
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msA, msA+"-pool", modelA, cfg.UseSimulator, 2,
			func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(int32(4)) })).To(Succeed())
		Expect(fixtures.EnsureModelServiceLWS(ctx, crClient, ns, msDecode, msDecode+"-pool", modelB, cfg.UseSimulator, 2, 2,
			fixtures.WithLWSRole(domain.RoleDecode), fixtures.WithLWSGPUs(key, product, gpuResName, 1))).To(Succeed())
		Expect(fixtures.EnsureModelServiceLWS(ctx, crClient, ns, msPrefill, msPrefill+"-pool", modelB, cfg.UseSimulator, 2, 1,
			fixtures.WithLWSRole(domain.RolePrefill), fixtures.WithLWSGPUs(key, product, gpuResName, 1))).To(Succeed())
		for _, ms := range []string{msA, msDecode, msPrefill} {
			// The decode group's worker pod runs the simulator too. Routed to
			// it, requests queue where no metric the scaling manager reads can see them -- a
			// replica is read from its leader -- and B's decode read idle for
			// minutes under full load while its queue sat on the worker, long
			// enough for A's spare to be returned instead of transferred. The
			// Service sends traffic to the group leaders only, as llm-d does.
			var opts []fixtures.ServiceOption
			if ms == msDecode {
				opts = append(opts, fixtures.WithServiceSelector(lwsv1.WorkerIndexLabelKey, "0"))
			}
			Expect(fixtures.EnsureService(ctx, k8sClient, ns, ms, ms+"-decode", 8000, opts...)).To(Succeed())
			Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, ms, ms+"-decode")).To(Succeed())
			monitor := ms + "-monitor"
			DeferCleanup(func() {
				_ = crClient.Delete(context.Background(), &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{Name: monitor, Namespace: cfg.MonitoringNS},
				})
			})
		}
		Eventually(func(g Gomega) {
			a, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depA, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(a.Status.ReadyReplicas).To(Equal(int32(4)), unreadyPods(ns))
			for _, name := range []string{lwsDecode, lwsPrefill} {
				var l lwsv1.LeaderWorkerSet
				g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &l)).To(Succeed())
				g.Expect(l.Status.ReadyReplicas).To(Equal(int32(1)), name+"\n"+unreadyPods(ns))
			}
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering all three with the scaling manager; A's floor is 4 until the optimizer has started")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 4, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		for _, so := range []struct{ name, target string }{{soD, lwsDecode}, {soP, lwsPrefill}} {
			Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, so.name, so.target, so.target, 1, 3, cfg.MonitoringNS,
				fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"),
				fixtures.WithScalingManagerTriggerMetadata(modelB, "10.0"),
				fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		}
		DeferCleanup(func() {
			for _, so := range []string{soA, soD, soP} {
				_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, so)
			}
		})
		// A ScaledObject registered while the controller restarts -- the suite
		// restarts it in BeforeSuite, and other specs do -- gets "got empty
		// metric spec" and an HPA on the CPU default, and KEDA never asks
		// again. Decode's HPA so latched never raises the LWS: the set's
		// receiver ended fill-timeout, with the scaling manager asking for 2 groups and 1 Ready.
		By("Waiting for KEDA to wire the scaling manager's metric onto all three HPAs")
		for _, w := range []struct{ target, so string }{{depA, soA}, {lwsDecode, soD}, {lwsPrefill, soP}} {
			waitForKEDAExternalMetricWired(ns, w.target, w.so)
		}

		// 40 minutes: the spec's waits add up to 30 at worst, and B must stay
		// short for all of them. The namespace goes in cleanup, and the Job
		// with it, so a longer deadline costs a passing run nothing.
		By("Loading B's decode well past one group's capacity")
		job := buildThroughputSustainedLoadJob(ns, loadJob, fmt.Sprintf("http://%s-service:8000/v1/completions", msDecode),
			modelB, 8, int64(2400))
		_, err = k8sClient.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("grows B's decode by one group, funded by two of A's replicas as one donor set", func() {
		start := time.Now()
		Eventually(func(g Gomega) {
			g.Expect(controllerLogsSince(start.Add(-5 * time.Minute))).To(ContainSubstring("scope\": \"" + ns))
		}, 5*time.Minute, 10*time.Second).Should(Succeed(), "the optimizer never evaluated the namespace's group")

		// Before A can give: a spare offered while B's decode is not yet seen
		// short is returned to the quota, not transferred (roleShortOf). The
		// planner plans nothing for a new group's quiet period, so the readings
		// that count are the ones it plans on: B's decode short on
		// shortCycles analyzer results in a row after planning starts. A
		// single short reading before the quiet period ends says nothing about
		// the first cycles A can give in; a return needs only two of them that
		// do not read short.
		const shortCycles = 4
		By(fmt.Sprintf("Waiting until the planner runs for the group and sees B's decode short %d cycles in a row", shortCycles))
		Eventually(func(g Gomega) {
			from, ok := sharePlanningFrom(controllerLogsSince(start.Add(-5*time.Minute)), ns)
			g.Expect(ok).To(BeTrue(), "no \"ledger started\" line for %s yet", ns)
			g.Expect(time.Now()).To(BeTemporally(">", from), "the share planner's quiet period for %s runs until %s", ns, from)
			short, read := roleShortFor(ns, modelB, domain.RoleDecode, from, shortCycles)
			g.Expect(short).To(BeTrue(), read)
		}, 10*time.Minute, 10*time.Second).Should(Succeed(), "B's decode never read short while the planner ran, so A's spare would be returned, not transferred")

		By("Lowering A's floor to 1, so it has replicas to give")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 1, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())

		// The set is found from the controller's own log, not by catching the
		// marked pods: a set on a simulator can start and release within ~30 s,
		// and a poll that must see both marks at once races it (a run missed
		// them entirely while the controller did exactly the right thing).
		By("Waiting for a donor set of two of A's replicas to start, funding B's decode")
		started := regexp.MustCompile(`"scope": "` + ns + `".*"set": "([^"]+)", "donor": "[^"]*/` + modelA +
			`/both", "receiver": "([^"]*)"`)
		var setID string
		Eventually(func(g Gomega) {
			members := map[string]int{}
			receivers := map[string]string{}
			returns := 0
			logs := controllerLogsSince(start)
			for _, line := range strings.Split(logs, "\n") {
				// Counted when a return STARTS, so one still draining, or
				// called off, is seen too; the "spare returned" line comes
				// only once the release lands.
				if strings.Contains(line, "transfer started") && strings.Contains(line, `"returned": true`) &&
					strings.Contains(line, ns) {
					returns++
				}
				if !strings.Contains(line, "transfer started") {
					continue
				}
				if m := started.FindStringSubmatch(line); m != nil {
					members[m[1]]++
					if m[2] != "" {
						receivers[m[1]] = m[2]
					}
				}
			}
			for id, n := range members {
				if n == 2 && strings.HasSuffix(receivers[id], "/"+modelB+"/decode") {
					setID = id
				}
			}
			// The returns are the diagnosis of the failure this used to hit
			// before roleShortOf gated the floor change: A's spare given back
			// to the quota, and B grown from idle GPUs with no transfer. The
			// planner's own lines and B's readings say why: a return goes
			// while no role that can grow reads short, so the decode
			// demand in the analyzer results is the first thing to read.
			g.Expect(setID).NotTo(BeEmpty(), "no set of two A replicas funding B's decode has started: %v; spare returns in %s: %d\n%s",
				members, ns, returns, sharePlannerLines(logs, ns, modelB, 80))
		}, 12*time.Minute, 5*time.Second).Should(Succeed())
		GinkgoWriter.Printf("donor set: %s\n", setID)

		// Marks, where the pods are still there to read: only A's, only this set.
		for _, p := range markedSharePods(Default, ns) {
			Expect(p.Labels["app"]).To(Equal(depA), "only A has replicas to give")
			var m struct {
				SetID string `json:"setID"`
			}
			Expect(json.Unmarshal([]byte(p.Annotations["llm-d.ai/utilization-share-transfer"]), &m)).To(Succeed())
			Expect(m.SetID).To(Equal(setID), "pod %s carries a mark of another set", p.Name)
		}

		By("Waiting for the new decode group to be ready and the set's transfer to complete")
		Eventually(func(g Gomega) {
			var l lwsv1.LeaderWorkerSet
			g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: lwsDecode}, &l)).To(Succeed())
			g.Expect(l.Status.ReadyReplicas).To(BeNumerically(">=", 2))
			// The set, from the controller's own account: two members started,
			// the primary (whose ID is the set's) raised the receiver and ended
			// done, and its contributor released -- ended done -- no later than
			// that raise: the receiver is raised only when all its holes are open.
			expectSetCompleted(g, controllerLogsSince(start), setID)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
	})
})

// logLineTime parses the RFC 3339 timestamp a controller log line starts with.
func logLineTime(line string) (time.Time, bool) {
	ts, _, ok := strings.Cut(line, "\t")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	return t, err == nil
}

// sharePlanningFrom is when the share planner starts planning the group of
// namespace ns: the planningFrom of its latest "ledger started" line. Until
// then the group is in its quiet period and nothing is planned for it.
func sharePlanningFrom(logs, ns string) (time.Time, bool) {
	from := regexp.MustCompile(`"planningFrom": "([^"]+)"`)
	var at time.Time
	found := false
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "Utilization share: ledger started") || !strings.Contains(line, `"scope": "`+ns+`"`) {
			continue
		}
		if m := from.FindStringSubmatch(line); m != nil {
			if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
				at, found = t, true
			}
		}
	}
	return at, found
}

// sharePlannerLines is the controller's account of the share planner for
// namespace ns, for a failure message: its "Utilization share" lines, and the
// saturation analyzer's results and shape holds for model, whose readings
// decide whether a role reads short. The first limit lines -- what decides
// whether A's spare is transferred or returned happens in the first minutes
// after the floor drops, and the analyzer logs every cycle until the timeout
// -- or a note that there were none.
func sharePlannerLines(logs, ns, model string, limit int) string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, ns) {
			continue
		}
		forModel := strings.Contains(line, `"modelID": "`+model+`"`)
		if strings.Contains(line, "Utilization share:") ||
			(forModel && strings.Contains(line, "analyzer-result") && strings.Contains(line, `"analyzer": "saturation"`)) ||
			(forModel && strings.Contains(line, "fleet-shape-hold")) {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "no share-planner or analyzer lines for " + ns + " in the controller log"
	}
	head := fmt.Sprintf("share planner and %s analyzer lines for %s (%d", model, ns, len(out))
	if len(out) > limit {
		out = out[:limit]
		head += fmt.Sprintf(", the first %d", limit)
	}
	return head + "):\n" + strings.Join(out, "\n")
}

// expectSetCompleted checks a donor set of two from the controller's log:
// both members started, the primary (whose ID is the set's) raised its
// receiver, the contributor ended done no later than that raise -- a
// contributor completes at its release -- and the primary ended done.
func expectSetCompleted(g Gomega, logs, setID string) {
	idOf := regexp.MustCompile(`"id": "([^"]+)"`)
	var members []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "transfer started") && strings.Contains(line, `"set": "`+setID+`"`) {
			if m := idOf.FindStringSubmatch(line); m != nil {
				members = append(members, m[1])
			}
		}
	}
	g.Expect(members).To(HaveLen(2), "want exactly the two members of set %s started", setID)
	var raised, contributorDone time.Time
	for _, line := range strings.Split(logs, "\n") {
		at, ok := logLineTime(line)
		if !ok {
			continue
		}
		for _, id := range members {
			if !strings.Contains(line, `"id": "`+id+`"`) {
				continue
			}
			switch {
			case id == setID && strings.Contains(line, "released, raising the receiver"):
				raised = at
			case id != setID && strings.Contains(line, "transfer ended") && strings.Contains(line, `"outcome": "done"`):
				contributorDone = at
			}
		}
	}
	g.Expect(raised.IsZero()).To(BeFalse(), "the set's primary never raised the receiver")
	g.Expect(contributorDone.IsZero()).To(BeFalse(), "the set's contributor never ended done")
	g.Expect(contributorDone).NotTo(BeTemporally(">", raised), "the contributor released after the receiver was raised")
	g.Expect(logs).To(MatchRegexp(`transfer ended.*"id": "` + setID + `".*"outcome": "done"`))
}
