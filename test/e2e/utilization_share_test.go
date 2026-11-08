package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d-incubation/llm-d-scaling-manager/test/e2e/fixtures"
	"github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// The utilization-share optimizer on a real cluster
// (docs/proposals/utilization-share-optimizer.md, section 12): two models
// share one namespace quota that is fully spent. Model A is idle and holds
// three of the four GPUs; model B is loaded and holds one.
//
//   - In shadow mode the optimizer reports the rebalance and touches nothing:
//     no pod is marked, A keeps its replicas. This is the negative control for
//     everything after it.
//   - Active, it moves GPUs from A to B the way section 6 says: A's pod is
//     marked with the lowest deletion cost and the transfer annotation, the
//     ReplicaSet removes exactly that pod, and B is raised only after it --
//     scale-down first.
//
// What the unit tests cannot show and this does: that the HPA honours the
// lowered target through KEDA, that the ReplicaSet really picks the marked pod,
// and that the controller's held-GPU count sees the release.
var _ = Describe("Utilization share optimizer", Label("full", "utilization-share"), Ordered, func() {
	const (
		quotaGPUs = 4
		msA       = "ushare-a"
		msB       = "ushare-b"
		modelA    = "ushare-model-a"
		modelB    = "ushare-model-b"
		loadJob   = "ushare-b-load"
		// The ScaledObjects' scale-down window: short, so a release takes
		// about a minute rather than the HPA default of five.
		window = int32(30)
	)
	var (
		ns         string
		product    string
		configName string
	)
	soA, soB := msA+"-so", msB+"-so"
	depA, depB := msA+"-decode", msB+"-decode"

	optimizer := func(shadow bool) fixtures.QuotaOption {
		return fixtures.WithOptimizer(map[string]any{
			"type":             "utilizationShare",
			"utilizationShare": map[string]any{"shadow": shadow},
		})
	}

	BeforeAll(func() {
		var ok bool
		_, product, ok = fixtures.DiscoverAcceleratorProduct(ctx, k8sClient)
		if !ok {
			Skip("no GPU product label on any schedulable node; the quota has no accelerator to bound")
		}

		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ushare-"},
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
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, configName,
			ns, product, quotaGPUs, optimizer(true))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })

		By("Creating model A at three replicas and model B at one: the quota is spent")
		replicas := func(n int32) fixtures.ModelServiceOption {
			return func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(n) }
		}
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msA, msA+"-pool", modelA, cfg.UseSimulator, 2, replicas(3))).To(Succeed())
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msB, msB+"-pool", modelB, cfg.UseSimulator, 2, replicas(1))).To(Succeed())
		for _, ms := range []string{msA, msB} {
			Expect(fixtures.EnsureService(ctx, k8sClient, ns, ms, ms+"-decode", 8000)).To(Succeed())
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
			g.Expect(a.Status.ReadyReplicas).To(Equal(int32(3)), unreadyPods(ns))
			b, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depB, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(b.Status.ReadyReplicas).To(Equal(int32(1)), unreadyPods(ns))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering both with the scaling manager. A's floor is 3 while in shadow, so today's optimizer cannot shrink it meanwhile")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 3, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soB, depB, depB, 1, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelB, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, soA)
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, soB)
		})

		By("Loading model B well past one replica's capacity")
		job := buildThroughputSustainedLoadJob(ns, loadJob, fmt.Sprintf("http://%s-service:8000/v1/completions", msB),
			modelB, 8, int64(1800))
		_, err = k8sClient.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("in shadow mode, evaluates the group and touches nothing", func() {
		// A's floor is pinned at 3 here, and A 3 + B 1 is the whole quota, so
		// there is no whole replica to move and nothing is actionable -- the
		// "would rebalance" line is (rightly) not logged. What this phase proves
		// is that the group IS evaluated, with B short, and that shadow mode
		// still touches nothing.
		pc := roundTripProm()
		By("Waiting for the shadow evaluation of this namespace's group, with B short")
		Eventually(func(g Gomega) {
			_, err := pc.QueryWithRetry(ctx, fmt.Sprintf(`llmsm_utilization_share_spare_gpus{scope=%q}`, ns))
			g.Expect(err).NotTo(HaveOccurred(), "no evaluation published for the group")
			h, err := pc.QueryWithRetry(ctx, fmt.Sprintf(`llmsm_utilization_share_headroom{model_name=%q}`, modelB))
			g.Expect(err).NotTo(HaveOccurred(), "no headroom published for B")
			g.Expect(h).To(BeNumerically("<", 0), "B is loaded past its one replica")
		}, 8*time.Minute, 15*time.Second).Should(Succeed())

		By("Holding for two minutes: no pod is marked and A keeps its replicas")
		Consistently(func(g Gomega) {
			g.Expect(markedSharePods(g, ns)).To(BeEmpty())
			a, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depA, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(*a.Spec.Replicas).To(Equal(int32(3)))
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("active, gives A's GPUs to B: the marked pod goes first, then B grows", func() {
		By("Switching the optimizer to act")
		switched := time.Now()
		_, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.LLMSMNamespace, configName,
			ns, product, quotaGPUs, optimizer(false))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(controllerLogsSince(switched)).To(ContainSubstring("Utilization share: ledger started"))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("Lowering A's floor to 1, so it has GPUs to give")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 1, 4, cfg.MonitoringNS,
			fixtures.WithScalingManagerTriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())

		By("Waiting for an A pod to be marked as a donor")
		var marked []corev1.Pod
		Eventually(func(g Gomega) {
			marked = markedSharePods(g, ns)
			g.Expect(marked).NotTo(BeEmpty())
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
		for _, p := range marked {
			Expect(p.Labels["app"]).To(Equal(depA), "only model A has GPUs to give")
			Expect(p.Annotations["controller.kubernetes.io/pod-deletion-cost"]).To(Equal("-1000"))
		}
		// Every marked pod, not only the first: the optimizer may start a second
		// transfer from A before the first lands, and the ReplicaSet removes
		// whichever marked pod it ranks first. Any of them leaving releases a GPU.
		markedNames := map[string]bool{}
		for _, p := range marked {
			markedNames[p.Name] = true
		}
		GinkgoWriter.Printf("donor pod marked: %s\n", marked[0].Name)
		livePods := func(g Gomega, dep string) map[string]bool {
			pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + dep})
			g.Expect(err).NotTo(HaveOccurred())
			live := map[string]bool{}
			for _, p := range pods.Items {
				if p.DeletionTimestamp == nil {
					live[p.Name] = true
				}
			}
			return live
		}
		before := livePods(Default, depA)

		By("Watching the release and the raise: a marked pod must start terminating before B grows")
		var released, raised time.Time
		var leaving []string
		Eventually(func(g Gomega) {
			now := time.Now()
			if released.IsZero() {
				for _, p := range markedSharePods(g, ns) {
					markedNames[p.Name] = true
				}
				live := livePods(g, depA)
				var gone []string
				for name := range before {
					if !live[name] {
						gone = append(gone, name)
					}
				}
				if len(gone) > 0 {
					leaving, released = gone, now
				}
			}
			b, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depB, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			if raised.IsZero() && *b.Spec.Replicas >= 2 {
				raised = now
			}
			g.Expect(released.IsZero()).To(BeFalse(), "no marked pod has left")
			g.Expect(raised.IsZero()).To(BeFalse(), "B has not been raised")
		}, 12*time.Minute, time.Second).Should(Succeed())
		GinkgoWriter.Printf("%v left at +%s, B raised at +%s\n", leaving,
			released.Sub(switched).Round(time.Second), raised.Sub(switched).Round(time.Second))
		Expect(raised).NotTo(BeTemporally("<", released), "B was raised before A's GPUs were released")

		By("Checking the ReplicaSet removed only marked pods")
		for _, name := range leaving {
			Expect(markedNames).To(HaveKey(name), "the ReplicaSet removed %s, which no transfer marked", name)
		}
		Eventually(func(g Gomega) {
			// A later transfer may mark, and the ReplicaSet remove, another A
			// pod while this waits: it is a marked pod too.
			for _, p := range markedSharePods(g, ns) {
				markedNames[p.Name] = true
			}
			live := livePods(g, depA)
			for name := range before {
				if !live[name] {
					g.Expect(markedNames).To(HaveKey(name), "the ReplicaSet removed %s, which no transfer marked", name)
				}
			}
			g.Expect(len(live)).To(BeNumerically("<=", 2))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("Waiting for B's new replica to be ready and the transfer to complete")
		Eventually(func(g Gomega) {
			b, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depB, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(b.Status.ReadyReplicas).To(BeNumerically(">=", 2))
			g.Expect(controllerLogsSince(switched)).To(MatchRegexp(`transfer ended.*\bdone\b`))
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
	})
})

// markedSharePods lists the pods in ns that carry a utilization-share transfer
// mark.
//
// It asserts through g, so inside a poll a transient API error retries the poll
// instead of failing the spec.
func markedSharePods(g Gomega, ns string) []corev1.Pod {
	pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	var out []corev1.Pod
	for _, p := range pods.Items {
		if _, ok := p.Annotations["llm-d.ai/utilization-share-transfer"]; ok {
			out = append(out, p)
		}
	}
	return out
}

// unreadyPods says why each pod in ns is not Ready, for a readiness wait's
// failure description: these specs stand up four to seven replicas on the
// one node the suite pins every workload to, and a node short of room shows
// as "2 to equal 4" unless the scheduler's own reason is read off the pod.
func unreadyPods(ns string) string {
	pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "listing pods in " + ns + ": " + err.Error()
	}
	var why []string
	for i := range pods.Items {
		p := &pods.Items[i]
		ready := false
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			why = append(why, unschedulableReason(p))
		}
	}
	if len(why) == 0 {
		return "every pod in " + ns + " is Ready"
	}
	return strings.Join(why, "\n")
}

// controllerLogsSince returns the controller pods' logs since t, all pods
// together: only the leader logs the optimizer, and which pod leads is not
// this spec's business. No controller Pod fails the spec: an empty log would
// otherwise read as "the controller said nothing".
func controllerLogsSince(t time.Time) string {
	pods, err := utils.ControllerPods(ctx, k8sClient, cfg.LLMSMNamespace, utils.ControllerDeploymentName)
	Expect(err).NotTo(HaveOccurred())
	Expect(pods).NotTo(BeEmpty(), "no Pod of Deployment %s in %s", utils.ControllerDeploymentName, cfg.LLMSMNamespace)
	since := metav1.NewTime(t)
	var b strings.Builder
	for _, p := range pods {
		// Bounded, so a stuck read fails the poll instead of stalling the spec.
		readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rc, err := k8sClient.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{SinceTime: &since}).Stream(readCtx)
		if err != nil {
			cancel()
			continue
		}
		out, _ := io.ReadAll(rc)
		_ = rc.Close()
		cancel()
		b.Write(out)
	}
	return b.String()
}

// roleShortOf reports whether the controller's two most recent analyzer
// results for model in ns ask its role for at least one more replica than it
// holds, and says what it read. "Short" here is the condition the
// utilization-share planner's returnSpare tests: while a role that can grow is
// short of its claim, a donor's spare goes to it by a transfer; while none is,
// the spare is RETURNED to the quota and the short role fills from idle GPUs
// later, with no transfer at all.
//
// A donor-set spec lowers the donor's floor and then waits for a transfer, so
// the receiver has to be seen short BEFORE the floor drops. This removes the
// race that was observed; it does not make a return impossible. returnSpare
// also lets the spare go while a short receiver is marked Unfunded -- every
// single donor misfits and the set was refused that cycle by a hold, the pace
// or the concurrency limit -- which no log line here can be gated on. Waiting only for
// the group to be evaluated was not enough: on a GitHub runner the receiver's
// load can take longer to show than the donor takes to qualify for a return,
// and the spare was given back -- the run ended with the receiver grown by the
// idle fill and no "transfer started" line, a spec failure that was the
// planner doing the right thing for what it could see.
//
// A role's required capacity is what it needs beyond what it holds (rc, or
// roleRC on a disaggregated model); it is short once that is positive. At
// least one whole replica's capacity is asked for, not just above zero, so a
// role a hair over is not counted as short for a cycle and then not.
func roleShortOf(ns, model, role string, since time.Time) (bool, string) {
	return roleShortFor(ns, model, role, since, 2)
}

// roleShortFor is roleShortOf over the last cycles analyzer results since
// since: short only when every one of them asks for a whole replica more.
func roleShortFor(ns, model, role string, since time.Time, cycles int) (bool, string) {
	type result struct {
		RC       float64            `json:"rc"`
		RoleRC   map[string]float64 `json:"roleRC"`
		Variants []struct {
			PRC  float64 `json:"prc"`
			Role string  `json:"role"`
		} `json:"variants"`
	}
	var recent []result
	for _, line := range strings.Split(controllerLogsSince(since), "\n") {
		// Saturation's result only: each analyzer logs its own line per cycle,
		// and the share planner reads saturation's alone.
		i := strings.Index(line, "analyzer-result")
		if i < 0 || !strings.Contains(line, `"modelID": "`+model+`"`) || !strings.Contains(line, `"namespace": "`+ns+`"`) ||
			!strings.Contains(line, `"analyzer": "saturation"`) {
			continue
		}
		j := strings.Index(line[i:], "{")
		if j < 0 {
			continue
		}
		var r result
		if json.Unmarshal([]byte(line[i+j:]), &r) == nil {
			recent = append(recent, r)
		}
	}
	if len(recent) < cycles {
		return false, fmt.Sprintf("%d analyzer results for %s in %s so far", len(recent), model, ns)
	}
	var why []string
	short := true
	for _, r := range recent[len(recent)-cycles:] {
		rc, ok := r.RoleRC[role]
		if !ok {
			rc = r.RC
		}
		prc := 0.0
		for _, v := range r.Variants {
			if v.Role == role {
				prc = max(prc, v.PRC)
			}
		}
		why = append(why, fmt.Sprintf("rc=%.1f replicaCapacity=%.1f", rc, prc))
		short = short && prc > 0 && rc >= prc
	}
	return short, fmt.Sprintf("%s/%s, last %d cycles: %s", model, role, cycles, strings.Join(why, "; "))
}
